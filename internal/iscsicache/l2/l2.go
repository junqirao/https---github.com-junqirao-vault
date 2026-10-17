// Package l2 implements the file backed second level cache of the iSCSI
// read-cache proxy.
//
// Slots are one cache block each and the slot index is persisted in an index
// region of the file, so a cache that was closed cleanly is still there on the
// next open: the second mount finds the blocks the previous run had cached
// instead of starting cold. L1 is volatile and refills from the restored index
// as reads come in.
//
// Persistence is snapshot based and two sided. Close writes the index together
// with a "clean" marker in the superblock; Open rewrites that marker to "open"
// before any slot can change. A crash (process kill, power loss) after that
// point leaves the marker "open", and the next open still restores the index —
// but every entry it restores is unverified until the slot's own stamp is read
// back. That is what keeps a restored mapping honest without paying for it in
// the common case:
//
//   - A slot starts with a stamp sector holding the block number and the
//     generation it was written under, and the stamp is written *after* the
//     data it describes. A torn data write therefore leaves the previous stamp
//     in place and the slot fails verification instead of serving stale bytes.
//   - A cleanly closed file is self consistent by construction (nothing was
//     written after the snapshot), so its entries are trusted and cost nothing.
//   - An unclean file is restored with every entry marked unverified; the first
//     read of each block reads its stamp, and a mismatch — a slot that a later
//     session recycled or rewrote before the crash — drops the entry and turns
//     into an ordinary miss. An invalidation the proxy only applied in memory
//     cannot come back either: its slot still carries the pre-write stamp under
//     the session that was killed, so the block it belonged to verifies only if
//     it was never overwritten.
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
	"sync/atomic"
	"time"
)

const (
	// SuperBlockSize is the reserved header at the start of the L2 file.
	SuperBlockSize = 4096
	superBlockMag  = "VLTISCL2"
	// superBlockVer 2 added the persisted index region and the snapshot marker;
	// 3 added the per-slot stamp sector and the statically reserved index;
	// 4 moved every stamp out of the slot area into one of its own, which is
	// what makes the data of neighbouring slots neighbouring bytes. An older
	// file has its stamps wedged between the slots, so it is discarded and
	// rebuilt rather than migrated: see initSuperBlock.
	superBlockVer = 4
)

// Slot stamp. Every slot has one reserved sector describing the data it holds,
// so a slot can be validated without trusting the index that points at it. All
// of them live in one region after the slots, not between them: a run of
// neighbouring slots is then a run of neighbouring bytes on disk, which is what
// lets a range read be served in one transfer straight into the caller's buffer.
//
// The version is what makes an invalidation visible to the next open: dropping
// sectors only changes the version, and a snapshot that predates the drop then
// fails to match the entry it would otherwise resurrect.
const (
	stampSize = indexPageSize
	stampMag  = "VLTSTAMP"
	// Stamp field offsets; the magic is followed by the block number, the
	// generation, the slot version and the checksum over the preceding fields.
	stampOffBlk = 8
	stampOffGen = 16
	stampOffVer = 24
	stampOffCRC = 32
)

// Index region geometry. Records are one per slot and spread over 4 KiB pages so
// that every transfer stays aligned (the Windows store requires it) and so that
// a crash in the middle of a page write only costs that page: the page checksum
// rejects a torn page instead of trusting it.
const (
	indexPageSize = 4096
	indexPageMag  = "L2IX"
	indexHdrSize  = 16
	// recordSize is the block number, the valid mask and the slot version the
	// stamp on disk has to agree with.
	recordSize  = 20
	recsPerPage = (indexPageSize - indexHdrSize) / recordSize
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
	// sbOffRegion holds the region size the slots were laid out with. It takes
	// part in the geometry check because it decides which slot a block belongs
	// in: a file written with another one cannot be homed under this session's
	// mapping.
	sbOffRegion = 64
	sbOffState  = 72
	sbOffCRC    = 76
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
	// DefaultMaxIOBytes is how many bytes one transfer against the store may
	// cover. It is one mebibyte because that is what a run of neighbouring
	// blocks adds up to under the region layout, and because the store pays for
	// the cap once per shard rather than once per transfer: four shards of
	// scratch, not one buffer per read. Config.MaxIOBytes overrides it.
	DefaultMaxIOBytes = 1 << 20
	// DefaultRegionBlocks is how many consecutive blocks of the LUN one slot
	// home holds: the granularity at which neighbouring blocks are kept
	// physically adjacent, and so the length of the runs a range read turns
	// into. A home is recycled as a unit, so this is also how much of the cache
	// one newly touched region can cost.
	DefaultRegionBlocks = 128
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
	// MaxIOBytes caps one transfer against the store, in bytes. It is what lets
	// a run of neighbouring blocks be read in a single transfer, so it has to
	// cover at least one block; Open clamps it to the size of the file. Zero
	// takes DefaultMaxIOBytes.
	MaxIOBytes int
	// RegionBlocks is how many consecutive blocks of the LUN one slot home
	// holds. Zero takes DefaultRegionBlocks; Open lowers it to the slot count
	// when the budget holds fewer slots than that, because a home needs at
	// least one slot and a file needs at least one home.
	RegionBlocks int
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
	if n := c.BlockSize / c.SectorSize; n < 1 || n > secPerSlotMax {
		return fmt.Errorf("l2: %d sectors per slot is out of range (1..%d)", n, secPerSlotMax)
	}
	// Every IO is issued on a sector boundary, so the index page size has to stay
	// a whole number of sectors.
	if c.SectorSize > indexPageSize || indexPageSize%c.SectorSize != 0 {
		return fmt.Errorf("l2: sector size %d does not divide the %d byte index page", c.SectorSize, indexPageSize)
	}
	if c.SizeBytes <= SuperBlockSize+int64(c.slotBytes()) {
		return fmt.Errorf("l2: size %d is too small", c.SizeBytes)
	}
	if c.RegionBlocks < 0 {
		return fmt.Errorf("l2: region blocks %d is negative", c.RegionBlocks)
	}
	if c.RegionBlocks == 0 {
		c.RegionBlocks = DefaultRegionBlocks
	}
	if c.MaxIOBytes <= 0 {
		c.MaxIOBytes = DefaultMaxIOBytes
	}
	// A cap below one slot would make a single block unreadable, and the
	// superblock is read through the same scratch on open.
	if min := maxInt(c.slotBytes(), SuperBlockSize); c.MaxIOBytes < min {
		c.MaxIOBytes = min
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return nil
}

// slotBytes is the file space one slot occupies: the stamp sector plus the data
// block it describes.
func (c *Config) slotBytes() int { return c.BlockSize + stampSize }

// slot is one cached block.
type slot struct {
	num   int
	blk   uint64
	valid uint32
	// version changes whenever the entry loses sectors or the slot is handed
	// to another block, and is mirrored in the slot stamp. A restored record
	// whose version the stamp disagrees with describes a state the disk never
	// had, so it is dropped rather than served.
	version uint64
	// verified is false for an entry restored from a snapshot that was not
	// closed cleanly. Such an entry has to prove itself against the slot stamp
	// before its data may be handed out.
	verified bool
	// stamped is true while the slot's stamp on disk is known to describe this
	// block and version under this session's generation, which lets repeated
	// partial writes of an entry skip rewriting it.
	stamped bool
}

// File is a fixed slot L2 cache held in a single file. A block is handed the
// slot its LBA region owns rather than the next one free, so the blocks of a
// region are always neighbours on disk and a range read of them is a streaming
// read rather than one transfer per block.
//
// Layout: superblock, then the slots, then the slot stamps, then the index
// region. Slots start at a fixed offset, so a session that gets a different
// budget addresses the slots it already has in exactly the same place; the stamp
// and index regions move with the slot count and are rewritten when they do. The
// superblock records the slot count its index region belongs to, which is what
// lets a later session find it, and the region size, because that decides which
// slot a block belongs in.
type File struct {
	store      Store
	slotSize   int
	sectorSize int
	secPerSlot int
	slotCount  int
	// maxIO is Config.MaxIOBytes clamped to the file. One transfer may cover
	// this many bytes, which is what sets how long a run of neighbouring slots
	// ReadBlocks can serve per IO.
	maxIO     int
	namespace string
	log       *slog.Logger

	indexPages int
	// snapshotSlots is the slot count the index region on disk belongs to. It
	// differs from slotCount after a budget change and until the next flush.
	snapshotSlots int
	generation    uint64
	fullMask      uint32

	// mu guards the index, the slots and the pages. Reads take it for read and
	// hold it for the whole store transfer, so reads of different blocks run in
	// parallel (one transfer per store shard) instead of queueing behind each
	// other; anything that changes a slot, a page or the ring takes it for write,
	// which is also what keeps a slot from being recycled under a read.
	mu         sync.RWMutex
	index      map[uint64]*slot
	slotsByNum []*slot

	// Region homing. A block belongs in the slot its LBA region owns:
	// num = home*regionBlocks + blk%regionBlocks. The blocks of one region are
	// therefore always a run of neighbouring slots, which is what makes a range
	// read of a region one streaming transfer instead of one transfer per block.
	//
	// homeOf says which home each region owns, freeHomes holds the ones no
	// region owns, homeLive marks the owned ones and hand sweeps them when a
	// new region needs a home — giving the second chance a home's reference bit
	// earns it. A home is recycled whole: homing a region over one that is
	// already there gives up every block that home held.
	regionBlocks int
	homes        int
	homeOf       map[uint64]int
	freeHomes    []int
	homeLive     []bool
	// homeUsed is how many slots each home holds. It is a hint for picking a
	// victim and for reporting the fill, never something a read or a write is
	// addressed by: a count that drifted would send an eviction to a home that
	// is not empty, which costs those blocks and nothing else.
	homeUsed []int
	// homeRef is the second chance a home gets: it is set by a read that was
	// actually served out of it and cleared by the eviction hand as it sweeps.
	// A home therefore survives one sweep after its last read, and the one
	// whose turn comes up with nothing having read it since the previous sweep
	// is the one given away — which is what keeps a region that is being read
	// resident instead of recycling it because its number came up.
	//
	// It is atomic and lives outside the lock because reads hold the index for
	// read: they run in parallel with each other, so the bit has to be set
	// without excluding them. That is still safe against the sweep, which runs
	// under the write lock: a writer cannot start while a reader holds the
	// index, so no read can set a bit between the sweep's first clear and its
	// last — every bit it has already cleared stays clear for that whole pass,
	// which is what makes the pass terminate.
	homeRef []atomic.Bool
	// hand is where the next sweep starts, and evictions counts how many homes
	// it has taken away: a number that keeps climbing says the cache is too
	// small for the working set, and one that settles says the working set fits
	// and the layout is stable.
	hand      int
	evictions atomic.Int64
	// dirty holds the index pages that changed since the last flush; dirtyAll
	// means every page has to be rewritten (Drop).
	dirty    map[int]struct{}
	dirtyAll bool
	// recovered, unverified and dropped describe how this session's snapshot
	// held up: how many slots the last Open restored, how many of those still
	// have to be checked against their slot stamp, and how many the stamps have
	// disproved since. A file that was closed cleanly reports zero unverified
	// and can never report a drop.
	recovered  int
	unverified int
	dropped    int

	// stampBuf and pageBuf are the scratch sectors the metadata is built in and
	// read into. Every user of them holds the write lock, so one buffer of each
	// serves all of them and neither a stamp nor an index page rewrite costs an
	// allocation. pageBuf is cleared before it is filled, because a record for a
	// slot that is not live has to read as zeros.
	stampBuf []byte
	pageBuf  []byte
}

// stampBufLocked returns the sector a stamp is built in or read into. The caller
// holds the write lock.
//
// Everything a stamp is compared or checksummed on — the magic and the block,
// generation, version and checksum fields — is written before the sector is used
// or written out, so the tail beyond them never has to be cleared: it can only
// ever hold bytes read out of this file's own stamp sectors.
func (f *File) stampBufLocked() []byte {
	if f.stampBuf == nil {
		f.stampBuf = make([]byte, stampSize)
	}
	return f.stampBuf
}

// pageBufLocked returns the index page buffer. The caller holds the write lock
// and clears it before filling it in.
func (f *File) pageBufLocked() []byte {
	if f.pageBuf == nil {
		f.pageBuf = make([]byte, indexPageSize)
	}
	return f.pageBuf
}

// Open opens (and if needed creates) the L2 file. When the file was closed
// cleanly under the same geometry and namespace, the index written by that
// close is loaded, so the cache is warm again instead of empty. A file that was
// not closed cleanly is loaded too: its entries are simply restored unverified
// (see the package comment).
func Open(cfg Config) (*File, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	slotSize := cfg.BlockSize
	slotCount, indexPages, fileSize, err := layout(cfg.SizeBytes, slotSize)
	if err != nil {
		return nil, err
	}
	// A home needs at least one slot and a file needs at least one home, so a
	// budget too small for the configured region size is given a smaller one
	// rather than no homing at all.
	regionBlocks := cfg.RegionBlocks
	if regionBlocks > slotCount {
		regionBlocks = slotCount
	}
	// The store sizes its scratch once and reads through it, so the cap is
	// clamped to the file it can never read past.
	maxIO := cfg.MaxIOBytes
	if int64(maxIO) > fileSize {
		maxIO = int(fileSize)
	}
	if maxIO < maxInt(slotSize, SuperBlockSize) {
		maxIO = maxInt(slotSize, SuperBlockSize)
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
		s, err := openStore(cfg.Path, fileSize, maxIO)
		if err != nil {
			return nil, err
		}
		store = s
	}
	// Counting from here on means every transfer the file issues goes through
	// the wrapper, including the superblock read below, so the counters describe
	// what the file really did rather than a list of paths someone remembered to
	// instrument.
	store = newStatStore(store)

	f := &File{
		store:      store,
		slotSize:   slotSize,
		sectorSize: cfg.SectorSize,
		secPerSlot: slotSize / cfg.SectorSize,
		slotCount:  slotCount,
		maxIO:      maxIO,
		namespace:  cfg.Namespace,
		log:        cfg.Logger,
		indexPages: indexPages,
		index:      make(map[uint64]*slot),
		slotsByNum: make([]*slot, slotCount),
		dirty:      make(map[int]struct{}),

		regionBlocks: regionBlocks,
		homes:        slotCount / regionBlocks,
	}
	f.fullMask = sectorMask(0, f.secPerSlot)
	f.initHomes()
	if err := f.initSuperBlock(); err != nil {
		store.Close()
		return nil, err
	}
	// Only now can the file be trimmed to the size this budget implies: the
	// index region of the previous snapshot can sit past that size, and it had
	// to be read first. A trimmed tail costs the part of the snapshot that no
	// longer fits, which the next flush writes back at its new place.
	if err := store.Truncate(fileSize); err != nil {
		store.Close()
		return nil, fmt.Errorf("l2: size store: %w", err)
	}
	cfg.Logger.Info("l2 opened", "path", cfg.Path, "slots", slotCount,
		"slot_size", slotSize, "max_io", maxIO, "namespace", shortHash(cfg.Namespace),
		"homes", f.homes, "region_blocks", f.regionBlocks,
		"recovered", f.recovered, "unverified", f.unverified)
	return f, nil
}

// layout sizes the file from the byte budget: whole slots from a fixed start,
// then the index pages holding one record per slot, then nothing. The budget is
// a ceiling, so a small one loses a slot rather than growing the file. Because
// the slots come first and are not sized by the index, the same slot number is
// at the same offset in every session — which is what lets a budget change keep
// the cached slots instead of discarding them.
func layout(sizeBytes int64, slotSize int) (slots, indexPages int, fileSize int64, err error) {
	slotBytes := int64(slotSize + stampSize)
	if sizeBytes <= SuperBlockSize+slotBytes {
		return 0, 0, 0, fmt.Errorf("l2: size %d is too small", sizeBytes)
	}
	// Estimate from the per-slot index share, then shrink until the rounded up
	// page count really fits.
	slots = int((sizeBytes - SuperBlockSize) / (slotBytes + indexPerSlotBytes))
	if slots < 1 {
		slots = 1
	}
	for slots > 0 {
		pages := pagesFor(slots)
		if total := SuperBlockSize + int64(slots)*slotBytes + int64(pages)*indexPageSize; total <= sizeBytes {
			return slots, pages, total, nil
		}
		slots--
	}
	return 0, 0, 0, fmt.Errorf("l2: size %d is too small", sizeBytes)
}

// pagesFor is the number of index pages a slot count needs.
func pagesFor(slots int) int { return (slots + recsPerPage - 1) / recsPerPage }

// indexOffset is where the index region of a file holding that many slots
// begins, plus the pages ahead of the one asked for. Slots come first, so this
// is the only place the slot count takes part in addressing.
func (f *File) indexOffset(slots, page int) int64 {
	return SuperBlockSize + int64(slots)*int64(f.slotSize+stampSize) + int64(page)*indexPageSize
}

// Namespace returns the identity the file is bound to.
func (f *File) Namespace() string { return f.namespace }

// Slots returns the number of slots in the file.
func (f *File) Slots() int { return f.slotCount }

// SlotSize returns the size of one slot in bytes.
func (f *File) SlotSize() int { return f.slotSize }

// UsedSlots returns the number of slots currently holding data.
func (f *File) UsedSlots() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.index)
}

// RegionBlocks returns how many consecutive blocks of the LUN one home holds.
func (f *File) RegionBlocks() int { return f.regionBlocks }

// Homes returns how many regions the file can hold at once. It is Slots()
// divided by RegionBlocks(), and any slots left over cannot be used at all:
// a region takes a whole run or none of it.
func (f *File) Homes() int { return f.homes }

// Evictions returns how many regions have had their home taken away since the
// file was opened, counting only the ones that still held blocks: a home handed
// over empty gives up nothing and is not reported here.
//
// Together with Slots it is the number that says whether the cache fits the
// working set. A cache whose regions fit stops recycling them entirely — the
// layout reaches a steady state and stays there — so a count that climbs for as
// long as the workload runs means the regions in use do not fit, and the size
// (or the region granularity) is what has to change rather than the read path.
func (f *File) Evictions() int64 { return f.evictions.Load() }

// RegionFill returns how many of the slots each region in the cache is using,
// one entry per region held.
//
// It exists because the region size is the one parameter of the layout that
// cannot be settled by reading the code: a region that is only ever partly
// filled pays for slots no other block may have, and how often that happens is
// a property of the workload, not of the file. Measuring the fill is what
// decides whether the size is right, and it is why the number is returned
// rather than summarised.
func (f *File) RegionFill() []int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	fill := make([]int, 0, len(f.homeOf))
	for _, h := range f.homeOf {
		fill = append(fill, f.homeUsed[h])
	}
	return fill
}

// Restored returns how many slots the last Open found in the snapshot, whether
// it was closed cleanly or not.
func (f *File) Restored() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.recovered
}

// Unverified returns how many restored slots still have to prove themselves
// against their slot stamp before they may serve data. It is zero for a file
// that was closed cleanly, and falls as the reads that touch those slots
// confirm or disprove them.
func (f *File) Unverified() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.unverified
}

// Dropped returns how many restored entries this session could not use, either
// because the slot stamp disagreed or because the slot was handed to another
// block before the entry was ever read. Each one would otherwise have been
// answered from a slot another write had already taken over, so a value that
// keeps growing says the snapshot on disk is not being flushed as often as the
// writes that follow it.
func (f *File) Dropped() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.dropped
}

// IOStats reports the physical IO this file has issued since it was opened. It
// is what tells a merged read of a run from the per block reads it replaced:
// see the type's own comment for why the counters are read without a lock.
func (f *File) IOStats() IOStats {
	if st, ok := f.store.(*statStore); ok {
		return st.stats()
	}
	// A caller that replaced the store (the concurrency tests do) is not
	// counting, and reporting zeroes is honest about that.
	return IOStats{}
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
		int(le32(buf[sbOffSectorSize:])) != f.sectorSize:
		f.log.Warn("l2: geometry changed, starting with an empty index")
		f.reset()
	case int(le32(buf[sbOffRegion:])) != f.regionBlocks:
		// The region size decides which slot a block belongs in, so records
		// written under another one are not places this session can serve from.
		f.log.Warn("l2: region size changed, discarding previous contents",
			"old", le32(buf[sbOffRegion:]), "new", f.regionBlocks)
		f.reset()
	case string(buf[sbOffNamespace:sbOffNamespace+32]) != string(ns[:]):
		f.log.Warn("l2: cache namespace changed, discarding previous contents",
			"old", hex.EncodeToString(buf[sbOffNamespace:sbOffNamespace+32]),
			"new", hex.EncodeToString(ns[:]))
		f.reset()
	case int(le32(buf[sbOffSlotCount:])) < 1:
		f.log.Warn("l2: superblock holds no slots, starting with an empty index")
		f.reset()
	default:
		// The generation has to come back even when the snapshot is discarded:
		// it is what tells this session's pages apart from the ones on disk.
		f.generation = le64(buf[sbOffGeneration:])
		// The index region sits after the slots, so the count the snapshot was
		// written with is what finds it — even when this session was given a
		// different budget than the session that wrote it.
		f.snapshotSlots = int(le32(buf[sbOffSlotCount:]))
		// A clean close wrote both the index and the marker after every slot was
		// final, so its records can be trusted as they are. Any other state means
		// slots may have changed since the last flush: the records are still
		// worth loading, but each one has to be confirmed by its slot stamp
		// before it may serve data.
		clean := le32(buf[sbOffState:]) == stateClean
		if err := f.loadIndex(); err != nil {
			return err
		}
		// The directory is derived from the slots rather than persisted, so it
		// is rebuilt here — after the records, because an entry the mapping
		// cannot home is dropped from the index.
		f.rebuildHomesLocked()
		f.recovered = len(f.index)
		if clean {
			for _, sl := range f.index {
				sl.verified = true
			}
			f.log.Info("l2: restored the index of the previous close", "slots", f.recovered)
			break
		}
		f.unverified = f.recovered
		f.log.Info("l2: previous session did not close cleanly; "+
			"restored entries will be checked against their slot stamps",
			"slots", f.recovered)
	}
	return f.writeSuperBlock(stateOpen)
}

// reset starts from an empty index under a fresh generation, which hides every
// page written by an earlier session.
func (f *File) reset() {
	f.generation = newGeneration()
	f.index = make(map[uint64]*slot)
	f.slotsByNum = make([]*slot, f.slotCount)
	f.initHomes()
	f.dirty = make(map[int]struct{})
	f.dirtyAll = false
	f.recovered = 0
	f.unverified = 0
	f.dropped = 0
	f.snapshotSlots = f.slotCount
}

// writeSuperBlock persists the header with the given snapshot state. The header
// is only useful once it is durable, so it is flushed here.
func (f *File) writeSuperBlock(state uint32) error {
	buf := make([]byte, SuperBlockSize)
	copy(buf[:8], superBlockMag)
	buf[8] = superBlockVer
	put32(buf[sbOffSlotSize:], uint32(f.slotSize))
	put32(buf[sbOffSectorSize:], uint32(f.sectorSize))
	put32(buf[sbOffSlotCount:], uint32(f.snapshotSlots))
	put64(buf[sbOffGeneration:], f.generation)
	ns := sha256.Sum256([]byte(f.namespace))
	copy(buf[sbOffNamespace:sbOffNamespace+32], ns[:])
	put32(buf[sbOffRegion:], uint32(f.regionBlocks))
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
	for page := 0; page < pagesFor(f.snapshotSlots); page++ {
		err := f.store.ReadAt(buf, f.indexOffset(f.snapshotSlots, page))
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// A smaller budget truncates the file, and with it the tail of the
			// region the previous snapshot lived in. Losing that part of the
			// snapshot is the expected cost of the shrink, not a failure.
			f.log.Debug("l2: index page beyond the end of the file", "page", page)
			continue
		}
		if err != nil {
			return fmt.Errorf("l2: read index page %d: %w", page, err)
		}
		if string(buf[:4]) != indexPageMag ||
			le64(buf[4:]) != f.generation ||
			le32(buf[12:]) != checksum(buf, 12) {
			continue
		}
		for i := 0; i < recsPerPage; i++ {
			num := page*recsPerPage + i
			// Beyond the snapshot is not part of it; beyond this session's slots
			// is a slot the smaller budget no longer has.
			if num >= f.snapshotSlots || num >= f.slotCount {
				break
			}
			rec := buf[indexHdrSize+i*recordSize:]
			valid := le32(rec[8:12]) & f.fullMask
			if valid == 0 {
				continue
			}
			f.insertLocked(num, le64(rec[0:8]), valid, le64(rec[12:20]))
		}
	}
	return nil
}

// insertLocked installs one recovered record. A snapshot holds one slot per
// block, so a block appearing twice means the older record came from a page
// written before the block moved; the record read last wins and the loser is
// detached so recycling cannot delete the winner's entry.
func (f *File) insertLocked(num int, blk uint64, valid uint32, version uint64) {
	if prev := f.index[blk]; prev != nil {
		if prev.num == num {
			return
		}
		f.slotsByNum[prev.num] = nil
	}
	sl := &slot{num: num, blk: blk, valid: valid, version: version}
	f.slotsByNum[num] = sl
	f.index[blk] = sl
}

// writeIndexPage rewrites one page of records from the in-memory index. Slots
// that are not live get a zeroed record, which is what makes a page rewrite
// drop the entries that were invalidated since the last flush.
func (f *File) writeIndexPage(page int) error {
	buf := f.pageBufLocked()
	// Zeroed first: a slot that is not live keeps its record untouched below, and
	// a record that reads as zeros is what tells a later load there is no entry
	// there. A recycled buffer would otherwise rewrite the previous page's
	// records into this one, and the page checksum would bless them.
	clear(buf)
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
		put64(rec[12:20], sl.version)
	}
	put32(buf[12:], checksum(buf, 12))
	if err := f.store.WriteAt(buf, f.indexOffset(f.slotCount, page)); err != nil {
		return fmt.Errorf("l2: write index page %d: %w", page, err)
	}
	return nil
}

// ReadSectors implements cache.L2.
//
// The index lock is taken for read and held across the transfer, so reads of
// different blocks proceed in parallel instead of sharing one I/O lane. The
// transfer is still covered: a slot can only be recycled by a writer, and a
// writer waits for the read lock to be released.
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
	mask := sectorMask(sec, n)
	// At most two passes: the first one may have to settle an entry restored
	// from a crash, which changes slot state and so needs the write lock.
	for attempt := 0; attempt < 2; attempt++ {
		f.mu.RLock()
		sl := f.index[blk]
		if sl == nil || sl.valid&mask != mask {
			f.mu.RUnlock()
			return false, nil
		}
		if sl.verified {
			err := f.store.ReadAt(dst[:n*f.sectorSize], f.slotOffset(sl.num, sec))
			if err == nil {
				// A read that was served is the only thing that tells the
				// eviction hand a region is being used rather than merely
				// resident, so it is marked here and nowhere else: a miss, a
				// partial entry or one that had to be confirmed first is not
				// evidence that anyone wants this home kept.
				f.homeRef[sl.num/f.regionBlocks].Store(true)
			}
			f.mu.RUnlock()
			if err != nil {
				return false, err
			}
			return true, nil
		}
		f.mu.RUnlock()
		f.confirmEntry(blk)
	}
	// Still unverified after confirming it: the entry is gone, so the read is a
	// plain miss rather than a guess.
	return false, nil
}

// ReadBlocks reads consecutive blocks starting at blk0 into dst, which has to
// hold at least nblk blocks, and returns how many blocks it served.
//
// The count is always a prefix of the request: a block that is absent, only
// partly valid, or still waiting to be confirmed against its stamp ends the
// run. A short count is not an error, because the caller is expected to carry
// on with ReadSectors — which is also what confirms the entry that stopped it.
//
// Neighbouring blocks whose slots are physically adjacent are covered by one
// transfer, up to maxIO bytes of it, and the whole range is served under a
// single read lock: a run that used to cost one lock acquisition, one lookup
// and one transfer per block costs one of each per run instead. Under the
// region layout that adjacency is by construction, so a read of a region is a
// streaming read; under the ring it holds for runs that were filled in order
// and have not been recycled yet.
func (f *File) ReadBlocks(blk0 uint64, nblk int, dst []byte) (int, error) {
	if nblk <= 0 {
		return 0, nil
	}
	if len(dst) < nblk*f.slotSize {
		return 0, io.ErrShortBuffer
	}
	f.mu.RLock()
	defer f.mu.RUnlock()

	served := f.readyRunLocked(blk0, nblk)
	perIO := f.slotsPerIO()
	for i := 0; i < served; {
		first := f.index[blk0+uint64(i)]
		run := 1
		for run < served-i && run < perIO {
			// Compared against the first slot of the run rather than the one
			// before it, because adjacency is exactly a step in slot numbers.
			if f.index[blk0+uint64(i+run)].num != first.num+run {
				break
			}
			run++
		}
		out := dst[i*f.slotSize : (i+run)*f.slotSize]
		if err := f.readRunLocked(out, first.num, run); err != nil {
			// What was already read stays served: the count is a prefix, so
			// the caller can keep those blocks and re-read only the rest.
			return i, err
		}
		// Serving the run is what keeps the region it came from resident. A run
		// can straddle two homes — the last slot of one followed by the first
		// slot of the next — so every home it covers is marked, not just the
		// one it started in.
		for n := 0; n < run; n++ {
			f.homeRef[(first.num+n)/f.regionBlocks].Store(true)
		}
		i += run
	}
	return served, nil
}

// readRunLocked serves run adjacent slots whose data areas follow each other at
// the slot stride. Since stamps live in their own region, nothing sits between
// the slots and the run is one transfer straight into out — no staging buffer,
// no copy.
func (f *File) readRunLocked(out []byte, first, run int) error {
	return f.store.ReadAt(out, f.slotDataOffset(first))
}

// readyRunLocked returns how many blocks from blk0 are present in full and may
// be served without changing any state. An entry that still has to prove itself
// against its stamp stops the run rather than being skipped over, because
// confirming it takes the write lock — the caller's per block fallback does it.
func (f *File) readyRunLocked(blk0 uint64, nblk int) int {
	n := 0
	for n < nblk {
		sl := f.index[blk0+uint64(n)]
		if sl == nil || sl.valid != f.fullMask || !sl.verified {
			break
		}
		n++
	}
	return n
}

// slotsPerIO is how many adjacent slots one transfer may cover. Neighbouring
// slots are neighbouring bytes, so it is the transfer cap divided by the slot
// size, and never less than one: a cap below one slot would make a block
// unreadable, and Open does not allow one.
func (f *File) slotsPerIO() int {
	if n := f.maxIO / f.slotSize; n > 1 {
		return n
	}
	return 1
}

// confirmEntry settles the entry for blk when it came back from a snapshot that
// was not closed cleanly. Verification changes slot state, so it runs under the
// write lock: an unconfirmed entry may not serve data until its stamp agrees.
func (f *File) confirmEntry(blk uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sl := f.index[blk]
	if sl == nil || sl.verified {
		return
	}
	// A stamp that disagrees means the slot does not hold this block under this
	// session: an entry restored from a crash, pointing at a slot a later write
	// recycled before the snapshot was flushed. Dropping it makes the read a
	// plain miss instead of serving another block's bytes.
	f.ensureVerifiedLocked(sl)
}

// verifyLocked decides whether an unverified entry really describes its slot,
// by reading the stamp the slot was last written under. Both outcomes are
// counted, so a caller can tell a recovery that worked from one that is paying
// for a snapshot the session never got to flush.
func (f *File) verifyLocked(sl *slot) (bool, error) {
	st := f.stampBufLocked()
	if err := f.store.ReadAt(st, f.stampOffset(sl.num)); err != nil {
		return false, err
	}
	if string(st[:8]) != stampMag ||
		le64(st[stampOffBlk:]) != sl.blk ||
		le64(st[stampOffGen:]) != f.generation ||
		le64(st[stampOffVer:]) != sl.version ||
		le32(st[stampOffCRC:]) != checksum(st, stampOffCRC) {
		f.settleUnverifiedLocked(false)
		f.log.Debug("l2: stamp disproved a restored entry",
			"block", sl.blk, "slot", sl.num, "version", sl.version)
		return false, nil
	}
	f.settleUnverifiedLocked(true)
	sl.verified = true
	sl.stamped = true
	return true, nil
}

// settleUnverifiedLocked retires one restored entry once its fate is known:
// good when the slot stamp agreed with it, otherwise when it turned out the slot
// no longer holds it. The counters only ever move for entries this Open
// restored, so what they report stays tied to the snapshot that was loaded.
func (f *File) settleUnverifiedLocked(good bool) {
	if f.unverified > 0 {
		f.unverified--
	}
	if !good {
		f.dropped++
	}
}

// forgetSlotLocked drops one entry, detached rather than deleted so a slot
// another entry already claimed is not cleared by accident.
func (f *File) forgetSlotLocked(blk uint64, sl *slot) {
	if cur := f.index[blk]; cur == sl {
		delete(f.index, blk)
	}
	if f.slotsByNum[sl.num] == sl {
		f.slotsByNum[sl.num] = nil
		f.homeUsed[sl.num/f.regionBlocks]--
	}
	// The next index page rewrite has to drop the record too.
	f.markDirtyLocked(sl.num)
	sl.valid = 0
}

// writeStampLocked records the block a slot holds now. It is written after the
// data, so a slot whose data write was torn keeps the previous stamp and fails
// verification instead of being taken for the block it used to describe.
func (f *File) writeStampLocked(sl *slot) error {
	st := f.stampBufLocked()
	copy(st[:8], stampMag)
	put64(st[stampOffBlk:], sl.blk)
	put64(st[stampOffGen:], f.generation)
	put64(st[stampOffVer:], sl.version)
	put32(st[stampOffCRC:], checksum(st, stampOffCRC))
	if err := f.store.WriteAt(st, f.stampOffset(sl.num)); err != nil {
		return err
	}
	sl.stamped = true
	sl.verified = true
	return nil
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
	if sl != nil && !f.ensureVerifiedLocked(sl) {
		sl = nil
	}
	if sl == nil {
		sl = f.allocSlotLocked(blk)
	}
	if err := f.store.WriteAt(src[:n*f.sectorSize], f.slotOffset(sl.num, sec)); err != nil {
		return err
	}
	if !sl.stamped {
		if err := f.writeStampLocked(sl); err != nil {
			return err
		}
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
	mask := sectorMask(sec, n)
	if sl.valid&mask == 0 {
		return
	}
	if !f.ensureVerifiedLocked(sl) {
		return
	}
	sl.valid &^= mask
	// The version bump has to reach the slot, and it has to get there before the
	// index page does: a snapshot that predates this invalidation then fails to
	// match its stamp, so a crash cannot bring the sectors back.
	sl.version++
	if err := f.writeStampLocked(sl); err != nil {
		f.log.Warn("l2: stamp an invalidated slot", "block", blk, "slot", sl.num, "err", err)
	}
	if sl.valid == 0 {
		// Every sector is gone, so the slot is free again. It goes through the
		// same path as any other release, so the home it belongs to is told.
		f.forgetSlotLocked(blk, sl)
		return
	}
	f.markDirtyLocked(sl.num)
}

// Drop forgets every slot without touching the slot contents, by moving to a
// new generation: every index page on disk was written under the old one, so
// none of them can be loaded again. Moving the generation and syncing it is one
// sector instead of rewriting the whole index, and it cannot be lost by a crash
// the way a lazily rewritten index could.
func (f *File) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.index = make(map[uint64]*slot)
	f.slotsByNum = make([]*slot, f.slotCount)
	f.initHomes()
	f.dirty = make(map[int]struct{})
	f.dirtyAll = false
	f.recovered = 0
	f.unverified = 0
	f.dropped = 0
	f.generation = newGeneration()
	if err := f.writeSuperBlock(stateOpen); err != nil {
		f.log.Warn("l2: record a drop in the superblock", "err", err)
	}
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
	// The index region follows the slots, so a different budget moves it. When
	// it moved, every page has to be written where it now belongs rather than
	// only the ones that changed; losing that to a crash is safe either way,
	// because until the header names the new region the old one still holds the
	// snapshot this session loaded.
	moved := f.snapshotSlots != f.slotCount
	written := 0
	if f.dirtyAll || moved {
		for page := 0; page < f.indexPages; page++ {
			if err := f.writeIndexPage(page); err != nil {
				return err
			}
			written++
		}
	} else {
		for page := range f.dirty {
			if page >= f.indexPages {
				continue
			}
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
		if moved {
			f.snapshotSlots = f.slotCount
		}
	}
	if !clean {
		return nil
	}
	return f.writeSuperBlock(stateClean)
}

// allocSlotLocked hands a block the slot its region owns, claiming a home for
// that region first if it does not have one. Because the slot is derived from
// the block, it never steers a block into a slot the region's own neighbours are
// not already in: the blocks of a region fill their home in order, and a run of
// them stays a run on disk until the home is recycled.
func (f *File) allocSlotLocked(blk uint64) *slot {
	num := f.homeSlotLocked(blk)
	version := uint64(1)
	if old := f.slotsByNum[num]; old != nil {
		// The slot still holds another block. Homing makes this rare — a home
		// belongs to one region, so the slot is either free or holds the block
		// that belongs in it — but a record restored from a layout this session
		// cannot place can leave one behind.
		delete(f.index, old.blk)
		version = old.version + 1
		if !old.verified {
			// The slot is being handed to another block while the entry it
			// still holds was never confirmed: the snapshot just lost it, which
			// is what the counters are there to report.
			f.settleUnverifiedLocked(false)
		}
	} else if v, ok := f.readStampVersionLocked(num); ok {
		// The slot was last written by an earlier session, whose stamp is still
		// there. Starting above it keeps this block's record from matching it.
		version = v + 1
	}
	if f.slotsByNum[num] == nil {
		f.homeUsed[num/f.regionBlocks]++
	}
	sl := &slot{num: num, blk: blk, version: version}
	f.slotsByNum[num] = sl
	f.index[blk] = sl
	f.markDirtyLocked(num)
	return sl
}

// homeSlotLocked is the slot a block belongs in: its region's home plus the
// block's own place in that region.
func (f *File) homeSlotLocked(blk uint64) int {
	reg := blk / uint64(f.regionBlocks)
	h, ok := f.homeOf[reg]
	if !ok {
		h = f.claimHomeLocked()
		f.homeOf[reg] = h
	}
	return h*f.regionBlocks + int(blk%uint64(f.regionBlocks))
}

// claimHomeLocked returns a home for a region that does not have one, preferring
// one that was never used over recycling a region that is in the cache. The free
// ones are handed out in order, which is also the order the eviction hand sweeps
// them: a sequential walk over the LUN then fills the cache in one pass and
// gives up its oldest region first.
func (f *File) claimHomeLocked() int {
	if len(f.freeHomes) > 0 {
		h := f.freeHomes[0]
		f.freeHomes = f.freeHomes[1:]
		f.homeLive[h] = true
		return h
	}
	return f.evictHomeLocked()
}

// evictHomeLocked takes a home away from the region that holds it, giving up
// whatever was in it.
//
// A home with nothing left in it goes first, because it costs nothing: a region
// whose blocks were all invalidated still owns its home, and taking a full one
// ahead of it would throw away data to no purpose.
//
// Only when they are all holding something does the hand decide, and it decides
// like a clock: every home it passes whose reference bit is set is cleared and
// left alone for this round, and the first one it finds with the bit clear is
// the one given away. The bit is set by the reads a home serves, so what the
// hand gives away is a home nothing has read since the previous sweep — under a
// workload whose hot regions keep being read, those are the ones that are not
// hot, which is the whole of the steady state the layout is after: the regions
// being read stay put and stop being recycled at all.
//
// A pass always terminates: it runs under the write lock, so no read can set a
// bit while it sweeps, and every bit it has cleared for this pass is still clear
// when it comes back around to it.
func (f *File) evictHomeLocked() int {
	for i := 0; i < f.homes; i++ {
		h := (f.hand + i) % f.homes
		if f.homeUsed[h] != 0 {
			continue
		}
		f.giveHomeLocked(h)
		return h
	}
	for {
		h := f.hand
		f.hand = (h + 1) % f.homes
		if f.homeRef[h].Swap(false) {
			continue
		}
		f.giveHomeLocked(h)
		return h
	}
}

// giveHomeLocked hands one home to the region that has just claimed it. The
// reference bit is cleared rather than inherited: the home is about to hold
// another region, and the reads that set it were reads of the region leaving.
//
// Only a home that still holds blocks counts as an eviction. Handing over one
// that is already empty gives up nothing, and counting it would make the number
// climb for a cache that is only ever reclaiming the room a region left behind.
func (f *File) giveHomeLocked(h int) {
	if f.homeUsed[h] != 0 {
		f.evictions.Add(1)
	}
	f.releaseHomeLocked(h)
	f.homeRef[h].Store(false)
	f.homeLive[h] = true
}

// releaseHomeLocked drops every block the home holds, so it can be handed to
// another region. The blocks are detached from the index rather than deleted
// blindly, and the version their slot carries is left where it is: the next
// write into the slot reads it back and starts above it, so a record this
// session flushed can never be matched by a stamp an earlier one left.
func (f *File) releaseHomeLocked(h int) {
	base := h * f.regionBlocks
	for i := 0; i < f.regionBlocks; i++ {
		sl := f.slotsByNum[base+i]
		if sl == nil {
			continue
		}
		if reg := sl.blk / uint64(f.regionBlocks); f.homeOf[reg] == h {
			delete(f.homeOf, reg)
		}
		if !sl.verified {
			f.settleUnverifiedLocked(false)
		}
		f.forgetSlotLocked(sl.blk, sl)
	}
	f.homeLive[h] = false
}

// initHomes starts with an empty directory: no region owns anything and every
// home is free.
func (f *File) initHomes() {
	f.homeOf = make(map[uint64]int, f.homes)
	f.freeHomes = make([]int, 0, f.homes)
	f.homeLive = make([]bool, f.homes)
	f.homeUsed = make([]int, f.homes)
	f.homeRef = make([]atomic.Bool, f.homes)
	f.hand = 0
	for h := 0; h < f.homes; h++ {
		f.freeHomes = append(f.freeHomes, h)
	}
}

// rebuildHomesLocked reads the directory back out of the slots a snapshot
// restored, which is the only thing that outlives a crash: a slot's number says
// which home and which region it belongs to, so nothing about homing has to be
// persisted.
//
// An entry whose slot does not sit where its block says it should cannot be
// homed, and serving it would break the one invariant the read path rests on. It
// is dropped instead: an older region size, or a slot a crash left behind from
// one, costs those entries rather than the mapping.
func (f *File) rebuildHomesLocked() {
	f.initHomes()
	R := uint64(f.regionBlocks)
	for num, sl := range f.slotsByNum {
		if sl == nil {
			continue
		}
		if uint64(num%f.regionBlocks) != sl.blk%R {
			f.forgetSlotLocked(sl.blk, sl)
			continue
		}
		h := num / f.regionBlocks
		f.homeLive[h] = true
		f.homeUsed[h]++
		f.homeOf[sl.blk/R] = h
	}
	f.freeHomes = f.freeHomes[:0]
	for h := 0; h < f.homes; h++ {
		if !f.homeLive[h] {
			f.freeHomes = append(f.freeHomes, h)
		}
	}
}

// ensureVerifiedLocked prepares an entry for a state change. An entry restored
// from a crash must not be written to before it is confirmed, because writing
// its stamp would overwrite the version that tells this session's snapshot
// apart from the one on disk, and an invalidation the killed session applied
// would then look like it never happened.
func (f *File) ensureVerifiedLocked(sl *slot) bool {
	if sl.verified {
		return true
	}
	ok, err := f.verifyLocked(sl)
	if err != nil {
		f.log.Warn("l2: verify a restored slot", "slot", sl.num, "err", err)
	}
	if !ok {
		f.forgetSlotLocked(sl.blk, sl)
		return false
	}
	return true
}

// readStampVersionLocked returns the version the slot on disk carries, so a
// slot handed to a new block can pick one the disk cannot confuse with what it
// still holds.
func (f *File) readStampVersionLocked(num int) (uint64, bool) {
	st := f.stampBufLocked()
	if err := f.store.ReadAt(st, f.stampOffset(num)); err != nil {
		return 0, false
	}
	if string(st[:8]) != stampMag || le32(st[stampOffCRC:]) != checksum(st, stampOffCRC) {
		return 0, false
	}
	return le64(st[stampOffVer:]), true
}

func (f *File) markDirtyLocked(num int) {
	if f.dirtyAll {
		return
	}
	f.dirty[num/recsPerPage] = struct{}{}
}

// slotDataOffset is the first byte of a slot's data. The slots start right after
// the super block and follow each other at the slot size, so two slots with
// consecutive numbers hold two adjacent runs of bytes, with no metadata between
// them — which is what lets ReadBlocks serve a run in one transfer.
//
// It does not depend on the budget, so a session that changes the budget keeps
// the slots the previous one wrote, at the same offsets.
func (f *File) slotDataOffset(num int) int64 {
	return SuperBlockSize + int64(num)*int64(f.slotSize)
}

// stampOffset is where a slot's stamp lives. Every stamp is in one region after
// the slot area, so a slot's data is not interrupted by its metadata and the
// whole region can be walked in order.
//
// It is addressed through this session's slot count rather than the snapshot's.
// A budget change therefore moves the region, and entries restored from the
// previous budget fail to match the stamp read for them: that costs the restored
// entries, the same way a relocated index region already costs the pages that no
// longer fit, and the next flush writes every stamp at its new place.
func (f *File) stampOffset(num int) int64 {
	return SuperBlockSize + int64(f.slotCount)*int64(f.slotSize) + int64(num)*stampSize
}

// slotOffset is the start of the sector run inside a slot's data area.
func (f *File) slotOffset(num, sec int) int64 {
	return f.slotDataOffset(num) + int64(sec*f.sectorSize)
}

// secPerSlotMax bounds the valid mask: one bit per sector of a slot, held in a
// uint32, is how the index records which parts of a slot hold data.
const secPerSlotMax = 32

// sectorMasks[n] is the mask of n consecutive sectors starting at sector zero,
// so a range starting at sec is that pattern shifted up. Every read, write and
// invalidation builds one of these, which makes it a lookup instead of a loop
// over the sectors of the range.
var sectorMasks = buildSectorMasks()

func buildSectorMasks() [secPerSlotMax + 1]uint32 {
	var m [secPerSlotMax + 1]uint32
	for i := 1; i <= secPerSlotMax; i++ {
		m[i] = m[i-1] | 1<<uint(i-1)
	}
	return m
}

func sectorMask(sec, n int) uint32 {
	if sec < 0 || n <= 0 || sec+n > secPerSlotMax {
		return 0
	}
	return sectorMasks[n] << uint(sec)
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
	if size < 0 {
		return errors.New("l2: negative store size")
	}
	if int64(len(m.data)) == size {
		return nil
	}
	// Resizing a real file keeps what is already there and zero fills the part
	// it grows by, so the store has to do the same.
	grown := make([]byte, size)
	copy(grown, m.data)
	m.data = grown
	return nil
}

// Sync implements Store.
func (m *MemoryStore) Sync() error { return nil }

// Close implements Store.
func (m *MemoryStore) Close() error { return nil }
