package l2

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

const (
	testBlock  = 16 << 10
	testSector = 4 << 10
	// testSlots is what layout makes of the test budget.
	testSlots = 3
	// Three slots, their stamps, one index page and the super block. The stamps
	// are one region of their own after the slots rather than a sector wedged
	// between them, so the three slots are three contiguous blocks of bytes.
	testSize = SuperBlockSize + testSlots*(testBlock+stampSize) + indexPageSize
	// testStampBase is where that region starts.
	testStampBase = SuperBlockSize + testSlots*testBlock
	// testRegionBlocks is the region size the helpers below open with. It is a
	// whole file's worth of slots at testSize, so blocks 0..2 are one region in
	// one home, and it stays the same across a budget change — which is what the
	// region size has to do for a resize to keep the slots it already has.
	testRegionBlocks = testSlots
)

func openTestL2(t *testing.T, store Store, namespace string) *File {
	t.Helper()
	f, err := Open(Config{
		Path:         "test.bin",
		SizeBytes:    testSize,
		BlockSize:    testBlock,
		SectorSize:   testSector,
		RegionBlocks: testRegionBlocks,
		Namespace:    namespace,
		Store:        store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// openSizedL2 opens the same store with an explicit budget, letting a test
// shrink or grow the file between two opens.
func openSizedL2(t *testing.T, store Store, size int64) *File {
	t.Helper()
	f, err := Open(Config{
		Path:         "test.bin",
		SizeBytes:    size,
		BlockSize:    testBlock,
		SectorSize:   testSector,
		RegionBlocks: testRegionBlocks,
		Namespace:    "ns",
		Store:        store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
}

// testStampOffset is where the stamp of a slot sits in the test file.
func testStampOffset(num int) int64 {
	return testStampBase + int64(num)*int64(stampSize)
}

// stampOf reads the stamp sector of a slot straight from the store, the way a
// crashed session would have left it. The stamps follow the slot area in one
// region of their own, so a slot's stamp is not at its own offset.
func stampOf(t *testing.T, store Store, num int) []byte {
	t.Helper()
	b := make([]byte, stampSize)
	if err := store.ReadAt(b, testStampOffset(num)); err != nil {
		t.Fatalf("read stamp %d: %v", num, err)
	}
	return b
}

// slotNumOf is the slot a block is cached in, or -1. It reads the index the way
// the file does, so a test can check where a block was homed.
func slotNumOf(t *testing.T, f *File, blk uint64) int {
	t.Helper()
	f.mu.RLock()
	defer f.mu.RUnlock()
	sl := f.index[blk]
	if sl == nil {
		return -1
	}
	return sl.num
}

func pattern(n int, tag byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = tag
	}
	return b
}

func TestWriteThenReadSectors(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	if f.Slots() != 3 {
		t.Fatalf("slots = %d, want 3", f.Slots())
	}
	if f.SlotSize() != testBlock {
		t.Fatalf("slot size = %d", f.SlotSize())
	}
	if f.Namespace() != "ns-a" {
		t.Fatalf("namespace = %q", f.Namespace())
	}

	want := pattern(testSector, 0x5a)
	if err := f.WriteSectors(1, 2, 1, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if f.UsedSlots() != 1 {
		t.Fatalf("used slots = %d, want 1", f.UsedSlots())
	}

	got := make([]byte, testSector)
	ok, err := f.ReadSectors(1, 2, 1, got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("read reported a missing sector after a successful write")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("payload not preserved")
	}
}

func TestReadSectorsRequiresAllValid(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	if err := f.WriteSectors(7, 1, 1, pattern(testSector, 1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Sectors 1 and 2 requested, only 1 is present.
	ok, err := f.ReadSectors(7, 1, 2, make([]byte, 2*testSector))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if ok {
		t.Fatal("read succeeded although only part of the range was present")
	}
	// An unknown block is a clean miss, not an error.
	ok, err = f.ReadSectors(999, 0, 1, make([]byte, testSector))
	if err != nil || ok {
		t.Fatalf("unknown block: ok=%v err=%v", ok, err)
	}
}

func TestOutOfRangeWrites(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	if err := f.WriteSectors(1, 3, 2, make([]byte, 2*testSector)); err == nil {
		t.Fatal("expected a write beyond the slot to be rejected")
	}
	if _, err := f.ReadSectors(1, 4, 1, make([]byte, testSector)); err == nil {
		t.Fatal("expected a read beyond the slot to be rejected")
	}
	if err := f.WriteSectors(1, 0, 1, make([]byte, testSector/2)); err != io.ErrShortBuffer {
		t.Fatalf("short buffer error = %v", err)
	}
}

func TestInvalidateSectors(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	if err := f.WriteSectors(2, 0, 3, make([]byte, 3*testSector)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.InvalidateSectors(2, 1, 1)

	ok, err := f.ReadSectors(2, 1, 1, make([]byte, testSector))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if ok {
		t.Fatal("invalidated sector is still readable")
	}
	// Sectors 0 and 2 remain.
	if ok, _ := f.ReadSectors(2, 0, 1, make([]byte, testSector)); !ok {
		t.Fatal("sector 0 was dropped")
	}
	if ok, _ := f.ReadSectors(2, 2, 1, make([]byte, testSector)); !ok {
		t.Fatal("sector 2 was dropped")
	}

	// Dropping every sector releases the slot.
	f.InvalidateSectors(2, 0, 3)
	if f.UsedSlots() != 0 {
		t.Fatalf("used slots = %d after invalidating everything", f.UsedSlots())
	}
}

// TestRegionReplacesTheResidentOne checks what a full cache costs when a new
// region arrives: it takes a home over, and the region that held it loses every
// block at once. At this size the whole file is one region, so the second one
// touched is already one too many.
func TestRegionReplacesTheResidentOne(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	for blk := uint64(1); blk <= 2; blk++ {
		if err := f.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk))); err != nil {
			t.Fatalf("write block %d: %v", blk, err)
		}
	}
	if f.UsedSlots() != 2 {
		t.Fatalf("used slots = %d, want 2", f.UsedSlots())
	}

	// Block 3 is the first block of the next region. The only home is taken, so
	// it is recycled and takes the two blocks in it with it.
	if err := f.WriteSectors(3, 0, 1, pattern(testSector, 3)); err != nil {
		t.Fatalf("write block 3: %v", err)
	}
	for blk := uint64(1); blk <= 2; blk++ {
		if ok, _ := f.ReadSectors(blk, 0, 1, make([]byte, testSector)); ok {
			t.Fatalf("block %d survived the recycling of its home", blk)
		}
	}
	if ok, _ := f.ReadSectors(3, 0, 1, make([]byte, testSector)); !ok {
		t.Fatal("block 3 was not stored")
	}
	if f.UsedSlots() != 1 {
		t.Fatalf("used slots = %d, want 1", f.UsedSlots())
	}
}

// TestNamespaceChangeRewritesSuperBlock checks the identity binding: opening the
// same file under a different namespace must invalidate the previous contents.
func TestNamespaceChangeRewritesSuperBlock(t *testing.T) {
	store := NewMemoryStore(testSize)
	a := openTestL2(t, store, "namespace-a")

	header := make([]byte, SuperBlockSize)
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if string(header[:8]) != superBlockMag {
		t.Fatalf("magic = %q", header[:8])
	}
	if header[8] != superBlockVer {
		t.Fatalf("version = %d", header[8])
	}
	if int(binary.LittleEndian.Uint32(header[12:16])) != testBlock {
		t.Fatal("slot size not recorded")
	}
	if int(binary.LittleEndian.Uint32(header[20:24])) != a.Slots() {
		t.Fatal("slot count not recorded")
	}
	nsA := append([]byte(nil), header[32:64]...)

	b := openTestL2(t, store, "namespace-b")
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if bytes.Equal(nsA, header[32:64]) {
		t.Fatal("namespace hash was not rewritten")
	}
	if b.Namespace() != "namespace-b" {
		t.Fatalf("namespace = %q", b.Namespace())
	}
	nsB := append([]byte(nil), header[32:64]...)

	// Reopening with the same namespace is stable.
	openTestL2(t, store, "namespace-b")
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	if !bytes.Equal(nsB, header[32:64]) {
		t.Fatal("super block was rewritten for an unchanged namespace")
	}
}

// TestLayoutPutsStampsInTheirOwnRegion pins the shape the merged read rests on:
// the slots are contiguous bytes right after the super block, every stamp is in
// one region after them, and the index region follows that.
//
// The point of the shape is that a run of neighbouring blocks is a run of
// neighbouring bytes, so ReadBlocks serves it with one transfer straight into
// the caller's buffer, with no metadata wedged in between to read around.
func TestLayoutPutsStampsInTheirOwnRegion(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns-a")
	defer f.Close()

	if got := f.slotDataOffset(0); got != SuperBlockSize {
		t.Fatalf("slot 0 data at %d, want %d", got, SuperBlockSize)
	}
	for num := 1; num < f.slotCount; num++ {
		if got, want := f.slotDataOffset(num), SuperBlockSize+int64(num)*int64(f.slotSize); got != want {
			t.Fatalf("slot %d data at %d, want %d", num, got, want)
		}
		if gap := f.slotDataOffset(num) - f.slotDataOffset(num-1) - int64(f.slotSize); gap != 0 {
			t.Fatalf("gap of %d bytes before slot %d: slot data has to be contiguous", gap, num)
		}
	}
	// The stamps begin where the whole slot area ends, so no stamp can sit
	// between two slots.
	after := f.slotDataOffset(f.slotCount-1) + int64(f.slotSize)
	if got := f.stampOffset(0); got != after {
		t.Fatalf("stamp region at %d, want %d", got, after)
	}
	for num := 1; num < f.slotCount; num++ {
		if got, want := f.stampOffset(num), f.stampOffset(0)+int64(num)*int64(stampSize); got != want {
			t.Fatalf("stamp %d at %d, want %d", num, got, want)
		}
	}
	// And the index region starts after both, so nothing overlaps.
	if got, want := f.indexOffset(f.slotCount, 0), f.stampOffset(f.slotCount-1)+int64(stampSize); got != want {
		t.Fatalf("index region at %d, want %d", got, want)
	}

	// Every one of those offsets is handed to a store that may refuse a transfer
	// which is not a whole number of sectors, so every one of them has to be one.
	for _, off := range []int64{
		SuperBlockSize,
		f.slotDataOffset(0),
		f.slotDataOffset(f.slotCount - 1),
		f.stampOffset(0),
		f.stampOffset(f.slotCount - 1),
		f.indexOffset(f.slotCount, 0),
	} {
		if off%testSector != 0 {
			t.Fatalf("offset %d is not a multiple of the sector size", off)
		}
	}
	if f.slotSize%testSector != 0 {
		t.Fatalf("slot size %d is not a multiple of the sector size", f.slotSize)
	}
}

// TestPreviousLayoutIsDiscarded is the cost of moving the stamps: an older file
// keeps them between the slots, so its slots cannot be addressed under the
// current offsets. It is rebuilt from empty rather than migrated, which is what
// the format version is for.
func TestPreviousLayoutIsDiscarded(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	for blk := uint64(0); blk < 3; blk++ {
		writeFullBlock(t, f, blk)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Rewrite the header as the session that wrote those slots had it.
	header := make([]byte, SuperBlockSize)
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	header[8] = 3
	put32(header[sbOffCRC:], checksum(header, sbOffCRC))
	if err := store.WriteAt(header, 0); err != nil {
		t.Fatalf("write superblock: %v", err)
	}

	again := openTestL2(t, store, "ns")
	if n := again.UsedSlots(); n != 0 {
		t.Fatalf("used slots = %d after a format change, want 0", n)
	}
	if ok, _ := again.ReadSectors(0, 0, 1, make([]byte, testSector)); ok {
		t.Fatal("a slot written under the previous layout was served")
	}
}

// TestGeometryChangeRewritesSuperBlock checks that a different geometry resets
// the file instead of misinterpreting the old slots.
func TestGeometryChangeRewritesSuperBlock(t *testing.T) {
	store := NewMemoryStore(SuperBlockSize + indexPageSize + 8*(testBlock+stampSize))
	if _, err := Open(Config{
		Path: "g.bin", SizeBytes: SuperBlockSize + indexPageSize + 8*(testBlock+stampSize),
		BlockSize: testBlock, SectorSize: testSector, Namespace: "ns", Store: store,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	header := make([]byte, SuperBlockSize)
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	before := binary.LittleEndian.Uint32(header[12:16])

	f, err := Open(Config{
		Path: "g.bin", SizeBytes: SuperBlockSize + indexPageSize + 8*(2*testBlock+stampSize),
		BlockSize: 2 * testBlock, SectorSize: testSector, Namespace: "ns", Store: store,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := store.ReadAt(header, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if binary.LittleEndian.Uint32(header[12:16]) == before {
		t.Fatal("slot size was not updated")
	}
	if int(binary.LittleEndian.Uint32(header[20:24])) != f.Slots() {
		t.Fatal("slot count was not updated")
	}
}

func TestDropForgetsIndex(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(5, 0, 1, pattern(testSector, 9)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Drop()
	if f.UsedSlots() != 0 {
		t.Fatalf("used slots = %d after drop", f.UsedSlots())
	}
	ok, _ := f.ReadSectors(5, 0, 1, make([]byte, testSector))
	if ok {
		t.Fatal("a dropped block is still readable")
	}
}

func TestOpenValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no path and no store", Config{SizeBytes: testSize, BlockSize: testBlock, SectorSize: testSector}},
		{"size too small", Config{Path: "x", SizeBytes: testBlock, BlockSize: testBlock, SectorSize: testSector}},
		{"block not a multiple of sector", Config{Path: "x", SizeBytes: testSize, BlockSize: 10000, SectorSize: testSector}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(tc.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestSyncAndClose(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestReopenRestoresIndex is the point of the persisted index: a cache that was
// closed cleanly is still there the second time the file is opened.
func TestReopenRestoresIndex(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	want := pattern(2*testSector, 0x33)
	if err := f.WriteSectors(7, 0, 2, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openTestL2(t, store, "ns")
	if again.UsedSlots() != 1 {
		t.Fatalf("used slots after reopen = %d, want 1", again.UsedSlots())
	}
	// A clean close is trusted as it is, so nothing there needs confirming.
	if again.Restored() != 1 || again.Unverified() != 0 {
		t.Fatalf("restored/unverified = %d/%d, want 1/0 after a clean close",
			again.Restored(), again.Unverified())
	}
	got := make([]byte, 2*testSector)
	ok, err := again.ReadSectors(7, 0, 2, got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok {
		t.Fatal("a block cached before the restart is a miss after it")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("payload not preserved across a restart")
	}
}

// TestCrashRecoveryKeepsMatchingSlots is the crash rule in the direction that
// saves work: a session killed without closing still leaves slots whose stamps
// agree with the snapshot, and those are worth keeping.
func TestCrashRecoveryKeepsMatchingSlots(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	want := pattern(testSector, 0x44)
	if err := f.WriteSectors(7, 0, 1, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// No Close: the file is left the way a killed process leaves it.

	again := openTestL2(t, store, "ns")
	if again.UsedSlots() != 1 {
		t.Fatalf("used slots = %d after a crash, want 1", again.UsedSlots())
	}
	if again.Restored() != 1 || again.Unverified() != 1 {
		t.Fatalf("restored/unverified = %d/%d, want 1/1",
			again.Restored(), again.Unverified())
	}
	got := make([]byte, testSector)
	ok, err := again.ReadSectors(7, 0, 1, got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !ok || !bytes.Equal(got, want) {
		t.Fatal("a block whose stamp matches the snapshot was not restored")
	}
	// The read that confirmed the entry is what settles the counter, and the
	// entry is not counted as dropped: the recovery did its job.
	if again.Unverified() != 0 {
		t.Fatalf("unverified = %d after the slot was confirmed, want 0", again.Unverified())
	}
	if again.Dropped() != 0 {
		t.Fatalf("dropped = %d, want 0 for a slot that matched", again.Dropped())
	}
}

// TestCrashRecoveryDropsRecycledSlot covers the entry a crash makes dangerous:
// the snapshot points at a slot that a later write handed to another block.
func TestCrashRecoveryDropsRecycledSlot(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(7, 0, 1, pattern(testSector, 0x44)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The next session reaches block 7's slot again — three slots take the ring
	// back to where it started — and is killed before flushing the index.
	live := openTestL2(t, store, "ns")
	for blk := uint64(4); blk <= 6; blk++ {
		if err := live.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk))); err != nil {
			t.Fatalf("write block %d: %v", blk, err)
		}
	}

	again := openTestL2(t, store, "ns")
	if ok, _ := again.ReadSectors(7, 0, 1, make([]byte, testSector)); ok {
		t.Fatal("block 7 was served from the slot another block took over")
	}
	if again.UsedSlots() != 0 {
		t.Fatalf("used slots = %d after the stale record was disproved, want 0", again.UsedSlots())
	}
	// The disproved entry is exactly what the dropped counter is for.
	if again.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", again.Dropped())
	}
	if again.Unverified() != 0 {
		t.Fatalf("unverified = %d after the only entry was settled, want 0", again.Unverified())
	}
}

// TestCrashRecoveryCountsRecycledUnverifiedEntry covers the entry that never
// gets read before its slot is reused: the write path hands the slot to another
// block, and the counters have to record that the snapshot lost it just as they
// would for a stamp that disagreed.
func TestCrashRecoveryCountsRecycledUnverifiedEntry(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(7, 0, 1, pattern(testSector, 0x44)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// No Close: block 7 sits in slot 0, unverified, and the next is still 0 in
	// the superblock, so the next write takes that slot back.

	again := openTestL2(t, store, "ns")
	if err := again.WriteSectors(9, 0, 1, pattern(testSector, 0x99)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if again.Dropped() != 1 {
		t.Fatalf("dropped = %d after the slot was recycled, want 1", again.Dropped())
	}
	if again.Unverified() != 0 {
		t.Fatalf("unverified = %d, want 0 once the entry is settled", again.Unverified())
	}
	if again.UsedSlots() != 1 {
		t.Fatalf("used slots = %d, want only the new block", again.UsedSlots())
	}
	if ok, _ := again.ReadSectors(7, 0, 1, make([]byte, testSector)); ok {
		t.Fatal("block 7 was served from the slot block 9 took over")
	}
	if ok, _ := again.ReadSectors(9, 0, 1, make([]byte, testSector)); !ok {
		t.Fatal("the block that took the slot over is not readable")
	}
}

// TestCrashRecoveryDropsInvalidatedSectors is why the slot version exists: a
// snapshot that predates an invalidation must not resurrect the sectors, even
// though the index page was never rewritten.
func TestCrashRecoveryDropsInvalidatedSectors(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(3, 0, 3, pattern(3*testSector, 0x55)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	live := openTestL2(t, store, "ns")
	live.InvalidateSectors(3, 1, 1)

	again := openTestL2(t, store, "ns")
	// The first read has to disprove the restored record, so the slot goes away
	// instead of serving a sector the killed session had already dropped.
	if ok, _ := again.ReadSectors(3, 0, 1, make([]byte, testSector)); ok {
		t.Fatal("sector 0 came back from a snapshot that predates the invalidation")
	}
	if again.UsedSlots() != 0 {
		t.Fatalf("used slots = %d after the stale record was disproved, want 0", again.UsedSlots())
	}
	for sec := 0; sec < 3; sec++ {
		if ok, _ := again.ReadSectors(3, sec, 1, make([]byte, testSector)); ok {
			t.Fatalf("sector %d came back from a snapshot that predates its invalidation", sec)
		}
	}
}

// TestCrashRecoveryDropsForeignStamp checks that every field of the stamp is
// part of the decision: each case rewrites it with a valid checksum, so only
// the comparison can reject the entry.
func TestCrashRecoveryDropsForeignStamp(t *testing.T) {
	cases := []struct {
		name string
		edit func(st []byte, gen uint64)
	}{
		{"block", func(st []byte, _ uint64) { put64(st[stampOffBlk:], 99) }},
		{"generation", func(st []byte, gen uint64) { put64(st[stampOffGen:], gen+1) }},
		{"version", func(st []byte, _ uint64) { put64(st[stampOffVer:], 7) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore(testSize)
			f := openTestL2(t, store, "ns")
			if err := f.WriteSectors(7, 0, 1, pattern(testSector, 0x44)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := f.Sync(); err != nil {
				t.Fatalf("sync: %v", err)
			}
			num := slotNumOf(t, f, 7)
			st := stampOf(t, store, num)
			tc.edit(st, f.generation)
			put32(st[stampOffCRC:], checksum(st, stampOffCRC))
			if err := store.WriteAt(st, testStampOffset(num)); err != nil {
				t.Fatalf("write stamp: %v", err)
			}

			again := openTestL2(t, store, "ns")
			if ok, _ := again.ReadSectors(7, 0, 1, make([]byte, testSector)); ok {
				t.Fatal("a slot whose stamp disagrees was served anyway")
			}
			if again.UsedSlots() != 0 {
				t.Fatalf("used slots = %d, want 0", again.UsedSlots())
			}
		})
	}
}

// TestBudgetChangeKeepsIndex is the reason the slots come before the index: a
// budget that changes between two mounts has to leave the slot offsets alone,
// or every resize would drop the cache of a share that is only ever a guess.
func TestBudgetChangeKeepsIndex(t *testing.T) {
	roomy := int64(SuperBlockSize + 64*(testBlock+stampSize) + indexPageSize)
	store := NewMemoryStore(roomy)
	f := openSizedL2(t, store, testSize)
	want := pattern(testSector, 0x77)
	if err := f.WriteSectors(7, 0, 1, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	bigger := openSizedL2(t, store, roomy)
	if bigger.UsedSlots() != 1 {
		t.Fatalf("used slots = %d after growing the budget, want 1", bigger.UsedSlots())
	}
	got := make([]byte, testSector)
	if ok, _ := bigger.ReadSectors(7, 0, 1, got); !ok || !bytes.Equal(got, want) {
		t.Fatal("the cache was lost when the budget grew")
	}
	if bigger.Slots() <= f.Slots() {
		t.Fatalf("slots = %d, want more than %d", bigger.Slots(), f.Slots())
	}
}

// TestBudgetShrinkKeepsIndex checks the other direction: a smaller budget only
// truncates slots off the end, it does not move the ones that remain.
func TestBudgetShrinkKeepsIndex(t *testing.T) {
	roomy := int64(SuperBlockSize + 64*(testBlock+stampSize) + indexPageSize)
	store := NewMemoryStore(roomy)
	f := openSizedL2(t, store, roomy)
	want := pattern(testSector, 0x88)
	if err := f.WriteSectors(7, 0, 1, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	smaller := openSizedL2(t, store, testSize)
	if smaller.UsedSlots() != 1 {
		t.Fatalf("used slots = %d after shrinking the budget, want 1", smaller.UsedSlots())
	}
	got := make([]byte, testSector)
	if ok, _ := smaller.ReadSectors(7, 0, 1, got); !ok || !bytes.Equal(got, want) {
		t.Fatal("the cache was lost when the budget shrank")
	}
}

// TestInvalidationSurvivesReopen checks that dropped sectors stay dropped: the
// flushed index has to describe the invalidated state, not just the cached one.
func TestInvalidationSurvivesReopen(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(3, 0, 3, pattern(3*testSector, 0x55)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.InvalidateSectors(3, 1, 1)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openTestL2(t, store, "ns")
	if ok, _ := again.ReadSectors(3, 1, 1, make([]byte, testSector)); ok {
		t.Fatal("an invalidated sector came back after a restart")
	}
	for _, sec := range []int{0, 2} {
		if ok, _ := again.ReadSectors(3, sec, 1, make([]byte, testSector)); !ok {
			t.Fatalf("sector %d was lost across a restart", sec)
		}
	}
}

// TestDropSurvivesReopen checks that a whole device invalidation is persisted.
func TestDropSurvivesReopen(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	for blk := uint64(1); blk <= 3; blk++ {
		if err := f.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	f.Drop()
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	again := openTestL2(t, store, "ns")
	if again.UsedSlots() != 0 {
		t.Fatalf("used slots = %d after a persisted drop, want 0", again.UsedSlots())
	}
}

// TestTornIndexPageIsIgnored checks the page checksum: a page that was only
// partly written must be rejected instead of trusted.
func TestTornIndexPageIsIgnored(t *testing.T) {
	store := NewMemoryStore(testSize)
	f := openTestL2(t, store, "ns")
	if err := f.WriteSectors(7, 0, 1, pattern(testSector, 0x66)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Corrupt one record of the first index page.
	off := f.indexOffset(f.slotCount, 0) + indexHdrSize
	b := make([]byte, 1)
	if err := store.ReadAt(b, off); err != nil {
		t.Fatalf("read index page: %v", err)
	}
	b[0] ^= 0xff
	if err := store.WriteAt(b, off); err != nil {
		t.Fatalf("write index page: %v", err)
	}

	again := openTestL2(t, store, "ns")
	if again.UsedSlots() != 0 {
		t.Fatalf("used slots = %d from a torn page, want 0", again.UsedSlots())
	}
}
