package cache

import (
	"context"
	"testing"
)

// The benchmarks here are the yardstick the index optimisations were planned
// against (docs/iscsi-cache/optimize_261017.md §6a). They run on the shipped
// geometry — 64 KiB blocks, 4 KiB sectors, 256 shards — because what the changes
// move is the per-block cost, and the doc counts them per command length: 1 block
// for a 4 KiB read, 16 for 1 MiB.
const (
	benchBlockSize  = 64 << 10
	benchSectorSize = 4 << 10
)

// benchCache builds a cache on the shipped geometry with blocks [0, nBlocks)
// installed and fully valid, so a read over that span is an L1 hit. l2 may be nil.
func benchCache(b *testing.B, nBlocks int, l2 L2) (*Cache, *fakeSource) {
	b.Helper()
	c, err := New(Config{
		BlockSize:  benchBlockSize,
		SectorSize: benchSectorSize,
		L1Bytes:    1 << 30,
		Shards:     DefaultShards,
		L2:         l2,
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	src := newFakeSource(benchSectorSize, int64(nBlocks+1)*benchBlockSize)
	buf := make([]byte, benchBlockSize)
	for i := 0; i < nBlocks; i++ {
		if err := c.Read(context.Background(), int64(i)*benchBlockSize, buf, src); err != nil {
			b.Fatalf("warm block %d: %v", i, err)
		}
	}
	// Fills are asynchronous: wait for them or the first reads would be misses.
	c.Wait()
	return c, src
}

// BenchmarkReadCachedL1 measures an entire-command L1 hit. The three lengths are
// the rows of the doc's per-block table: one block, one whole block, and the
// sixteen blocks of a 1 MiB read, which land in sixteen different shards under
// the round-robin sharding.
func BenchmarkReadCachedL1(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"4KiB", 4 << 10},
		{"64KiB", benchBlockSize},
		{"1MiB", 16 * benchBlockSize},
	} {
		b.Run(tc.name, func(b *testing.B) {
			const blocks = 16
			c, _ := benchCache(b, blocks, nil)
			dst := make([]byte, tc.size)
			b.SetBytes(int64(tc.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !c.ReadCached(0, dst) {
					b.Fatal("expected an L1 hit")
				}
			}
		})
	}
}

// BenchmarkReadCachedBoth measures the transparent proxy's read: L1 misses, L2
// holds the range, so the read copies out of L2 and promotes the sectors into L1.
//
// A pass leaves what it read in L1, so L1 is emptied again between passes to keep
// the read on the L2 path this benchmark is about. The reset runs outside the
// timed section; it is the synchronous half of an invalidation, not the read.
func BenchmarkReadCachedBoth(b *testing.B) {
	const blocks = 16
	l2 := newFakeL2(benchSectorSize, benchBlockSize/benchSectorSize)
	for blk := 0; blk < blocks; blk++ {
		off := int64(blk) * benchBlockSize
		if err := l2.WriteSectors(uint64(blk), 0, benchBlockSize/benchSectorSize,
			srcPattern(benchSectorSize, off, benchBlockSize)); err != nil {
			b.Fatalf("seed l2: %v", err)
		}
	}
	c, _ := benchCache(b, 0, l2)
	dst := make([]byte, blocks*benchBlockSize)
	span := int64(len(dst))
	b.SetBytes(span)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c.invalidateL1(0, span)
		b.StartTimer()
		if !c.ReadCachedBoth(0, dst) {
			b.Fatal("expected an L2 hit")
		}
	}
	c.Wait()
}

// BenchmarkStats measures one occupancy poll, which is what the agent's 5-second
// sampler takes per mounted library.
func BenchmarkStats(b *testing.B) {
	c, _ := benchCache(b, 16, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c.Stats().L1UsedBytes == 0 {
			b.Fatal("expected L1 occupancy to be reported")
		}
	}
}

// BenchmarkInstall measures one cold block fill, the work a miss hands to a fill
// worker. The block is invalidated before each install (outside the timed
// section, and bumped, as a write would) so the install always allocates the
// block and copies the whole sector run instead of finding it already valid.
func BenchmarkInstall(b *testing.B) {
	const blocks = DefaultShards // one block per shard, so no shard holds two
	c, _ := benchCache(b, 0, nil)
	data := make([]byte, benchBlockSize)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		blk := uint64(i % blocks)
		b.StopTimer()
		c.invalidateL1(int64(blk)*benchBlockSize, benchBlockSize)
		epoch := c.shard(blk).epoch.Load()
		b.StartTimer()
		c.installValidated(blk, 0, data, epoch, false, nil)
	}
}
