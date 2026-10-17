package cache

import "testing"

// The range read is what makes the physical layout of L2 matter: a request that
// covers several whole blocks L1 does not hold is handed to L2 as one run, so the
// file decides how many transfers that run costs. These tests pin the three
// things that decision depends on — that a covered run is asked for once, that a
// run cut short falls back instead of losing what it did serve, and that a block
// L1 can answer stays out of the run entirely.

const (
	rangeSecSize = 4 << 10
	rangeBlkSize = 16 << 10
	rangeBlocks  = 4
)

// seedWholeBlocks fills L2 with whole blocks in the source's own pattern, the
// way a fill would have left them.
func seedWholeBlocks(t *testing.T, l2 *fakeL2, blocks ...uint64) {
	t.Helper()
	for _, b := range blocks {
		off := int64(b) * rangeBlkSize
		if err := l2.WriteSectors(b, 0, testBlkSecs, srcPattern(rangeSecSize, off, testBlkSecs*rangeSecSize)); err != nil {
			t.Fatalf("seed L2 block %d: %v", b, err)
		}
	}
}

// TestReadCachedBothMergesARun pins the merge itself: four whole blocks missing
// from L1 cost one range request and one fill per block, and no per block read
// at all.
func TestReadCachedBothMergesARun(t *testing.T) {
	l2 := newFakeL2(rangeSecSize, testBlkSecs)
	seedWholeBlocks(t, l2, 0, 1, 2, 3)
	c := newTestCache(t, testOpts{l2: l2})

	buf := make([]byte, rangeBlocks*rangeBlkSize)
	if !c.ReadCachedBoth(0, buf) {
		t.Fatal("a range L2 holds whole must be served")
	}
	expectPattern(t, rangeSecSize, 0, buf)

	if calls, blocks := l2.rangeReads(); calls != 1 || blocks != rangeBlocks {
		t.Fatalf("range reads = %d calls / %d blocks, want 1 call serving %d", calls, blocks, rangeBlocks)
	}
	if st := c.Stats(); st.L2Hits != rangeBlocks || st.RequestHits != 1 || st.L1Hits != 0 {
		t.Fatalf("stats = %+v, want %d L2 hits for one request and no L1 hits", st, rangeBlocks)
	}

	// Every block of the run was promoted, so L1 answers the same range alone.
	c.Wait()
	if !c.ReadCached(0, make([]byte, rangeBlocks*rangeBlkSize)) {
		t.Fatal("the whole run should have been promoted into L1")
	}
}

// TestReadCachedBothShortRunFallsBack pins the meaning of a short count: the run
// stops at the first block L2 cannot serve whole, the blocks before it are not
// lost, and the request is still reported as a plain miss because the block that
// ended the run has to come from the backing store.
func TestReadCachedBothShortRunFallsBack(t *testing.T) {
	l2 := newFakeL2(rangeSecSize, testBlkSecs)
	// Block 2 is absent, so the run covers two blocks and stops.
	seedWholeBlocks(t, l2, 0, 1, 3)
	c := newTestCache(t, testOpts{l2: l2})

	if c.ReadCachedBoth(0, make([]byte, rangeBlocks*rangeBlkSize)) {
		t.Fatal("a range with an absent block must miss")
	}
	if calls, blocks := l2.rangeReads(); calls != 1 || blocks != 2 {
		t.Fatalf("range reads = %d calls / %d blocks, want one call serving the two present blocks", calls, blocks)
	}
	// The abandoned promote buffers are recycled rather than installed, and the
	// miss is not counted as a hit at either level.
	if st := c.Stats(); st.L2Hits != 0 || st.L1Hits != 0 || st.RequestHits != 0 {
		t.Fatalf("a partial run must not count as a hit: %+v", st)
	}
	c.Wait()
	if c.ReadCached(0, make([]byte, rangeBlkSize)) {
		t.Fatal("a missed read must not have promoted anything into L1")
	}
}

// TestReadCachedBothRunStopsAtL1 pins the boundary of a run: a block L1 already
// holds is never read back out of L2, so a run crossing one stops there. Four
// blocks are in L2 and one of them is in L1, which leaves a two block run on
// either side of the block L1 answers.
func TestReadCachedBothRunStopsAtL1(t *testing.T) {
	l2 := newFakeL2(rangeSecSize, testBlkSecs)
	seedWholeBlocks(t, l2, 0, 1, 2, 3)
	c := newTestCache(t, testOpts{l2: l2})

	// Warm L1 with block 2 only, so the range below straddles it.
	mid := make([]byte, rangeBlkSize)
	if !c.ReadCachedBoth(2*rangeBlkSize, mid) {
		t.Fatal("the warm up read must be served by L2")
	}
	c.Wait()
	expectPattern(t, rangeSecSize, 2*rangeBlkSize, mid)
	before := c.Stats()

	buf := make([]byte, rangeBlocks*rangeBlkSize)
	if !c.ReadCachedBoth(0, buf) {
		t.Fatal("the range is fully cached and must be served")
	}
	expectPattern(t, rangeSecSize, 0, buf)

	// Blocks 0 and 1 merge into one run, block 2 comes from L1, and block 3 is
	// on its own because the request ends there. The run counter is only
	// touched by the range read: the warm up covers one block, which is not a
	// run worth asking for.
	if calls, blocks := l2.rangeReads(); calls != 1 || blocks != 2 {
		t.Fatalf("range reads = %d calls / %d blocks, want one call serving two blocks short of L1", calls, blocks)
	}
	after := c.Stats()
	if n := after.L1Hits - before.L1Hits; n != 1 {
		t.Fatalf("L1 hits = %d, want 1 for the block L1 already held", n)
	}
	if n := after.L2Hits - before.L2Hits; n != 3 {
		t.Fatalf("L2 hits = %d, want 3 for the blocks read around it", n)
	}
	if n := after.RequestHits - before.RequestHits; n != 1 {
		t.Fatalf("request hits = %d, want 1", n)
	}
}
