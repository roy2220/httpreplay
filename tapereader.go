package main

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"github.com/edsrzf/mmap-go"
)

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

func b2s(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }
