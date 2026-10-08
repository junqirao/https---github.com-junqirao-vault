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
	// Three slots plus the super block.
	testSize = SuperBlockSize + 3*testBlock
)

func openTestL2(t *testing.T, store Store, namespace string) *File {
	t.Helper()
	f, err := Open(Config{
		Path:       "test.bin",
		SizeBytes:  testSize,
		BlockSize:  testBlock,
		SectorSize: testSector,
		Namespace:  namespace,
		Store:      store,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return f
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

// TestRingRecyclesOldestSlot checks the FIFO slot allocation: the fourth
// distinct block must evict the first one.
func TestRingRecyclesOldestSlot(t *testing.T) {
	f := openTestL2(t, NewMemoryStore(testSize), "ns")
	for blk := uint64(1); blk <= 3; blk++ {
		if err := f.WriteSectors(blk, 0, 1, pattern(testSector, byte(blk))); err != nil {
			t.Fatalf("write block %d: %v", blk, err)
		}
	}
	if f.UsedSlots() != 3 {
		t.Fatalf("used slots = %d, want 3", f.UsedSlots())
	}
	if err := f.WriteSectors(4, 0, 1, pattern(testSector, 4)); err != nil {
		t.Fatalf("write block 4: %v", err)
	}

	ok, _ := f.ReadSectors(1, 0, 1, make([]byte, testSector))
	if ok {
		t.Fatal("block 1 should have been recycled")
	}
	ok, _ = f.ReadSectors(4, 0, 1, make([]byte, testSector))
	if !ok {
		t.Fatal("block 4 was not stored")
	}
	if f.UsedSlots() != 3 {
		t.Fatalf("used slots = %d, want 3", f.UsedSlots())
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

// TestGeometryChangeRewritesSuperBlock checks that a different geometry resets
// the file instead of misinterpreting the old slots.
func TestGeometryChangeRewritesSuperBlock(t *testing.T) {
	store := NewMemoryStore(SuperBlockSize + 8*testBlock)
	if _, err := Open(Config{
		Path: "g.bin", SizeBytes: SuperBlockSize + 8*testBlock,
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
		Path: "g.bin", SizeBytes: SuperBlockSize + 8*(2*testBlock),
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
