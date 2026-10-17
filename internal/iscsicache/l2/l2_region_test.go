package l2

import (
	"sort"
	"testing"
)

// The region tests want more than one home, so that a region can be recycled by
// another one, and a region small enough to fill a home exactly.
const (
	regionSlots  = 8
	regionBlocks = 4
)

// regionSize is a budget that layout makes exactly regionSlots slots of.
var regionSize = SuperBlockSize +
	int64(regionSlots*(testBlock+stampSize)+pagesFor(regionSlots)*indexPageSize)

func openRegionL2(t *testing.T, store Store, blocks int) *File {
	t.Helper()
	f, err := Open(Config{
		Path:         "region.bin",
		SizeBytes:    regionSize,
		BlockSize:    testBlock,
		SectorSize:   testSector,
		RegionBlocks: blocks,
		Namespace:    "ns",
		Store:        store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// TestRegionHomesNeighbouringBlocks is the invariant the layout is built on: the
// blocks of one LBA region are homed into one run of neighbouring slots, so a
// run of them is a run of neighbouring bytes on disk and a range read of the
// region is one transfer.
func TestRegionHomesNeighbouringBlocks(t *testing.T) {
	f := openRegionL2(t, NewMemoryStore(regionSize), regionBlocks)
	defer f.Close()

	for blk := uint64(0); blk < regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	first := slotNumOf(t, f, 0)
	if first < 0 {
		t.Fatal("block 0 was not stored")
	}
	for blk := uint64(1); blk < regionBlocks; blk++ {
		if got, want := slotNumOf(t, f, blk), first+int(blk); got != want {
			t.Fatalf("block %d is in slot %d, want %d: a region's blocks have to be neighbours", blk, got, want)
		}
	}

	// The next region is homed into a home of its own, so its blocks are a run
	// of their own and not a continuation of this one.
	for blk := uint64(regionBlocks); blk < 2*regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	second := slotNumOf(t, f, regionBlocks)
	if second == first {
		t.Fatal("two live regions were homed into one home")
	}
	for blk := uint64(regionBlocks + 1); blk < 2*regionBlocks; blk++ {
		if got, want := slotNumOf(t, f, blk), second+int(blk-regionBlocks); got != want {
			t.Fatalf("block %d is in slot %d, want %d", blk, got, want)
		}
	}
	if f.UsedSlots() != regionSlots {
		t.Fatalf("used slots = %d, want %d", f.UsedSlots(), regionSlots)
	}

	// And the run reads as one transfer: that is what the homing is for.
	before := f.IOStats()
	dst := make([]byte, regionBlocks*testBlock)
	served, err := f.ReadBlocks(0, regionBlocks, dst)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if served != regionBlocks {
		t.Fatalf("served = %d, want %d", served, regionBlocks)
	}
	if n := f.IOStats().Reads - before.Reads; n != 1 {
		t.Fatalf("a whole region cost %d transfers, want 1", n)
	}
	if want := int64(regionBlocks * testBlock); f.IOStats().ReadBytes-before.ReadBytes != want {
		t.Fatalf("read bytes = %d, want %d", f.IOStats().ReadBytes-before.ReadBytes, want)
	}
}

// openSlotsL2 opens a file whose budget holds about slots slots, with a region
// size of blocks. The exact slot count is whatever layout makes of it.
func openSlotsL2(t *testing.T, store Store, slots, blocks int) *File {
	t.Helper()
	size := SuperBlockSize + int64(slots*(testBlock+stampSize)+pagesFor(slots)*indexPageSize)
	f, err := Open(Config{
		Path:         "region.bin",
		SizeBytes:    size,
		BlockSize:    testBlock,
		SectorSize:   testSector,
		RegionBlocks: blocks,
		Namespace:    "ns",
		Store:        store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// TestSequentialScanCostsOneTransferPerRegion is the payoff of homing blocks by
// region, and the reason the fill order below is reversed: the cache is filled
// region by region in the order the requests arrive, so which home a region gets
// has nothing to do with where it sits in the LUN. Reading the whole LUN back in
// order then costs one transfer per region rather than one per block, and reads
// exactly the bytes it returns — nothing is read around a gap and thrown away.
func TestSequentialScanCostsOneTransferPerRegion(t *testing.T) {
	const (
		blocks  = 32
		perHome = 4
	)
	f := openSlotsL2(t, NewMemoryStore(1<<20), blocks, perHome)
	defer f.Close()
	if f.Slots() != blocks {
		t.Fatalf("slots = %d, want %d", f.Slots(), blocks)
	}

	// Fill in reverse region order, so the file holds the regions backwards.
	for reg := blocks/perHome - 1; reg >= 0; reg-- {
		for blk := uint64(reg * perHome); blk < uint64((reg+1)*perHome); blk++ {
			writeFullBlock(t, f, blk)
		}
	}
	if f.UsedSlots() != blocks {
		t.Fatalf("used slots = %d, want every slot", f.UsedSlots())
	}

	dst := make([]byte, blocks*testBlock)
	before := f.IOStats()
	served, err := f.ReadBlocks(0, blocks, dst)
	if err != nil {
		t.Fatalf("read the whole file: %v", err)
	}
	if served != blocks {
		t.Fatalf("served = %d, want every block", served)
	}
	got := f.IOStats()
	if n := got.Reads - before.Reads; n != blocks/perHome {
		t.Fatalf("transfers = %d for %d blocks, want one per region (%d)", n, blocks, blocks/perHome)
	}
	if n := got.ReadBytes - before.ReadBytes; n != int64(blocks*testBlock) {
		t.Fatalf("read bytes = %d, want exactly the %d bytes asked for", n, blocks*testBlock)
	}
	for blk := uint64(0); blk < blocks; blk++ {
		want := byte('a' + blk%26)
		if had := dst[blk*testBlock]; had != want {
			t.Fatalf("block %d holds %q, want %q", blk, had, want)
		}
	}
}

// TestRegionFillIsWhatTheSizeIsChosenBy checks the measurement that decides the
// region size: one entry per region held, counting the slots in use. A region
// that is only ever partly filled is paying for slots nothing else may have, and
// this is how that shows up.
func TestRegionFillIsWhatTheSizeIsChosenBy(t *testing.T) {
	f := openRegionL2(t, NewMemoryStore(regionSize), regionBlocks)
	defer f.Close()

	if n := len(f.RegionFill()); n != 0 {
		t.Fatalf("regions = %d on an empty file, want none", n)
	}
	if got := f.RegionBlocks(); got != regionBlocks {
		t.Fatalf("region blocks = %d, want %d", got, regionBlocks)
	}
	if got, want := f.Homes(), regionSlots/regionBlocks; got != want {
		t.Fatalf("homes = %d, want %d", got, want)
	}

	// Two blocks of one region, three of another.
	for blk := uint64(0); blk < 2; blk++ {
		writeFullBlock(t, f, blk)
	}
	for blk := uint64(regionBlocks); blk < regionBlocks+3; blk++ {
		writeFullBlock(t, f, blk)
	}
	fill := f.RegionFill()
	sort.Ints(fill)
	if len(fill) != 2 || fill[0] != 2 || fill[1] != 3 {
		t.Fatalf("fill = %v, want one region of 2 and one of 3", fill)
	}

	// A region that is emptied keeps its home: it is the run its blocks will be
	// written back into, and giving it up would mean taking a home from a region
	// that is using one. The measurement reports it as empty, which is exactly
	// the waste the size has to be chosen against.
	for blk := uint64(0); blk < 2; blk++ {
		f.InvalidateSectors(blk, 0, 4)
	}
	fill = f.RegionFill()
	sort.Ints(fill)
	if len(fill) != 2 || fill[0] != 0 || fill[1] != 3 {
		t.Fatalf("fill = %v after one region was emptied, want an empty one kept", fill)
	}
}

// TestRegionRecyclesWholeHome checks what a home costs to take over: the region
// that held it loses every block at once, and the hand picks the home to take by
// what has been read out of it since the previous sweep.
func TestRegionRecyclesWholeHome(t *testing.T) {
	f := openRegionL2(t, NewMemoryStore(regionSize), regionBlocks)
	defer f.Close()

	for blk := uint64(0); blk < 2*regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	buf := make([]byte, testBlock)

	// Both homes are taken, so a third region has to displace the one the hand
	// starts on.
	third := uint64(2 * regionBlocks)
	for blk := third; blk < third+regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	if got := slotNumOf(t, f, third); got != 0 {
		t.Fatalf("the new region is in slot %d, want the home the hand was on", got)
	}
	for blk := uint64(0); blk < regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); ok {
			t.Fatalf("block %d survived the recycling of its home", blk)
		}
	}
	for blk := uint64(regionBlocks); blk < 2*regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); !ok {
			t.Fatalf("block %d was dropped with another region", blk)
		}
	}

	// The blocks of the second region were read on the way here, and that is the
	// second chance its home gets: the fourth region takes the home the hand is
	// on — the third region's, which nothing has read — and the second region
	// keeps the home it has. The hand did move on; what it passed over is the
	// home that was read out of.
	fourth := uint64(3 * regionBlocks)
	for blk := fourth; blk < fourth+regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	if got := slotNumOf(t, f, fourth); got != 0 {
		t.Fatalf("the fourth region is in slot %d, want the home nothing had read", got)
	}
	for blk := uint64(regionBlocks); blk < 2*regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); !ok {
			t.Fatalf("block %d was dropped although its region was the one being read", blk)
		}
	}
	for blk := third; blk < third+regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); ok {
			t.Fatalf("block %d survived: a region nothing has read is the one to give up", blk)
		}
	}
	// Two regions lost a home and both still held four blocks.
	if got := f.Evictions(); got != 2 {
		t.Fatalf("evictions = %d, want 2", got)
	}
}

// TestRegionEvictionSparesTheRegionBeingRead is the steady state the layout is
// for, pinned as behaviour rather than as a throughput number: a region that
// keeps being read is not the one recycled when another region needs a home,
// even when its turn under the hand comes up first. Blind sweeping would take
// the first home it walked over, which is how a cache that is working churns
// anyway and never settles.
func TestRegionEvictionSparesTheRegionBeingRead(t *testing.T) {
	f := openRegionL2(t, NewMemoryStore(regionSize), regionBlocks)
	defer f.Close()

	// Two regions fill both homes. The second is the one read below, and the
	// order the two were filled in is what puts the first under the hand.
	for blk := uint64(0); blk < 2*regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	buf := make([]byte, testBlock)
	hot := uint64(regionBlocks)
	for i := 0; i < 4; i++ {
		if ok, _ := f.ReadSectors(hot, 0, 1, buf); !ok {
			t.Fatalf("block %d is not in the cache to be read", hot)
		}
	}

	// A third region arrives, and the hand starts on the home holding the
	// region that was never read.
	third := uint64(2 * regionBlocks)
	for blk := third; blk < third+regionBlocks; blk++ {
		writeFullBlock(t, f, blk)
	}
	for blk := uint64(0); blk < regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); ok {
			t.Fatalf("block %d survived although nothing had read its region", blk)
		}
	}
	for blk := hot; blk < hot+regionBlocks; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, buf); !ok {
			t.Fatalf("block %d was recycled although its region was the one being read", blk)
		}
	}
	if got := f.Evictions(); got != 1 {
		t.Fatalf("evictions = %d, want 1: one region held the home that was taken", got)
	}

	// And the read run that keeps the rest of the region alive is still one
	// transfer, so sparing it does not cost the merging it was measured by.
	dst := make([]byte, regionBlocks*testBlock)
	before := f.IOStats()
	if n, err := f.ReadBlocks(hot, regionBlocks, dst); err != nil || n != regionBlocks {
		t.Fatalf("served = %d, err = %v, want the whole region", n, err)
	}
	if n := f.IOStats().Reads - before.Reads; n != 1 {
		t.Fatalf("the resident region cost %d transfers, want 1", n)
	}
}

// TestRegionDirectorySurvivesReopen checks that homing needs nothing persisted:
// a cleanly closed file comes back with the region in the same home, read back
// out of the slots themselves, and a block written after the reopen lands beside
// the ones already there rather than in a home of its own.
func TestRegionDirectorySurvivesReopen(t *testing.T) {
	store := NewMemoryStore(regionSize)
	f := openRegionL2(t, store, regionBlocks)
	// Two blocks of the second region, so the first home stays free and a home
	// handed out fresh would not be the one the region already has.
	for blk := uint64(regionBlocks); blk < regionBlocks+2; blk++ {
		writeFullBlock(t, f, blk)
	}
	before := slotNumOf(t, f, regionBlocks)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openRegionL2(t, store, regionBlocks)
	defer again.Close()
	if got := slotNumOf(t, again, regionBlocks); got != before {
		t.Fatalf("block %d came back in slot %d, want %d", regionBlocks, got, before)
	}
	writeFullBlock(t, again, regionBlocks+2)
	if got, want := slotNumOf(t, again, regionBlocks+2), before+2; got != want {
		t.Fatalf("block %d is in slot %d, want %d beside its region", regionBlocks+2, got, want)
	}

	// The other two blocks of the region complete it, and the run is one read.
	writeFullBlock(t, again, regionBlocks+3)
	buf := make([]byte, regionBlocks*testBlock)
	before2 := again.IOStats()
	served, err := again.ReadBlocks(regionBlocks, regionBlocks, buf)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if served != regionBlocks {
		t.Fatalf("served = %d, want the whole region", served)
	}
	if n := again.IOStats().Reads - before2.Reads; n != 1 {
		t.Fatalf("a recovered region cost %d transfers, want 1", n)
	}
}
