package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"math"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/alexflint/go-arg"
	"github.com/edsrzf/mmap-go"
	"github.com/mattn/go-shellwords"
	"go.uber.org/ratelimit"
)

const (
	tapePositionFileExt      = ".httpreplay-pos"
	failureTapeFileExt       = ".httpreplay-failure"
	dryRunFileExt            = ".dry-run"
	minFailureTapeBufferSize = 4 * 1024
	minSyncToDiskInterval    = 10 * time.Millisecond
)

var (
	version          string
	defaultUserAgent []string
)

func init() {
	version = "unknown"
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if v := buildInfo.Main.Version; v != "" {
			version = v
		}
	}
	defaultUserAgent = []string{"httpreplay/" + strings.TrimPrefix(version, "v")}
}

func main() {
	debug := os.Getenv("DEBUG") == "1"
	exitSignal := make(chan os.Signal, 1)
	signal.Notify(exitSignal, syscall.SIGINT, syscall.SIGTERM)

	Main(os.Args[1:], os.Stdout, debug, os.Exit, exitSignal)
}

// Main is the entry point of the program.
func Main(
	rawArgs []string,
	output io.Writer,
	debug bool,
	exit func(int),
	exitSignal <-chan os.Signal,
) {
	logger := log.New(output, "", log.LstdFlags)

	httpRequester, err := newHttpRequester(mustParseHttpRequesterConfig(rawArgs, output, logger, debug, exit))
	if err != nil {
		logger.Printf("[FATAL] failed to create http requester: %v", err)
		exit(1)
	}
	defer httpRequester.Close()

	select {
	case <-httpRequester.Idleness():
	case <-exitSignal:
		logger.Printf("[INFO] http requester is stopping...")
	}
}

type httpRequester struct {
	config httpRequesterConfig

	tapeReader          *tapeReader
	tapePositionTracker *tapePositionTracker
	failureTapeFile     *os.File
	failureTapeLock     sync.Mutex
	failureTape         *bufio.Writer
	failureTapeIsDirty  bool
	httpClient          *http.Client

	backgroundCtx context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	idleness      chan struct{}

	stats struct {
		concurrency atomic.Int64
		skipped     atomic.Int64
		successful  atomic.Int64
		failed      atomic.Int64
		done        atomic.Int64
	}
}

type httpRequesterConfig struct {
	TapeFileName            string
	MaxNumberOfHttpRequests int
	QpsLimit                int
	ConcurrencyLimit        int
	RequestTimeout          time.Duration
	FollowRedirects         bool
	DryRun                  bool
	DeliverySemantics       deliverySemantics
	FailureTapeBufferSize   int
	SyncToDiskInterval      time.Duration
	Logger                  *log.Logger
	Debug                   bool
}

type deliverySemantics int

const (
	deliveryAtLeastOnce deliverySemantics = iota
	deliveryAtMostOnce
)

type args struct {
	TapeFileName            string  `arg:"required,positional" placeholder:"TAPE-FILE" help:"path to the tape file containing HTTP requests"`
	MaxNumberOfHttpRequests int     `arg:"-n,--" placeholder:"NUM" help:"stop after NUM requests, no limit if less than 0" default:"-1"`
	QpsLimit                int     `arg:"-q,--" placeholder:"QPS" help:"maximum QPS, no limit if less than 1" default:"1"`
	ConcurrencyLimit        int     `arg:"-c,--" placeholder:"CONCURRENCY" help:"maximum concurrency, no limit if less than 1" default:"1"`
	RequestTimeout          float64 `arg:"-t,--" placeholder:"SECONDS" help:"request timeout in seconds (float), no timeout if less than or equal to 0.0" default:"10.0"`
	FollowRedirects         bool    `arg:"-f,--" help:"follow HTTP redirects" default:"false"`
	DryRun                  bool    `arg:"-d,--" help:"dry-run mode" default:"false"`
	DeliverySemantics       string  `arg:"--,--delivery-semantics" placeholder:"SEMANTICS" help:"delivery semantics for resuming after a crash (one of at-least-once, at-most-once)" default:"at-least-once"`
	FailureTapeBufferSize   int     `arg:"--,--failure-tape-buffer-size" placeholder:"BYTES" help:"buffer size of the failure tape file, in bytes; values too small will be raised" default:"16777216"`
	SyncToDiskInterval      float64 `arg:"--,--sync-to-disk-interval" placeholder:"SECONDS" help:"interval in seconds (float) for syncing the failure tape and position file to disk; values too small will be raised" default:"0.5"`
}

func (args) Version() string { return "httpreplay " + version }

func mustParseHttpRequesterConfig(
	rawArgs []string,
	output io.Writer,
	logger *log.Logger,
	debug bool,
	exit func(int),
) httpRequesterConfig {
	var args args
	var deliverySemantics deliverySemantics
	{
		parser, err := arg.NewParser(arg.Config{Exit: exit, Out: output}, &args)
		if err != nil {
			fmt.Fprintln(output, err)
			exit(1)
		}
		parser.MustParse(rawArgs)

		if args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
			parser.Fail("should limit at least one of qps or concurrency")
		}
		if math.IsNaN(args.RequestTimeout) {
			parser.Fail("request timeout should not be NaN")
		}
		switch args.DeliverySemantics {
		case "at-least-once":
			deliverySemantics = deliveryAtLeastOnce
		case "at-most-once":
			deliverySemantics = deliveryAtMostOnce
		default:
			parser.Fail("delivery semantics should be one of at-least-once or at-most-once")
		}
		if math.IsNaN(args.SyncToDiskInterval) {
			parser.Fail("sync to disk interval should not be NaN")
		}
	}

	return httpRequesterConfig{
		TapeFileName:            args.TapeFileName,
		MaxNumberOfHttpRequests: args.MaxNumberOfHttpRequests,
		QpsLimit:                args.QpsLimit,
		ConcurrencyLimit:        args.ConcurrencyLimit,
		RequestTimeout:          max(time.Duration(args.RequestTimeout*float64(time.Second)), 0),
		FollowRedirects:         args.FollowRedirects,
		DryRun:                  args.DryRun,
		FailureTapeBufferSize:   max(args.FailureTapeBufferSize, minFailureTapeBufferSize),
		SyncToDiskInterval:      max(time.Duration(args.SyncToDiskInterval*float64(time.Second)), minSyncToDiskInterval),
		DeliverySemantics:       deliverySemantics,
		Logger:                  logger,
		Debug:                   debug,
	}
}

func newHttpRequester(config httpRequesterConfig) (_ *httpRequester, returnedErr error) {
	tapeReader, err := newTapeReader(config.TapeFileName)
	if err != nil {
		return nil, fmt.Errorf("create tape reader: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			tapeReader.Close()
		}
	}()
	tapePositionFileName := config.TapeFileName + tapePositionFileExt
	if config.DryRun {
		tapePositionFileName += dryRunFileExt
	}
	tapePositionTracker, err := newTapePositionTracker(tapePositionFileName)
	if err != nil {
		return nil, fmt.Errorf("create tape position tracker: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			tapePositionTracker.Close()
		}
	}()
	failureTapeFileName := config.TapeFileName + failureTapeFileExt
	failureTapeFile, err := os.OpenFile(failureTapeFileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open failure tape file: %w", err)
	}
	defer func() {
		if returnedErr != nil {
			var failureTapeFileIsEmpty bool
			if fi, err := failureTapeFile.Stat(); err == nil && fi.Size() == 0 {
				failureTapeFileIsEmpty = true
			}
			failureTapeFile.Close()
			if failureTapeFileIsEmpty {
				os.Remove(failureTapeFile.Name())
			}
		}
	}()
	httpClient := http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   config.RequestTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10000,
			MaxIdleConnsPerHost:   max(10, config.ConcurrencyLimit),
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   config.RequestTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},

		Timeout: config.RequestTimeout,
	}
	if !config.FollowRedirects {
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	r := &httpRequester{
		config: config,

		tapeReader:          tapeReader,
		tapePositionTracker: tapePositionTracker,
		failureTapeFile:     failureTapeFile,
		failureTape:         bufio.NewWriterSize(failureTapeFile, config.FailureTapeBufferSize),
		httpClient:          &httpClient,
		idleness:            make(chan struct{}),
	}
	r.start()
	return r, nil
}

func (r *httpRequester) start() {
	r.backgroundCtx, r.cancel = context.WithCancel(context.Background())

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.dispatchHttpRequests()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.syncToDiskPeriodically()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.logProgress()
	}()
}

func (r *httpRequester) Close() {
	r.stop()

	err := r.tapeReader.Close()
	if err != nil {
		r.config.Logger.Printf("[WARN] failed to close tape reader: %v", err)
	}

	err = r.tapePositionTracker.Close()
	if err != nil {
		r.config.Logger.Printf("[WARN] failed to close tape position tracker: %v", err)
	}

	var failureTapeFileIsEmpty bool
	if fi, err := r.failureTapeFile.Stat(); err == nil && fi.Size() == 0 {
		failureTapeFileIsEmpty = true
	}
	err = r.failureTapeFile.Close()
	if err != nil {
		r.config.Logger.Printf("[WARN] failed to close failure tape file: %v", err)
	}
	if failureTapeFileIsEmpty {
		err = os.Remove(r.failureTapeFile.Name())
		if err != nil {
			r.config.Logger.Printf("[WARN] failed to remove empty failure tape file: %v", err)
		}
	}
}

func (r *httpRequester) stop() {
	r.cancel()
	r.wg.Wait()
}

func (r *httpRequester) dispatchHttpRequests() {
	var wg sync.WaitGroup
	var noMoreHttpRequests bool
	defer func() {
		wg.Wait()
		close(r.idleness)

		if noMoreHttpRequests {
			r.config.Logger.Print("[INFO] no more http requests")
		}
	}()

	parser := shellwords.NewParser()
	parser.ParseComment = true

	skipLine := func(tapePosition int64) {
		r.stats.skipped.Add(1)
		r.tapePositionTracker.SubmitTapePosition(tapePosition)
	}

	acquireConcurrencyToken := func() (func(), bool) { return func() {}, true }
	if r.config.ConcurrencyLimit >= 1 {
		concurrencyTokens := make(chan struct{}, r.config.ConcurrencyLimit)
		acquireConcurrencyToken = func() (func(), bool) {
			select {
			case <-r.backgroundCtx.Done():
				return nil, false
			case concurrencyTokens <- struct{}{}:
				return func() { <-concurrencyTokens }, true
			}
		}
	}

	acquireQpsToken := func() bool { return true }
	if r.config.QpsLimit >= 1 {
		limiter := ratelimit.New(r.config.QpsLimit)
		acquireQpsToken = func() bool {
			if r.backgroundCtx.Err() != nil {
				return false
			}
			limiter.Take()
			return true
		}
	}

	r.config.Logger.Print("===== Feel free to stop the program with CTRL+C; progress will be saved. =====")

	var numberOfHttpRequests int
	for tapePosition, line := range r.readTape() {
		args, err := parser.Parse(line)
		if err != nil {
			r.config.Logger.Printf("[WARN] failed to parse args; tapePosition=%v: %v", tapePosition, err)
			skipLine(tapePosition)
			continue
		}
		if len(args) == 0 {
			skipLine(tapePosition)
			continue
		}

		curlCommand, err := parseCurlCommand(args)
		if err != nil {
			r.config.Logger.Printf("[WARN] failed to parse curl command; tapePosition=%v: %v", tapePosition, err)
			skipLine(tapePosition)
			continue
		}

		httpRequest, err := r.buildHttpRequest(curlCommand)
		if err != nil {
			r.config.Logger.Printf("[WARN] failed to build http request; tapePosition=%v: %v", tapePosition, err)
			skipLine(tapePosition)
			continue
		}

		numberOfHttpRequests++
		if r.config.MaxNumberOfHttpRequests >= 0 && numberOfHttpRequests > r.config.MaxNumberOfHttpRequests {
			r.config.Logger.Print("[INFO] reached max number of http requests")
			return // exit
		}

		releaseConcurrencyToken, ok := acquireConcurrencyToken()
		if !ok {
			return // exit
		}

		ok = acquireQpsToken()
		if !ok {
			releaseConcurrencyToken()
			return // exit
		}

		if r.config.DeliverySemantics == deliveryAtMostOnce {
			r.tapePositionTracker.SubmitTapePosition(tapePosition)
		}

		wg.Add(1)
		go func() {
			defer func() {
				wg.Done()
				releaseConcurrencyToken()
			}()

			r.doHttpRequest(httpRequest, line)

			if r.config.DeliverySemantics == deliveryAtLeastOnce {
				r.tapePositionTracker.SubmitTapePosition(tapePosition)
			}
		}()
	}

	noMoreHttpRequests = true
}

func (r *httpRequester) readTape() iter.Seq2[int64, string] {
	lastTapePosition := r.tapePositionTracker.TapePosition()

	return func(yield func(int64, string) bool) {
		for tapePosition := int64(1); ; tapePosition++ {
			line, ok := r.tapeReader.ReadLine()
			if !ok {
				break
			}
			if tapePosition <= lastTapePosition {
				continue
			}
			if !yield(tapePosition, line) {
				return
			}
		}
	}
}

type curlCommand struct {
	URL     string
	Request string
	Header  *bytes.Buffer
	Data    *bytes.Buffer
}

func parseCurlCommand(args []string) (curlCommand, error) {
	var (
		curlCommand1           curlCommand
		contentTypeHeaderIsSet bool
		urlIsPresent           bool
	)
	i := 0
	n := len(args)
	popNextArg := func() (string, bool) {
		i++
		if i < n {
			return args[i], true
		}
		return "", false
	}
	for ; i < n; i++ {
		arg := args[i]
		if v, err, ok := getFlagValue(arg, "-X", "--request", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			curlCommand1.Request = v
			continue
		}
		if v, err, ok := getFlagValue(arg, "-H", "--header", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			key, _, ok := strings.Cut(v, ":")
			if !ok {
				return curlCommand{}, fmt.Errorf("invalid header: %v", v)
			}
			if strings.ToLower(key) == "content-type" {
				contentTypeHeaderIsSet = true
			}
			header := curlCommand1.Header
			if header == nil {
				header = new(bytes.Buffer)
				curlCommand1.Header = header
			} else {
				header.WriteString("\r\n")
			}
			header.WriteString(v)
			continue
		}
		if v, err, ok := getFlagValue(arg, "-d", "--data", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			data := curlCommand1.Data
			if data == nil {
				data = new(bytes.Buffer)
				curlCommand1.Data = data
			} else {
				data.WriteByte('&')
			}
			data.WriteString(v)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return curlCommand{}, fmt.Errorf("unsupported flag: %v", arg)
		}
		if !urlIsPresent {
			curlCommand1.URL = arg
			urlIsPresent = true
		}
	}
	if !urlIsPresent {
		return curlCommand{}, fmt.Errorf("missing url")
	}
	if curlCommand1.URL == "" {
		return curlCommand{}, fmt.Errorf("empty url")
	}
	if curlCommand1.Data != nil {
		if curlCommand1.Request == "" {
			curlCommand1.Request = http.MethodPost
		}
		if !contentTypeHeaderIsSet {
			header := curlCommand1.Header
			if header == nil {
				header = new(bytes.Buffer)
				curlCommand1.Header = header
			} else {
				header.WriteString("\r\n")
			}
			header.WriteString("Content-Type: application/x-www-form-urlencoded")
		}
	}
	if curlCommand1.Request == "" {
		curlCommand1.Request = http.MethodGet
	}
	if curlCommand1.Header != nil {
		curlCommand1.Header.WriteString("\r\n\r\n")
	}
	return curlCommand1, nil
}

func getFlagValue(arg, flagName, longFlagName string, popNextArg func() (string, bool)) (string, error, bool) {
	longFlagMode := false
	v := strings.TrimPrefix(arg, flagName)
	if len(v) == len(arg) {
		longFlagMode = true
		v = strings.TrimPrefix(arg, longFlagName)
	}
	if len(v) == len(arg) {
		return "", nil, false
	}
	if v == "" {
		var ok bool
		v, ok = popNextArg()
		if !ok {
			return "", fmt.Errorf("missing flag value for %v/%v", flagName, longFlagName), true
		}
	} else if v[0] == '=' {
		v = v[1:]
	} else {
		if longFlagMode {
			return "", nil, false
		}
	}
	return v, nil, true
}

func (r *httpRequester) buildHttpRequest(curlCommand curlCommand) (*http.Request, error) {
	rawBody := curlCommand.Data
	var body io.Reader
	if rawBody != nil {
		body = rawBody
	}
	httpRequest, err := http.NewRequest(curlCommand.Request, curlCommand.URL, body)
	if err != nil {
		return nil, fmt.Errorf("new http request: %w", err)
	}
	if curlCommand.Header != nil {
		reader := textproto.NewReader(bufio.NewReader(curlCommand.Header))
		header, err := reader.ReadMIMEHeader()
		if err != nil {
			return nil, fmt.Errorf("read MIME header: %w", err)
		}
		httpRequest.Header = http.Header(header)
	}
	if vs := httpRequest.Header["Host"]; len(vs) >= 1 {
		httpRequest.Host = vs[0]
	}
	if len(httpRequest.Header["User-Agent"]) == 0 {
		httpRequest.Header["User-Agent"] = defaultUserAgent
	}
	if r.config.Debug {
		if rawBody == nil {
			r.config.Logger.Printf("[DEBUG] http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL, httpRequest.Header)
		} else {
			r.config.Logger.Printf("[DEBUG] http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL, httpRequest.Header, rawBody.Bytes())
		}
	}
	return httpRequest, nil
}

func (r *httpRequester) doHttpRequest(httpRequest *http.Request, line string) {
	r.stats.concurrency.Add(1)
	defer func() {
		r.stats.concurrency.Add(-1)
		r.stats.done.Add(1)
	}()

	if r.config.DryRun {
		if httpRequest.Body == nil {
			r.config.Logger.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			data, _ := io.ReadAll(httpRequest.Body)
			rawBody := string(data)
			r.config.Logger.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, rawBody)
		}
		r.stats.successful.Add(1)
		return
	}

	resp, err := r.httpClient.Do(httpRequest)
	if err != nil {
		if r.config.Debug {
			r.config.Logger.Printf("[DEBUG] failed to do http request: %v", err)
		}
		r.stats.failed.Add(1)
		line = fmt.Sprintf("%v  # ERROR: %v", line, strings.ReplaceAll(err.Error(), "\n", ""))
		r.recordFailedHttpRequest(line)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n := resp.StatusCode / 100; !(n >= 2 && n <= 3) {
		if r.config.Debug {
			r.config.Logger.Printf("[DEBUG] %v %q responded exception status code: %v", httpRequest.Method, httpRequest.URL.String(), resp.StatusCode)
		}
		r.stats.failed.Add(1)
		line = fmt.Sprintf("%v  # STATUS CODE: %v", line, resp.StatusCode)
		r.recordFailedHttpRequest(line)
		return
	}
	r.stats.successful.Add(1)
}

func (r *httpRequester) recordFailedHttpRequest(line string) {
	r.failureTapeLock.Lock()
	_, err1 := r.failureTape.WriteString(line)
	err2 := r.failureTape.WriteByte('\n')
	r.failureTapeIsDirty = true
	r.failureTapeLock.Unlock()

	err := errors.Join(err1, err2)
	if err != nil {
		r.config.Logger.Printf("[WARN] failed to write failure tape file: %v", err)
	}
}

func (r *httpRequester) syncToDiskPeriodically() {
	ticker := time.NewTicker(r.config.SyncToDiskInterval)
	defer ticker.Stop()

	for next := true; next; {
		select {
		case <-r.idleness:
			next = false
		case <-ticker.C:
		}

		if r.config.Debug {
			r.config.Logger.Print("[DEBUG] syncing to disk...")
		}

		{
			var err1, err2 error
			r.failureTapeLock.Lock()
			if r.failureTapeIsDirty {
				err1 = r.failureTape.Flush()
				err2 = r.failureTapeFile.Sync()
				if err1 == nil && err2 == nil {
					r.failureTapeIsDirty = false
				}
			}
			r.failureTapeLock.Unlock()

			err := errors.Join(err1, err2)
			if err != nil {
				r.config.Logger.Printf("[WARN] failed to sync failure tape to disk: %v", err)
			}
		}

		{
			err := r.tapePositionTracker.Sync()
			if err != nil {
				r.config.Logger.Printf("[WARN] failed to sync tape position to disk: %v", err)
			}
		}
	}
}

func (r *httpRequester) logProgress() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	prevDone := int64(0)
	for next := true; next; {
		select {
		case <-r.idleness:
			next = false
		case <-ticker.C:
		}

		var title string
		if next {
			title = "current progress"
		} else {
			title = "final progress"
		}

		tapePosition := r.tapePositionTracker.TapePosition()
		concurrency := r.stats.concurrency.Load()
		skipped := r.stats.skipped.Load()
		done := r.stats.done.Load()
		qps := done - prevDone
		prevDone = done
		successful := r.stats.successful.Load()
		failed := r.stats.failed.Load()

		var successRate string
		if total := successful + failed; total == 0 {
			successRate = "N/A"
		} else {
			successRate = fmt.Sprintf("%.2f", float64(successful)/float64(total))
		}

		r.config.Logger.Printf("[INFO] %v: tapePosition=%v qps=%v concurrency=%v skipped=%v successful=%v failed=%v successRate=%v",
			title, tapePosition, qps, concurrency, skipped, successful, failed, successRate)
	}
}

func (r *httpRequester) Idleness() <-chan struct{} { return r.idleness }

type tapeReader struct {
	mMap        mmap.MMap
	unreadLines string
}

func newTapeReader(tapeFileName string) (_ *tapeReader, returnedErr error) {
	file, err := os.Open(tapeFileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !fileInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not regular file", tapeFileName)
	}
	if fileInfo.Size() == 0 {
		return &tapeReader{}, nil
	}

	mMap, err := mmap.Map(file, mmap.RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnedErr != nil {
			mMap.Unmap()
		}
	}()

	return &tapeReader{
		mMap:        mMap,
		unreadLines: b2s(mMap),
	}, nil
}

func (r *tapeReader) Close() error {
	if r.mMap == nil {
		return nil
	}
	r.unreadLines = ""
	return r.mMap.Unmap()
}

func (r *tapeReader) ReadLine() (string, bool) {
	if r.unreadLines == "" {
		return "", false
	}
	var line string
	line, r.unreadLines, _ = strings.Cut(r.unreadLines, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, true
}

type tapePositionTracker struct {
	mMap         mmap.MMap
	tapePosition *int64

	lock                 sync.Mutex
	pendingTapePositions map[int64]struct{}
}

func newTapePositionTracker(tapePositionFileName string) (_ *tapePositionTracker, returnedErr error) {
	file, err := os.OpenFile(tapePositionFileName, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	switch n := fileInfo.Size(); n {
	case 0:
		err = file.Truncate(8)
		if err != nil {
			return nil, err
		}
	case 8:
	default:
		return nil, fmt.Errorf("invalid tape position file %q with size %v", tapePositionFileName, n)
	}

	mMap, err := mmap.Map(file, mmap.RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnedErr != nil {
			mMap.Unmap()
		}
	}()

	tapePosition := (*int64)(unsafe.Pointer(unsafe.SliceData(mMap)))
	if *tapePosition < 0 {
		return nil, fmt.Errorf("invalid tape position %v from file %q", *tapePosition, tapePositionFileName)
	}

	return &tapePositionTracker{
		mMap:                 mMap,
		tapePosition:         tapePosition,
		pendingTapePositions: map[int64]struct{}{},
	}, nil
}

func (t *tapePositionTracker) Close() error {
	t.tapePosition = nil
	return t.mMap.Unmap()
}

func (t *tapePositionTracker) TapePosition() int64 { return atomic.LoadInt64(t.tapePosition) }

func (t *tapePositionTracker) SubmitTapePosition(tapePosition int64) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if tapePosition == *t.tapePosition+1 {
		for n := len(t.pendingTapePositions); n >= 1; {
			delete(t.pendingTapePositions, tapePosition+1)
			nn := len(t.pendingTapePositions)
			if nn == n {
				break
			}
			tapePosition++
			n = nn
		}
		atomic.StoreInt64(t.tapePosition, tapePosition)
	} else {
		t.pendingTapePositions[tapePosition] = struct{}{}
	}
}

func (t *tapePositionTracker) Sync() error { return t.mMap.Flush() }

func b2s(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }
