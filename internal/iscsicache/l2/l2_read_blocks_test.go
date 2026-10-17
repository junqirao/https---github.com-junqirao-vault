package l2

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// openMaxIOL2 opens the standard test geometry with an explicit transfer cap, so
// a test can force a run of adjacent slots to be split across several transfers.
func openMaxIOL2(t *testing.T, store Store, maxIO int) *File {
	t.Helper()
	f, err := Open(Config{
		Path:       "test.bin",
		SizeBytes:  testSize,
		BlockSize:  testBlock,
		SectorSize: testSector,
		MaxIOBytes: maxIO,
		Namespace:  "ns",
		Store:      store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// writeFullBlock writes every sector of a block, which is what makes it
// servable by a run. The tag identifies the block in the destination buffer.
func writeFullBlock(t *testing.T, f *File, blk uint64) byte {
	t.Helper()
	tag := byte('a' + blk%26)
	if err := f.WriteSectors(blk, 0, testBlock/testSector, pattern(testBlock, tag)); err != nil {
		t.Fatalf("write block %d: %v", blk, err)
	}
	return tag
}

// A run of blocks that landed in adjacent slots has to cost one transfer. This
// is the whole point of the interface: the caller asks for a range, not for a
// block, and the file decides how many transfers that range is worth.
func TestReadBlocksMergesAdjacentSlots(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	defer f.Close()
	// Filling the ring in order is what leaves the three blocks in three
	// adjacent slots.
	var tags []byte
	for blk := uint64(0); blk < 3; blk++ {
		tags = append(tags, writeFullBlock(t, f, blk))
	}

	dst := make([]byte, 3*testBlock)
	before := f.IOStats()
	n, err := f.ReadBlocks(0, 3, dst)
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if n != 3 {
		t.Fatalf("served = %d, want 3", n)
	}
	for i, tag := range tags {
		if !bytes.Equal(dst[i*testBlock:(i+1)*testBlock], pattern(testBlock, tag)) {
			t.Fatalf("block %d content mismatch", i)
		}
	}
	got := f.IOStats()
	if reads := got.Reads - before.Reads; reads != 1 {
		t.Fatalf("reads = %d, want 1: the run should be a single transfer", reads)
	}
	// The transfer is exactly the three blocks: the stamps live in their own
	// region, so nothing has to be read around them or split back out.
	if want := int64(3 * testBlock); got.ReadBytes-before.ReadBytes != want {
		t.Fatalf("read bytes = %d, want %d", got.ReadBytes-before.ReadBytes, want)
	}
}

// A run stops at the first block that is not there, and reports the prefix it
// did serve rather than failing: the caller re-reads only the rest.
func TestReadBlocksStopsAtMissingBlock(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	defer f.Close()
	writeFullBlock(t, f, 0)
	// Block 1 is never written, so its slot stays empty.
	writeFullBlock(t, f, 2)

	dst := make([]byte, 3*testBlock)
	n, err := f.ReadBlocks(0, 3, dst)
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if n != 1 {
		t.Fatalf("served = %d, want 1", n)
	}
	if !bytes.Equal(dst[:testBlock], pattern(testBlock, 'a')) {
		t.Fatal("block 0 content mismatch")
	}
}

// A block whose sectors are only partly valid is not servable as part of a run,
// because a run answers with whole blocks.
func TestReadBlocksStopsAtPartialBlock(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	defer f.Close()
	writeFullBlock(t, f, 0)
	if err := f.WriteSectors(1, 0, 1, pattern(testSector, 'b')); err != nil {
		t.Fatalf("write sector: %v", err)
	}

	n, err := f.ReadBlocks(0, 2, make([]byte, 2*testBlock))
	if err != nil {
		t.Fatalf("ReadBlocks: %v", err)
	}
	if n != 1 {
		t.Fatalf("served = %d, want 1: a partly valid block ends the run", n)
	}
}

// A destination that cannot hold the range is the caller's bug, and it is
// reported before any transfer is issued.
func TestReadBlocksShortBuffer(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	defer f.Close()
	writeFullBlock(t, f, 0)

	if _, err := f.ReadBlocks(0, 2, make([]byte, testBlock)); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("err = %v, want ErrShortBuffer", err)
	}
	if n, err := f.ReadBlocks(0, 0, nil); n != 0 || err != nil {
		t.Fatalf("served = %d, err = %v, want 0, nil for an empty range", n, err)
	}
}

// The transfer cap is what bounds a merge, and it is counted in whole slots.
func TestReadBlocksSplitsAtMaxIO(t *testing.T) {
	// Two slots per transfer, so three adjacent blocks cost two transfers.
	f := openMaxIOL2(t, NewMemoryStore(testSize), 2*testBlock)
	defer f.Close()
	for blk := uint64(0); blk < 3; blk++ {
		writeFullBlock(t, f, blk)
	}

	before := f.IOStats()
	n, err := f.ReadBlocks(0, 3, make([]byte, 3*testBlock))
	if err != nil || n != 3 {
		t.Fatalf("served = %d, err = %v, want 3, nil", n, err)
	}
	if reads := f.IOStats().Reads - before.Reads; reads != 2 {
		t.Fatalf("reads = %d, want 2", reads)
	}
}

// Entries restored from a session that did not close cleanly may not be served
// until their stamp agrees. A run stops there instead of skipping the block, and
// the per block read that confirms it lets the run resume: the fallback the
// cache layer already does is what heals this.
func TestReadBlocksStopsAtUnverifiedEntry(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns-a")
	for blk := uint64(0); blk < 3; blk++ {
		writeFullBlock(t, f, blk)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Sync does not mark the file clean, so the reopen restores unverified.
	g := openTestL2(t, store, "ns-a")
	if g.Unverified() != 3 {
		t.Fatalf("unverified = %d, want 3", g.Unverified())
	}
	dst := make([]byte, 3*testBlock)
	if n, err := g.ReadBlocks(0, 3, dst); err != nil || n != 0 {
		t.Fatalf("served = %d, err = %v, want 0, nil", n, err)
	}
	if ok, err := g.ReadSectors(0, 0, testBlock/testSector, dst[:testBlock]); err != nil || !ok {
		t.Fatalf("ok = %v, err = %v, want true, nil", ok, err)
	}
	// Block 0 is settled now, the two behind it are not, so the run is that one
	// block long until they are read as well.
	if n, err := g.ReadBlocks(0, 3, dst); err != nil || n != 1 {
		t.Fatalf("served = %d, err = %v, want 1, nil", n, err)
	}
}

// The counters have to describe the transfers, not the calls: the point of a
// merged run is visible only in the ratio of reads to bytes read.
func TestStatStoreClassifiesReads(t *testing.T) {
	st := newStatStore(NewMemoryStore(testSize))
	buf := make([]byte, 4096)
	// 0 is first, 8192 leaves a hole, 12288 picks the run back up.
	for _, off := range []int64{0, 8192, 12288} {
		if err := st.ReadAt(buf, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	// A transfer that failed is not traffic, and must not be counted as any.
	if err := st.ReadAt(buf, testSize); err == nil {
		t.Fatal("read past the end should fail")
	}

	got := st.stats()
	if got.Reads != 3 || got.ReadBytes != 3*4096 {
		t.Fatalf("reads = %d, bytes = %d, want 3, %d", got.Reads, got.ReadBytes, 3*4096)
	}
	if got.SequentialReads != 1 {
		t.Fatalf("sequential reads = %d, want 1 (only the third continues the second)", got.SequentialReads)
	}
	if got.AvgReadBytes() != 4096 {
		t.Fatalf("average transfer = %d, want 4096", got.AvgReadBytes())
	}
}
