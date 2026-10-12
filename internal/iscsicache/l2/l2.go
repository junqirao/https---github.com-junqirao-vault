// Package l2 implements the file backed second level cache of the iSCSI
// read-cache proxy.
//
// Slots are one cache block each and the slot index is persisted in an index
// region of the file, so a cache that was closed cleanly is still there on the
// next open: the second mount finds the blocks the previous run had cached
// instead of starting cold. L1 is volatile and refills from the restored index
// as reads come in.
//
// Persistence is snapshot based and deliberately one sided. The index is
// written to disk only by Close, together with a "clean" marker in the
// superblock; Open rewrites that marker to "open" before any slot can change.
// A crash (process kill, power loss) after that point therefore makes the next
// open discard the mapping and start cold. That is what keeps a restored
// mapping honest: an invalidation the proxy only applied in memory (it
// acknowledges writes and drops L2 sectors asynchronously) can never come back
// from disk, because the snapshot it belongs to is no longer marked clean.
//
// The backing file is a platform specific Store. On Windows it is opened with
// FILE_FLAG_NO_BUFFERING; on other platforms the store is a stub.
package l2

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// SuperBlockSize is the reserved header at the start of the L2 file.
	SuperBlockSize = 4096
	superBlockMag  = "VLTISCL2"
	// superBlockVer 2 adds the persisted index region and the snapshot marker.
	superBlockVer = 2
)

// Index region geometry. Records are one per slot and spread over 4 KiB pages so
// that every transfer stays aligned (the Windows store requires it) and so that
// a crash in the middle of a page write only costs that page: the page checksum
// rejects a torn page instead of trusting it.
const (
	indexPageSize = 4096
	indexPageMag  = "L2IX"
	indexHdrSize  = 16
	recordSize    = 12
	recsPerPage   = (indexPageSize - indexHdrSize) / recordSize
	// indexPerSlotBytes is the index space one slot needs rounded up to a share
	// of a page. Sizing from it keeps the file at or below the configured size.
	indexPerSlotBytes = (indexPageSize + recsPerPage - 1) / recsPerPage
)

// Superblock field offsets.
const (
	sbOffSlotSize   = 12
	sbOffSectorSize = 16
	sbOffSlotCount  = 20
	sbOffGeneration = 24
	sbOffNamespace  = 32
	sbOffNext       = 64
	sbOffState      = 72
	sbOffCRC        = 76
)

// Snapshot states. Open marks the file open; only Close marks it clean, and
// only a clean file has its index loaded again.
const (
	stateOpen  uint32 = 1
	stateClean uint32 = 2
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

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
	// Every IO is issued on a sector boundary, so the index page size has to stay
	// a whole number of sectors.
	if c.SectorSize > indexPageSize || indexPageSize%c.SectorSize != 0 {
		return fmt.Errorf("l2: sector size %d does not divide the %d byte index page", c.SectorSize, indexPageSize)
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
//
// Layout: superblock, then the index region (one record per slot), then the
// slots themselves. Only the superblock and the geometry in it are needed to
// find the other two, which is what makes the mapping recoverable.
type File struct {
	store      Store
	slotSize   int
	sectorSize int
	secPerSlot int
	slotCount  int
	namespace  string
	log        *slog.Logger

	indexPages int
	dataStart  int64
	generation uint64
	fullMask   uint32

	mu         sync.Mutex
	index      map[uint64]*slot
	slotsByNum []*slot
	next       int
	// dirty holds the index pages that changed since the last flush; dirtyAll
	// means every page has to be rewritten (Drop).
	dirty    map[int]struct{}
	dirtyAll bool
	// recovered counts the slots restored by the last Open, for the log line.
	recovered int
}

// Open opens (and if needed creates) the L2 file. When the file was closed
// cleanly under the same geometry and namespace, the index written by that
// close is loaded, so the cache is warm again instead of empty.
func Open(cfg Config) (*File, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	slotSize := cfg.BlockSize
	slotCount, indexPages, dataStart, fileSize, err := layout(cfg.SizeBytes, slotSize)
	if err != nil {
		return nil, err
	}

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
		indexPages: indexPages,
		dataStart:  dataStart,
		index:      make(map[uint64]*slot),
		slotsByNum: make([]*slot, slotCount),
		dirty:      make(map[int]struct{}),
	}
	f.fullMask = sectorMask(0, f.secPerSlot)
	if err := f.initSuperBlock(); err != nil {
		store.Close()
		return nil, err
	}
	cfg.Logger.Info("l2 opened", "path", cfg.Path, "slots", slotCount,
		"slot_size", slotSize, "namespace", shortHash(cfg.Namespace), "recovered", f.recovered)
	return f, nil
}

// layout sizes the file from the byte budget: each slot needs a slot of its own
// plus its share of the index region, and the index pages covering the slots
// round up to whole pages. The configured size is a ceiling, so a small budget
// loses a slot rather than growing the file.
func layout(sizeBytes int64, slotSize int) (slots, indexPages int, dataStart, fileSize int64, err error) {
	slots = int((sizeBytes - SuperBlockSize) / (int64(slotSize) + indexPerSlotBytes))
	for slots > 0 {
		pages := (slots + recsPerPage - 1) / recsPerPage
		dataStart = SuperBlockSize + int64(pages)*indexPageSize
		fileSize = dataStart + int64(slots)*int64(slotSize)
		if fileSize <= sizeBytes {
			return slots, pages, dataStart, fileSize, nil
		}
		slots--
	}
	return 0, 0, 0, 0, fmt.Errorf("l2: size %d is too small", sizeBytes)
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

// initSuperBlock validates the header, restores a cleanly closed index, and
// finally marks the file open so that a crash from here on cannot bring back
// the mapping this session is about to change.
func (f *File) initSuperBlock() error {
	buf := make([]byte, SuperBlockSize)
	if err := f.store.ReadAt(buf, 0); err != nil {
		return fmt.Errorf("l2: read superblock: %w", err)
	}
	ns := sha256.Sum256([]byte(f.namespace))
	switch {
	case string(buf[:8]) != superBlockMag:
		// No header yet: a new file.
		f.reset()
	case buf[8] != superBlockVer:
		f.log.Warn("l2: cache file version changed, discarding previous contents")
		f.reset()
	case le32(buf[sbOffCRC:]) != checksum(buf, sbOffCRC):
		// A torn header would otherwise be read as a valid one.
		f.log.Warn("l2: superblock checksum mismatch, discarding previous contents")
		f.reset()
	case int(le32(buf[sbOffSlotSize:])) != f.slotSize ||
		int(le32(buf[sbOffSectorSize:])) != f.sectorSize ||
		int(le32(buf[sbOffSlotCount:])) != f.slotCount:
		f.log.Warn("l2: geometry changed, starting with an empty index")
		f.reset()
	case string(buf[sbOffNamespace:sbOffNamespace+32]) != string(ns[:]):
		f.log.Warn("l2: cache namespace changed, discarding previous contents",
			"old", hex.EncodeToString(buf[sbOffNamespace:sbOffNamespace+32]),
			"new", hex.EncodeToString(ns[:]))
		f.reset()
	default:
		// The generation has to come back even when the snapshot is discarded:
		// it is what tells this session's pages apart from the ones on disk.
		f.generation = le64(buf[sbOffGeneration:])
		if le32(buf[sbOffState:]) != stateClean {
			f.log.Info("l2: previous session did not close cleanly, starting with an empty index")
			f.reset()
			break
		}
		if err := f.loadIndex(); err != nil {
			return err
		}
		if n := le64(buf[sbOffNext:]); n < uint64(f.slotCount) {
			f.next = int(n)
		}
		f.recovered = len(f.index)
		f.log.Info("l2: restored the index of the previous close", "slots", f.recovered)
	}
	return f.writeSuperBlock(stateOpen)
}

// reset starts from an empty index under a fresh generation, which hides every
// page written by an earlier session.
func (f *File) reset() {
	f.generation = newGeneration()
	f.index = make(map[uint64]*slot)
	f.slotsByNum = make([]*slot, f.slotCount)
	f.next = 0
	f.dirty = make(map[int]struct{})
	f.dirtyAll = false
	f.recovered = 0
}

// writeSuperBlock persists the header with the given snapshot state. The header
// is only useful once it is durable, so it is flushed here.
func (f *File) writeSuperBlock(state uint32) error {
	buf := make([]byte, SuperBlockSize)
	copy(buf[:8], superBlockMag)
	buf[8] = superBlockVer
	put32(buf[sbOffSlotSize:], uint32(f.slotSize))
	put32(buf[sbOffSectorSize:], uint32(f.sectorSize))
	put32(buf[sbOffSlotCount:], uint32(f.slotCount))
	put64(buf[sbOffGeneration:], f.generation)
	ns := sha256.Sum256([]byte(f.namespace))
	copy(buf[sbOffNamespace:sbOffNamespace+32], ns[:])
	put64(buf[sbOffNext:], uint64(f.next))
	put32(buf[sbOffState:], state)
	put32(buf[sbOffCRC:], checksum(buf, sbOffCRC))
	if err := f.store.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("l2: write superblock: %w", err)
	}
	return f.store.Sync()
}

// loadIndex rebuilds the slot index from the index region. Pages that were
// never written, belong to another generation or are torn (checksum mismatch)
// are skipped, so a partially written region costs entries instead of serving
// records that were never meant to be there.
func (f *File) loadIndex() error {
	buf := make([]byte, indexPageSize)
	for page := 0; page < f.indexPages; page++ {
		off := SuperBlockSize + int64(page)*indexPageSize
		if err := f.store.ReadAt(buf, off); err != nil {
			return fmt.Errorf("l2: read index page %d: %w", page, err)
		}
		if string(buf[:4]) != indexPageMag ||
			le64(buf[4:]) != f.generation ||
			le32(buf[12:]) != checksum(buf, 12) {
			continue
		}
		for i := 0; i < recsPerPage; i++ {
			num := page*recsPerPage + i
			if num >= f.slotCount {
				break
			}
			rec := buf[indexHdrSize+i*recordSize:]
			valid := le32(rec[8:12]) & f.fullMask
			if valid == 0 {
				continue
			}
			f.insertLocked(num, le64(rec[0:8]), valid)
		}
	}
	return nil
}

// insertLocked installs one recovered record. A clean snapshot holds one slot
// per block, so a block appearing twice means the older record came from a page
// written before the block moved; the record read last wins and the loser is
// detached so recycling cannot delete the winner's entry.
func (f *File) insertLocked(num int, blk uint64, valid uint32) {
	if prev := f.index[blk]; prev != nil {
		if prev.num == num {
			return
		}
		f.slotsByNum[prev.num] = nil
	}
	sl := &slot{num: num, blk: blk, valid: valid}
	f.slotsByNum[num] = sl
	f.index[blk] = sl
}

// writeIndexPage rewrites one page of records from the in-memory index. Slots
// that are not live get a zeroed record, which is what makes a page rewrite
// drop the entries that were invalidated since the last flush.
func (f *File) writeIndexPage(page int) error {
	buf := make([]byte, indexPageSize)
	copy(buf[:4], indexPageMag)
	put64(buf[4:], f.generation)
	for i := 0; i < recsPerPage; i++ {
		num := page*recsPerPage + i
		if num >= f.slotCount {
			break
		}
		sl := f.slotsByNum[num]
		if sl == nil || sl.valid == 0 {
			continue
		}
		rec := buf[indexHdrSize+i*recordSize:]
		put64(rec[0:8], sl.blk)
		put32(rec[8:12], sl.valid)
	}
	put32(buf[12:], checksum(buf, 12))
	off := SuperBlockSize + int64(page)*indexPageSize
	if err := f.store.WriteAt(buf, off); err != nil {
		return fmt.Errorf("l2: write index page %d: %w", page, err)
	}
	return nil
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
	if err := f.store.ReadAt(dst[:n*f.sectorSize], f.slotOffset(sl.num, sec)); err != nil {
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
	if err := f.store.WriteAt(src[:n*f.sectorSize], f.slotOffset(sl.num, sec)); err != nil {
		return err
	}
	sl.valid |= sectorMask(sec, n)
	f.markDirtyLocked(sl.num)
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
	f.markDirtyLocked(sl.num)
	if sl.valid == 0 {
		delete(f.index, blk)
		f.slotsByNum[sl.num] = nil
	}
}

// Drop forgets every slot without touching the file contents. On disk the whole
// index is rewritten by the next flush, so a dropped range cannot come back.
func (f *File) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.index = make(map[uint64]*slot)
	f.slotsByNum = make([]*slot, f.slotCount)
	f.next = 0
	f.dirty = make(map[int]struct{})
	f.dirtyAll = true
}

// Sync flushes the slot data and the index pages changed so far. It does not
// mark the snapshot reusable, because a snapshot is only coherent once the
// session stops writing: Close does that.
func (f *File) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushLocked(false)
}

// Close flushes the index, marks the snapshot clean and releases the store. The
// clean marker is what lets the next Open load this cache again, so Close (not
// Sync) is the call that makes the L2 survive a restart.
func (f *File) Close() error {
	f.mu.Lock()
	err := f.flushLocked(true)
	f.mu.Unlock()
	if cerr := f.store.Close(); err == nil {
		err = cerr
	}
	return err
}

// flushLocked writes the index pages that changed and, when clean is set, marks
// the file cleanly closed. The marker is written last and only after the pages
// are durable, so a crash in the middle of a flush costs the whole snapshot
// instead of leaving half of it marked as good.
func (f *File) flushLocked(clean bool) error {
	written := 0
	if f.dirtyAll {
		for page := 0; page < f.indexPages; page++ {
			if err := f.writeIndexPage(page); err != nil {
				return err
			}
			written++
		}
	} else {
		for page := range f.dirty {
			if err := f.writeIndexPage(page); err != nil {
				return err
			}
			written++
		}
	}
	if written > 0 {
		if err := f.store.Sync(); err != nil {
			return err
		}
		f.dirty = make(map[int]struct{})
		f.dirtyAll = false
	}
	if !clean {
		return nil
	}
	return f.writeSuperBlock(stateClean)
}

func (f *File) allocSlotLocked(blk uint64) *slot {
	num := f.next
	f.next = (f.next + 1) % f.slotCount
	if old := f.slotsByNum[num]; old != nil {
		delete(f.index, old.blk)
	}
	sl := &slot{num: num, blk: blk}
	f.slotsByNum[num] = sl
	f.index[blk] = sl
	f.markDirtyLocked(num)
	return sl
}

func (f *File) markDirtyLocked(num int) {
	if f.dirtyAll {
		return
	}
	f.dirty[num/recsPerPage] = struct{}{}
}

func (f *File) slotOffset(num, sec int) int64 {
	return f.dataStart + int64(num)*int64(f.slotSize) + int64(sec*f.sectorSize)
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

// checksum returns the CRC32C of buf with the four byte field at off read as
// zero, so a page can carry its own checksum.
func checksum(buf []byte, off int) uint32 {
	c := crc32.Checksum(buf[:off], crcTable)
	c = crc32.Update(c, crcTable, []byte{0, 0, 0, 0})
	return crc32.Update(c, crcTable, buf[off+4:])
}

// newGeneration returns the tag that separates the pages of one session from
// the ones an earlier session left behind. It is random rather than a counter:
// an unreadable superblock must not be able to guess the generation it is
// trying to match.
func newGeneration() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return le64(b[:])
	}
	return uint64(time.Now().UnixNano())
}

func le64(b []byte) uint64 {
	return uint64(le32(b)) | uint64(le32(b[4:]))<<32
}

func put64(b []byte, v uint64) {
	put32(b, uint32(v))
	put32(b[4:], uint32(v>>32))
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
