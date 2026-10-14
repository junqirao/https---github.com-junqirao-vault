//go:build windows

package l2

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// allocGranularity is the alignment the L2 scratch buffer is rounded up to. It
// is the Windows allocation granularity (64 KiB) and also covers every logical
// sector size Windows uses (512 B and 4 KiB), which is what
// FILE_FLAG_NO_BUFFERING actually requires.
const allocGranularity = 64 * 1024

// storeShards is how many handles the L2 file is served through. Every transfer
// is unbuffered, so a transfer occupies its handle for the whole physical IO:
// sharing one handle (as this store used to) made a 4 KiB slot stamp wait behind
// a 64 KiB data transfer and every read wait behind both. Independent handles do
// not share a file pointer and do not share a mutex, so a stamp write and a data
// transfer overlap instead of queueing.
//
// It is deliberately small: the point is to decouple the small metadata IO from
// the bulk IO, not to saturate the device, and every shard costs one aligned
// scratch buffer.
const storeShards = 4

// winStore is the Windows L2 file store. Unbuffered IO requires aligned offsets,
// lengths and buffers, so transfers are bounced through an aligned scratch
// buffer, one per shard.
//
// The handles are deliberately not opened with FILE_FLAG_OVERLAPPED: the Go
// runtime drives file IO through its own poller, and mixing OVERLAPPED with it
// is a known source of subtle bugs. FILE_FLAG_NO_BUFFERING is the flag that
// actually matters here.
type winStore struct {
	shards []*winShard
	// next spreads a burst of transfers over the shards instead of letting them
	// pile onto one.
	next atomic.Uint32
	path string
}

// winShard is one handle plus the scratch buffer and the lock that keep it to a
// single transfer at a time.
type winShard struct {
	h       windows.Handle
	mu      sync.Mutex
	scratch *alignedBuf
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
	s := &winStore{path: path}
	for i := 0; i < storeShards; i++ {
		sh := &winShard{}
		// FILE_SHARE_READ|FILE_SHARE_WRITE is what lets this process hold the
		// several handles the shards need. It is not opened for FILE_SHARE_DELETE:
		// the cache file still cannot be unlinked out from under a running session.
		h, err := createStoreFile(path)
		if err != nil {
			s.Close()
			return nil, err
		}
		sh.h = h
		buf, err := newAlignedBuf(maxIO)
		if err != nil {
			windows.CloseHandle(h)
			s.Close()
			return nil, err
		}
		sh.scratch = buf
		s.shards = append(s.shards, sh)
	}
	// Only ever grow here. The index region of the previous snapshot can sit
	// past the size a smaller budget implies, and Open still has to read it
	// before the file may be trimmed, so shrinking is left to Open.
	if size > 0 {
		cur, err := s.fileSize()
		if err != nil {
			s.Close()
			return nil, err
		}
		if cur < size {
			if err := s.Truncate(size); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func createStoreFile(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING,
		0)
	if err != nil {
		return 0, fmt.Errorf("l2: create %s: %w", path, err)
	}
	return h, nil
}

// pick returns a shard locked for one transfer. It prefers a shard nobody is
// using and only blocks when every one of them is busy, which keeps a small
// stamp write from queueing behind the bulk transfers it used to sit behind.
func (s *winStore) pick() *winShard {
	n := uint32(len(s.shards))
	base := s.next.Add(1)
	for i := uint32(0); i < n; i++ {
		sh := s.shards[(base+i)%n]
		if sh.mu.TryLock() {
			return sh
		}
	}
	sh := s.shards[base%n]
	sh.mu.Lock()
	return sh
}

// lockAll takes every shard, to run an operation that concerns the file as a
// whole. Shards are locked in order and only ever one at a time elsewhere, so
// this cannot deadlock.
func (s *winStore) lockAll() {
	for _, sh := range s.shards {
		sh.mu.Lock()
	}
}

func (s *winStore) unlockAll() {
	for _, sh := range s.shards {
		sh.mu.Unlock()
	}
}

// fileSize reports the current end of the file.
func (s *winStore) fileSize() (int64, error) {
	sh := s.shards[0]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.fileSize(s.path)
}

func (sh *winShard) fileSize(path string) (int64, error) {
	if sh.h == 0 {
		return 0, fmt.Errorf("l2: %s is closed", path)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(sh.h, &info); err != nil {
		return 0, fmt.Errorf("l2: query size of %s: %w", path, err)
	}
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}

func (s *winStore) ReadAt(p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	sh := s.pick()
	defer sh.mu.Unlock()
	if len(p) > len(sh.scratch.buf) {
		return fmt.Errorf("l2: read of %d bytes exceeds aligned scratch (%d)", len(p), len(sh.scratch.buf))
	}
	if _, err := windows.Seek(sh.h, off, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", off, err)
	}
	var done uint32
	if err := windows.ReadFile(sh.h, sh.scratch.buf[:len(p)], &done, nil); err != nil {
		return fmt.Errorf("l2: read at %d: %w", off, err)
	}
	if int(done) != len(p) {
		return io.ErrUnexpectedEOF
	}
	copy(p, sh.scratch.buf[:len(p)])
	return nil
}

func (s *winStore) WriteAt(p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	sh := s.pick()
	defer sh.mu.Unlock()
	if len(p) > len(sh.scratch.buf) {
		return fmt.Errorf("l2: write of %d bytes exceeds aligned scratch (%d)", len(p), len(sh.scratch.buf))
	}
	copy(sh.scratch.buf[:len(p)], p)
	if _, err := windows.Seek(sh.h, off, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", off, err)
	}
	var done uint32
	if err := windows.WriteFile(sh.h, sh.scratch.buf[:len(p)], &done, nil); err != nil {
		return fmt.Errorf("l2: write at %d: %w", off, err)
	}
	if int(done) != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *winStore) Truncate(size int64) error {
	// The file is one object behind every shard, so resizing holds them all: a
	// shard sitting in a transfer must not find its transfer truncated away.
	s.lockAll()
	defer s.unlockAll()
	h := s.shards[0].h
	if _, err := windows.Seek(h, size, 0); err != nil {
		return fmt.Errorf("l2: seek %d: %w", size, err)
	}
	if err := windows.SetEndOfFile(h); err != nil {
		return fmt.Errorf("l2: setendoffile %d: %w", size, err)
	}
	return nil
}

func (s *winStore) Sync() error {
	s.lockAll()
	defer s.unlockAll()
	for _, sh := range s.shards {
		if sh.h == 0 {
			continue
		}
		if err := windows.FlushFileBuffers(sh.h); err != nil {
			return err
		}
	}
	return nil
}

func (s *winStore) Close() error {
	s.lockAll()
	defer s.unlockAll()
	var err error
	for _, sh := range s.shards {
		if sh.scratch != nil {
			sh.scratch.free()
			sh.scratch = nil
		}
		if sh.h == 0 {
			continue
		}
		if cerr := windows.CloseHandle(sh.h); cerr != nil && err == nil {
			err = cerr
		}
		sh.h = 0
	}
	return err
}
