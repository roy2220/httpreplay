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
	"github.com/google/shlex"
	"go.uber.org/ratelimit"
)

func main() {
	var args struct {
		TapeFileName     string `arg:"required,positional" placeholder:"TAPE-FILE" help:"The tape file containing http requests"`
		QpsLimit         int    `arg:"-q,--" placeholder:"QPS" help:"The limt of qps, no limit if less than 1" default:"1"`
		ConcurrencyLimit int    `arg:"-c,--" placeholder:"CONCURRENCY" help:"The limt of concurrency, no limit if less than 1" default:"1"`
		Timeout          int    `arg:"-t,--" placeholder:"TIMEOUT" help:"The timeout of http request in seconds, no timeout if less than 1" default:"10"`
		DryRun           bool   `arg:"-d,--" help:"dry-run mode" default:"false"`
	}
	if parser := arg.MustParse(&args); args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
		parser.Fail("should limit at least one of qps or concurrency")
	}

	httpRequester, err := newHttpRequester(args.TapeFileName, args.QpsLimit, args.ConcurrencyLimit, time.Duration(args.Timeout)*time.Second, args.DryRun)
	if err != nil {
		log.Fatalf("[FATAL] failed to create http requester: %v", err)
	}
	defer httpRequester.Close()

	exit := make(chan os.Signal, 1)
	signal.Notify(exit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-httpRequester.Idleness():
	case <-exit:
		log.Printf("[INFO] http requester is stopping...")
	}
}

const (
	bufferSize               = 16 * 1024 * 1024
	failureTapeFileExt       = ".httpreplay-failure"
	tapePositionFileExt      = ".httpreplay-pos"
	flushFailureTapeInterval = 500 * time.Millisecond
	saveTapePositionInterval = 200 * time.Millisecond
)

var debug = os.Getenv("DEBUG") == "1"

type httpRequester struct {
	tapeFile         *os.File
	tapePosition     atomic.Int64
	failureTapeFile  *os.File
	failureTapeLock  sync.Mutex
	failureTape      *bufio.Writer
	qpsLimit         int
	concurrencyLimit int
	httpClient       *http.Client
	dryRun           bool

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
	dryRun bool,
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
	tapePosition, err := loadTapePosition(tapeFileName, dryRun)
	if err == nil {
		log.Printf("[INFO] tape position loaded; tapePosition=%v", tapePosition)
	} else {
		log.Printf("[WARN] failed to load tape position: %v", err)
	}
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
	r := &httpRequester{
		tapeFile:         tapeFile,
		failureTapeFile:  failureTapeFile,
		failureTape:      bufio.NewWriterSize(failureTapeFile, bufferSize),
		qpsLimit:         qpsLimit,
		concurrencyLimit: concurrencyLimit,
		httpClient: &http.Client{
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
		},
		dryRun:   dryRun,
		idleness: make(chan struct{}),
	}
	r.tapePosition.Store(int64(tapePosition))
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
		r.saveTapePositionPeriodically()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.logProgress()
	}()
}

func (r *httpRequester) dispatchHttpRequests() {
	var wg sync.WaitGroup
	var noMoreHttpRequests bool
	defer func() {
		wg.Wait()
		close(r.idleness)

		if noMoreHttpRequests {
			log.Printf("[INFO] no more http requests")
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

	for httpRequest, line := range readHttpRequests(r.tapeFile, int(r.tapePosition.Load())) {
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

		r.tapePosition.Add(1)
		wg.Add(1)
		go func() {
			defer releaseConcurrencyToken()
			defer wg.Done()
			r.doHttpRequest(httpRequest, line)
		}()
	}

	noMoreHttpRequests = true
}

func (r *httpRequester) doHttpRequest(httpRequest *http.Request, line string) {
	r.stats.concurrency.Add(1)
	defer r.stats.concurrency.Add(-1)

	r.stats.total.Add(1)
	if r.dryRun {
		if httpRequest.Body == nil {
			log.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			data, _ := io.ReadAll(httpRequest.Body)
			rawBody := string(data)
			log.Printf("[INFO] <dry-run> http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, rawBody)
		}
		r.stats.successful.Add(1)
		return
	}

	resp, err := r.httpClient.Do(httpRequest)
	if err != nil {
		if debug {
			log.Printf("[DEBUG] failed to do http request: %v", err)
		}
		r.stats.failed.Add(1)
		r.recordFailedHttpRequest(line)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if debug {
			log.Printf("[DEBUG] %v %q responded non-2xx status code: %v", httpRequest.Method, httpRequest.URL.String(), resp.StatusCode)
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
		log.Printf("[WARN] failed to write failure tape file: %v", err)
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
			log.Printf("[WARN] failed to flush failure tape: %v", err)
		}
	}

	log.Printf("[INFO] failure tape flushed; failedHttpRequestCount=%v", r.stats.failed.Load())
}

func (r *httpRequester) saveTapePositionPeriodically() {
	ticker := time.NewTicker(saveTapePositionInterval)
	defer ticker.Stop()

	var tapePosition int
	for next := true; next; {
		select {
		case <-r.idleness:
			next = false
		case <-ticker.C:
		}

		tapePosition = int(r.tapePosition.Load())
		err := saveTapePosition(r.tapeFile.Name(), tapePosition, r.dryRun)
		if err != nil {
			log.Printf("[WARN] failed to save tape position: %v", err)
		}
	}

	log.Printf("[INFO] tape position saved; tapePosition=%v", tapePosition)
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

		tapePosition := r.tapePosition.Load()
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
		log.Printf("[INFO] %s: tapePosition=%d qps=%d concurrency=%d successful=%d failed=%d successRate=%.2f",
			title, tapePosition, qps, concurrency, successful, failed, successRate)
	}
}

func (r *httpRequester) Idleness() <-chan struct{} { return r.idleness }

func (r *httpRequester) Close() {
	r.stop()

	err := r.tapeFile.Close()
	if err != nil {
		log.Printf("[WARN] failed to close tape file: %v", err)
	}

	err = r.failureTapeFile.Close()
	if err != nil {
		log.Printf("[WARN] failed to close failure tape file: %v", err)
	}
}

func (r *httpRequester) stop() {
	r.cancel()
	r.wg.Wait()
}

func loadTapePosition(tapeFileName string, dryRun bool) (int, error) {
	tapePositionFileName := makeTapePositionFileName(tapeFileName, dryRun)
	data, err := os.ReadFile(tapePositionFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	tapePositionStr := string(data)
	tapePosition, err := strconv.ParseUint(tapePositionStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid tape position %q from file %q", tapePositionStr, tapePositionFileName)
	}
	return int(tapePosition), nil
}

func saveTapePosition(tapeFileName string, tapePosition int, dryRun bool) error {
	tapePositionFileName := makeTapePositionFileName(tapeFileName, dryRun)
	tapePositionStr := strconv.FormatUint(uint64(tapePosition), 10)
	err := os.WriteFile(tapePositionFileName, []byte(tapePositionStr), 0644)
	return err
}

func makeTapePositionFileName(tapeFileName string, dryRun bool) string {
	tapePositionFileName := tapeFileName + tapePositionFileExt
	if dryRun {
		tapePositionFileName += ".dry-run"
	}
	return tapePositionFileName
}

func readHttpRequests(reader io.Reader, numberOfHttpRequestsToSkip int) iter.Seq2[*http.Request, string] {
	return func(yield func(*http.Request, string) bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(nil, bufferSize)

		for scanner.Scan() {
			if numberOfHttpRequestsToSkip >= 1 {
				numberOfHttpRequestsToSkip--
				continue
			}

			line := scanner.Text()

			httpRequest, err := parseHttpRequest(line)
			if err != nil {
				log.Printf("[WARN] failed to parse http request from line %q: %v", line, err)
				continue
			}

			if !yield(httpRequest, line) {
				return
			}
		}

		if err := scanner.Err(); err != nil {
			log.Printf("[WARN] failed to read tape file: %v", err)
		}
	}
}

func parseHttpRequest(line string) (*http.Request, error) {
	var args struct {
		URL    string   `arg:"required,positional"`
		Method string   `arg:"-X,--request" default:"GET"`
		Header []string `arg:"separate,-H,--header"`
		Data   *string  `arg:"-d,--data"`
	}
	rawArgs, err := shlex.Split(line)
	if err != nil {
		return nil, fmt.Errorf("split line: %w", err)
	}
	parser, err := arg.NewParser(arg.Config{}, &args)
	if err != nil {
		return nil, fmt.Errorf("new argument parser: %w", err)
	}
	err = parser.Parse(rawArgs)
	if err != nil {
		return nil, fmt.Errorf("parse arguments: %w", err)
	}
	var rawBody *string
	var body io.Reader
	if args.Data != nil {
		rawBody = args.Data
		body = strings.NewReader(*rawBody)
	}
	httpRequest, err := http.NewRequest(args.Method, args.URL, body)
	if err != nil {
		return nil, fmt.Errorf("new http request: %w", err)
	}
	if len(args.Header) >= 1 {
		reader := textproto.NewReader(
			bufio.NewReader(
				strings.NewReader(
					strings.Join(args.Header, "\r\n") +
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
	if debug {
		if rawBody == nil {
			log.Printf("[DEBUG] http request: method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			log.Printf("[DEBUG] http request: method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, *rawBody)
		}
	}
	return httpRequest, nil
}
