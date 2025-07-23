package main

import (
	"bufio"
	"context"
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
		TapFileName      string `arg:"required,positional" placeholder:"TAP-FILE" help:"The tap file containing http requests"`
		QpsLimit         int    `arg:"-q,--" placeholder:"QPS" help:"The limt of qps, no limit if less than 1" default:"1"`
		ConcurrencyLimit int    `arg:"-c,--" placeholder:"CONCURRENCY" help:"The limt of concurrency, no limit if less than 1" default:"1"`
		Timeout          int    `arg:"-t,--" placeholder:"TIMEOUT" help:"The timeout of http request in seconds, no limit if less than 1" default:"10"`
	}
	parser := arg.MustParse(&args)
	if args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
		parser.Fail("should limit at least one of qps or concurrency")
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	httpRequester, err := newHttpRequester(args.TapFileName, args.QpsLimit, args.ConcurrencyLimit, time.Duration(args.Timeout)*time.Second, c)
	if err != nil {
		log.Fatalf("[FATAL] failed to create http requester: %v", err)
	}
	defer httpRequester.Close()

	<-c
}

const (
	bufferSize              = 16 * 1024 * 1024
	failureTapFileExt       = ".httpreplay-failure"
	tapPositionFileExt      = ".httpreplay-pos"
	flushFailureTapInterval = 500 * time.Millisecond
	saveTapPositionInterval = 200 * time.Millisecond
)

var debugMode = os.Getenv("DEBUG") == "1"

type httpRequester struct {
	tapFile          *os.File
	tapPosition      atomic.Int64
	failureTapFile   *os.File
	failureTapLock   sync.Mutex
	failureTap       *bufio.Writer
	qpsLimit         int
	concurrencyLimit int
	httpClient       *http.Client
	idleSignal       chan<- os.Signal

	backgroundCtx context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup

	concurrency            atomic.Int64
	requestCount           atomic.Int64
	successfulRequestCount atomic.Int64
	failedRequestCount     atomic.Int64
}

func newHttpRequester(
	tapFileName string,
	qpsLimit, concurrencyLimit int,
	timeout time.Duration,
	idleSignal chan<- os.Signal,
) (_ *httpRequester, returnedErr error) {
	tapFile, err := os.Open(tapFileName)
	if err != nil {
		return nil, fmt.Errorf("open tap file %q: %w", tapFileName, err)
	}
	defer func() {
		if returnedErr != nil {
			tapFile.Close()
		}
	}()
	tapPosition, err := loadTapPosition(tapFileName)
	if err != nil {
		log.Printf("[WARN] failed to load tap position: %v", err)
	}
	failureTapFileName := tapFileName + failureTapFileExt
	failureTapFile, err := os.OpenFile(failureTapFileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open failure tap file %q: %w", failureTapFileName, err)
	}
	defer func() {
		if returnedErr != nil {
			failureTapFile.Close()
		}
	}()
	if timeout < 0 {
		timeout = 0
	}
	r := &httpRequester{
		tapFile:          tapFile,
		failureTapFile:   failureTapFile,
		failureTap:       bufio.NewWriterSize(failureTapFile, bufferSize),
		qpsLimit:         qpsLimit,
		concurrencyLimit: concurrencyLimit,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   timeout,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          0,
				MaxIdleConnsPerHost:   max(10, concurrencyLimit),
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   timeout,
				ExpectContinueTimeout: 1 * time.Second,
			},

			Timeout: timeout,
		},
		idleSignal: idleSignal,
	}
	r.tapPosition.Store(int64(tapPosition))
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
		r.flushFailureTapPeriodically()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.saveTapPositionPeriodically()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.logStats()
	}()
}

func (r *httpRequester) dispatchHttpRequests() {
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

	for httpRequest, line := range readHttpRequests(r.tapFile, int(r.tapPosition.Load())) {
		if next := func() bool {
			ok := acquireQpsToken()
			if !ok {
				return false
			}

			releaseConcurrencyToken, ok := acquireConcurrencyToken()
			if !ok {
				return false
			}
			defer releaseConcurrencyToken()

			r.tapPosition.Add(1)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.doHttpRequest(httpRequest, line)
			}()
			return true
		}(); !next {
			return
		}
	}

	select {
	case r.idleSignal <- syscall.Signal(0):
	}
	log.Printf("[INFO] all http requests have been processed")
}

func (r *httpRequester) doHttpRequest(request *http.Request, line string) {
	r.concurrency.Add(1)
	defer r.concurrency.Add(-1)

	r.requestCount.Add(1)
	resp, err := r.httpClient.Do(request)
	if err != nil {
		r.failedRequestCount.Add(1)
		r.recordFailedHttpRequest(line)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		r.failedRequestCount.Add(1)
		r.recordFailedHttpRequest(line)
		return
	}
	r.successfulRequestCount.Add(1)
}

func (r *httpRequester) recordFailedHttpRequest(line string) {
	var err error

	r.failureTapLock.Lock()
	_, err1 := r.failureTap.WriteString(line)
	if err1 != nil {
		err = err1
	}
	err2 := r.failureTap.WriteByte('\n')
	if err2 != nil {
		err = err2
	}
	r.failureTapLock.Unlock()

	if err != nil {
		log.Printf("[WARN] failed to write failure tap file: %v", err)
	}
}
func (r *httpRequester) flushFailureTapPeriodically() {
	ticker := time.NewTicker(flushFailureTapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.backgroundCtx.Done():
			return
		case <-ticker.C:
		}

		r.failureTapLock.Lock()
		err := r.failureTap.Flush()
		r.failureTapLock.Unlock()
		if err != nil {
			log.Printf("[WARN] failed to flush failure tap: %v", err)
		}
	}
}

func (r *httpRequester) saveTapPositionPeriodically() {
	ticker := time.NewTicker(saveTapPositionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.backgroundCtx.Done():
			return
		case <-ticker.C:
		}

		err := saveTapPosition(r.tapFile.Name(), int(r.tapPosition.Load()))
		if err != nil {
			log.Printf("[WARN] failed to save tap position: %v", err)
		}
	}
}

func (r *httpRequester) logStats() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	prevCount := int64(0)
	for next := true; next; {
		select {
		case <-r.backgroundCtx.Done():
			next = false
		case <-ticker.C:
		}

		position := r.tapPosition.Load()
		concurrency := r.concurrency.Load()
		count := r.requestCount.Load()
		qps := count - prevCount
		prevCount = count
		successfulCount := r.successfulRequestCount.Load()
		failedCount := r.failedRequestCount.Load()
		successRate := float64(successfulCount) / float64(count)
		log.Printf("[INFO] position: %d, qps: %d, concurrency: %d, successful: %d, failed: %d, success rate: %.2f",
			position, qps, concurrency, successfulCount, failedCount, successRate)
	}
}

func (r *httpRequester) Close() {
	r.stop()

	r.tapFile.Close()
	err := r.failureTap.Flush()
	if err != nil {
		log.Printf("[WARN] failed to flush failure tap: %v", err)
	}
	r.failureTapFile.Close()
	err = saveTapPosition(r.tapFile.Name(), int(r.tapPosition.Load()))
	if err != nil {
		log.Printf("[WARN] failed to save tap position: %v", err)
	}
}

func (r *httpRequester) stop() {
	r.cancel()
	r.wg.Wait()
}

func loadTapPosition(tapFileName string) (int, error) {
	tapPositionFileName := tapFileName + tapPositionFileExt
	data, err := os.ReadFile(tapPositionFileName)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	tapPositionStr := string(data)
	tapPosition, err := strconv.ParseUint(tapPositionStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid tap position %q from file %q", tapPositionStr, tapPositionFileName)
	}
	return int(tapPosition), nil
}

func saveTapPosition(tapFileName string, tapPosition int) error {
	tapPositionFileName := tapFileName + tapPositionFileExt
	tapPositionStr := strconv.FormatUint(uint64(tapPosition), 10)
	err := os.WriteFile(tapPositionFileName, []byte(tapPositionStr), 0644)
	return err
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
			log.Printf("[WARN] failed to read tap file: %v", err)
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
	if debugMode {
		if rawBody == nil {
			log.Printf("[DEBUG] http request, method=%q url=%q header=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header)
		} else {
			log.Printf("[DEBUG] http request, method=%q url=%q header=%q body=%q", httpRequest.Method, httpRequest.URL.String(), httpRequest.Header, *rawBody)
		}
	}
	return httpRequest, nil
}
