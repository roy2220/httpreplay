package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/edsrzf/mmap-go"
)

type tapeReader struct {
	mMap        mmap.MMap
	unreadLines []byte
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
		unreadLines: mMap,
	}, nil
}

func (r *tapeReader) Close() error {
	if r.mMap == nil {
		return nil
	}
	r.unreadLines = nil
	return r.mMap.Unmap()
}

func (r *tapeReader) ReadLine() ([]byte, bool) {
	var i int
	if j := bytes.IndexByte(r.unreadLines, '\n'); j >= 0 {
		i = j + 1
	} else {
		i = len(r.unreadLines)
	}
	line := r.unreadLines[:i]
	r.unreadLines = r.unreadLines[i:]
	return line, len(line) >= 1
}
