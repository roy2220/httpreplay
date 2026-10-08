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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-shellwords"
	"go.uber.org/ratelimit"
)

const (
	tapePositionFileExt = ".httpreplay-pos"
	failureTapeFileExt  = ".httpreplay-failure"
	dryRunFileExt       = ".dry-run"
)

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
	DefaultUserAgent        []string
	Logger                  *log.Logger
	Debug                   bool
}

type deliverySemantics int

const (
	deliveryAtLeastOnce deliverySemantics = iota
	deliveryAtMostOnce
)

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
		httpRequest.Header["User-Agent"] = r.config.DefaultUserAgent
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
