package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/edsrzf/mmap-go"
)

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
