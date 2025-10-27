package main_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	. "github.com/roy2220/httpreplay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mockExit(n int) {
	panic(fmt.Sprintf("exit(%d)", n))
}

type request struct {
	Method string
	URI    string
	Header http.Header
	Body   string
}

func TestNormal(t *testing.T) {
	var requestsLock sync.Mutex
	var requests []request
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()

		if strings.HasSuffix(r.RequestURI, "v=2") {
			http.Redirect(w, r, server.URL+"/redirect", http.StatusFound)
		}
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=1
-H'X-Foo: Bar' %[1]s/api?v=2
%[1]s/api?v=3 -X=GET -H 'X-Foo: Bar'  # my comment
%[1]s/api?v=4 -H'X-Foo: Bar' --request POST --data='{"key": "value"}' --header 'X-Hello: World'
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
		mockExit,
		nil,
		true,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=4", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 4)
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=1",
		Header: nil,
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=2",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=3",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "POST",
		URI:    "/api?v=4",
		Header: http.Header{"X-Foo": []string{"Bar"}, "X-Hello": []string{"World"}},
		Body:   `{"key": "value"}`,
	})
}

func TestFollowRedirects(t *testing.T) {
	var requestsLock sync.Mutex
	var requests []request
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := request{
			Method: r.Method,
			URI:    r.RequestURI,
			Header: nil,
			Body:   string(body),
		}
		h := http.Header{}
		for k, vs := range r.Header {
			if strings.HasPrefix(k, "X-") {
				h[k] = vs
			}
		}
		if len(h) >= 1 {
			request.Header = h
		}
		requestsLock.Lock()
		requests = append(requests, request)
		requestsLock.Unlock()

		if strings.HasSuffix(r.RequestURI, "v=2") {
			http.Redirect(w, r, server.URL+"/redirect", http.StatusFound)
		}
	}))
	t.Cleanup(server.Close)

	tempDirPath := t.TempDir()
	tapeFilePath := filepath.Join(tempDirPath, "requests.txt")
	err := os.WriteFile(tapeFilePath, fmt.Appendf(nil, `
%[1]s/api?v=1
%[1]s/api?v=2 -H 'X-Foo: Bar'
%[1]s/api?v=3 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=4 -X POST -d '{"key": "value"}' -H 'X-Hello: World'
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
		mockExit,
		nil,
		true,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=4", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	server.Close()
	require.Len(t, requests, 5)
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/redirect",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	})
}

func TestDryRun(t *testing.T) {
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
			if strings.HasPrefix(k, "X-") {
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
%[1]s/api?v=1
%[1]s/api?v=2 -H 'X-Foo: Bar'
%[1]s/api?v=3 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=4 -X POST -d '{"key": "value"}' -H 'X-Hello: World'
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
		mockExit,
		nil,
		true,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=4", out.String())
	require.Regexp(t, "final progress:.* failed=0", out.String())

	fileExists := func(filePath string) bool {
		_, err := os.Stat(filePath)
		return !errors.Is(err, os.ErrNotExist)
	}

	server.Close()
	require.Len(t, requests, 0)
	require.False(t, fileExists(tapeFilePath+".httpreplay-pos"))
	require.True(t, fileExists(tapeFilePath+".httpreplay-pos.dry-run"))
}

func TestProgressResumption(t *testing.T) {
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
			if strings.HasPrefix(k, "X-") {
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
%[1]s/api?v=1

%[1]s/api?v=2 -H 'X-Foo: Bar'
%[1]s/api?v=3 -X GET -H 'X-Foo: Bar' # my comment

   # %[1]s/api?v=333 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=4 -X POST -d '{"key": "value"}' -H 'X-Hello: World'
`, server.URL)[1:], 0644)
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
		mockExit,
		exitSignal,
		true,
	)

	Main(
		[]string{
			"-c", "100",
			"-q", "0",
			tapeFilePath,
		},
		out,
		mockExit,
		nil,
		true,
	)

	server.Close()
	require.Len(t, requests, 4)
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=1",
		Header: nil,
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=2",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "GET",
		URI:    "/api?v=3",
		Header: http.Header{"X-Foo": []string{"Bar"}},
		Body:   "",
	})
	assert.Contains(t, requests, request{
		Method: "POST",
		URI:    "/api?v=4",
		Header: http.Header{"X-Hello": []string{"World"}},
		Body:   `{"key": "value"}`,
	})
}

func TestFailureTape(t *testing.T) {
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
			if strings.HasPrefix(k, "X-") {
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
		case strings.HasSuffix(r.RequestURI, "v=1"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.RequestURI, "v=3"):
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
%[1]s/api?v=1
%[1]s/api?v=2 -H 'X-Foo: Bar'
%[1]s/api?v=3 -X GET -H 'X-Foo: Bar'
%[1]s/api?v=4 -X POST -d '{"key": "value"}' -H 'X-Hello: World'
`, server.URL)[1:], 0644)
	require.NoError(t, err)

	out := bytes.NewBuffer(nil)
	defer func() { t.Log(out.String()) }()

	Main(
		[]string{
			"-c", "1",
			"-q", "0",
			"-t", "1",
			tapeFilePath,
		},
		out,
		mockExit,
		nil,
		true,
	)

	require.Regexp(t, "final progress:.* tapePosition=4", out.String())
	require.Regexp(t, "final progress:.* successful=2", out.String())
	require.Regexp(t, "final progress:.* failed=2", out.String())

	server.Close()
	require.Len(t, requests, 4)

	data, err := os.ReadFile(tapeFilePath + ".httpreplay-failure")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(`
%[1]s/api?v=1
%[1]s/api?v=3 -X GET -H 'X-Foo: Bar'
`, server.URL)[1:], string(data))
}

func TestBadArgs(t *testing.T) {
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
			mockExit,
			nil,
			true,
		)
	})

	require.Contains(t, out.String(), "should limit at least one of qps or concurrency")
}
