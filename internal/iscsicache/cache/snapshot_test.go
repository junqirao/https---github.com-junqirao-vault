package cache

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// blockQueues maps every live block to the S3-FIFO queue it sits in.
func blockQueues(c *Cache) map[uint64]uint8 {
	out := make(map[uint64]uint8)
	for _, sh := range c.shards {
		sh.mu.Lock()
		for blk, e := range sh.blocks {
			out[blk] = e.q
		}
		sh.mu.Unlock()
	}
	return out
}

func TestSnapshotRoundTrip(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	ctx := context.Background()
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})

	// Two whole blocks, plus one that only holds its first two sectors: a partly
	// valid block has to survive the round trip without pretending to be full.
	buf := make([]byte, bs)
	for i := 0; i < 2; i++ {
		if err := c.Read(ctx, int64(i)*int64(bs), buf, src); err != nil {
			t.Fatalf("read block %d: %v", i, err)
		}
	}
	if err := c.Read(ctx, 2*int64(bs), buf[:2*ss], src); err != nil {
		t.Fatalf("read partial block: %v", err)
	}
	c.Wait()

	queues := blockQueues(c)

	path := filepath.Join(t.TempDir(), "l1.bin")
	blocks, size, err := c.SaveSnapshot(path, "ns-a")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if blocks != 3 {
		t.Fatalf("saved %d blocks, want 3", blocks)
	}
	// Only valid sectors are written, so this stays well under three whole blocks.
	if size <= 0 || size >= int64(3*bs) {
		t.Fatalf("snapshot size = %d, want 0 < size < %d", size, 3*bs)
	}
	reads := src.count()

	c2 := newTestCache(t, testOpts{})
	n, err := c2.LoadSnapshot(path, "ns-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n != 3 {
		t.Fatalf("loaded %d blocks, want 3", n)
	}
	// Where a block sat in S3-FIFO is the whole point of a warm start.
	for blk, want := range queues {
		if got := blockQueues(c2)[blk]; got != want {
			t.Fatalf("block %d restored into queue %d, want %d", blk, got, want)
		}
	}

	// A whole restored block answers from L1: no backend traffic at all.
	out := make([]byte, bs)
	if err := c2.Read(ctx, 0, out, src); err != nil {
		t.Fatalf("read restored block: %v", err)
	}
	expectPattern(t, ss, 0, out)
	if src.count() != reads {
		t.Fatalf("backend reads after a restored block = %d, want %d", src.count(), reads)
	}
	// The partly filled block only answers for the sectors it holds.
	if err := c2.Read(ctx, 2*int64(bs), out[:2*ss], src); err != nil {
		t.Fatalf("read restored sectors: %v", err)
	}
	expectPattern(t, ss, 2*int64(bs), out[:2*ss])
	if src.count() != reads {
		t.Fatalf("restored sectors missed: backend reads = %d, want %d", src.count(), reads)
	}
	if err := c2.Read(ctx, 2*int64(bs), out, src); err != nil {
		t.Fatalf("read whole partly filled block: %v", err)
	}
	expectPattern(t, ss, 2*int64(bs), out)
	if src.count() != reads+1 {
		t.Fatalf("a partly filled block must still fetch the sectors it lacks (reads = %d)", src.count())
	}
}

func TestSnapshotIsNotTrustedTwice(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	if err := c.Read(context.Background(), 0, make([]byte, bs), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Wait()

	path := filepath.Join(t.TempDir(), "l1.bin")
	if _, _, err := c.SaveSnapshot(path, "ns"); err != nil {
		t.Fatalf("save: %v", err)
	}

	first := newTestCache(t, testOpts{})
	if n, err := first.LoadSnapshot(path, "ns"); err != nil || n != 1 {
		t.Fatalf("first load = %d blocks, %v", n, err)
	}
	// Loading clears the clean flag: if the process dies before the next clean
	// stop, the file describes a state writes may already have moved past.
	second := newTestCache(t, testOpts{})
	if _, err := second.LoadSnapshot(path, "ns"); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("second load error = %v, want ErrSnapshotStale", err)
	}
}

func TestSnapshotRejectsAnotherLUNOrGeometry(t *testing.T) {
	const bs, ss = 16 << 10, 4 << 10
	src := newFakeSource(ss, 4<<20)
	c := newTestCache(t, testOpts{})
	if err := c.Read(context.Background(), 0, make([]byte, bs), src); err != nil {
		t.Fatalf("read: %v", err)
	}
	c.Wait()

	path := filepath.Join(t.TempDir(), "l1.bin")
	if _, _, err := c.SaveSnapshot(path, "ns-a"); err != nil {
		t.Fatalf("save: %v", err)
	}

	otherLUN := newTestCache(t, testOpts{})
	if _, err := otherLUN.LoadSnapshot(path, "ns-b"); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("another LUN load error = %v, want ErrSnapshotStale", err)
	}

	otherGeometry := newTestCache(t, testOpts{blockSize: 32 << 10})
	if _, err := otherGeometry.LoadSnapshot(path, "ns-a"); !errors.Is(err, ErrSnapshotStale) {
		t.Fatalf("geometry change load error = %v, want ErrSnapshotStale", err)
	}

	// A missing file is not a failure: it just means L1 starts cold.
	fresh := newTestCache(t, testOpts{})
	if n, err := fresh.LoadSnapshot(filepath.Join(t.TempDir(), "absent.bin"), "ns-a"); n != 0 || err != nil {
		t.Fatalf("missing snapshot = %d blocks, %v, want 0, nil", n, err)
	}
}

func TestSnapshotWithoutBlocksWritesNothing(t *testing.T) {
	c := newTestCache(t, testOpts{})
	path := filepath.Join(t.TempDir(), "l1.bin")
	blocks, size, err := c.SaveSnapshot(path, "ns")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if blocks != 0 || size < int64(snapshotHeaderSize) {
		t.Fatalf("empty snapshot = %d blocks, %d bytes", blocks, size)
	}
	if n, err := newTestCache(t, testOpts{}).LoadSnapshot(path, "ns"); n != 0 || err != nil {
		t.Fatalf("loading an empty snapshot = %d blocks, %v", n, err)
	}
}
