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
	// WriteSectors stores n sectors starting at sector sec of block blk.
	WriteSectors(blk uint64, sec, n int, src []byte) error
	// InvalidateSectors drops n sectors starting at sector sec of block blk.
	InvalidateSectors(blk uint64, sec, n int)
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
}

// Snapshot is a plain copy of the counters, for logging.
//
// L1UsedBytes/L1LimitBytes report how much of the L1 memory budget currently
// holds cached blocks and what the budget is, so a caller can show occupancy
// next to the hit counters.
type Snapshot struct {
	Reads, L1Hits, PartialHits, L2Hits, BackendReads, Fills, Evictions, Invalidations, ScanBypass int64
	RequestHits                                                                                   int64
	L1UsedBytes                                                                                   int64
	L1LimitBytes                                                                                  int64
}

func (s *Stats) snapshot() Snapshot {
	return Snapshot{
		Reads: s.Reads.Load(), L1Hits: s.L1Hits.Load(), PartialHits: s.PartialHits.Load(),
		L2Hits: s.L2Hits.Load(), BackendReads: s.BackendReads.Load(), Fills: s.Fills.Load(),
		Evictions: s.Evictions.Load(), Invalidations: s.Invalidations.Load(), ScanBypass: s.ScanBypass.Load(),
		RequestHits: s.RequestHits.Load(),
	}
}

// Cache is an L1+L2 read cache over a block device.
type Cache struct {
	blockSize   int
	sectorSize  int
	sectorsPer  int
	totalBlocks int

	shards []*shard
	scan   *scanDetector
	l2     L2
	log    *slog.Logger

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
	if c.l2 != nil {
		c.l2Sem = make(chan struct{}, l2WriteConcurrency)
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
	s.L1UsedBytes, s.L1LimitBytes = c.Usage()
	return s
}

// Usage reports the L1 memory currently holding cached blocks and the L1
// budget, both in bytes.
//
// It walks the shards, so it is meant for periodic reporting (the agent polls
// it for the storage page), not for the read path.
func (c *Cache) Usage() (used, limit int64) {
	var blocks int64
	for _, s := range c.shards {
		blocks += int64(s.liveBlocks())
	}
	return blocks * int64(c.blockSize), int64(c.totalBlocks) * int64(c.blockSize)
}

// Drop discards every cached block, keeping the L2 file contents untouched
// (the L2 index is memory only, so dropping it makes the L2 logically empty).
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
	buf := make([]byte, int64(n)*sec)

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
	// populate it.
	c.enqueueFill(blk, int(start-blkStart), buf, epoch, !fromL2)
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
	epochs := make([]uint64, lastBlk-firstBlk+1)
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
		c.installValidated(blk, int(off-blkStart), buf[off-readStart:segEnd-readStart], epochs[blk-firstBlk], false)
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
	epochs := make([]uint64, last-first+1)
	for i := range epochs {
		epochs[i] = c.shard(first + uint64(i)).epoch.Load()
	}
	return &StreamFill{c: c, sec: c.sectorSize, start: off, firstBlk: first, epochs: epochs}
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
				f.c.installValidated(blk, blockOff, data[:n], f.epochs[i], false)
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

// fillReq is one sector run a worker installs into L1, optionally populating L2.
type fillReq struct {
	blk      uint64
	blockOff int
	data     []byte
	epoch    uint64
	writeL2  bool
}

// enqueueFill hands a just read sector run to the fill workers.
//
// The queue is bounded and a full queue drops the fill: the read it came from
// has already been served, so a dropped fill only costs a future backing read.
// That is also why the enqueue never blocks — a cold random workload enqueues
// one fill per read, and making the read wait for queue space would put the
// queue's backpressure back on the client.
func (c *Cache) enqueueFill(blk uint64, blockOff int, data []byte, epoch uint64, writeL2 bool) {
	if c.closed.Load() {
		return
	}
	// Counted before the send so a concurrent waitFills that observes zero cannot
	// miss this fill: the worker only decrements after installing it.
	c.fillPending.Add(1)
	select {
	case c.fillCh <- fillReq{blk: blk, blockOff: blockOff, data: data, epoch: epoch, writeL2: writeL2}:
	default:
		c.fillPending.Add(-1)
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
			c.installValidated(req.blk, req.blockOff, req.data, req.epoch, req.writeL2)
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
func (c *Cache) installValidated(blk uint64, blockOff int, data []byte, epoch uint64, writeL2 bool) {
	if len(data) == 0 {
		return
	}
	ss := c.sectorSize
	first := blockOff / ss
	last := (blockOff + len(data)) / ss

	sh := c.shard(blk)
	sh.mu.Lock()
	if sh.epoch.Load() != epoch {
		sh.mu.Unlock()
		return
	}
	e := sh.blocks[blk]
	if e == nil {
		e = &entry{blk: blk, data: make([]byte, c.blockSize)}
		sh.blocks[blk] = e
		e.q = queueNew
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
			go c.writeL2(blk, first, data)
			return
		default:
		}
	}
	sh.mu.Unlock()
}

// writeL2 populates L2 with a sector run and releases its slot in the fence, so
// an invalidation waits for the populations that carry pre-write data.
func (c *Cache) writeL2(blk uint64, firstSector int, data []byte) {
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

	// The bump and the clear share one critical section per block, and the bump
	// runs first so no fill can still be validated against the pre-write epoch
	// afterwards.
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
	if c.l2 == nil {
		return
	}

	// An L2 population enqueued before a bump carries pre-write data and must not
	// land after the drop below, or it would resurrect it. The bump above is what
	// makes this wait a complete fence: a population can only be enqueued from
	// inside the critical section that validated the fill's epoch, so every
	// population allocated before this target is one that ran before a bump. The
	// target is fixed, so reads enqueueing later populations cannot make it spin.
	c.fence.wait(c.fence.target())
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

func (c *Cache) shard(blk uint64) *shard { return c.shards[blk%uint64(len(c.shards))] }

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
	buf := make([]byte, (s1-s0+1)*sector)
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
	q     uint8 // guarded by the owning shard mutex
}

func (e *entry) setValid(i int)   { e.valid.Or(1 << uint(i)) }
func (e *entry) clearValid(i int) { e.valid.And(^(uint32(1) << uint(i))) }

func (e *entry) rangeValid(s0, s1 int) bool {
	mask := uint32(0)
	for i := s0; i <= s1; i++ {
		mask |= 1 << uint(i)
	}
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
	queueNew uint8 = iota
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

// liveBlocks reports how many blocks the shard currently holds, for occupancy
// reporting.
func (s *shard) liveBlocks() int {
	s.mu.Lock()
	n := len(s.blocks)
	s.mu.Unlock()
	return n
}

// touch records a hit: S3-FIFO promotes an entry that was reused while sitting
// in the old queue back into the middle queue.
func (s *shard) touch(e *entry) {
	if e.freq.Load() < 3 {
		e.freq.Add(1)
	}
	s.mu.Lock()
	if e.q == queueOld && e.freq.Load() >= 2 && s.blocks[e.blk] == e {
		s.removeFromQueueLocked(e)
		e.q = queueMid
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
	switch e.q {
	case queueMid:
		q = &s.midQ
	case queueOld:
		q = &s.oldQ
	}
	for i, x := range *q {
		if x == e {
			*q = append((*q)[:i], (*q)[i+1:]...)
			return
		}
	}
}

// maintainLocked keeps the three FIFO queues inside their quotas, evicting from
// the old queue down to the low watermark once the shard exceeds the high one.
func (s *shard) maintainLocked() {
	for len(s.newQ) > s.newQuota {
		e := s.newQ[0]
		s.newQ = s.newQ[1:]
		if e.freq.Load() >= 1 {
			e.q = queueMid
			s.midQ = append(s.midQ, e)
		} else {
			e.q = queueOld
			s.oldQ = append(s.oldQ, e)
		}
	}
	for len(s.midQ) > s.midQuota {
		e := s.midQ[0]
		s.midQ = s.midQ[1:]
		e.q = queueOld
		s.oldQ = append(s.oldQ, e)
	}
	for len(s.blocks) > s.high && len(s.oldQ) > 0 {
		e := s.oldQ[0]
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
