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
		TapFileName       string `arg:"required,positional" placeholder:"TAP-FILE" help:"The tap file containing http requests"`
		FailedTapFileName string `arg:"required,positional" placeholder:"FAILED-TAP-FILE" help:"The tap file containing failed http requests"`
		QpsLimit          int    `arg:"-q,--" placeholder:"QPS" help:"The limt of qps, no limit if less than 1" default:"1"`
		ConcurrencyLimit  int    `arg:"-c,--" placeholder:"CONCURRENCY" help:"The limt of concurrency, no limit if less than 1" default:"1"`
		Timeout           int    `arg:"-t,--" placeholder:"TIMEOUT" help:"The timeout of http request in seconds, no limit if less than 1" default:"10"`
	}
	p := arg.MustParse(&args)
	if args.QpsLimit < 1 && args.ConcurrencyLimit < 1 {
		p.Fail("should limit at least one of qps or concurrency")
	}

	r, err := newHttpRequester(args.TapFileName, args.FailedTapFileName, args.QpsLimit, args.ConcurrencyLimit, time.Duration(args.Timeout)*time.Second)
	if err != nil {
		log.Fatalf("[FATAL] failed to create http requester: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c

	r.Close()
}

type httpRequester struct {
	tapFile          *os.File
	tapPosition      atomic.Int64
	failureTapFile   *os.File
	failureTapWriter *bufio.Writer
	qpsLimit         int
	concurrencyLimit int
	httpClient       *http.Client

	backgroundCtx context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup

	successfulRequestCount atomic.Int64
	failedRequestCount     atomic.Int64
}

func newHttpRequester(
	tapFileName string,
	failureTapFileName string,
	qpsLimit, concurrencyLimit int,
	timeout time.Duration,
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
		log.Println("[WARN] failed to load tap position: " + err.Error())
	}
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
		failureTapWriter: bufio.NewWriterSize(failureTapFile, 16*1024*1024),
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
		r.run()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.autoSaveTapPosition()
	}()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.showStats()
	}()
}

func (r *httpRequester) run() {
	acquireQpsToken := func() (func(), bool) { return func() {}, true }
	if r.qpsLimit >= 1 {
		limiter := ratelimit.New(r.qpsLimit)
		acquireQpsToken = func() (func(), bool) {
			limiter.Take()
			if r.backgroundCtx.Err() != nil {
				return nil, false
			}
			return func() {}, true
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
		func() {
			releaseQpsToken, ok := acquireQpsToken()
			if !ok {
				return
			}
			defer releaseQpsToken()

			releaseConcurrencyToken, ok := acquireConcurrencyToken()
			if !ok {
				return
			}
			defer releaseConcurrencyToken()

			r.tapPosition.Add(1)
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				r.doHttpRequest(httpRequest, line)
			}()
		}()
	}
}

func (r *httpRequester) doHttpRequest(request *http.Request, line string) {
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
	_, err1 := r.failureTapWriter.WriteString(line)
	if err1 != nil {
		err = err1
	}
	err2 := r.failureTapWriter.WriteByte('\n')
	if err2 != nil {
		err = err2
	}
	if err != nil {
		log.Println("[WARN] failed to write failure tap file: " + err.Error())
	}
}

func (r *httpRequester) autoSaveTapPosition() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.backgroundCtx.Done():
			return
		case <-ticker.C:
			err := saveTapPosition(r.tapFile.Name(), int(r.tapPosition.Load()))
			if err != nil {
				log.Println("[WARN] failed to save tap position: " + err.Error())
			}
		}
	}
}

func (r *httpRequester) showStats() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.backgroundCtx.Done():
			return
		case <-ticker.C:
			successfulCount := r.successfulRequestCount.Load()
			failedCount := r.failedRequestCount.Load()
			totalCount := successfulCount + failedCount
			if totalCount == 0 {
				continue
			}
			successRate := float64(successfulCount) / float64(totalCount) * 100
			log.Printf("[INFO] successful: %d, failed: %d, total: %d, success rate: %.2f%%\n",
				successfulCount, failedCount, totalCount, successRate)
		}
	}
}

func (r *httpRequester) Close() {
	r.stop()

	r.tapFile.Close()
	err := r.failureTapWriter.Flush()
	if err != nil {
		log.Println("[WARN] failed to flush failure tap file: " + err.Error())
	}
	r.failureTapFile.Close()
	err = saveTapPosition(r.tapFile.Name(), int(r.tapPosition.Load()))
	if err != nil {
		log.Println("[WARN] failed to save tap position: " + err.Error())
	}
}

func (r *httpRequester) stop() {
	r.cancel()
	r.wg.Wait()
}

func loadTapPosition(tapFileName string) (int, error) {
	tapPositionFileName := makeTapPositionFileName(tapFileName)
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
	tapPositionFileName := makeTapPositionFileName(tapFileName)
	tapPositionStr := strconv.FormatUint(uint64(tapPosition), 10)
	err := os.WriteFile(tapPositionFileName, []byte(tapPositionStr), 0644)
	return err
}

func makeTapPositionFileName(tapFileName string) string { return tapFileName + ".pos" }

func readHttpRequests(reader io.Reader, numberOfHttpRequestsToSkip int) iter.Seq2[*http.Request, string] {
	return func(yield func(*http.Request, string) bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(nil, 16*1024*1024)

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
		URL string   `arg:"required,positional"`
		X   string   `arg:"-X,--" default:"GET"`
		H   []string `arg:"-H,--"`
	}
	rawArgs, err := shlex.Split(line)
	if err != nil {
		return nil, fmt.Errorf("split line: %w", err)
	}
	p, err := arg.NewParser(arg.Config{}, &args)
	if err != nil {
		return nil, fmt.Errorf("new argument parser: %w", err)
	}
	err = p.Parse(rawArgs)
	if err != nil {
		return nil, fmt.Errorf("parse arguments: %w", err)
	}
	httpRequest, err := http.NewRequest(args.X, args.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("new http request: %w", err)
	}
	if len(args.H) >= 1 {
		reader := textproto.NewReader(
			bufio.NewReader(
				strings.NewReader(strings.Join(args.H, "\r\n") + "\r\n"),
			),
		)
		header, err := reader.ReadMIMEHeader()
		if err != nil {
			return nil, fmt.Errorf("read MIME header: %w", err)
		}
		httpRequest.Header = http.Header(header)
	}
	return httpRequest, nil
}
