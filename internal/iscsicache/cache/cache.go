// Package cache implements the multi-level read cache of the iSCSI read-cache
// proxy: an L1 in-memory block cache with per-sector validity, an optional L2
// file-backed cache, S3-FIFO eviction, asynchronous block fills and
// sequential-scan admission control.
//
// The cache is read-only: writes never populate an entry, they invalidate it.
// Entries therefore carry a valid bitmap only, never a dirty bitmap.
//
// A miss is served straight from the backing store and the sectors it read are
// handed to a fill worker, so the read never waits for an entry to be allocated
// and installed. Fills and invalidations are ordered by a per-shard epoch: a
// fill captures the epoch before it reads and only installs while the shard
// still carries it, so a write's invalidation either drops the fill or runs
// before it — it can never leave pre-write data behind.
package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Defaults for the cache geometry.
const (
	DefaultBlockSize  = 64 * 1024
	DefaultSectorSize = 4096
	DefaultShards     = 256
)

// Defaults for the asynchronous fill queue.
const (
	// DefaultFillWorkers is how many goroutines install queued fills.
	DefaultFillWorkers = 4
	// DefaultFillQueueDepth bounds the number of queued fills.
	DefaultFillQueueDepth = 1024
)

// scanBuckets is how many independent sequential-scan states the detector
// keeps, one per 1 MiB region (scanBucketShift). Fanning the state out keeps a
// scan's bookkeeping off other streams' read path.
const (
	scanBuckets     = 64
	scanBucketShift = 20
)

// Source is the backing store the cache reads from. ReadAt must fill exactly
// len(p) bytes at byte offset off. The cache always aligns both the offset and
// the length to its sector size.
type Source interface {
	ReadAt(ctx context.Context, off int64, p []byte) error
}

// L2 is the secondary cache. Sectors are addressed by cache block index and by
// sector index inside that block.
type L2 interface {
	// ReadSectors fills dst with n sectors starting at sector sec of block blk.
	// ok is false when any of the requested sectors is absent, in which case
	// dst content is unspecified.
	ReadSectors(blk uint64, sec, n int, dst []byte) (ok bool, err error)
	// ReadBlocks fills dst, which holds nblk blocks, with consecutive blocks
	// starting at blk0 and returns how many it served. The count is a prefix of
	// the request, and a short count is not an error: the block it stopped on
	// may be absent or may still be waiting to be confirmed, so the caller
	// carries on with ReadSectors for it — which is also what settles it.
	//
	// It exists because the block granularity and the transfer granularity are
	// not the same question: what a run of neighbouring blocks costs depends on
	// how the file laid them out, and only the file can answer that.
	ReadBlocks(blk0 uint64, nblk int, dst []byte) (served int, err error)
	// WriteSectors stores n sectors starting at sector sec of block blk.
	WriteSectors(blk uint64, sec, n int, src []byte) error
	// InvalidateSectors drops n sectors starting at sector sec of block blk.
	InvalidateSectors(blk uint64, sec, n int)
	// Drop forgets every sector. It backs Cache.Drop, which a whole-device
	// invalidation (UNMAP, WRITE SAME) uses to keep L2 from serving sectors a
	// write has made stale.
	Drop()
}

// Config configures a Cache.
type Config struct {
	BlockSize  int
	SectorSize int
	L1Bytes    int64
	Shards     int

	// NewQuotaPct is the percentage of each shard reserved for the S3-FIFO new
	// queue. Defaults to 10.
	NewQuotaPct int
	// HighWatermark/LowWatermark drive eviction: a shard evicts down to the low
	// watermark once it exceeds the high watermark. Defaults 0.9 / 0.7.
	HighWatermark float64
	LowWatermark  float64

	// ScanEnabled turns sequential-scan admission control on.
	ScanEnabled bool
	// ScanThreshold is the number of consecutive "start == previous end"
	// requests that mark a stream as a scan. Defaults to 32.
	ScanThreshold int

	// FillWorkers is how many goroutines consume the fill queue. Defaults to
	// DefaultFillWorkers.
	FillWorkers int
	// FillQueueDepth bounds the fill queue. Defaults to DefaultFillQueueDepth. A
	// full queue drops a fill, never a read.
	FillQueueDepth int

	L2     L2
	Logger *slog.Logger
}

func (c *Config) normalize() error {
	if c.BlockSize == 0 {
		c.BlockSize = DefaultBlockSize
	}
	if c.SectorSize == 0 {
		c.SectorSize = DefaultSectorSize
	}
	if c.Shards <= 0 {
		c.Shards = DefaultShards
	}
	if c.NewQuotaPct <= 0 {
		c.NewQuotaPct = 10
	}
	if c.HighWatermark <= 0 {
		c.HighWatermark = 0.9
	}
	if c.LowWatermark <= 0 {
		c.LowWatermark = 0.7
	}
	if c.ScanThreshold <= 0 {
		c.ScanThreshold = 32
	}
	if c.FillWorkers <= 0 {
		c.FillWorkers = DefaultFillWorkers
	}
	if c.FillQueueDepth <= 0 {
		c.FillQueueDepth = DefaultFillQueueDepth
	}
	if c.BlockSize%c.SectorSize != 0 {
		return fmt.Errorf("cache: block size %d is not a multiple of sector size %d", c.BlockSize, c.SectorSize)
	}
	if n := c.BlockSize / c.SectorSize; n < 1 || n > 32 {
		return fmt.Errorf("cache: %d sectors per block is out of range (1..32)", n)
	}
	if c.L1Bytes < int64(c.BlockSize*c.Shards) {
		return fmt.Errorf("cache: l1 size %d is too small for %d shards", c.L1Bytes, c.Shards)
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Stats holds cache counters.
type Stats struct {
	Reads         atomic.Int64
	L1Hits        atomic.Int64
	PartialHits   atomic.Int64
	L2Hits        atomic.Int64
	BackendReads  atomic.Int64
	Fills         atomic.Int64
	Evictions     atomic.Int64
	Invalidations atomic.Int64
	ScanBypass    atomic.Int64
	// RequestHits counts read commands whose data came entirely from the cache
	// (no backend read at all). It is the counter a hit rate can be computed
	// from: the other hit counters are per block segment or per sector run.
	RequestHits atomic.Int64
	// FillSkips counts streamed reads that were forwarded without being
	// installed because the stream had already covered more ground than L1 can
	// hold. See Cache.FillAdmit.
	FillSkips atomic.Int64
}

// Snapshot is a plain copy of the counters, for logging.
//
// L1UsedBytes/L1LimitBytes report how much of the L1 memory budget currently
// holds cached blocks and what the budget is, so a caller can show occupancy
// next to the hit counters. L1ResidentBytes is the estimate of what the L1
// index really costs the collector; see Cache.ResidentBytes.
type Snapshot struct {
	Reads, L1Hits, PartialHits, L2Hits, BackendReads, Fills, Evictions, Invalidations, ScanBypass int64
	RequestHits                                                                                   int64
	FillSkips                                                                                     int64
	L1UsedBytes                                                                                   int64
	L1LimitBytes                                                                                  int64
	L1ResidentBytes                                                                               int64
}

func (s *Stats) snapshot() Snapshot {
	return Snapshot{
		Reads: s.Reads.Load(), L1Hits: s.L1Hits.Load(), PartialHits: s.PartialHits.Load(),
		L2Hits: s.L2Hits.Load(), BackendReads: s.BackendReads.Load(), Fills: s.Fills.Load(),
		Evictions: s.Evictions.Load(), Invalidations: s.Invalidations.Load(), ScanBypass: s.ScanBypass.Load(),
		RequestHits: s.RequestHits.Load(), FillSkips: s.FillSkips.Load(),
	}
}

// Cache is an L1+L2 read cache over a block device.
type Cache struct {
	blockSize   int
	sectorSize  int
	sectorsPer  int
	totalBlocks int

	shards []*shard
	// shardMask/shardsPow2 pick how shard() indexes shards: a mask when the count
	// is a power of two (the default geometry, and the one the agent builds) and a
	// modulo otherwise. Every block lookup goes through shard(), and the mask keeps
	// a 64-bit division off that path. Both are written once in New, before the
	// cache is reachable from another goroutine, and are never mutated after.
	shardMask  uint64
	shardsPow2 bool
	scan       *scanDetector
	l2         L2
	log        *slog.Logger

	// l2Sem bounds how many L2 populations may be in flight and the fence orders
	// them against an invalidation, which must wait for the populations already
	// in flight before it drops a sector they carry pre-write data for. Both are
	// unused when the cache has no L2.
	l2Sem chan struct{}
	fence l2Fence

	// l2Gate orders a read's L2 lookup against an invalidation. A write holds it
	// exclusively across its epoch bump and its L2 drop, so a sector a read takes
	// out of L2 is either gone from L2 (the read started after the drop) or the
	// fill it feeds is rejected by the epoch check.
	l2Gate sync.RWMutex

	// l2InvCh and l2InvWG implement InvalidateAsync: the L1 half of an
	// invalidation runs synchronously on the caller, the L2 half is queued here
	// and drained by l2InvWorker. They are nil/unused when the cache has no L2.
	l2InvCh chan l2InvReq
	l2InvWG sync.WaitGroup

	// l2InvN is how many deferred L2 drops are queued or in progress. It is
	// raised under l2Gate before an invalidation bumps L1 and lowered by the
	// worker once the drop lands. ReadCachedBoth holds l2Gate for read across its
	// L2 lookups and skips L2 while it is non-zero: a non-zero count means some
	// completed write's sector is still in L2, which a read would otherwise serve
	// stale.
	l2InvN atomic.Int64

	// The fill queue decouples the read path from populating L1. fillCh is
	// deliberately never closed: a racing enqueue after Close would otherwise
	// panic, and a fill that runs into a shut cache only costs a future backing
	// read. fillPending counts the fills queued or being installed; Wait and
	// Close block on it until the queue has drained (fillCond), and exitCh then
	// tells the workers to leave.
	fillCh chan fillReq
	fillWG sync.WaitGroup
	closed atomic.Bool
	exitCh chan struct{}

	// fillPending is incremented before a fill is queued and decremented once a
	// worker has installed it, so it is zero exactly when the queue is empty. The
	// increment is a plain atomic: the read path never takes fillMu.
	fillPending atomic.Int64
	fillMu      sync.Mutex
	fillCond    *sync.Cond

	stats Stats
}

// l2WriteConcurrency bounds the background L2 populations. L2 is a secondary
// cache: when this many writes are already outstanding a miss simply skips
// populating L2 instead of delaying the read.
const l2WriteConcurrency = 8

// defaultL2InvQueueDepth bounds the deferred L2 invalidations queued by
// InvalidateAsync. A full queue drops the L2 half of an invalidation: L1 — the
// level a caller that answers locally reads from — is already invalidated
// synchronously, so a dropped L2 drop only leaves a sector the deferred path was
// never going to serve.
const defaultL2InvQueueDepth = 1024

// l2InvReq is one deferred L2 invalidation: the byte range and the fence target
// captured after its L1 bumps, so the worker waits out exactly the populations
// that could carry pre-write data.
type l2InvReq struct {
	off, length int64
	target      uint64
}

// New creates a Cache.
func New(cfg Config) (*Cache, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	total := cfg.L1Bytes / int64(cfg.BlockSize)
	if total < int64(cfg.Shards)*4 {
		total = int64(cfg.Shards) * 4
	}
	per := int(total / int64(cfg.Shards))
	per = per / 4 * 4
	if per < 8 {
		per = 8
	}

	c := &Cache{
		blockSize:   cfg.BlockSize,
		sectorSize:  cfg.SectorSize,
		sectorsPer:  cfg.BlockSize / cfg.SectorSize,
		totalBlocks: int(total),
		l2:          cfg.L2,
		log:         cfg.Logger,
		scan:        newScanDetector(cfg.ScanEnabled, cfg.ScanThreshold),
		fillCh:      make(chan fillReq, cfg.FillQueueDepth),
		exitCh:      make(chan struct{}),
	}
	c.fence.cond = sync.NewCond(&c.fence.mu)
	c.fillCond = sync.NewCond(&c.fillMu)
	c.shards = make([]*shard, cfg.Shards)
	for i := range c.shards {
		c.shards[i] = newShard(c, per, cfg.NewQuotaPct, cfg.HighWatermark, cfg.LowWatermark)
	}
	c.shardsPow2 = cfg.Shards&(cfg.Shards-1) == 0
	c.shardMask = uint64(cfg.Shards) - 1
	if c.l2 != nil {
		c.l2Sem = make(chan struct{}, l2WriteConcurrency)
		c.l2InvCh = make(chan l2InvReq, defaultL2InvQueueDepth)
		c.l2InvWG.Add(1)
		go c.l2InvWorker()
	}
	for i := 0; i < cfg.FillWorkers; i++ {
		c.fillWG.Add(1)
		go c.fillWorker()
	}
	return c, nil
}

// Close stops the fill workers once the queue has drained and waits for the L2
// populations they started. It must run before the L2 store is closed, or a
// worker would write to a closed file.
func (c *Cache) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	// No new fills are accepted now, so once the pending count reaches zero the
	// queue is empty and the workers can be told to leave.
	c.waitFills()
	close(c.exitCh)
	c.fillWG.Wait()
	// The L2 invalidation worker only drops in-memory index entries, so it needs no
	// drain before the store closes; stopping it here is enough.
	c.l2InvWG.Wait()
	if c.l2 != nil {
		c.fence.wait(c.fence.target())
	}
}

// Wait blocks until the fills queued so far have been installed and their L2
// populations have completed. It is a synchronisation point for tests; the read
// path never calls it.
func (c *Cache) Wait() {
	c.waitFills()
	if c.l2 != nil {
		c.fence.wait(c.fence.target())
	}
}

// waitFills blocks until every fill queued before the call has been installed.
//
// It waits on the pending count rather than on queue sentinels: a sentinel per
// worker only proves the fill was dequeued, because a fast worker can consume a
// later sentinel while another is still installing an earlier fill.
func (c *Cache) waitFills() {
	c.fillMu.Lock()
	defer c.fillMu.Unlock()
	for c.fillPending.Load() != 0 {
		c.fillCond.Wait()
	}
}

// BlockSize reports the cache block size in bytes.
func (c *Cache) BlockSize() int { return c.blockSize }

// SectorSize reports the cache sector size in bytes.
func (c *Cache) SectorSize() int { return c.sectorSize }

// Stats returns a snapshot of the counters, including L1 occupancy.
func (c *Cache) Stats() Snapshot {
	s := c.stats.snapshot()
	blocks, slots := c.census()
	s.L1UsedBytes = blocks * int64(c.blockSize)
	s.L1LimitBytes = int64(c.totalBlocks) * int64(c.blockSize)
	s.L1ResidentBytes = c.residentBytes(blocks, slots)
	return s
}

// census walks the shards once and returns how many blocks L1 holds and how many
// FIFO slots its queues are holding.
//
// Usage, ResidentBytes and Stats need both numbers, so they take them in one
// pass: each used to walk the shards on its own, which made one status poll cost
// two lock acquisitions per shard (512 at the default geometry) and made the
// agent's 5-second sampler a periodic spike proportional to the number of
// mounted libraries. Sampling both under one hold per shard halves that and
// changes no number: the two counts are read from the same instant, where they
// were previously read one shard-walk apart.
func (c *Cache) census() (blocks, slots int64) {
	for _, s := range c.shards {
		b, n := s.census()
		blocks += b
		slots += n
	}
	return blocks, slots
}

// residentBytes applies ResidentBytes' per-block and per-slot constants to a
// census, so Stats and ResidentBytes cannot drift apart.
func (c *Cache) residentBytes(blocks, slots int64) int64 {
	perEntry := int64(c.blockSize) + entryHeaderBytes + mapSlotBytes
	return blocks*perEntry + slots*ptrBytes
}

// Per-entry overhead outside the payload, used by ResidentBytes.
const (
	// entryHeaderBytes is sizeof(entry): block number, data slice header, three
	// atomic words (valid bits, reuse count, queue tag) and the padding to 8-byte
	// alignment. Kept in step with the struct by entryHeaderBytesMatchesStruct.
	entryHeaderBytes = 48
	// mapSlotBytes is the per-entry share of map[uint64]*entry: key, pointer and
	// tophash across a bucket at Go's load factor, plus slack for overflow.
	mapSlotBytes = 24
	// ptrBytes is one FIFO queue slot.
	ptrBytes = 8
)

// ResidentBytes estimates what the L1 index costs the collector: the block data
// Usage() counts, plus the entry headers, map buckets and FIFO slots it does
// not. Usage() stays the logical number the budget is expressed in; this is the
// one to compare against MemStats.HeapAlloc (the agent reports both).
//
// It is an estimate, not a measurement: the map share is a flat per-entry
// constant rather than the real bucket layout, and the queue slices are counted
// at capacity because that is the memory the array is holding.
func (c *Cache) ResidentBytes() int64 {
	blocks, slots := c.census()
	return c.residentBytes(blocks, slots)
}

// Usage reports the L1 memory currently holding cached blocks and the L1
// budget, both in bytes.
//
// It walks the shards, so it is meant for periodic reporting (the agent polls
// it for the storage page), not for the read path.
func (c *Cache) Usage() (used, limit int64) {
	blocks, _ := c.census()
	return blocks * int64(c.blockSize), int64(c.totalBlocks) * int64(c.blockSize)
}

// Drop discards every cached block from both levels. It backs a whole-device
// invalidation (UNMAP, WRITE SAME): the L2 index must be cleared too, or a
// subsequent read would serve a sector the invalidation just discarded.
func (c *Cache) Drop() {
	if c.l2 != nil {
		c.l2Gate.Lock()
		defer c.l2Gate.Unlock()
	}
	// Dropping a shard bumps its epoch, so a fill that was reading before the
	// drop is rejected instead of re-populating what the drop just discarded.
	for _, s := range c.shards {
		s.drop()
	}
	if c.l2 != nil {
		c.fence.wait(c.fence.target())
		c.l2.Drop()
	}
}

// Read fills dst with the bytes at byte offset off, using src to load whatever
// is not cached. It is safe for concurrent use.
func (c *Cache) Read(ctx context.Context, off int64, dst []byte, src Source) error {
	if len(dst) == 0 {
		return nil
	}
	if off < 0 {
		return fmt.Errorf("cache: negative offset %d", off)
	}
	c.stats.Reads.Add(1)
	admitted := !c.scan.observe(off, int64(len(dst)))
	if !admitted {
		c.stats.ScanBypass.Add(1)
		// A bypassed stream never populates the cache, so the whole request is
		// served with one aligned backend read instead of one read per cache
		// block. A sequential stream lands here after the admission threshold, so
		// reading block by block would charge the backing target one round trip
		// per 64 KiB of a streamed read and cap throughput at the round trip time.
		return c.readAligned(ctx, off, dst, src)
	}

	end := off + int64(len(dst))
	backendBefore := c.stats.BackendReads.Load()
	bs := int64(c.blockSize)
	for cur := off; cur < end; {
		blk := uint64(cur / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > end {
			segEnd = end
		}
		sh := c.shard(blk)
		if e := sh.lookup(blk); e != nil {
			if e.copyIfValid(blkStart, cur, dst[cur-off:segEnd-off], c.sectorSize) {
				sh.touch(e)
				c.stats.L1Hits.Add(1)
				cur = segEnd
				continue
			}
			// A partly valid entry falls through to serveMiss, which fills only the
			// missing sectors. Counted here because after the miss the entry's state
			// is no longer distinguishable from a cold one.
			c.stats.PartialHits.Add(1)
		} else {
			// The block is absent, so the request can be served by loading the
			// whole run of consecutive absent blocks with one backend read. A
			// large miss otherwise costs the backing target one command per cache
			// block, which is what makes a streamed read through the proxy carry a
			// round trip every blockSize bytes and caps throughput at the round
			// trip time. Blocks that are present but only partly valid keep the
			// per-block fill below, which reads just the missing sectors.
			runEnd := segEnd
			for runEnd < end {
				next := uint64(runEnd / bs)
				if c.shard(next).lookup(next) != nil {
					break
				}
				runEnd = blockEnd(next, bs, end)
			}
			if runEnd > segEnd {
				if err := c.fillRun(ctx, cur, runEnd, dst[cur-off:runEnd-off], src); err != nil {
					return err
				}
				cur = runEnd
				continue
			}
		}
		if err := c.serveMiss(ctx, blk, blkStart, cur, dst[cur-off:segEnd-off], src); err != nil {
			return err
		}
		cur = segEnd
	}
	// A read that never touched the backing store was served entirely from the
	// cache. Counted per command (not per block) so it can be reported as a hit
	// rate the user recognises.
	if c.stats.BackendReads.Load() == backendBefore {
		c.stats.RequestHits.Add(1)
	}
	return nil
}

// serveMiss serves [off, off+len(dst)) inside block blk straight from L2 or the
// backing store and hands the sector run it read to a fill worker.
//
// The read deliberately does not go through the block path: no entry is
// allocated, no single-flight guard is taken and no 64 KiB buffer is zeroed on
// the read's critical path. Those costs are what made a cold random read slower
// through the proxy than a direct connection, and they are all paid by the
// worker once the client has already been served.
func (c *Cache) serveMiss(ctx context.Context, blk uint64, blkStart, off int64, dst []byte, src Source) error {
	s0, s1 := c.sectorRange(blkStart, off, int64(len(dst)))
	n := s1 - s0 + 1
	sec := int64(c.sectorSize)
	start := blkStart + int64(s0)*sec
	buf := fillBufGet(int(int64(n) * sec))

	// The epoch is captured before the data is read, never after: an invalidation
	// always follows the write it belongs to, so a fill whose epoch was captured
	// after a bump is guaranteed to be reading post-write data, and one captured
	// before a bump is rejected at install time.
	epoch := c.shard(blk).epoch.Load()
	fromL2 := false
	if c.l2 != nil {
		c.l2Gate.RLock()
		ok, err := c.l2.ReadSectors(blk, s0, n, buf)
		c.l2Gate.RUnlock()
		if err != nil {
			c.log.Warn("cache: l2 read failed", "block", blk, "err", err)
		} else if ok {
			fromL2 = true
			c.stats.L2Hits.Add(1)
		}
	}
	if !fromL2 {
		if err := src.ReadAt(ctx, start, buf); err != nil {
			return err
		}
		c.stats.BackendReads.Add(1)
	}
	copy(dst, buf[off-start:])
	// Data that came out of L2 is already there: only a backing read needs to
	// populate it. The staging buffer travels with the fill: the release returns
	// it to the pool once the worker has consumed the run, so the request pays
	// neither an allocation nor a zeroing for it.
	c.enqueueFill(blk, int(start-blkStart), buf, epoch, !fromL2,
		newFillRelease(func() { fillBufPut(buf) }))
	return nil
}

// blockEnd returns the exclusive end of blk's byte range, clipped to end.
func blockEnd(blk uint64, blockSize, end int64) int64 {
	e := int64(blk)*blockSize + blockSize
	if e > end {
		e = end
	}
	return e
}

// fillRun loads the sector aligned span of [start,end) with a single backend
// read, installs it into the L1 entries of the blocks it covers and copies the
// requested [start,end) bytes into dst. It returns on the first error, leaving
// the caller's per-block path to report it if it wants a retry.
//
// It deliberately does not populate L2: it serves the multi-block (bulk) reads
// that are the closest to a stream, and spending L2 write bandwidth on them
// would evict the scattered hot blocks L2 exists to keep.
func (c *Cache) fillRun(ctx context.Context, start, end int64, dst []byte, src Source) error {
	sector := int64(c.sectorSize)
	readStart := start / sector * sector
	readEnd := (end-1)/sector*sector + sector
	// A staging buffer is only needed to trim a partial sector at either end.
	// When the requested span is already sector aligned the backing read fills
	// dst directly, which takes an allocation and a copy per command off the
	// streaming path (the read that misses the cache most often).
	buf, staging := dst, false
	if readStart != start || readEnd != end {
		buf, staging = make([]byte, readEnd-readStart), true
	}
	bs := int64(c.blockSize)
	firstBlk := uint64(readStart / bs)
	lastBlk := uint64((readEnd - 1) / bs)
	// Every covered shard's epoch is captured before the read, so a write that
	// invalidates any of them while the backing store is being read drops the
	// whole install instead of publishing data the write has made stale.
	// The epochs stay on the stack for the spans that dominate this path (a
	// 1MiB run is 16 blocks); a longer one falls back to the heap.
	nBlocks := int(lastBlk - firstBlk + 1)
	var stackEpochs [16]uint64
	epochs := stackEpochs[:]
	if nBlocks > len(epochs) {
		epochs = make([]uint64, nBlocks)
	}
	epochs = epochs[:nBlocks]
	for i := range epochs {
		epochs[i] = c.shard(firstBlk + uint64(i)).epoch.Load()
	}
	if err := src.ReadAt(ctx, readStart, buf); err != nil {
		return err
	}
	c.stats.BackendReads.Add(1)

	for off := readStart; off < readEnd; {
		blk := uint64(off / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > readEnd {
			segEnd = readEnd
		}
		c.installValidated(blk, int(off-blkStart), buf[off-readStart:segEnd-readStart], epochs[blk-firstBlk], false, nil)
		off = segEnd
	}
	c.stats.Fills.Add(1)
	if staging {
		copy(dst, buf[start-readStart:end-readStart])
	}
	return nil
}

// ReadCached serves [off, off+len(dst)) entirely from L1, reporting whether every
// covered sector was valid. It never consults L2 or the backing store, so a
// caller can use it as a fast path and fall back to a backend read when it
// reports false.
//
// The range is verified before anything is copied, so a partial hit is reported
// as a plain miss without inflating the L1 counters for the blocks that were
// valid; a race that invalidates a block between the two passes likewise just
// reports a miss, and the caller re-reads the range from the backing store.
func (c *Cache) ReadCached(off int64, dst []byte) bool {
	if len(dst) == 0 {
		return true
	}
	if off < 0 {
		return false
	}
	c.stats.Reads.Add(1)
	return c.readCachedL1(off, dst)
}

// readCachedL1 is ReadCached's body without the request accounting, so
// ReadCachedBoth can try L1 first and fall through to L2 for what it misses.
func (c *Cache) readCachedL1(off int64, dst []byte) bool {
	bs := int64(c.blockSize)
	end := off + int64(len(dst))
	for cur := off; cur < end; {
		blk := uint64(cur / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > end {
			segEnd = end
		}
		s0, s1 := c.sectorRange(blkStart, cur, segEnd-cur)
		e := c.shard(blk).lookup(blk)
		if e == nil || !e.rangeValid(s0, s1) {
			return false
		}
		cur = segEnd
	}
	for cur := off; cur < end; {
		blk := uint64(cur / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > end {
			segEnd = end
		}
		sh := c.shard(blk)
		e := sh.lookup(blk)
		if e == nil || !e.copyIfValid(blkStart, cur, dst[cur-off:segEnd-off], c.sectorSize) {
			return false
		}
		sh.touch(e)
		c.stats.L1Hits.Add(1)
		cur = segEnd
	}
	c.stats.RequestHits.Add(1)
	return true
}

// ReadCachedBoth serves [off, off+len(dst)) from the cache, using L2 to cover
// whatever L1 misses and promoting the sectors it took out of L2 back into L1.
// It reports false unless every covered sector was available, in which case the
// caller reads the range from the backing store.
//
// It is the read path of the transparent proxy: ReadCached is L1 only, so
// without this the L2 file cache is written but never read back.
func (c *Cache) ReadCachedBoth(off int64, dst []byte) bool {
	if len(dst) == 0 {
		return true
	}
	if off < 0 {
		return false
	}
	c.stats.Reads.Add(1)
	if c.readCachedL1(off, dst) {
		return true
	}
	if c.l2 == nil {
		return false
	}
	return c.readCachedL1L2(off, dst)
}

// l2Promote is one sector run a read took out of L2, queued for installation
// into L1 once the whole request is known to hit.
type l2Promote struct {
	blk      uint64
	blockOff int
	data     []byte
	epoch    uint64
}

// readCachedL1L2 completes a read that readCachedL1 could not serve, taking the
// missing segments from L2.
//
// l2Gate is held for read across the L2 lookups and the epoch captures. A write
// raises l2InvN under it before it bumps L1 and defers the L2 drop, so a read
// either observes a pending drop and reports a miss, or runs while no drop is
// pending — and then no L1 bump can interleave, because the writer's raise is
// itself blocked on this read lock. The sectors in L2 are therefore either
// current or absent, never a completed write's pre-write data.
func (c *Cache) readCachedL1L2(off int64, dst []byte) bool {
	bs := int64(c.blockSize)
	ss := int64(c.sectorSize)
	end := off + int64(len(dst))

	c.l2Gate.RLock()
	defer c.l2Gate.RUnlock()
	if c.l2InvN.Load() != 0 {
		// A recent write's L2 drop has not landed yet; L2 may still hold the
		// sectors it will discard, so treat the range as a miss.
		return false
	}

	var l1Hits int64
	var promotes []l2Promote
	for cur := off; cur < end; {
		blk := uint64(cur / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > end {
			segEnd = end
		}
		sh := c.shard(blk)
		if e := sh.lookup(blk); e != nil && e.copyIfValid(blkStart, cur, dst[cur-off:segEnd-off], c.sectorSize) {
			sh.touch(e)
			l1Hits++
			cur = segEnd
			continue
		}
		// The block missed L1. When the request spans it whole and L1 holds
		// none of the blocks behind it either, hand L2 the range instead of the
		// block: how many transfers that range costs is the file's business,
		// and its layout is what decides.
		if run := c.l2Run(blk, cur, blkStart, segEnd, end); run > 1 {
			out := dst[cur-off : cur-off+int64(run)*bs]
			served, err := c.l2.ReadBlocks(blk, run, out)
			if err != nil {
				dropPromoteBufs(promotes)
				c.log.Warn("cache: l2 read failed", "block", blk, "blocks", run, "err", err)
				return false
			}
			for i := 0; i < served; i++ {
				// The run was read straight into dst, so promotion copies each
				// block out of it. That copy is what the staging buffer of the
				// per block path was for, minus the transfer it used to cost.
				b := blk + uint64(i)
				buf := fillBufGet(int(bs))
				copy(buf, out[i*int(bs):(i+1)*int(bs)])
				promotes = append(promotes, l2Promote{blk: b, blockOff: 0, data: buf, epoch: c.shard(b).epoch.Load()})
			}
			if served == run {
				cur += int64(run) * bs
				continue
			}
			// The run stopped short. The block it stopped on is served one at a
			// time below, which is also what confirms an entry a crash left
			// unverified; whatever was served keeps its place in dst.
			if served > 0 {
				cur += int64(served) * bs
				blk = uint64(cur / bs)
				blkStart = int64(blk) * bs
				segEnd = blkStart + bs
				if segEnd > end {
					segEnd = end
				}
				sh = c.shard(blk)
			}
		}
		s0, s1 := c.sectorRange(blkStart, cur, segEnd-cur)
		n := s1 - s0 + 1
		start := blkStart + int64(s0)*ss
		buf := fillBufGet(int(int64(n) * ss))
		ok, err := c.l2.ReadSectors(blk, s0, n, buf)
		if err != nil {
			// Nothing has been queued yet, so this buffer and the runs taken out
			// of L2 before this block are the caller's to recycle.
			fillBufPut(buf)
			dropPromoteBufs(promotes)
			c.log.Warn("cache: l2 read failed", "block", blk, "err", err)
			return false
		}
		if !ok {
			// Neither level holds the whole segment: report a plain miss. Nothing
			// has been committed to the counters, and the caller re-reads the
			// range from the backing store.
			fillBufPut(buf)
			dropPromoteBufs(promotes)
			return false
		}
		copy(dst[cur-off:segEnd-off], buf[cur-start:])
		promotes = append(promotes, l2Promote{blk: blk, blockOff: int(start - blkStart), data: buf, epoch: sh.epoch.Load()})
		cur = segEnd
	}

	c.stats.L1Hits.Add(l1Hits)
	c.stats.L2Hits.Add(int64(len(promotes)))
	c.stats.RequestHits.Add(1)
	// writeL2 is false: the data came out of L2, so only L1 needs it. Each
	// promote carries its staging buffer into the fill that consumes it.
	for _, p := range promotes {
		buf := p.data
		c.enqueueFill(p.blk, p.blockOff, buf, p.epoch, false,
			newFillRelease(func() { fillBufPut(buf) }))
	}
	return true
}

// l2Run is how many whole blocks one L2 range request may cover, or 1 when the
// range is not worth asking for.
//
// The range has to start on the block boundary and the block has to be covered
// whole, because a run is answered with whole blocks and a block the request
// only partly covers could only be stitched together from two sources. L1 has
// to hold none of the blocks behind it either: a block L1 can answer never
// reaches L2 at all, so a run through one would read more than the request
// needs and then throw the block away.
//
// Only the blocks behind the first one are asked about. The caller has just
// looked up the first one and found it missing, and looking again would be a
// second traversal of the same shard.
func (c *Cache) l2Run(blk uint64, cur, blkStart, segEnd, end int64) int {
	bs := int64(c.blockSize)
	if cur != blkStart || segEnd != blkStart+bs {
		return 1
	}
	run := 1
	for {
		b := blk + uint64(run)
		if start := int64(b) * bs; start+bs > end {
			// The request stops inside b, so b is not ours to read whole.
			return run
		}
		if e := c.shard(b).lookup(b); e != nil && e.rangeValid(0, c.sectorsPer-1) {
			return run
		}
		run++
	}
}

// dropPromoteBufs recycles the staging buffers of promotes a read abandoned
// before they were handed to a fill worker.
func dropPromoteBufs(ps []l2Promote) {
	for _, p := range ps {
		fillBufPut(p.data)
	}
}

// FillObserved installs bytes teed from a read the caller forwarded to the
// backing store instead of answering locally, so a miss still warms the cache
// from the traffic it carried. Only the sector-aligned interior of data is
// installed: a read can begin or end mid sector, and publishing a partial
// sector would let a later hit hand back a torn sector, so the sliver at either
// end is left to a future read.
//
// The fill is asynchronous and best effort, like the per-block miss path: a
// full queue drops it and only costs a future backing read, and an invalidation
// that bumps a covered shard before the install drops it rather than publishing
// data the write has made stale.
func (c *Cache) FillObserved(off int64, data []byte) {
	c.FillObservedRelease(off, data, nil)
}

// FillObservedRelease is FillObserved for a caller that owns data: release runs
// once the last byte of the range has been installed or dropped, so a caller
// that staged the observed read in a pooled buffer can recycle it.
//
// The release is called exactly once, on every path out of the function —
// including the ones that install nothing (a range that cannot be cut on a
// sector boundary, a closing cache). A caller therefore has a single place to
// account for the buffer it handed over.
func (c *Cache) FillObservedRelease(off int64, data []byte, release func()) {
	if len(data) == 0 || off < 0 || c.closed.Load() {
		if release != nil {
			release()
		}
		return
	}
	ss := int64(c.sectorSize)
	start := off
	if r := start % ss; r != 0 {
		start += ss - r
	}
	end := off + int64(len(data))
	end -= end % ss
	if end <= start {
		if release != nil {
			release()
		}
		return
	}
	c.stats.BackendReads.Add(1)
	bs := int64(c.blockSize)
	writeL2 := c.l2 != nil
	// The buffer is shared by every block the range covers, so its release holds
	// one reference of its own (dropped at the end of the loop) plus one per
	// queued run.
	var rel *fillRelease
	if release != nil {
		rel = newFillRelease(release)
	}
	for cur := start; cur < end; {
		blk := uint64(cur / bs)
		blkStart := int64(blk) * bs
		segEnd := blkStart + bs
		if segEnd > end {
			segEnd = end
		}
		from := int(cur - off)
		epoch := c.shard(blk).epoch.Load()
		rel.add()
		c.enqueueFill(blk, int(cur-blkStart), data[from:from+int(segEnd-cur)], epoch, writeL2, rel)
		cur = segEnd
	}
	rel.done()
}

// StreamFill installs a read's payload into L1 while the read is being
// forwarded, so populating the cache overlaps the transfer instead of running
// after it. It is what lets a large cached read stream to the client and still
// fill: the store-and-forward path reads the payload whole and copies it into L1
// before the first byte leaves, which is the whole cost of a proxied bulk read.
//
// Like fillRun it does not populate L2 — it serves the bulk reads that are
// closest to a stream, and spending L2 write bandwidth on them would evict the
// scattered hot blocks L2 exists to keep. A nil StreamFill is a no-op, so a
// caller can stream through it unconditionally.
type StreamFill struct {
	c        *Cache
	sec      int
	start    int64
	firstBlk uint64
	epochs   []uint64
	// inline backs epochs for the spans this path usually covers (up to 16
	// blocks, a 1MiB read), so preparing a fill costs one allocation, not two.
	inline [16]uint64
}

// FillAdmit reports whether a streamed read of length bytes at off is worth
// installing into L1 as it passes.
//
// It is the streaming path's half of the sequential-scan admission rule. That
// path forwards a long read block by block and never consults the run detector,
// so without this a sequential run over a region larger than L1 installs every
// block it reads and evicts every one of them unused: the copy, the eviction and
// the allocation are all paid on the client's path for data no later read can
// come back for, which is the run that loses to running uncached.
//
// The budget compared against is the whole L1, not a shard: a stream's blocks
// are spread over the shards by block number, so what decides whether a block
// survives until the stream comes round again is how far the stream has
// travelled against all of L1.
func (c *Cache) FillAdmit(off, length int64) bool {
	if c.scan.admitStream(off, length, int64(c.totalBlocks)*int64(c.blockSize)) {
		return true
	}
	c.stats.FillSkips.Add(1)
	return false
}

// BeginFill prepares an L1 fill for [off, off+length), capturing the covered
// shards' epochs before any data is read. It returns nil when the range cannot
// be installed block by block (empty, misaligned, or the cache is closing), in
// which case the caller streams without populating.
//
// The epoch is captured before the read, never after: an invalidation always
// follows the write it belongs to, so a fill whose epoch was captured after a
// bump is guaranteed to be reading post-write data, and one captured before a
// bump is rejected at install time.
func (c *Cache) BeginFill(off, length int64) *StreamFill {
	bs := int64(c.blockSize)
	if length <= 0 || off < 0 || off%bs != 0 || length%bs != 0 || c.closed.Load() {
		return nil
	}
	first := uint64(off / bs)
	last := uint64(off/bs + length/bs - 1)
	f := &StreamFill{c: c, sec: c.sectorSize, start: off, firstBlk: first}
	nBlocks := int(last - first + 1)
	epochs := f.inline[:]
	if nBlocks > len(epochs) {
		epochs = make([]uint64, nBlocks)
	}
	f.epochs = epochs[:nBlocks]
	for i := range f.epochs {
		f.epochs[i] = c.shard(first + uint64(i)).epoch.Load()
	}
	return f
}

// Write installs the bytes delivered at off, a byte offset within the transfer,
// into L1. It drops any sector a concurrent invalidation made stale.
func (f *StreamFill) Write(off int64, data []byte) {
	if f == nil {
		return
	}
	bs := int64(f.c.blockSize)
	for len(data) > 0 {
		blockOff := int((f.start + off) % bs)
		n := int(bs) - blockOff
		if n > len(data) {
			n = len(data)
		}
		// installValidated addresses whole sectors; an unaligned segment boundary
		// is left uncached rather than published with a partial sector.
		if blockOff%f.sec == 0 && n%f.sec == 0 {
			blk := uint64((f.start + off) / bs)
			if i := blk - f.firstBlk; i < uint64(len(f.epochs)) {
				f.c.installValidated(blk, blockOff, data[:n], f.epochs[i], false, nil)
			}
		}
		data = data[n:]
		off += int64(n)
	}
}

// Done accounts a streamed read as one backend read and one fill.
func (f *StreamFill) Done() {
	if f == nil {
		return
	}
	f.c.stats.BackendReads.Add(1)
	f.c.stats.Fills.Add(1)
}

// Rollback drops the filled range after a failed transfer, in case a payload
// that reached L1 came from a command that did not complete with GOOD status.
func (f *StreamFill) Rollback() {
	if f == nil {
		return
	}
	f.c.Invalidate(f.start, int64(len(f.epochs))*int64(f.c.blockSize))
}

// fillBufPools recycle the staging buffers a read stages L1's data in: the
// sector run serveMiss reads out of L2 or the backing store, and the one
// readCachedL1L2 takes out of L2. Both are handed to a fill worker, so they are
// recycled by the fill's release rather than at the call site.
//
// The classes are the sizes a run inside one block can have: a 4KiB random read
// covers one sector and the largest run is a whole 64KiB block. A block larger
// than the largest class is allocated and left to the collector on the way back.
var fillBufPools [4]sync.Pool

var fillBufSizes = [...]int{4 << 10, 16 << 10, 64 << 10, 256 << 10}

// fillBufGet returns a buffer of n bytes whose capacity is its size class.
func fillBufGet(n int) []byte {
	for i, sz := range fillBufSizes {
		if n <= sz {
			if v := fillBufPools[i].Get(); v != nil {
				if b, ok := v.([]byte); ok && cap(b) >= n {
					return b[:n]
				}
			}
			return make([]byte, sz)[:n]
		}
	}
	return make([]byte, n)
}

// fillBufPut recycles a buffer handed out by fillBufGet.
func fillBufPut(b []byte) {
	if b == nil {
		return
	}
	for i, sz := range fillBufSizes {
		if cap(b) == sz {
			fillBufPools[i].Put(b[:sz])
			return
		}
	}
}

// fillRelease is the ownership handover for the staging buffer of a fill. It
// exists because one buffer can feed several runs: FillObserved cuts what it was
// handed into one run per cache block it covers, so the buffer can only be
// recycled once the last of those runs has been consumed — installed, or dropped
// for a stale epoch, a full queue or a closing cache.
//
// References are counted: the caller holds one and every queued run holds one.
// The callback runs on the drop of the last reference, on whichever goroutine
// drops it, and must not touch the fill's data.
type fillRelease struct {
	n  int32
	fn func()
}

// newFillRelease returns a release holding the caller's own reference; the
// caller must drop it with done once it has handed the buffer over.
func newFillRelease(fn func()) *fillRelease {
	return &fillRelease{n: 1, fn: fn}
}

// add takes one more reference, for a run about to be queued. It must happen
// before the run is queued: a worker that consumed it could otherwise drop the
// last reference before this one exists.
func (r *fillRelease) add() {
	if r != nil {
		atomic.AddInt32(&r.n, 1)
	}
}

// done drops one reference, running the release when the last one goes.
func (r *fillRelease) done() {
	if r == nil {
		return
	}
	if atomic.AddInt32(&r.n, -1) == 0 {
		r.fn()
	}
}

// fillReq is one sector run a worker installs into L1, optionally populating L2.
type fillReq struct {
	blk      uint64
	blockOff int
	data     []byte
	epoch    uint64
	writeL2  bool
	release  *fillRelease
}

// enqueueFill hands a just read sector run to the fill workers.
//
// The queue is bounded and a full queue drops the fill: the read it came from
// has already been served, so a dropped fill only costs a future backing read.
// That is also why the enqueue never blocks — a cold random workload enqueues
// one fill per read, and making the read wait for queue space would put the
// queue's backpressure back on the client.
//
// rel is consumed exactly once: by the worker that installs the run, or here
// when the run is dropped.
func (c *Cache) enqueueFill(blk uint64, blockOff int, data []byte, epoch uint64, writeL2 bool, rel *fillRelease) {
	if c.closed.Load() {
		rel.done()
		return
	}
	// Counted before the send so a concurrent waitFills that observes zero cannot
	// miss this fill: the worker only decrements after installing it.
	c.fillPending.Add(1)
	select {
	case c.fillCh <- fillReq{blk: blk, blockOff: blockOff, data: data, epoch: epoch, writeL2: writeL2, release: rel}:
	default:
		c.fillPending.Add(-1)
		rel.done()
	}
}

func (c *Cache) fillWorker() {
	defer c.fillWG.Done()
	for {
		select {
		case <-c.exitCh:
			return
		case req := <-c.fillCh:
			c.stats.Fills.Add(1)
			// A run the install handed to an L2 write is released by that write:
			// the data is not reusable before it has been written out.
			if !c.installValidated(req.blk, req.blockOff, req.data, req.epoch, req.writeL2, req.release) {
				req.release.done()
			}
			c.fillDone()
		}
	}
}

// fillDone drops the fill from the pending count and wakes waitFills when the
// queue has fully drained.
func (c *Cache) fillDone() {
	if c.fillPending.Add(-1) == 0 {
		c.fillMu.Lock()
		c.fillCond.Broadcast()
		c.fillMu.Unlock()
	}
}

// installValidated publishes a filled sector run into the block's entry and
// schedules its L2 population, dropping the run when the shard was invalidated
// after the data was read.
//
// The epoch check, the copy and the valid-bit update share one shard critical
// section. An invalidation therefore either bumped the epoch first — and the run
// is dropped — or runs after the run installed, and clears what it installed. It
// can never clear the bits and then have the run mark them valid with pre-write
// data.
// The returned bool reports whether the run was handed to an L2 write
// goroutine, which then owns rel: the caller must not release it.
//
// blockOff must sit on a sector boundary; a run that does not is dropped rather
// than published, because installValidated addresses whole sectors and cannot
// place a partial one.
func (c *Cache) installValidated(blk uint64, blockOff int, data []byte, epoch uint64, writeL2 bool, rel *fillRelease) bool {
	ss := c.sectorSize
	if len(data) == 0 || blockOff%ss != 0 {
		return false
	}
	first := blockOff / ss
	last := (blockOff + len(data)) / ss

	sh := c.shard(blk)
	sh.mu.Lock()
	if sh.epoch.Load() != epoch {
		sh.mu.Unlock()
		return false
	}
	e := sh.blocks[blk]
	if e == nil {
		e = &entry{blk: blk, data: make([]byte, c.blockSize)}
		sh.blocks[blk] = e
		e.q.Store(queueNew)
		sh.newQ = append(sh.newQ, e)
		sh.maintainLocked()
	}
	installed := false
	for i := first; i < last; i++ {
		if e.valid.Load()&(1<<uint(i)) != 0 {
			// An already valid sector was loaded by another fill of the same
			// range; leaving it keeps a concurrent reader's copy consistent.
			continue
		}
		start := i * ss
		copy(e.data[start:start+ss], data[start-blockOff:start-blockOff+ss])
		e.setValid(i)
		installed = true
	}
	if installed && writeL2 && c.l2 != nil {
		select {
		case c.l2Sem <- struct{}{}:
			// The sequence number is allocated inside the same critical section as
			// the epoch check, so an invalidation is guaranteed to see every
			// population that carries pre-write data.
			c.fence.enqueue()
			sh.mu.Unlock()
			go c.writeL2(blk, first, data, rel)
			return true
		default:
		}
	}
	sh.mu.Unlock()
	return false
}

// writeL2 populates L2 with a sector run and releases its slot in the fence, so
// an invalidation waits for the populations that carry pre-write data. rel is
// dropped at the end: the run's staging buffer cannot be recycled before it has
// been written out.
func (c *Cache) writeL2(blk uint64, firstSector int, data []byte, rel *fillRelease) {
	defer rel.done()
	defer func() { <-c.l2Sem }()
	defer c.fence.complete()
	if err := c.l2.WriteSectors(blk, firstSector, len(data)/c.sectorSize, data); err != nil {
		c.log.Warn("cache: l2 write failed", "block", blk, "err", err)
	}
}

// Invalidate drops [off, off+length) from both cache levels. It must complete
// before a completing write is acknowledged to the client.
func (c *Cache) Invalidate(off, length int64) {
	if length <= 0 {
		return
	}
	c.stats.Invalidations.Add(1)
	if c.l2 != nil {
		// Held across the epoch bumps and the L2 drop below: a read's L2 lookup
		// either completes before the bumps — and the fill it feeds is rejected by
		// the epoch check — or starts after the drop and cannot see the sectors the
		// write just made stale.
		c.l2Gate.Lock()
		defer c.l2Gate.Unlock()
	}
	c.invalidateL1(off, length)
	if c.l2 == nil {
		return
	}
	// The bump above is what makes this wait a complete fence: a population can
	// only be enqueued from inside the critical section that validated the fill's
	// epoch, so every population allocated before this target is one that ran
	// before a bump. The target is fixed, so reads enqueueing later populations
	// cannot make it spin.
	c.fence.wait(c.fence.target())
	c.l2Drop(off, length)
}

// InvalidateAsync drops [off, off+length) from L1 synchronously and defers the
// L2 drop to a background worker.
//
// It exists for a caller that answers reads from L1 and L2 (the transparent
// proxy): the synchronous L1 bump is what keeps a read that follows a write from
// returning pre-write data, while the L2 half is pure latency on the write path
// — it waits out in-flight L2 populations (background file writes) and walks the
// L2 index. Deferring it takes that wait off the write's critical path.
//
// The deferred drop is made safe for the read path by l2InvN: raised under l2Gate
// before the L1 bump, so ReadCachedBoth skips L2 until the drop has landed. A
// full queue therefore cannot simply drop the request (that would leave L2 stale
// or the count stuck); it falls back to doing the L2 drop inline.
func (c *Cache) InvalidateAsync(off, length int64) {
	if length <= 0 {
		return
	}
	c.stats.Invalidations.Add(1)
	if c.l2 == nil {
		c.invalidateL1(off, length)
		return
	}
	// Raise the pending count before the L1 bump: a read that consults L2 holds
	// l2Gate for read, so it either sees the raised count and skips L2, or its
	// lookups finished before the count was raised — in which case no bump has
	// happened yet and the sectors it read are still current.
	c.l2Gate.Lock()
	c.l2InvN.Add(1)
	c.l2Gate.Unlock()

	c.invalidateL1(off, length)

	// Captured after the bumps but before the drop, so the worker waits out every
	// population that could carry pre-write data without waiting on ones enqueued
	// afterwards.
	req := l2InvReq{off: off, length: length, target: c.fence.target()}
	select {
	case c.l2InvCh <- req:
	default:
		// Queue full: drop the L2 half inline instead of stranding the pending
		// count, which would keep every read off L2. The fence is waited outside
		// l2Gate for the same reason the worker does.
		c.fence.wait(req.target)
		c.l2Gate.Lock()
		c.l2Drop(off, length)
		c.l2InvN.Add(-1)
		c.l2Gate.Unlock()
	}
}

// invalidateL1 bumps every covered shard's epoch and clears the covered blocks'
// valid bits. It is the synchronous, in-memory half of an invalidation: cheap
// (no I/O, no fence) and required for correctness, since it is what makes a read
// that follows a write miss L1 rather than return pre-write data.
func (c *Cache) invalidateL1(off, length int64) {
	end := off + length
	for cur := off; cur < end; {
		blk := uint64(cur / int64(c.blockSize))
		blkStart := int64(blk) * int64(c.blockSize)
		segEnd := blkStart + int64(c.blockSize)
		if segEnd > end {
			segEnd = end
		}
		s0, s1 := c.sectorRange(blkStart, cur, segEnd-cur)
		sh := c.shard(blk)
		sh.mu.Lock()
		sh.epoch.Add(1)
		if e := sh.blocks[blk]; e != nil {
			for i := s0; i <= s1; i++ {
				e.clearValid(i)
			}
			if e.valid.Load() == 0 {
				sh.removeLocked(e)
			}
		}
		sh.mu.Unlock()
		cur = segEnd
	}
}

// l2Drop removes [off, off+length) from the L2 index. It must run after the L1
// bumps and after the fence has drained the populations that carried pre-write
// data, and under l2Gate so a concurrent L2 lookup cannot see a stale sector.
func (c *Cache) l2Drop(off, length int64) {
	end := off + length
	for cur := off; cur < end; {
		blk := uint64(cur / int64(c.blockSize))
		blkStart := int64(blk) * int64(c.blockSize)
		segEnd := blkStart + int64(c.blockSize)
		if segEnd > end {
			segEnd = end
		}
		s0, s1 := c.sectorRange(blkStart, cur, segEnd-cur)
		c.l2.InvalidateSectors(blk, s0, s1-s0+1)
		cur = segEnd
	}
}

// l2InvWorker drains the deferred L2 invalidations queued by InvalidateAsync.
//
// The fence is waited outside l2Gate: a read that arrives while the drop is
// pending sees l2InvN and skips L2, so there is no need to keep reads off the
// gate for the (potentially slow) wait on in-flight L2 populations. The gate is
// taken only for the drop itself, which is a memory-index update.
func (c *Cache) l2InvWorker() {
	defer c.l2InvWG.Done()
	for {
		select {
		case <-c.exitCh:
			return
		case req := <-c.l2InvCh:
			c.fence.wait(req.target)
			c.l2Gate.Lock()
			c.l2Drop(req.off, req.length)
			// Lowered last, under the gate: a read that acquires l2Gate after this
			// sees a drained L2 and may use it again.
			c.l2InvN.Add(-1)
			c.l2Gate.Unlock()
		}
	}
}

// shard returns the shard owning blk.
//
// The shard count is a power of two for every geometry the agent builds, so the
// index is a mask; the modulo is kept for a configured count that is not, which
// a mask would index wrongly.
func (c *Cache) shard(blk uint64) *shard {
	if c.shardsPow2 {
		return c.shards[blk&c.shardMask]
	}
	return c.shards[blk%uint64(len(c.shards))]
}

func (c *Cache) sectorRange(blkStart, off, length int64) (int, int) {
	return int((off - blkStart) / int64(c.sectorSize)), int((off - blkStart + length - 1) / int64(c.sectorSize))
}

// readAligned reads the sector aligned superset of [off, off+len(dst)) straight
// from src, copying the requested slice out of it unless the request is already
// sector aligned.
func (c *Cache) readAligned(ctx context.Context, off int64, dst []byte, src Source) error {
	sector := int64(c.sectorSize)
	s0 := off / sector
	s1 := (off + int64(len(dst)) - 1) / sector
	start := s0 * sector
	// An already aligned request needs no staging buffer: read straight into dst
	// instead of allocating a superset and copying the request out of it.
	if start == off && (s1-s0+1)*sector == int64(len(dst)) {
		return src.ReadAt(ctx, off, dst)
	}
	// Nothing is queued for this one — it is the bypass the scan detector sends a
	// streamed read down — so the staging buffer is recycled here.
	buf := fillBufGet(int((s1 - s0 + 1) * sector))
	defer fillBufPut(buf)
	if err := src.ReadAt(ctx, start, buf); err != nil {
		return err
	}
	copy(dst, buf[off-start:])
	return nil
}

// entry -------------------------------------------------------------------------

// entry is one cached block. data is allocated once and never reused, so a
// reader holding the pointer stays valid even if the entry is evicted.
type entry struct {
	blk   uint64
	data  []byte
	valid atomic.Uint32
	freq  atomic.Uint32
	// q is the S3-FIFO queue the entry sits in. Its writers all hold the owning
	// shard mutex; it is atomic because touch reads it before taking that mutex to
	// skip a promotion that cannot happen. A plain field would make that read a
	// data race. It still fits the header's padding, so sizeof(entry) is unchanged.
	q atomic.Uint32 // guarded by the owning shard mutex
}

func (e *entry) setValid(i int)   { e.valid.Or(1 << uint(i)) }
func (e *entry) clearValid(i int) { e.valid.And(^(uint32(1) << uint(i))) }

// sectorMask[n] has its low n bits set: n consecutive sectors' worth of valid
// bits. rangeValid needs that mask shifted up to the run's first sector, and it
// is called twice per block, so the mask is looked up rather than assembled one
// bit at a time.
//
// It is a table rather than (1<<(s1+1))-(1<<s0) because that form shifts a
// uint32 by 32 when the run covers all of them, which Go turns into zero — the
// run would be reported invalid with no error anywhere. The table has no such
// case.
var sectorMask = func() [33]uint32 {
	var m [33]uint32
	for n := range m {
		if n == 32 {
			m[n] = ^uint32(0)
			continue
		}
		m[n] = 1<<uint(n) - 1
	}
	return m
}()

func (e *entry) rangeValid(s0, s1 int) bool {
	if s1 < s0 {
		// An empty run is vacuously valid. The loop this replaced answered the
		// same, and the table has no entry for a non-positive sector count.
		return true
	}
	mask := sectorMask[s1-s0+1] << uint(s0)
	return e.valid.Load()&mask == mask
}

// copyIfValid copies [off, off+len(dst)) out of the entry, returning false when
// any sector in that range is not valid.
func (e *entry) copyIfValid(blkStart, off int64, dst []byte, sectorSize int) bool {
	s0 := int((off - blkStart) / int64(sectorSize))
	s1 := int((off - blkStart + int64(len(dst)) - 1) / int64(sectorSize))
	if !e.rangeValid(s0, s1) {
		return false
	}
	start := off - blkStart
	copy(dst, e.data[start:start+int64(len(dst))])
	return true
}

// shard -------------------------------------------------------------------------

const (
	queueNew uint32 = iota
	queueMid
	queueOld
)

// shard owns a slice of the L1 index together with its own S3-FIFO queues and
// byte budget, which keeps all eviction decisions lock local.
type shard struct {
	c  *Cache
	mu sync.Mutex

	// epoch is the shard's generation. Invalidate and drop bump it; a fill
	// captures it before reading and only installs while it is unchanged, which
	// is what orders an asynchronous install against an invalidation. It is
	// bumped under mu but read with a plain atomic load, so capturing it does not
	// put the shard lock on the read path.
	epoch atomic.Uint64

	blocks map[uint64]*entry
	newQ   []*entry
	midQ   []*entry
	oldQ   []*entry

	capacity int
	newQuota int
	midQuota int
	high     int
	low      int
}

func newShard(c *Cache, capacity, newPct int, high, low float64) *shard {
	newQuota := capacity * newPct / 100
	if newQuota < 2 {
		newQuota = 2
	}
	midQuota := capacity * 45 / 100
	if midQuota < 2 {
		midQuota = 2
	}
	return &shard{
		c:        c,
		blocks:   make(map[uint64]*entry, capacity),
		capacity: capacity,
		newQuota: newQuota,
		midQuota: midQuota,
		high:     int(float64(capacity) * high),
		low:      int(float64(capacity) * low),
	}
}

func (s *shard) lookup(blk uint64) *entry {
	s.mu.Lock()
	e := s.blocks[blk]
	s.mu.Unlock()
	return e
}

// census reports how many blocks the shard currently holds and how many slots
// its FIFO queues are holding, for occupancy reporting. One lock covers both,
// so a caller that wants the two numbers does not pay for two passes.
func (s *shard) census() (blocks, slots int64) {
	s.mu.Lock()
	blocks = int64(len(s.blocks))
	slots = int64(cap(s.newQ) + cap(s.midQ) + cap(s.oldQ))
	s.mu.Unlock()
	return blocks, slots
}

// touch records a hit: S3-FIFO promotes an entry that was reused while sitting
// in the old queue back into the middle queue.
//
// A promotion only ever moves an entry out of the old queue, and a hit almost
// always finds its entry in the new or middle queue, where touch changes nothing
// — so the queue tag is tested before the lock is taken. The tag is owned by the
// shard mutex, so that test can see a value a concurrent dequeue has already
// moved past: a stale "old" tag only costs the locked re-test below, and a stale
// "new"/"mid" tag costs one promotion that the next hit repeats. Neither is a
// correctness problem, S3-FIFO being a heuristic, and the common case stops
// paying a mutex round trip per block.
func (s *shard) touch(e *entry) {
	if e.freq.Load() < 3 {
		e.freq.Add(1)
	}
	if e.q.Load() != queueOld {
		return
	}
	s.mu.Lock()
	if e.q.Load() == queueOld && e.freq.Load() >= 2 && s.blocks[e.blk] == e {
		s.removeFromQueueLocked(e)
		e.q.Store(queueMid)
		s.midQ = append(s.midQ, e)
	}
	s.mu.Unlock()
}

func (s *shard) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The bump invalidates every fill that was reading before the drop, so none of
	// them can re-populate what this just discarded.
	s.epoch.Add(1)
	s.blocks = make(map[uint64]*entry, s.capacity)
	s.newQ, s.midQ, s.oldQ = nil, nil, nil
}

func (s *shard) removeLocked(e *entry) {
	delete(s.blocks, e.blk)
	s.removeFromQueueLocked(e)
}

func (s *shard) removeFromQueueLocked(e *entry) {
	q := &s.newQ
	switch e.q.Load() {
	case queueMid:
		q = &s.midQ
	case queueOld:
		q = &s.oldQ
	}
	for i, x := range *q {
		if x == e {
			// Shift manually rather than append((*q)[:i], (*q)[i+1:]...): that
			// form leaves the old last element duplicated in a slot past len,
			// where the backing array keeps it alive. Because the duplicate is
			// whatever happened to be last, a later removal of that element
			// would leave it pinned after it left the queue. Same contents,
			// no stale pointer.
			old := len(*q)
			copy((*q)[i:], (*q)[i+1:])
			(*q)[old-1] = nil
			*q = (*q)[:old-1]
			return
		}
	}
}

// maintainLocked keeps the three FIFO queues inside their quotas, evicting from
// the old queue down to the low watermark once the shard exceeds the high one.
//
// Every dequeue clears the vacated slot before reslicing. q = q[1:] on its own
// only moves the window: the backing array still holds the *entry, and for an
// evicted entry (already deleted from the map) that reference is enough to keep
// the entry and its whole block allocation alive until append happens to
// reallocate. Those slots are invisible to Usage(), which counts blocks.
// Measured on the shipped geometry (512 MiB budget, 256 shards, skewed access)
// they held ~200 MiB — the bulk of the gap between Usage() and MemStats.
func (s *shard) maintainLocked() {
	for len(s.newQ) > s.newQuota {
		e := s.newQ[0]
		s.newQ[0] = nil
		s.newQ = s.newQ[1:]
		if e.freq.Load() >= 1 {
			e.q.Store(queueMid)
			s.midQ = append(s.midQ, e)
		} else {
			e.q.Store(queueOld)
			s.oldQ = append(s.oldQ, e)
		}
	}
	for len(s.midQ) > s.midQuota {
		e := s.midQ[0]
		s.midQ[0] = nil
		s.midQ = s.midQ[1:]
		e.q.Store(queueOld)
		s.oldQ = append(s.oldQ, e)
	}
	for len(s.blocks) > s.high && len(s.oldQ) > 0 {
		e := s.oldQ[0]
		s.oldQ[0] = nil
		s.oldQ = s.oldQ[1:]
		delete(s.blocks, e.blk)
		s.c.stats.Evictions.Add(1)
		if len(s.blocks) <= s.low {
			break
		}
	}
}

// l2Fence -----------------------------------------------------------------------

// l2Fence orders the background L2 populations against an invalidation.
//
// enqueue is called inside the shard critical section that validated a fill's
// epoch, so a population carrying pre-write data is always allocated before the
// bump that invalidates it. Invalidate therefore snapshots a target after its
// bumps and waits for that many completions, and because the target is fixed,
// reads enqueueing later populations cannot make the wait spin.
type l2Fence struct {
	mu   sync.Mutex
	cond *sync.Cond
	next uint64
	done uint64
}

// enqueue allocates the population's slot in the fence.
func (f *l2Fence) enqueue() {
	f.mu.Lock()
	f.next++
	f.mu.Unlock()
}

// complete records that one population finished.
func (f *l2Fence) complete() {
	f.mu.Lock()
	f.done++
	f.mu.Unlock()
	f.cond.Broadcast()
}

// target returns the number of populations enqueued so far.
func (f *l2Fence) target() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

// wait blocks until target populations have completed.
func (f *l2Fence) wait(target uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.done < target {
		f.cond.Wait()
	}
}

// scanDetector ------------------------------------------------------------------

// scanDetector implements the sequential-scan admission rule: a run of
// consecutive requests whose start equals the previous end marks a scan, and
// the stream is then served without populating the cache.
//
// The state is fanned out over one bucket per 1 MiB region, so a stream only
// ever locks the bucket its region maps to and concurrent streams on different
// regions do not contend on the read path.
type scanDetector struct {
	enabled   bool
	threshold int
	buckets   []scanBucket

	// streamMu guards the span of the stream currently being forwarded, the
	// state admitStream decides on. It is one span rather than one per bucket:
	// the question is about the L1 as a whole, and it is asked once per large
	// read rather than once per sector run, so there is nothing to fan out.
	streamMu    sync.Mutex
	streamStart int64
	streamEnd   int64
	streamLive  bool
}

type scanBucket struct {
	mu      sync.Mutex
	lastEnd int64
	run     int
}

func newScanDetector(enabled bool, threshold int) *scanDetector {
	return &scanDetector{
		enabled:   enabled,
		threshold: threshold,
		buckets:   make([]scanBucket, scanBuckets),
	}
}

func (d *scanDetector) observe(off, length int64) bool {
	if !d.enabled {
		return false
	}
	b := &d.buckets[(uint64(off)>>scanBucketShift)%scanBuckets]
	b.mu.Lock()
	defer b.mu.Unlock()
	if off == b.lastEnd {
		b.run++
	} else {
		b.run = 0
	}
	b.lastEnd = off + length
	return b.run >= d.threshold
}

// admitStream reports whether a read of length bytes at off, large enough to be
// forwarded as a stream, is worth installing into L1 on the way past.
//
// The rule is geometry rather than pattern matching, because a block the stream
// installs is only read back when the stream comes round again, and it is only
// still there if everything installed after it fits in L1 as well. So the
// detector follows how far the current stream has already travelled: once its
// span is wider than the whole L1, the blocks at the front of it have been
// evicted by the ones behind, and every further install pays a block-sized copy,
// an eviction and a slice allocation for data no read can come back for.
//
// A stream that stays inside the L1 span keeps installing, so a working set that
// fits behaves as before, and a read that jumps further than the span covered so
// far starts a new span instead of extending this one - which is what keeps
// large random reads, scattered over a device far wider than L1, from being
// mistaken for one long sequential run.
//
// The state is a consequence of the reads this detector is shown, so it only
// ever sees reads the caller considered streamable: a workload whose reads are
// all small never moves the span, and never reaches this decision.
func (d *scanDetector) admitStream(off, length, limit int64) bool {
	if !d.enabled {
		return true
	}
	end := off + length
	d.streamMu.Lock()
	span := d.streamEnd - d.streamStart
	switch {
	case !d.streamLive || end <= d.streamStart:
		// The first streamed read, or one that lands behind everything the
		// current stream has covered: a new stream, or this one come round.
		d.streamStart, d.streamEnd, d.streamLive = off, end, true
	case off > d.streamEnd && off-d.streamEnd > span:
		// A jump wider than the stream has travelled is unrelated to what came
		// before it, so it starts its own span rather than widening this one.
		d.streamStart, d.streamEnd = off, end
	case end > d.streamEnd:
		d.streamEnd = end
	}
	span = d.streamEnd - d.streamStart
	d.streamMu.Unlock()
	return span <= limit
}
