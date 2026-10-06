package main_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	. "github.com/roy2220/httpreplay"
	"github.com/stretchr/testify/require"
)

func mockExit(n int) {
	panic(fmt.Sprintf("exit(%d)", n))
}

type request struct {
	Method string
	Host   string
	URI    string
	Header http.Header
	Body   string
}

func TestGeneral(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			Host:   r.Host,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if k == "Content-Type" || k == "User-Agent" || strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()

		if strings.HasSuffix(r.RequestURI, "v=1") {
			http.Redirect(w, r, server.URL+"/api?v=6", http.StatusFound)
		}
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0
-H'X-Foo: Bar' %[1]s/api?v=1
%[1]s/api?v=2 -X=GET -H 'X-Foo: Bar'  # my comment
%[1]s/api?v=3 -H'X-Foo: Bar' --request POST --data='{"key": "value"}' --header 'X-Hello: World' --header='Content-Type: application/json'
%[1]s/api?v=4 -d 'foo=bar'
%[1]s/api?v=5 -d 'foo=bar' -d 'key=val' -H 'Host: example.com' -H 'User-Agent: Test'

 # invalid requests
%[1]s/api?v=7 --unsupported-flag
%[1]s/api?v=8 -H
%[1]s/api?v=9 -d 'foo
%[1]s/api?v=10 -X "\n"
%[1]s/api?v=11 -H ':::'
-X GET
''
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Regexp(t, "final progress:.* tapePosition=15", out.String())
	require.Regexp(t, "final progress:.* skipped=9", out.String())
	require.Regexp(t, "final progress:.* successful=6", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 6)
	slices.SortFunc(requests, func(x, y request) int { return strings.Compare(x.URI, y.URI) })
	host := server.Listener.Addr().String()
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=0",
		Header: http.Header{
			"User-Agent": []string{"httpreplay/(devel)"},
		},
		Body: "",
	}, requests[0])
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=1",
		Header: http.Header{
			"X-Foo":      []string{"Bar"},
			"User-Agent": []string{"httpreplay/(devel)"},
		},
		Body: "",
	}, requests[1])
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=2",
		Header: http.Header{
			"X-Foo":      []string{"Bar"},
			"User-Agent": []string{"httpreplay/(devel)"},
		},
		Body: "",
	}, requests[2])
	require.Equal(t, request{
		Method: "POST",
		Host:   host,
		URI:    "/api?v=3",
		Header: http.Header{
			"X-Foo":        []string{"Bar"},
			"X-Hello":      []string{"World"},
			"Content-Type": []string{"application/json"},
			"User-Agent":   []string{"httpreplay/(devel)"},
		},
		Body: `{"key": "value"}`,
	}, requests[3])
	require.Equal(t, request{
		Method: "POST",
		Host:   host,
		URI:    "/api?v=4",
		Header: http.Header{
			"Content-Type": []string{"application/x-www-form-urlencoded"},
			"User-Agent":   []string{"httpreplay/(devel)"},
		},
		Body: "foo=bar",
	}, requests[4])
	require.Equal(t, request{
		Method: "POST",
		Host:   "example.com",
		URI:    "/api?v=5",
		Header: http.Header{
			"Content-Type": []string{"application/x-www-form-urlencoded"},
			"User-Agent":   []string{"Test"},
		},
		Body: "foo=bar&key=val",
	}, requests[5])
	require.NoFileExists(t, tapeFilePath+".httpreplay-failure")
}

func TestFollowRedirects(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			Host:   r.Host,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if k == "Content-Type" || strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()

		if strings.HasSuffix(r.RequestURI, "v=1") {
			http.Redirect(w, r, server.URL+"/api?v=4", http.StatusFound)
		}
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0
%[1]s/api?v=1 -H 'X-Foo: Bar'
%[1]s/api?v=2 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=3 -X POST -d '{"key": "value"}' -H 'Content-Type: application/json'
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "1",
			"-q", "0",
			"-f",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=4", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 5)
	slices.SortFunc(requests, func(x, y request) int { return strings.Compare(x.URI, y.URI) })
	host := server.Listener.Addr().String()
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=4",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	}, requests[4])
	require.NoFileExists(t, tapeFilePath+".httpreplay-failure")
}

func TestDryRun(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			Host:   r.Host,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if k == "Content-Type" || strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0
%[1]s/api?v=1 -H 'X-Foo: Bar'
%[1]s/api?v=2 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=3 -X POST -d '{"key": "value"}' -H 'Content-Type: application/json'
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			"-d",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=4", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 0)
	require.NoFileExists(t, tapeFilePath+".httpreplay-pos")
	require.FileExists(t, tapeFilePath+".httpreplay-pos.dry-run")
	require.NoFileExists(t, tapeFilePath+".httpreplay-failure")
}

func TestProgressResumption(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			Host:   r.Host,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if k == "Content-Type" || strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0

%[1]s/api?v=1 -H 'X-Foo: Bar'
%[1]s/api?v=2 -X GET -H 'X-Foo: Bar' # my comment

   # %[1]s/api?v=333 -X GET -H 'X-Foo: Bar'
   %[1]s/api?v=3 -X POST -d '{"key": "value"}' -H 'Content-Type: application/json'`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	exitSignal := make(chan os.Signal, 1)
	time.AfterFunc(500*time.Millisecond, func() { exitSignal <- syscall.SIGTERM })

	Main(
		[]string{
			"-c", "1",
			"-q", "1",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		exitSignal,
	)

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	{
		f, err := os.OpenFile(tapeFilePath, os.O_APPEND|os.O_WRONLY, 0644)
		require.NoError(t, err)
		_, err = fmt.Fprintf(f, "\n%s/api?v=4 -d 'foo=bar'\n", server.URL)
		require.NoError(t, err)
		f.Close()
	}

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	server.Close()
	require.Len(t, requests, 5)
	slices.SortFunc(requests, func(x, y request) int { return strings.Compare(x.URI, y.URI) })
	host := server.Listener.Addr().String()
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=0",
		Header: nil,
		Body:   "",
	}, requests[0])
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=1",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	}, requests[1])
	require.Equal(t, request{
		Method: "GET",
		Host:   host,
		URI:    "/api?v=2",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	}, requests[2])
	require.Equal(t, request{
		Method: "POST",
		Host:   host,
		URI:    "/api?v=3",
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"key": "value"}`,
	}, requests[3])
	require.Equal(t, request{
		Method: "POST",
		Host:   host,
		URI:    "/api?v=4",
		Header: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:   "foo=bar",
	}, requests[4])
}

func TestFailureTape(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if k == "Content-Type" || strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()
		switch {
		case strings.HasSuffix(r.RequestURI, "v=0"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.RequestURI, "v=2"):
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0
%[1]s/api?v=1 -H 'X-Foo: Bar'
%[1]s/api?v=2 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=3 -X POST -d '{"key": "value"}' -H 'Content-Type: application/json'
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "1",
			"-q", "0",
			"-t", "1",
			"--failure-tape-buffer-size", "1024",
			"--sync-to-disk-interval", "1",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=2", out.String())
	require.Regexp(t, "final progress:.* failed=2", out.String())

	server.Close()
	require.Len(t, requests, 4)

	data, err := os.ReadFile(tapeFilePath + ".httpreplay-failure")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(`
%[1]s/api?v=0  # STATUS CODE: 500
%[1]s/api?v=2 -X GET -H 'X-Foo: Bar'  # ERROR: Get "%[1]s/api?v=2": context deadline exceeded (Client.Timeout exceeded while awaiting headers)
`, server.URL)[1:], string(data))
}

func TestMaxNumberOfHttpRequests(t *testing.T) {
	t.Parallel()

	var requestsLock sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestsLock.Lock()
		requests = append(requests, request{
			Method: r.Method,
			URI:    r.RequestURI,
			Body:   string(body),
		})
		requestsLock.Unlock()
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=0
%[1]s/api?v=1
%[1]s/api?v=2
%[1]s/api?v=3
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-n", "2",
			"-c", "1",
			"-q", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Contains(t, out.String(), "reached max number of http requests")
	require.Regexp(t, "final progress:.* tapePosition=2", out.String())
	require.Regexp(t, "final progress:.* successful=2", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 2)
	slices.SortFunc(requests, func(x, y request) int { return strings.Compare(x.URI, y.URI) })
	require.Equal(t, request{Method: "GET", URI: "/api?v=0", Body: ""}, requests[0])
	require.Equal(t, request{Method: "GET", URI: "/api?v=1", Body: ""}, requests[1])
	require.NoFileExists(t, tapeFilePath+".httpreplay-failure")
}

func TestBadArgs(t *testing.T) {
	t.Parallel()

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	require.Panics(t, func() {
		Main(
			[]string{
				"-c", "0",
				"-q", "0",
				"/tmp/httpreplay-requests.txt",
			},
			out,
			true,
			mockExit,
			nil,
		)
	})
	require.Contains(t, out.String(), "should limit at least one of qps or concurrency")

	require.Panics(t, func() {
		Main(
			[]string{
				"-t", "NaN",
				"-c", "1",
				"/tmp/httpreplay-requests.txt",
			},
			out,
			true,
			mockExit,
			nil,
		)
	})
	require.Contains(t, out.String(), "timeout should not be NaN")

	require.Panics(t, func() {
		Main(
			[]string{
				"--delivery-semantics", "invalid",
				"-c", "1",
				"/tmp/httpreplay-requests.txt",
			},
			out,
			true,
			mockExit,
			nil,
		)
	})
	require.Contains(t, out.String(), "delivery semantics should be one of at-least-once or at-most-once")

	require.Panics(t, func() {
		Main(
			[]string{
				"--sync-to-disk-interval", "NaN",
				"-c", "1",
				"/tmp/httpreplay-requests.txt",
			},
			out,
			true,
			mockExit,
			nil,
		)
	})
	require.Contains(t, out.String(), "sync to disk interval should not be NaN")
}

func TestEmptyTapeFile(t *testing.T) {
	t.Parallel()

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, nil, 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			"--failure-tape-buffer-size", "0",
			"--sync-to-disk-interval", "0",
			tapeFilePath,
		},
		out,
		true,
		mockExit,
		nil,
	)

	require.Regexp(t, "final progress:.* tapePosition=0", out.String())
	require.Regexp(t, "final progress:.* successful=0", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())
}

func TestLargeTapeFile(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
		requestCount.Add(1)
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	f, err := os.OpenFile(tapeFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	require.NoError(t, err)
	buffer := bufio.NewWriterSize(f, 8*1024*1024)
	line := fmt.Sprintf("%s/test\n", server.URL)
	for range 2000 {
		_, err = buffer.WriteString(line)
		require.NoError(t, err)
	}
	err = buffer.Flush()
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			"-t", "1",
			tapeFilePath,
		},
		io.Discard,
		false,
		mockExit,
		nil,
	)

	data, err := os.ReadFile(tapeFilePath + ".httpreplay-pos")
	require.NoError(t, err)
	require.Len(t, data, 8)
	tapePosition := *(*int64)(unsafe.Pointer(unsafe.SliceData(data)))
	require.Equal(t, int64(2000), tapePosition)
	require.Equal(t, int64(2000), requestCount.Load())
}

func TestDeliveryAtLeastOnce(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.RequestURI, "k=51") {
			<-t.Context().Done()
		}
		requestCount.Add(1)
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	f, err := os.OpenFile(tapeFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	require.NoError(t, err)
	buffer := bufio.NewWriterSize(f, 8*1024*1024)
	for i := range 100 {
		line := fmt.Sprintf("%s/test?k=%d\n", server.URL, i+1)
		_, err = buffer.WriteString(line)
		require.NoError(t, err)
	}
	err = buffer.Flush()
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	go func() {
		Main(
			[]string{
				"-c", "10",
				"-q", "0",
				"-t", "-1",
				"--delivery-semantics", "at-least-once",
				tapeFilePath,
			},
			io.Discard,
			false,
			mockExit,
			nil,
		)
	}()

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(tapeFilePath + ".httpreplay-pos")
		if os.IsNotExist(err) {
			return false
		}
		require.NoError(t, err)
		if len(data) != 8 {
			return false
		}
		tapePosition := *(*int64)(unsafe.Pointer(unsafe.SliceData(data)))
		return tapePosition == 50 && requestCount.Load() == 99
	}, 5*time.Second, 500*time.Millisecond)
}

func TestDeliveryAtMostOnce(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.RequestURI, "k=51") {
			<-t.Context().Done()
		}
		requestCount.Add(1)
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	f, err := os.OpenFile(tapeFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	require.NoError(t, err)
	buffer := bufio.NewWriterSize(f, 8*1024*1024)
	for i := range 100 {
		line := fmt.Sprintf("%s/test?k=%d\n", server.URL, i+1)
		_, err = buffer.WriteString(line)
		require.NoError(t, err)
	}
	err = buffer.Flush()
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	go func() {
		Main(
			[]string{
				"-c", "10",
				"-q", "0",
				"-t", "-1",
				"--delivery-semantics", "at-most-once",
				tapeFilePath,
			},
			io.Discard,
			false,
			mockExit,
			nil,
		)
	}()

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(tapeFilePath + ".httpreplay-pos")
		if os.IsNotExist(err) {
			return false
		}
		require.NoError(t, err)
		if len(data) != 8 {
			return false
		}
		tapePosition := *(*int64)(unsafe.Pointer(unsafe.SliceData(data)))
		return tapePosition == 100 && requestCount.Load() == 99
	}, 5*time.Second, 500*time.Millisecond)
}
