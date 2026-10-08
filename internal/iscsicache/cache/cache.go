// Package cache implements the multi-level read cache of the iSCSI read-cache
// proxy: an L1 in-memory block cache with per-sector validity, an optional L2
// file-backed cache, S3-FIFO eviction, single-flight block fills and
// sequential-scan admission control.
//
// The cache is read-only: writes never populate an entry, they invalidate it.
// Entries therefore carry a valid bitmap only, never a dirty bitmap.
package cache

import (
	"context"
	"errors"
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

// sourceReadAttempts bounds the single-flight retry loop. A waiter that joins a
// fill for a different sector range of the same block re-runs the fill for its
// own range; two passes are normally enough.
const sourceReadAttempts = 4

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

	shards  []*shard
	flights flightGroup
	scan    *scanDetector
	l2      L2
	log     *slog.Logger

	stats Stats
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
		scan:        &scanDetector{enabled: cfg.ScanEnabled, threshold: cfg.ScanThreshold},
	}
	c.shards = make([]*shard, cfg.Shards)
	for i := range c.shards {
		c.shards[i] = newShard(c, per, cfg.NewQuotaPct, cfg.HighWatermark, cfg.LowWatermark)
	}
	return c, nil
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
	for _, s := range c.shards {
		s.drop()
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
	}

	end := off + int64(len(dst))
	backendBefore := c.stats.BackendReads.Load()
	for cur := off; cur < end; {
		blk := uint64(cur / int64(c.blockSize))
		blkStart := int64(blk) * int64(c.blockSize)
		segEnd := blkStart + int64(c.blockSize)
		if segEnd > end {
			segEnd = end
		}
		if err := c.readSegment(ctx, blk, blkStart, cur, dst[cur-off:segEnd-off], src, admitted); err != nil {
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

func (c *Cache) readSegment(ctx context.Context, blk uint64, blkStart, off int64, dst []byte, src Source, admitted bool) error {
	sh := c.shard(blk)
	if e := sh.lookup(blk); e != nil {
		if e.copyIfValid(blkStart, off, dst, c.sectorSize) {
			sh.touch(e)
			c.stats.L1Hits.Add(1)
			return nil
		}
		c.stats.PartialHits.Add(1)
	}
	if !admitted {
		return c.readAligned(ctx, off, dst, src)
	}

	s0, s1 := c.sectorRange(blkStart, off, int64(len(dst)))
	for attempt := 0; attempt < sourceReadAttempts; attempt++ {
		err := c.flights.do(blk, func() error { return c.fillBlock(ctx, blk, blkStart, s0, s1, src) })
		if err != nil {
			return err
		}
		e := sh.lookup(blk)
		if e != nil && e.rangeValid(s0, s1) {
			if !e.copyIfValid(blkStart, off, dst, c.sectorSize) {
				return errors.New("cache: entry reported valid range but copy failed")
			}
			sh.touch(e)
			return nil
		}
	}
	// Contention fallback: read straight from the backing store.
	c.log.Warn("cache: fill did not converge, bypassing cache", "block", blk, "sector0", s0, "sector1", s1)
	return c.readAligned(ctx, off, dst, src)
}

// fillBlock makes the sectors [s0,s1] of blk valid inside its entry, loading
// whatever is missing from L2 and then from src. It runs under the block's
// single-flight guard so only one goroutine writes an entry at a time.
func (c *Cache) fillBlock(ctx context.Context, blk uint64, blkStart int64, s0, s1 int, src Source) error {
	sh := c.shard(blk)
	e := sh.lookup(blk)
	if e != nil && e.rangeValid(s0, s1) {
		return nil
	}

	type seg struct {
		sector int
		data   []byte
	}
	var pending []seg
	for _, r := range e.missingRuns(s0, s1) {
		n := r[1] - r[0] + 1
		buf := make([]byte, n*c.sectorSize)
		fromL2 := false
		if c.l2 != nil {
			ok, err := c.l2.ReadSectors(blk, r[0], n, buf)
			if err != nil {
				c.log.Warn("cache: l2 read failed", "block", blk, "err", err)
			} else if ok {
				fromL2 = true
				c.stats.L2Hits.Add(1)
			}
		}
		if !fromL2 {
			off := blkStart + int64(r[0]*c.sectorSize)
			if err := src.ReadAt(ctx, off, buf); err != nil {
				return err
			}
			c.stats.BackendReads.Add(1)
		}
		pending = append(pending, seg{sector: r[0], data: buf})
	}
	if len(pending) == 0 {
		return nil
	}

	if e == nil {
		e = sh.insert(blk)
		if e == nil {
			return errors.New("cache: failed to allocate entry")
		}
	}
	for _, p := range pending {
		copy(e.data[p.sector*c.sectorSize:], p.data)
		n := len(p.data) / c.sectorSize
		for i := 0; i < n; i++ {
			e.setValid(p.sector + i)
		}
	}
	c.stats.Fills.Add(1)

	if c.l2 != nil {
		for _, p := range pending {
			n := len(p.data) / c.sectorSize
			if err := c.l2.WriteSectors(blk, p.sector, n, p.data); err != nil {
				c.log.Warn("cache: l2 write failed", "block", blk, "err", err)
			}
		}
	}
	return nil
}

// Invalidate drops [off, off+length) from both cache levels. It must complete
// before a completing write is acknowledged to the client.
func (c *Cache) Invalidate(off, length int64) {
	if length <= 0 {
		return
	}
	c.stats.Invalidations.Add(1)
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
		if e := sh.blocks[blk]; e != nil {
			for i := s0; i <= s1; i++ {
				e.clearValid(i)
			}
			if e.valid.Load() == 0 {
				sh.removeLocked(e)
			}
		}
		sh.mu.Unlock()
		if c.l2 != nil {
			c.l2.InvalidateSectors(blk, s0, s1-s0+1)
		}
		cur = segEnd
	}
}

func (c *Cache) shard(blk uint64) *shard { return c.shards[blk%uint64(len(c.shards))] }

func (c *Cache) sectorRange(blkStart, off, length int64) (int, int) {
	return int((off - blkStart) / int64(c.sectorSize)), int((off - blkStart + length - 1) / int64(c.sectorSize))
}

// readAligned reads the sector aligned superset of [off, off+len(dst)) straight
// from src and copies the requested slice out of it.
func (c *Cache) readAligned(ctx context.Context, off int64, dst []byte, src Source) error {
	s0 := off / int64(c.sectorSize)
	s1 := (off + int64(len(dst)) - 1) / int64(c.sectorSize)
	start := s0 * int64(c.sectorSize)
	buf := make([]byte, (s1-s0+1)*int64(c.sectorSize))
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

// missingRuns returns the sub-ranges of [s0,s1] whose sectors are not valid.
func (e *entry) missingRuns(s0, s1 int) [][2]int {
	var runs [][2]int
	start := -1
	for i := s0; i <= s1; i++ {
		valid := e != nil && e.valid.Load()&(1<<uint(i)) != 0
		if valid {
			if start >= 0 {
				runs = append(runs, [2]int{start, i - 1})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		runs = append(runs, [2]int{start, s1})
	}
	return runs
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

// insert registers a new entry for blk and returns it.
func (s *shard) insert(blk uint64) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.blocks[blk]; ok {
		return e
	}
	e := &entry{blk: blk, data: make([]byte, s.c.blockSize)}
	s.blocks[blk] = e
	e.q = queueNew
	s.newQ = append(s.newQ, e)
	s.maintainLocked()
	return e
}

func (s *shard) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// flightGroup -------------------------------------------------------------------

type flightGroup struct {
	mu    sync.Mutex
	calls map[uint64]*flightCall
}

type flightCall struct {
	done chan struct{}
	err  error
}

func (g *flightGroup) do(key uint64, fn func() error) error {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		<-c.done
		return c.err
	}
	if g.calls == nil {
		g.calls = make(map[uint64]*flightCall)
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.err = fn()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	close(c.done)
	return c.err
}

// scanDetector ------------------------------------------------------------------

// scanDetector implements the sequential-scan admission rule: a run of
// consecutive requests whose start equals the previous end marks a scan, and
// the stream is then served without populating the cache.
type scanDetector struct {
	mu        sync.Mutex
	enabled   bool
	threshold int
	lastEnd   int64
	run       int
}

func (d *scanDetector) observe(off, length int64) bool {
	if !d.enabled {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if off == d.lastEnd {
		d.run++
	} else {
		d.run = 0
	}
	d.lastEnd = off + length
	return d.run >= d.threshold
}
