//go:build windows

package l2

import (
	"fmt"
	"io"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// allocGranularity is the alignment the L2 scratch buffer is rounded up to. It
// is the Windows allocation granularity (64 KiB) and also covers every logical
// sector size Windows uses (512 B and 4 KiB), which is what
// FILE_FLAG_NO_BUFFERING actually requires.
const allocGranularity = 64 * 1024

// winStore is the Windows L2 file store. Unbuffered IO requires aligned
// offsets, lengths and buffers, so transfers are bounced through an aligned
// scratch buffer.
//
// The handle is deliberately not opened with FILE_FLAG_OVERLAPPED: the Go
// runtime drives file IO through its own poller, and mixing OVERLAPPED with it
// is a known source of subtle bugs. FILE_FLAG_NO_BUFFERING is the flag that
// actually matters here.
type winStore struct {
	h       windows.Handle
	mu      sync.Mutex
	scratch *alignedBuf
	path    string
}

// alignedBuf is a scratch buffer whose first byte is allocGranularity aligned.
// back is over-allocated by one alignment unit; the GC keeps it alive because
// buf is an interior pointer into it.
type alignedBuf struct {
	back []byte
	buf  []byte
}

func newAlignedBuf(n int) (*alignedBuf, error) {
	if n <= 0 {
		n = 4096
	}
	if n > 1<<30 {
		return nil, fmt.Errorf("l2: scratch size %d is too large", n)
	}
	size := (n + allocGranularity - 1) / allocGranularity * allocGranularity
	back := make([]byte, size+allocGranularity)
	p := unsafe.Pointer(unsafe.SliceData(back))
	off := (allocGranularity - uintptr(p)%allocGranularity) % allocGranularity
	aligned := unsafe.Slice((*byte)(unsafe.Add(p, off)), n)
	return &alignedBuf{back: back, buf: aligned}, nil
}

func (b *alignedBuf) free() {
	b.back = nil
	b.buf = nil
}

func openStore(path string, size int64, maxIO int) (Store, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING,
		0)
	if err != nil {
		return nil, fmt.Errorf("l2: create %s: %w", path, err)
	}
	buf, err := newAlignedBuf(maxIO)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	s := &winStore{h: h, scratch: buf, path: path}
	if size > 0 {
		if err := s.Truncate(size); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *winStore) ReadAt(p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > len(s.scratch.buf) {
		return fmt.Errorf("l2: read of %d bytes exceeds aligned scratch (%d)", len(p), len(s.scratch.buf))
	}
	if _, err := windows.Seek(s.h, off, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", off, err)
	}
	var done uint32
	if err := windows.ReadFile(s.h, s.scratch.buf[:len(p)], &done, nil); err != nil {
		return fmt.Errorf("l2: read at %d: %w", off, err)
	}
	if int(done) != len(p) {
		return io.ErrUnexpectedEOF
	}
	copy(p, s.scratch.buf[:len(p)])
	return nil
}

func (s *winStore) WriteAt(p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > len(s.scratch.buf) {
		return fmt.Errorf("l2: write of %d bytes exceeds aligned scratch (%d)", len(p), len(s.scratch.buf))
	}
	copy(s.scratch.buf[:len(p)], p)
	if _, err := windows.Seek(s.h, off, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", off, err)
	}
	var done uint32
	if err := windows.WriteFile(s.h, s.scratch.buf[:len(p)], &done, nil); err != nil {
		return fmt.Errorf("l2: write at %d: %w", off, err)
	}
	if int(done) != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *winStore) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := windows.Seek(s.h, size, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", size, err)
	}
	if err := windows.SetEndOfFile(s.h); err != nil {
		return fmt.Errorf("l2: setendoffile %d: %w", size, err)
	}
	return nil
}

func (s *winStore) Sync() error {
	return windows.FlushFileBuffers(s.h)
}

func (s *winStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scratch != nil {
		s.scratch.free()
		s.scratch = nil
	}
	if s.h == 0 {
		return nil
	}
	err := windows.CloseHandle(s.h)
	s.h = 0
	return err
}
