package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/edsrzf/mmap-go"
	"github.com/google/shlex"
	"go.uber.org/ratelimit"
)

func main() {
	exitSignal := make(chan os.Signal, 1)
	signal.Notify(exitSignal, syscall.SIGINT, syscall.SIGTERM)
	debug := os.Getenv("DEBUG") == "1"

	Main(os.Args[1:], os.Stdout, os.Exit, exitSignal, debug)
}

// Main is the entry point of the program.
func Main(
	rawArgs []string,
	out io.Writer,
	exit func(int),
	exitSignal <-chan os.Signal,
	debug bool,
) {
	var args struct {
		TapeFileName     string `arg:"required,positional" placeholder:"TAPE-FILE" help:"the tape file containing HTTP requests"`
		QpsLimit         int    `arg:"-q,--" placeholder:"QPS" help:"the limt of qps, no limit if less than 1" default:"1"`
		ConcurrencyLimit int    `arg:"-c,--" placeholder:"CONCURRENCY" help:"the limt of concurrency, no limit if less than 1" default:"1"`
		Timeout          int    `arg:"-t,--" placeholder:"TIMEOUT" help:"the timeout of HTTP request in seconds, no timeout if less than 1" default:"10"`
		FollowRedirects  bool   `arg:"-f,--" help:"follow HTTP redirects" default:"false"`
		DryRun           bool   `arg:"-d,--" help:"dry-run mode" default:"false"`
	}
	{
		parser, err := arg.NewParser(arg.Config{Exit: exit, Out: out}, &args)
		if err != nil {
			fmt.Fprintln(out, err)
			exit(1)
		}
		parser.MustParse(rawArgs)
		if args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
			parser.Fail("should limit at least one of qps or concurrency")
		}
	}

	logger := log.New(out, "", log.LstdFlags)

	httpRequester, err := newHttpRequester(
		args.TapeFileName,
		args.QpsLimit,
		args.ConcurrencyLimit,
		time.Duration(args.Timeout)*time.Second,
		args.FollowRedirects,
		args.DryRun,
		debug,
		logger,
	)
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

const (
	tapeBufferSize           = 16 * 1024 * 1024
	tapePositionFileExt      = ".httpreplay-pos"
	failureTapeFileExt       = ".httpreplay-failure"
	flushFailureTapeInterval = 500 * time.Millisecond
)

type httpRequester struct {
	tapeFile            *os.File
	tapePositionTracker *tapePositionTracker
	failureTapeFile     *os.File
	failureTapeLock     sync.Mutex
	failureTape         *bufio.Writer
	qpsLimit            int
	concurrencyLimit    int
	httpClient          *http.Client
	dryRun              bool
	debug               bool
	logger              *log.Logger

	backgroundCtx context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	idleness      chan struct{}

	stats struct {
		concurrency atomic.Int64
		total       atomic.Int64
		successful  atomic.Int64
		failed      atomic.Int64
	}
}

func newHttpRequester(
	tapeFileName string,
	qpsLimit, concurrencyLimit int,
	timeout time.Duration,
	followRedirects bool,
	dryRun bool,
	debug bool,
	logger *log.Logger,
) (_ *httpRequester, returnedErr error) {
	tapeFile, err := os.Open(tapeFileName)
	if err != nil {
		return nil, fmt.Errorf("open tape file %q: %w", tapeFileName, err)
	}
	defer func() {
		if returnedErr != nil {
			tapeFile.Close()
		}
	}()
	tapePositionFileName := tapeFileName + tapePositionFileExt
	if dryRun {
		tapePositionFileName += ".dry-run"
	}
	tapePositionTracker, err := newTapePositionTracker(tapePositionFileName)
	if err != nil {
		return nil, fmt.Errorf("open tape position file %q: %w", tapePositionFileName, err)
	}
	defer func() {
		if returnedErr != nil {
			tapePositionTracker.Close()
		}
	}()
	failureTapeFileName := tapeFileName + failureTapeFileExt
	failureTapeFile, err := os.OpenFile(failureTapeFileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open failure tape file %q: %w", failureTapeFileName, err)
	}
	defer func() {
		if returnedErr != nil {
			failureTapeFile.Close()
		}
	}()
	if timeout < 0 {
		timeout = 0
	}
	httpClient := http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10000,
			MaxIdleConnsPerHost:   max(10, concurrencyLimit),
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   timeout,
			ExpectContinueTimeout: 1 * time.Second,
		},

		Timeout: timeout,
	}
	if !followRedirects {
		httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	r := &httpRequester{
		tapeFile:            tapeFile,
		tapePositionTracker: tapePositionTracker,
		failureTapeFile:     failureTapeFile,
		failureTape:         bufio.NewWriterSize(failureTapeFile, tapeBufferSize),
		qpsLimit:            qpsLimit,
		concurrencyLimit:    concurrencyLimit,
		httpClient:          &httpClient,
		dryRun:              dryRun,
		debug:               debug,
		logger:              logger,
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
		r.flushFailureTapePeriodically()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.logProgress()
	}()
}

func (r *httpRequester) Close() {
	r.stop()

	err := r.tapeFile.Close()
	if err != nil {
		r.logger.Printf("[WARN] failed to close tape file: %v", err)
	}

	err = r.tapePositionTracker.Close()
	if err != nil {
		r.logger.Printf("[WARN] failed to close tape position file: %v", err)
	}

	err = r.failureTapeFile.Close()
	if err != nil {
		r.logger.Printf("[WARN] failed to close failure tape file: %v", err)
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
			r.logger.Printf("[INFO] no more http requests")
		}
	}()

	acquireQpsToken := func() bool { return true }
	if r.qpsLimit >= 1 {
		limiter := ratelimit.New(r.qpsLimit)
		acquireQpsToken = func() bool {
			if r.backgroundCtx.Err() != nil {
				return false
			}
			limiter.Take()
			return true
		}
	}

	acquireConcurrencyToken := func() (func(), bool) { return func() {}, true }
	if r.concurrencyLimit >= 1 {
		concurrencyTokens := make(chan struct{}, r.concurrencyLimit)
		acquireConcurrencyToken = func() (func(), bool) {
			select {
			case <-r.backgroundCtx.Done():
				return nil, false
			case concurrencyTokens <- struct{}{}:
				return func() { <-concurrencyTokens }, true
			}
		}
	}

	r.logger.Println("===== Feel free to stop the program with CTRL+C; progress will be saved. =====")

	for tapePosition, line := range r.readTape() {
		curlCommand, err := parseCurlCommand(line)
		if err != nil {
			r.tapePositionTracker.UpdateTapePosition(tapePosition)
			r.logger.Printf("[WARN] failed to parse curl command from line %q: %v", line, err)
			continue
		}
		if curlCommand.URL == nil {
			// ignore empty curl command
			r.tapePositionTracker.UpdateTapePosition(tapePosition)
			continue
		}

		httpRequest, err := r.buildHttpRequest(curlCommand)
		if err != nil {
			r.tapePositionTracker.UpdateTapePosition(tapePosition)
			r.logger.Printf("[WARN] failed to build http request: %v", err)
			continue
		}

		ok := acquireQpsToken()
		if !ok {
			// exit
			return
		}

		releaseConcurrencyToken, ok := acquireConcurrencyToken()
		if !ok {
			// exit
			return
		}

		r.tapePositionTracker.UpdateTapePosition(tapePosition)
		wg.Add(1)
		go func() {
			defer func() {
				wg.Done()
				releaseConcurrencyToken()
			}()

			r.doHttpRequest(httpRequest, line)
		}()
	}

	noMoreHttpRequests = true
}

func (r *httpRequester) readTape() iter.Seq2[int64, string] {
	lastTapePosition := r.tapePositionTracker.TapePosition()

	return func(yield func(int64, string) bool) {
		scanner := bufio.NewScanner(r.tapeFile)
		scanner.Buffer(nil, tapeBufferSize)

		for tapePosition := int64(0); scanner.Scan(); tapePosition++ {
			if tapePosition <= lastTapePosition {
				continue
			}

			line := scanner.Text()

			if !yield(tapePosition, line) {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			r.logger.Printf("[WARN] failed to scan tape file: %v", err)
		}
	}
}

type curlCommand struct {
	URL     *string
	Request string
	Header  []string
	Data    *string
}

func parseCurlCommand(line string) (curlCommand, error) {
	args, err := shlex.Split(line)
	if err != nil {
		return curlCommand{}, fmt.Errorf("split line: %w", err)
	}
	if len(args) == 0 {
		return curlCommand{}, nil
	}

	var curlCommand1 curlCommand
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
			curlCommand1.Header = append(curlCommand1.Header, v)
			continue
		}
		if v, err, ok := getFlagValue(arg, "-d", "--data", popNextArg); ok {
			if err != nil {
				return curlCommand{}, err
			}
			curlCommand1.Data = &v
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return curlCommand{}, fmt.Errorf("unsupported flag: %s", arg)
		}
		if curlCommand1.URL == nil {
			curlCommand1.URL = &arg
		}
	}
	if curlCommand1.URL == nil {
		return curlCommand{}, fmt.Errorf("missing url")
	}
	if curlCommand1.Request == "" {
		curlCommand1.Request = "GET"
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
			return "", fmt.Errorf("missing flag value for %s/%s", flagName, longFlagName), true
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
	var rawBody *string
	var body io.Reader
	if curlCommand.Data != nil {
		rawBody = curlCommand.Data
		body = strings.NewReader(*rawBody)
	}
	httpRequest, err := http.NewRequest(curlCommand.Request, *curlCommand.URL, body)
	if err != nil {
		return nil, fmt.Errorf("new http request: %w", err)
	}
	if len(curlCommand.Header) >= 1 {
		reader := textproto.NewReader(
			bufio.NewReader(
				strings.NewReader(
					strings.Join(curlCommand.Header, "\r\n") +
						"\r\n\r\n",
				),
			),
		)
		header, err := reader.ReadMIMEHeader()
		if err != nil {
			return nil, fmt.Errorf("read MIME header: %w", err)
		}
		httpRequest.Header = http.Header(header)
	}
	if r.debug {
		if rawBody == nil {
			r.logger.Printf("[DEBUG] http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			r.logger.Printf("[DEBUG] http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, *rawBody)
		}
	}
	return httpRequest, nil
}

func (r *httpRequester) doHttpRequest(httpRequest *http.Request, line string) {
	r.stats.concurrency.Add(1)
	defer r.stats.concurrency.Add(-1)

	r.stats.total.Add(1)
	if r.dryRun {
		if httpRequest.Body == nil {
			r.logger.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			data, _ := io.ReadAll(httpRequest.Body)
			rawBody := string(data)
			r.logger.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, rawBody)
		}
		r.stats.successful.Add(1)
		return
	}

	resp, err := r.httpClient.Do(httpRequest)
	if err != nil {
		if r.debug {
			r.logger.Printf("[DEBUG] failed to do http request: %v", err)
		}
		r.stats.failed.Add(1)
		r.recordFailedHttpRequest(line)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n := resp.StatusCode / 100; !(n >= 2 && n <= 3) {
		if r.debug {
			r.logger.Printf("[DEBUG] %v %q responded exception status code: %v", httpRequest.Method, httpRequest.URL.String(), resp.StatusCode)
		}
		r.stats.failed.Add(1)
		r.recordFailedHttpRequest(line)
		return
	}
	r.stats.successful.Add(1)
}

func (r *httpRequester) recordFailedHttpRequest(line string) {
	r.failureTapeLock.Lock()
	_, err1 := r.failureTape.WriteString(line)
	err2 := r.failureTape.WriteByte('\n')
	r.failureTapeLock.Unlock()

	err := errors.Join(err1, err2)
	if err != nil {
		r.logger.Printf("[WARN] failed to write failure tape file: %v", err)
	}
}

func (r *httpRequester) flushFailureTapePeriodically() {
	ticker := time.NewTicker(flushFailureTapeInterval)
	defer ticker.Stop()

	for next := true; next; {
		select {
		case <-r.idleness:
			next = false
		case <-ticker.C:
		}

		r.failureTapeLock.Lock()
		err := r.failureTape.Flush()
		r.failureTapeLock.Unlock()
		if err != nil {
			r.logger.Printf("[WARN] failed to flush failure tape: %v", err)
		}
	}

	if n := r.stats.failed.Load(); n >= 1 {
		r.logger.Printf("[INFO] failure tape flushed; failedHttpRequestCount=%v", n)
	}
}

func (r *httpRequester) logProgress() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	prevTotal := int64(0)
	for next := true; next; {
		select {
		case <-r.idleness:
			next = false
		case <-ticker.C:
		}

		tapePosition := r.tapePositionTracker.TapePosition()
		concurrency := r.stats.concurrency.Load()
		total := r.stats.total.Load()
		qps := total - prevTotal
		prevTotal = total
		successful := r.stats.successful.Load()
		failed := r.stats.failed.Load()
		successRate := float64(successful) / (float64(successful) + float64(failed))

		var title string
		if next {
			title = "current progress"
		} else {
			title = "final progress"
		}
		r.logger.Printf("[INFO] %s: tapePosition=%d qps=%d concurrency=%d successful=%d failed=%d successRate=%.2f",
			title, tapePosition, qps, concurrency, successful, failed, successRate)
	}
}

func (r *httpRequester) Idleness() <-chan struct{} { return r.idleness }

type tapePositionTracker struct {
	file *os.File
	mMap mmap.MMap

	tapePosition atomic.Int64
	buffer       *tapePositionBuffer
}

type tapePositionBuffer [20]byte

func newTapePositionTracker(tapePositionFileName string) (_ *tapePositionTracker, returnedErr error) {
	file, err := os.OpenFile(tapePositionFileName, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnedErr != nil {
			file.Close()
		}
	}()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	var tapePosition int64
	if len(data) >= 1 {
		tapePositionStr := string(data)
		tapePosition, err = strconv.ParseInt(strings.TrimSpace(tapePositionStr), 10, 64)
		if err != nil || tapePosition < 0 {
			return nil, fmt.Errorf("invalid tape position %q", tapePositionStr)
		}

		_, err = file.Seek(0, 0)
		if err != nil {
			return nil, err
		}
		err = file.Truncate(0)
		if err != nil {
			return nil, err
		}
	}
	var buffer tapePositionBuffer
	dumpTapePosition(tapePosition, &buffer)
	_, err = file.Write(buffer[:])
	if err != nil {
		return nil, err
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

	t := &tapePositionTracker{
		file:   file,
		mMap:   mMap,
		buffer: (*tapePositionBuffer)(mMap),
	}
	t.tapePosition.Store(tapePosition)
	return t, nil
}

func (t *tapePositionTracker) Close() error {
	t.buffer = nil
	err1 := t.mMap.Unmap()
	err2 := t.file.Close()
	return errors.Join(err1, err2)
}

func (t *tapePositionTracker) TapePosition() int64 { return t.tapePosition.Load() }

func (t *tapePositionTracker) UpdateTapePosition(tapePosition int64) {
	t.tapePosition.Store(tapePosition)
	dumpTapePosition(tapePosition, t.buffer)
}

func dumpTapePosition(tapePosition int64, buffer *tapePositionBuffer) {
	*buffer = [...]byte{'0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '\n'}
	i := len(buffer) - 2
	for ; tapePosition >= 1; tapePosition /= 10 {
		buffer[i] = '0' + byte(tapePosition%10)
		i--
	}
}
