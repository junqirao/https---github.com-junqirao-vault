// Package l2 implements the file backed second level cache of the iSCSI
// read-cache proxy. Slots are one cache block each and the slot index lives in
// memory only, so a restart starts with a logically empty L2 (the MVP trades a
// cold L2 for zero crash-recovery complexity).
//
// The backing file is a platform specific Store. On Windows it is opened with
// FILE_FLAG_NO_BUFFERING; on other platforms the store is a stub.
package l2

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	// SuperBlockSize is the reserved header at the start of the L2 file.
	SuperBlockSize = 4096
	superBlockMag  = "VLTISCL2"
	superBlockVer  = 1
)

// Defaults matching the cache geometry.
const (
	DefaultBlockSize  = 64 * 1024
	DefaultSectorSize = 4096
)

// ErrUnsupported reports that no L2 store exists for the running platform.
var ErrUnsupported = errors.New("l2: file store is not implemented on this platform")

// Store is the platform file abstraction behind the L2. Offsets and lengths are
// always multiples of the configured alignment; implementations that require
// aligned buffers (Windows FILE_FLAG_NO_BUFFERING) copy through their own
// aligned scratch space.
type Store interface {
	ReadAt(p []byte, off int64) error
	WriteAt(p []byte, off int64) error
	Truncate(size int64) error
	Sync() error
	Close() error
}

// Config configures an L2 file.
type Config struct {
	Path       string
	SizeBytes  int64
	BlockSize  int
	SectorSize int
	// Namespace binds the file to a backing LUN identity. A mismatch is
	// reported and the in-memory index simply starts empty.
	Namespace string
	Logger    *slog.Logger
	// Store overrides the platform store; used by tests.
	Store Store
}

func (c *Config) normalize() error {
	if c.BlockSize == 0 {
		c.BlockSize = DefaultBlockSize
	}
	if c.SectorSize == 0 {
		c.SectorSize = DefaultSectorSize
	}
	if c.BlockSize%c.SectorSize != 0 {
		return fmt.Errorf("l2: block size %d is not a multiple of sector size %d", c.BlockSize, c.SectorSize)
	}
	if n := c.BlockSize / c.SectorSize; n < 1 || n > 32 {
		return fmt.Errorf("l2: %d sectors per slot is out of range (1..32)", n)
	}
	if c.SizeBytes <= SuperBlockSize+int64(c.BlockSize) {
		return fmt.Errorf("l2: size %d is too small", c.SizeBytes)
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return nil
}

type slot struct {
	num   int
	blk   uint64
	valid uint32
}

// File is a fixed slot L2 cache held in a single file. Slots are handed out in
// a ring, so a slot is recycled FIFO and never needs explicit eviction.
type File struct {
	store      Store
	slotSize   int
	sectorSize int
	secPerSlot int
	slotCount  int
	namespace  string
	log        *slog.Logger

	mu         sync.Mutex
	index      map[uint64]*slot
	slotsByNum []*slot
	next       int
}

// Open opens (and if needed creates) the L2 file.
func Open(cfg Config) (*File, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	slotSize := cfg.BlockSize
	slotCount := int((cfg.SizeBytes - SuperBlockSize) / int64(slotSize))
	fileSize := SuperBlockSize + int64(slotCount)*int64(slotSize)

	store := cfg.Store
	if store == nil {
		if cfg.Path == "" {
			return nil, errors.New("l2: path is required")
		}
		if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("l2: create cache directory %s: %w", dir, err)
			}
		}
		s, err := openStore(cfg.Path, fileSize, maxInt(slotSize, SuperBlockSize))
		if err != nil {
			return nil, err
		}
		store = s
	} else if err := store.Truncate(fileSize); err != nil {
		return nil, fmt.Errorf("l2: size store: %w", err)
	}

	f := &File{
		store:      store,
		slotSize:   slotSize,
		sectorSize: cfg.SectorSize,
		secPerSlot: slotSize / cfg.SectorSize,
		slotCount:  slotCount,
		namespace:  cfg.Namespace,
		log:        cfg.Logger,
		index:      make(map[uint64]*slot),
		slotsByNum: make([]*slot, slotCount),
	}
	if err := f.initSuperBlock(); err != nil {
		store.Close()
		return nil, err
	}
	cfg.Logger.Info("l2 opened", "path", cfg.Path, "slots", slotCount,
		"slot_size", slotSize, "namespace", shortHash(cfg.Namespace))
	return f, nil
}

// Namespace returns the identity the file is bound to.
func (f *File) Namespace() string { return f.namespace }

// Slots returns the number of slots in the file.
func (f *File) Slots() int { return f.slotCount }

// SlotSize returns the size of one slot in bytes.
func (f *File) SlotSize() int { return f.slotSize }

// UsedSlots returns the number of slots currently holding data.
func (f *File) UsedSlots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.index)
}

// initSuperBlock validates the header, rewriting it when the geometry or the
// bound namespace changed.
func (f *File) initSuperBlock() error {
	buf := make([]byte, SuperBlockSize)
	if err := f.store.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("l2: read superblock: %w", err)
	}
	ns := sha256.Sum256([]byte(f.namespace))
	if string(buf[:8]) != superBlockMag {
		return f.writeSuperBlock(ns)
	}
	if buf[8] != superBlockVer ||
		int(le32(buf[12:16])) != f.slotSize ||
		int(le32(buf[16:20])) != f.sectorSize ||
		int(le32(buf[20:24])) != f.slotCount {
		f.log.Warn("l2: geometry changed, starting with an empty index")
		return f.writeSuperBlock(ns)
	}
	if string(buf[32:64]) != string(ns[:]) {
		f.log.Warn("l2: cache namespace changed, discarding previous contents",
			"old", hex.EncodeToString(buf[32:64]), "new", hex.EncodeToString(ns[:]))
		return f.writeSuperBlock(ns)
	}
	return nil
}

func (f *File) writeSuperBlock(ns [32]byte) error {
	buf := make([]byte, SuperBlockSize)
	copy(buf[:8], superBlockMag)
	buf[8] = superBlockVer
	put32(buf[12:16], uint32(f.slotSize))
	put32(buf[16:20], uint32(f.sectorSize))
	put32(buf[20:24], uint32(f.slotCount))
	copy(buf[32:64], ns[:])
	if err := f.store.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("l2: write superblock: %w", err)
	}
	return f.store.Sync()
}

// ReadSectors implements cache.L2.
func (f *File) ReadSectors(blk uint64, sec, n int, dst []byte) (bool, error) {
	if n <= 0 {
		return true, nil
	}
	if sec < 0 || sec+n > f.secPerSlot {
		return false, fmt.Errorf("l2: sector range [%d,%d) out of slot bounds", sec, sec+n)
	}
	if len(dst) < n*f.sectorSize {
		return false, io.ErrShortBuffer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sl := f.index[blk]
	if sl == nil {
		return false, nil
	}
	mask := sectorMask(sec, n)
	if sl.valid&mask != mask {
		return false, nil
	}
	off := f.slotOffset(sl.num) + int64(sec*f.sectorSize)
	if err := f.store.ReadAt(dst[:n*f.sectorSize], off); err != nil {
		return false, err
	}
	return true, nil
}

// WriteSectors implements cache.L2.
func (f *File) WriteSectors(blk uint64, sec, n int, src []byte) error {
	if n <= 0 {
		return nil
	}
	if sec < 0 || sec+n > f.secPerSlot {
		return fmt.Errorf("l2: sector range [%d,%d) out of slot bounds", sec, sec+n)
	}
	if len(src) < n*f.sectorSize {
		return io.ErrShortBuffer
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sl := f.index[blk]
	if sl == nil {
		sl = f.allocSlotLocked(blk)
	}
	off := f.slotOffset(sl.num) + int64(sec*f.sectorSize)
	if err := f.store.WriteAt(src[:n*f.sectorSize], off); err != nil {
		return err
	}
	sl.valid |= sectorMask(sec, n)
	return nil
}

// InvalidateSectors implements cache.L2.
func (f *File) InvalidateSectors(blk uint64, sec, n int) {
	if n <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sl := f.index[blk]
	if sl == nil {
		return
	}
	sl.valid &^= sectorMask(sec, n)
	if sl.valid == 0 {
		delete(f.index, blk)
		f.slotsByNum[sl.num] = nil
	}
}

// Drop forgets every slot without touching the file contents.
func (f *File) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.index = make(map[uint64]*slot)
	f.slotsByNum = make([]*slot, f.slotCount)
	f.next = 0
}

// Sync flushes the file.
func (f *File) Sync() error { return f.store.Sync() }

// Close releases the underlying store.
func (f *File) Close() error { return f.store.Close() }

func (f *File) allocSlotLocked(blk uint64) *slot {
	num := f.next
	f.next = (f.next + 1) % f.slotCount
	if old := f.slotsByNum[num]; old != nil {
		delete(f.index, old.blk)
	}
	sl := &slot{num: num, blk: blk}
	f.slotsByNum[num] = sl
	f.index[blk] = sl
	return sl
}

func (f *File) slotOffset(num int) int64 {
	return SuperBlockSize + int64(num)*int64(f.slotSize)
}

func sectorMask(sec, n int) uint32 {
	var m uint32
	for i := 0; i < n; i++ {
		m |= 1 << uint(sec+i)
	}
	return m
}

func shortHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func put32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// MemoryStore is an in-memory Store used by tests and by dry runs.
type MemoryStore struct {
	mu   sync.Mutex
	data []byte
}

// NewMemoryStore creates a MemoryStore of the given size.
func NewMemoryStore(size int64) *MemoryStore { return &MemoryStore{data: make([]byte, size)} }

// ReadAt implements Store.
func (m *MemoryStore) ReadAt(p []byte, off int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < 0 || off+int64(len(p)) > int64(len(m.data)) {
		return io.ErrUnexpectedEOF
	}
	copy(p, m.data[off:])
	return nil
}

// WriteAt implements Store.
func (m *MemoryStore) WriteAt(p []byte, off int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < 0 || off+int64(len(p)) > int64(len(m.data)) {
		return io.ErrShortWrite
	}
	copy(m.data[off:], p)
	return nil
}

// Truncate implements Store.
func (m *MemoryStore) Truncate(size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int64(len(m.data)) != size {
		m.data = make([]byte, size)
	}
	return nil
}

// Sync implements Store.
func (m *MemoryStore) Sync() error { return nil }

// Close implements Store.
func (m *MemoryStore) Close() error { return nil }
