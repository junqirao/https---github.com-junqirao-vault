package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/iscsi"
)

const (
	testBlockSize  = 512
	testBlockCount = 4096
	testCapacity   = testBlockSize * testBlockCount
)

// fakeBackend is an in-memory LUN with INQUIRY/READ CAPACITY/READ/WRITE support.
type fakeBackend struct {
	mu    sync.Mutex
	data  []byte
	reads int
	execs []uint8
}

func newFakeBackend() *fakeBackend {
	f := &fakeBackend{data: make([]byte, testCapacity)}
	for i := range f.data {
		f.data[i] = byte(i / testBlockSize)
	}
	return f
}

func (f *fakeBackend) Info() backend.DeviceInfo {
	return backend.DeviceInfo{
		Vendor: "FAKE", Product: "Loopback Disk", Revision: "0001",
		Serial: "FAKESN01", WWID: "naa.6001405fake",
		BlockSize: testBlockSize, BlockCount: testBlockCount,
		TargetIQN: "iqn.test:fake",
	}
}

func (f *fakeBackend) Exec(_ context.Context, cdb []byte, dataOut []byte, inLen int) (*backend.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(cdb) == 0 {
		return &backend.Result{Status: iscsi.StatusCheckCondition, Sense: iscsi.IllegalRequestSense(0x24)}, nil
	}
	f.execs = append(f.execs, cdb[0])
	switch cdb[0] {
	case iscsi.SCSIRead10, iscsi.SCSIRead16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		f.reads++
		off := int64(p.LBA) * testBlockSize
		n := int64(p.Blocks) * testBlockSize
		if off+n > int64(len(f.data)) {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
		}
		out := append([]byte(nil), f.data[off:off+n]...)
		return &backend.Result{Status: iscsi.StatusGood, Data: out}, nil

	case iscsi.SCSIWrite10, iscsi.SCSIWrite16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		off := int64(p.LBA) * testBlockSize
		n := int64(p.Blocks) * testBlockSize
		if off+n > int64(len(f.data)) {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
		}
		if int64(len(dataOut)) != n {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidFieldInCDB)}, nil
		}
		copy(f.data[off:], dataOut)
		return &backend.Result{Status: iscsi.StatusGood}, nil

	case iscsi.SCSIUnmap:
		return &backend.Result{Status: iscsi.StatusGood}, nil

	case iscsi.SCSIModeSense6:
		return &backend.Result{Status: iscsi.StatusGood, Data: make([]byte, 8)}, nil

	default:
		return &backend.Result{Status: iscsi.StatusCheckCondition,
			Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidCommandOperationCode)}, nil
	}
}

func (f *fakeBackend) Close() error { return nil }

func (f *fakeBackend) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeBackend) snapshot(off, n int64) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data[off:off+n]...)
}

func newTestProxy(t *testing.T, withCache bool) (*Proxy, *fakeBackend, *cache.Cache) {
	t.Helper()
	be := newFakeBackend()
	var c *cache.Cache
	if withCache {
		var err error
		c, err = cache.New(cache.Config{
			BlockSize: 16 << 10, SectorSize: 4 << 10,
			L1Bytes: 1 << 20, Shards: 4,
		})
		if err != nil {
			t.Fatalf("cache.New: %v", err)
		}
	}
	p, err := New(Config{
		Backend:   be,
		Cache:     c,
		Vendor:    "VAULT",
		Product:   "iSCSI Read Cache",
		Revision:  "0001",
		TargetIQN: "iqn.2024-01.local.vault:test",
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, be, c
}

func read10(lba uint32, blocks uint16) []byte {
	cdb := make([]byte, 10)
	cdb[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(cdb[2:6], lba)
	binary.BigEndian.PutUint16(cdb[7:9], blocks)
	return cdb
}

func write10(lba uint32, blocks uint16) []byte {
	cdb := read10(lba, blocks)
	cdb[0] = iscsi.SCSIWrite10
	return cdb
}

func TestDeviceReportsBackendGeometry(t *testing.T) {
	p, _, _ := newTestProxy(t, false)
	d := p.Device()
	if d.BlockSize != testBlockSize || d.BlockCount != testBlockCount {
		t.Fatalf("geometry = %d/%d", d.BlockSize, d.BlockCount)
	}
	if d.Vendor != "VAULT" {
		t.Fatalf("vendor override = %q", d.Vendor)
	}
	if d.Serial != "FAKESN01" {
		t.Fatalf("serial = %q", d.Serial)
	}
	if d.CapacityBytes() != testCapacity {
		t.Fatalf("capacity = %d", d.CapacityBytes())
	}
}

func TestLocalCommandsDoNotReachTheBackend(t *testing.T) {
	p, be, _ := newTestProxy(t, false)
	ctx := context.Background()

	cases := []struct {
		name string
		cdb  []byte
		in   int
	}{
		{"test unit ready", []byte{iscsi.SCSITestUnitReady}, 0},
		{"start stop unit", []byte{iscsi.SCSIStartStopUnit}, 0},
		{"request sense", []byte{iscsi.SCSIRequestSense}, 18},
		{"report luns", []byte{iscsi.SCSIReportLuns}, 16},
		{"read capacity 10", []byte{iscsi.SCSIReadCapacity10}, 8},
		{"read capacity 16", []byte{iscsi.SCSIServiceActionIn16, 0x10}, 32},
		{"synchronize cache", []byte{iscsi.SCSISynchronizeCache10}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := p.Execute(ctx, tc.cdb, nil, tc.in)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if res.Status != iscsi.StatusGood {
				t.Fatalf("status = 0x%02x sense=%s", res.Status, iscsi.SenseFromResult(res.Sense))
			}
		})
	}
	if len(be.execs) != 0 {
		t.Fatalf("local commands reached the backend: %v", be.execs)
	}
	if st := p.Stats(); st.LocalCmds != int64(len(cases)) {
		t.Fatalf("local commands = %d, want %d", st.LocalCmds, len(cases))
	}
}

func TestReadCapacityValues(t *testing.T) {
	p, _, _ := newTestProxy(t, false)
	ctx := context.Background()

	res, _ := p.Execute(ctx, []byte{iscsi.SCSIReadCapacity10}, nil, 8)
	if len(res.Data) != 8 {
		t.Fatalf("read capacity 10 length = %d", len(res.Data))
	}
	if got := binary.BigEndian.Uint32(res.Data[0:4]); got != testBlockCount-1 {
		t.Fatalf("last lba = %d, want %d", got, testBlockCount-1)
	}
	if got := binary.BigEndian.Uint32(res.Data[4:8]); got != testBlockSize {
		t.Fatalf("block length = %d", got)
	}

	res, _ = p.Execute(ctx, []byte{iscsi.SCSIServiceActionIn16, 0x10}, nil, 32)
	if len(res.Data) != 32 {
		t.Fatalf("read capacity 16 length = %d", len(res.Data))
	}
	if got := binary.BigEndian.Uint64(res.Data[0:8]); got != testBlockCount-1 {
		t.Fatalf("last lba 16 = %d", got)
	}
}

func TestInquiryPages(t *testing.T) {
	p, _, _ := newTestProxy(t, false)
	ctx := context.Background()

	// Standard INQUIRY.
	res, err := p.Execute(ctx, []byte{iscsi.SCSIInquiry, 0x00, 0x00, 0x00, 36, 0x00}, nil, 36)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Data) != 36 {
		t.Fatalf("standard inquiry length = %d", len(res.Data))
	}
	if !bytes.Equal(res.Data[8:16], iscsi.ASCIIField("VAULT", 8)) {
		t.Fatalf("vendor field = %q", res.Data[8:16])
	}

	// VPD 0x00, 0x80 and 0x83.
	for _, page := range []uint8{0x00, 0x80, 0x83} {
		cdb := []byte{iscsi.SCSIInquiry, 0x01, page, 0x00, 0xff, 0x00}
		res, err := p.Execute(ctx, cdb, nil, 255)
		if err != nil {
			t.Fatalf("vpd 0x%02x: %v", page, err)
		}
		if res.Status != iscsi.StatusGood {
			t.Fatalf("vpd 0x%02x: status 0x%02x", page, res.Status)
		}
		if len(res.Data) < 4 || res.Data[1] != page {
			t.Fatalf("vpd 0x%02x: page code = 0x%02x", page, res.Data[1])
		}
	}

	// An unsupported page is an illegal request, not a silent GOOD.
	res, _ = p.Execute(ctx, []byte{iscsi.SCSIInquiry, 0x01, 0x2f, 0x00, 0xff, 0x00}, nil, 255)
	if res.Status != iscsi.StatusCheckCondition {
		t.Fatalf("unsupported VPD page status = 0x%02x", res.Status)
	}
}

func TestInquiryIsTruncatedToAllocationLength(t *testing.T) {
	p, _, _ := newTestProxy(t, false)
	res, err := p.Execute(context.Background(), []byte{iscsi.SCSIInquiry, 0x00, 0x00, 0x00, 8, 0x00}, nil, 8)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Data) != 8 {
		t.Fatalf("length = %d, want 8", len(res.Data))
	}
}

func TestReadIsCached(t *testing.T) {
	p, be, _ := newTestProxy(t, true)
	ctx := context.Background()

	res, err := p.Execute(ctx, read10(0, 8), nil, 4096)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Status != iscsi.StatusGood || len(res.Data) != 4096 {
		t.Fatalf("read: status=0x%02x len=%d", res.Status, len(res.Data))
	}
	if !bytes.Equal(res.Data, be.snapshot(0, 4096)) {
		t.Fatal("data does not match the backing LUN")
	}
	if be.readCount() != 1 {
		t.Fatalf("backend reads = %d, want 1", be.readCount())
	}

	// The same range again must be served from L1.
	res2, err := p.Execute(ctx, read10(0, 8), nil, 4096)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if be.readCount() != 1 {
		t.Fatalf("backend reads after an L1 hit = %d, want 1", be.readCount())
	}
	if !bytes.Equal(res.Data, res2.Data) {
		t.Fatal("cached data differs from the first read")
	}
	if st := p.Stats(); st.Reads != 2 {
		t.Fatalf("reads = %d, want 2", st.Reads)
	}
}

func TestReadOutOfRange(t *testing.T) {
	p, _, _ := newTestProxy(t, true)
	// Start beyond the LUN.
	res, err := p.Execute(context.Background(), read10(testBlockCount+10, 8), nil, 4096)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Status != iscsi.StatusCheckCondition {
		t.Fatalf("status = 0x%02x", res.Status)
	}
	if len(res.Sense) < 14 || res.Sense[12] != iscsi.ASCLogicalBlockAddressOutOfRange {
		t.Fatalf("sense = %v", res.Sense)
	}
	// Extend past the end of the LUN.
	res, _ = p.Execute(context.Background(), read10(testBlockCount-1, 8), nil, 4096)
	if res.Status != iscsi.StatusCheckCondition {
		t.Fatalf("status = 0x%02x", res.Status)
	}
}

// TestWriteInvalidatesCache is the consistency property: a cached range must not
// survive a write to that range.
func TestWriteInvalidatesCache(t *testing.T) {
	p, be, _ := newTestProxy(t, true)
	ctx := context.Background()

	// Warm the cache.
	if _, err := p.Execute(ctx, read10(0, 8), nil, 4096); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if be.readCount() != 1 {
		t.Fatalf("backend reads = %d", be.readCount())
	}

	payload := bytes.Repeat([]byte{0xaa}, 4096)
	res, err := p.Execute(ctx, write10(0, 8), payload, 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Status != iscsi.StatusGood {
		t.Fatalf("write status = 0x%02x", res.Status)
	}
	if !bytes.Equal(be.snapshot(0, 4096), payload) {
		t.Fatal("payload did not reach the backing LUN")
	}
	if st := p.Stats(); st.Writes != 1 || st.Invalidations != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// The read must go back to the backing LUN and see the new data.
	res, err = p.Execute(ctx, read10(0, 8), nil, 4096)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if be.readCount() != 2 {
		t.Fatalf("backend reads = %d, want 2 (the invalidated range must be re-read)", be.readCount())
	}
	if !bytes.Equal(res.Data, payload) {
		t.Fatal("stale data was served after a write")
	}
}

func TestWriteLengthMismatch(t *testing.T) {
	p, be, _ := newTestProxy(t, true)
	res, err := p.Execute(context.Background(), write10(0, 8), make([]byte, 512), 0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Status != iscsi.StatusCheckCondition {
		t.Fatalf("status = 0x%02x", res.Status)
	}
	if len(be.execs) != 0 {
		t.Fatal("a malformed write must not reach the backend")
	}
}

func TestUnmapInvalidatesRanges(t *testing.T) {
	p, be, _ := newTestProxy(t, true)
	ctx := context.Background()

	// Warm the whole first block.
	if _, err := p.Execute(ctx, read10(0, 32), nil, 16384); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if be.readCount() != 1 {
		t.Fatalf("backend reads = %d", be.readCount())
	}

	// Unmap eight 512-byte blocks starting at LBA 8 (byte offset 4096).
	list := make([]byte, 24)
	binary.BigEndian.PutUint16(list[0:2], 16)
	binary.BigEndian.PutUint16(list[2:4], 8)
	binary.BigEndian.PutUint64(list[8:16], 8)
	binary.BigEndian.PutUint32(list[16:20], 8)

	res, err := p.Execute(ctx, []byte{iscsi.SCSIUnmap}, list, 0)
	if err != nil {
		t.Fatalf("unmap: %v", err)
	}
	if res.Status != iscsi.StatusGood {
		t.Fatalf("unmap status = 0x%02x", res.Status)
	}

	// Reading the unmapped range must go back to the backend, while the
	// untouched part of the block stays cached.
	if _, err := p.Execute(ctx, read10(8, 8), nil, 4096); err != nil {
		t.Fatalf("read: %v", err)
	}
	before := be.readCount()
	if _, err := p.Execute(ctx, read10(0, 8), nil, 4096); err != nil {
		t.Fatalf("read: %v", err)
	}
	if be.readCount() != before {
		t.Fatalf("the untouched range was read again (%d -> %d)", before, be.readCount())
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	p, be, _ := newTestProxy(t, true)
	res, err := p.Execute(context.Background(), []byte{0x1c, 0, 0, 0, 0, 0}, nil, 0)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Status != iscsi.StatusCheckCondition {
		t.Fatalf("status = 0x%02x", res.Status)
	}
	if len(res.Sense) < 14 || res.Sense[12] != iscsi.ASCInvalidCommandOperationCode {
		t.Fatalf("sense = %v", res.Sense)
	}
	if len(be.execs) != 0 {
		t.Fatal("an unknown command must not be forwarded")
	}
	if st := p.Stats(); st.Unsupported != 1 {
		t.Fatalf("unsupported = %d", st.Unsupported)
	}
}

func TestModeSenseIsPassedThrough(t *testing.T) {
	p, _, _ := newTestProxy(t, true)
	res, err := p.Execute(context.Background(), []byte{iscsi.SCSIModeSense6, 0, 0x3f, 0, 8, 0}, nil, 8)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Status != iscsi.StatusGood {
		t.Fatalf("status = 0x%02x", res.Status)
	}
	if st := p.Stats(); st.Passthrough != 1 {
		t.Fatalf("passthrough = %d", st.Passthrough)
	}
}

func TestReadWithoutCache(t *testing.T) {
	p, be, _ := newTestProxy(t, false)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := p.Execute(ctx, read10(0, 8), nil, 4096)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if res.Status != iscsi.StatusGood {
			t.Fatalf("status = 0x%02x", res.Status)
		}
	}
	if be.readCount() != 3 {
		t.Fatalf("backend reads = %d, want 3", be.readCount())
	}
}

func TestNewValidatesGeometry(t *testing.T) {
	be := newFakeBackend()
	// A geometry the cache accepts but that cannot be expressed in whole 512
	// byte blocks.
	c, err := cache.New(cache.Config{
		BlockSize: 16000, SectorSize: 1000,
		L1Bytes: 1 << 20, Shards: 4,
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	if _, err := New(Config{Backend: be, Cache: c}); err == nil {
		t.Fatal("expected a sector size that is not a multiple of the block size to be rejected")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected a missing backend to be rejected")
	}
}

// TestReadBufferIsPooled pins the contract the frontend relies on: a read hands
// back its buffer through Release, and the pool then serves that same buffer to
// the next read instead of allocating a fresh one per command.
func TestReadBufferIsPooled(t *testing.T) {
	p, _, _ := newTestProxy(t, false)
	ctx := context.Background()

	var first *byte
	reused := false
	for i := 0; i < 4 && !reused; i++ {
		res, err := p.Execute(ctx, read10(0, 8), nil, 4096)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if res.Release == nil {
			t.Fatal("a read must hand its buffer back through Release")
		}
		if first == nil {
			first = &res.Data[0]
		} else if &res.Data[0] == first {
			reused = true
		}
		res.Release()
	}
	if !reused {
		t.Fatal("the pool never handed the read buffer back")
	}

	// A buffer too large to be reused is not retained.
	huge := getReadBuf(maxPooledRead + 1)
	putReadBuf(huge)
	next := getReadBuf(maxPooledRead + 1)
	if &next[0] == &huge[0] {
		t.Fatal("an oversized read buffer must not be retained by the pool")
	}
}
