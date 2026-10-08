package frontend_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsicache/proxy"
)

const (
	loopBlockSize  = 512
	loopBlockCount = 4096
)

// loopBackend is an in-memory LUN that speaks just enough SCSI for discovery and
// read/write.
type loopBackend struct {
	mu    sync.Mutex
	data  []byte
	reads int
}

func newLoopBackend() *loopBackend {
	b := &loopBackend{data: make([]byte, loopBlockSize*loopBlockCount)}
	for i := range b.data {
		b.data[i] = byte(i / loopBlockSize)
	}
	return b
}

func (b *loopBackend) Info() backend.DeviceInfo {
	return backend.DeviceInfo{
		Vendor: "LOOP", Product: "Loopback Disk", Revision: "0001",
		Serial: "LOOPSN01", WWID: "naa.6001405loop",
		BlockSize: loopBlockSize, BlockCount: loopBlockCount,
		TargetIQN: "iqn.test:loop",
	}
}

func (b *loopBackend) Exec(_ context.Context, cdb []byte, dataOut []byte, inLen int) (*backend.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch cdb[0] {
	case iscsi.SCSIRead10, iscsi.SCSIRead16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		b.reads++
		off := int64(p.LBA) * loopBlockSize
		n := int64(p.Blocks) * loopBlockSize
		if off+n > int64(len(b.data)) {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
		}
		return &backend.Result{Status: iscsi.StatusGood, Data: append([]byte(nil), b.data[off:off+n]...)}, nil
	case iscsi.SCSIWrite10, iscsi.SCSIWrite16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		off := int64(p.LBA) * loopBlockSize
		n := int64(p.Blocks) * loopBlockSize
		if off+n > int64(len(b.data)) || int64(len(dataOut)) != n {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidFieldInCDB)}, nil
		}
		copy(b.data[off:], dataOut)
		return &backend.Result{Status: iscsi.StatusGood}, nil
	default:
		return &backend.Result{Status: iscsi.StatusCheckCondition,
			Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidCommandOperationCode)}, nil
	}
}

func (b *loopBackend) Close() error { return nil }

func (b *loopBackend) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

func (b *loopBackend) snapshot(off, n int64) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data[off:off+n]...)
}

// TestLoopback drives the whole module with its own two protocol endpoints: the
// frontend target is exercised through the backend initiator over TCP.
func TestLoopback(t *testing.T) {
	const targetIQN = "iqn.2024-01.local.vault:loopback-test"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	be := newLoopBackend()
	c, err := cache.New(cache.Config{
		BlockSize: 16 << 10, SectorSize: 4 << 10, L1Bytes: 1 << 20, Shards: 4,
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	px, err := proxy.New(proxy.Config{
		Backend: be, Cache: c, TargetIQN: targetIQN,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	tgt, err := frontend.New(frontend.Config{
		ListenAddr: "127.0.0.1:0",
		TargetIQN:  targetIQN,
	}, px)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	defer tgt.Close()

	serveErr := make(chan error, 1)
	go func() { serveErr <- tgt.Serve(ctx) }()

	// --- Login and discovery through the real wire format -------------------
	it, err := backend.Dial(ctx, backend.Config{
		Address:     tgt.Addr().String(),
		TargetIQN:   targetIQN,
		IOTimeout:   10 * time.Second,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("initiator dial: %v", err)
	}
	defer it.Close()

	info := it.Info()
	if info.BlockSize != loopBlockSize || info.BlockCount != loopBlockCount {
		t.Fatalf("discovered geometry = %d/%d", info.BlockSize, info.BlockCount)
	}
	if info.Serial != "LOOPSN01" {
		t.Fatalf("discovered serial = %q", info.Serial)
	}
	if info.Product != "Loopback Disk" {
		t.Fatalf("discovered product = %q", info.Product)
	}

	// --- TEST UNIT READY ----------------------------------------------------
	res, err := it.Exec(ctx, []byte{iscsi.SCSITestUnitReady, 0, 0, 0, 0, 0}, nil, 0)
	if err != nil {
		t.Fatalf("test unit ready: %v", err)
	}
	if !res.OK() {
		t.Fatalf("test unit ready status: %v", res.Error())
	}

	// --- READ(10) and the L1 hit -------------------------------------------
	read := func(lba uint64, blocks uint32) []byte {
		t.Helper()
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
		binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
		r, err := it.Exec(ctx, cdb, nil, int(blocks)*loopBlockSize)
		if err != nil {
			t.Fatalf("read lba=%d: %v", lba, err)
		}
		if !r.OK() {
			t.Fatalf("read lba=%d status: %v", lba, r.Error())
		}
		if len(r.Data) != int(blocks)*loopBlockSize {
			t.Fatalf("read lba=%d returned %d bytes", lba, len(r.Data))
		}
		return r.Data
	}

	got := read(0, 32)
	if !bytes.Equal(got, be.snapshot(0, 32*loopBlockSize)) {
		t.Fatal("read data does not match the backing LUN")
	}
	firstReads := be.readCount()
	if firstReads != 1 {
		t.Fatalf("backend reads = %d, want 1 after the first read", firstReads)
	}

	again := read(0, 32)
	if !bytes.Equal(got, again) {
		t.Fatal("the second read returned different data")
	}
	if be.readCount() != firstReads {
		t.Fatalf("backend reads = %d, want %d: the second read must be an L1 hit", be.readCount(), firstReads)
	}

	// --- WRITE(10) then read back ------------------------------------------
	payload := bytes.Repeat([]byte{0x5a}, 4*loopBlockSize)
	wcdb := make([]byte, 10)
	wcdb[0] = iscsi.SCSIWrite10
	binary.BigEndian.PutUint32(wcdb[2:6], 8)
	binary.BigEndian.PutUint16(wcdb[7:9], 4)
	wres, err := it.Exec(ctx, wcdb, payload, 0)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wres.OK() {
		t.Fatalf("write status: %v", wres.Error())
	}
	if !bytes.Equal(be.snapshot(8*loopBlockSize, int64(len(payload))), payload) {
		t.Fatal("the payload did not reach the backing LUN")
	}

	got = read(8, 4)
	if !bytes.Equal(got, payload) {
		t.Fatal("stale data was served after a write")
	}

	// --- Out-of-range read is rejected, not dropped -------------------------
	oorCDB := make([]byte, 10)
	oorCDB[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(oorCDB[2:6], loopBlockCount+16)
	binary.BigEndian.PutUint16(oorCDB[7:9], 1)
	oor, err := it.Exec(ctx, oorCDB, nil, loopBlockSize)
	if err != nil {
		t.Fatalf("out of range read: %v", err)
	}
	if oor.OK() {
		t.Fatal("an out-of-range read must not return GOOD")
	}
	if len(oor.Sense) < 14 || oor.Sense[12] != iscsi.ASCLogicalBlockAddressOutOfRange {
		t.Fatalf("sense = %v", oor.Sense)
	}

	// --- Shutdown ----------------------------------------------------------
	if err := it.Close(); err != nil {
		t.Fatalf("initiator close: %v", err)
	}
	if err := tgt.Close(); err != nil {
		t.Fatalf("target close: %v", err)
	}
	cancel()
	if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}

	st := c.Stats()
	if st.L1Hits == 0 {
		t.Fatalf("expected an L1 hit, cache stats = %+v", st)
	}
	if pst := px.Stats(); pst.Reads == 0 || pst.Writes == 0 {
		t.Fatalf("proxy stats = %+v", pst)
	}
}

// TestLoopbackDiscoverySession drives a raw discovery login and a SendTargets
// text request, which is the path a Windows initiator uses to enumerate the
// targets behind a portal.
func TestLoopbackDiscoverySession(t *testing.T) {
	const targetIQN = "iqn.2024-01.local.vault:discovery-test"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	be := newLoopBackend()
	px, err := proxy.New(proxy.Config{Backend: be, TargetIQN: targetIQN})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0", TargetIQN: targetIQN}, px)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	defer tgt.Close()
	go tgt.Serve(ctx)

	conn, err := net.DialTimeout("tcp", tgt.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var isid [6]byte
	copy(isid[:], "disc01")

	// Security stage, offering the keys an initiator normally sends.
	login := func(csg, nsg uint8, transit bool, keys [][2]string) *iscsi.PDU {
		t.Helper()
		p := iscsi.NewPDU(iscsi.OpLoginReq, iscsi.EncodeText(keys...))
		var flags uint8 = csg<<iscsi.LoginFlagCSGShift | (nsg & iscsi.LoginFlagNSGMask)
		if transit {
			flags |= iscsi.LoginFlagTransit
		}
		p.Header.SetFlags(flags)
		p.Header.SetImmediate(true)
		copy(p.Header[8:14], isid[:])
		p.Header.SetLoginTSIH(0)
		p.Header.SetITT(0x11)
		binary.BigEndian.PutUint16(p.Header[20:22], 1)
		if err := iscsi.WritePDU(conn, p); err != nil {
			t.Fatalf("send login: %v", err)
		}
		resp, err := iscsi.ReadPDU(conn)
		if err != nil {
			t.Fatalf("read login response: %v", err)
		}
		if resp.Header.Opcode() != iscsi.OpLoginResp {
			t.Fatalf("expected Login Response, got %s", iscsi.OpcodeName(resp.Header.Opcode()))
		}
		if class := resp.Header.LoginStatusClass(); class != 0 {
			t.Fatalf("login rejected: class=%d detail=%d", class, resp.Header.LoginStatusDetail())
		}
		return resp
	}

	resp := login(iscsi.StageSecurity, iscsi.StageOperational, true, [][2]string{
		{"AuthMethod", "None"},
		{"HeaderDigest", "None"},
		{"DataDigest", "None"},
		{"MaxRecvDataSegmentLength", "65536"},
	})
	if !resp.Header.LoginTransit() {
		t.Fatal("the target did not accept the transition to the operational stage")
	}

	resp = login(resp.Header.LoginNSG(), iscsi.StageFullFeature, true, [][2]string{
		{"SessionType", "Discovery"},
		{"MaxRecvDataSegmentLength", "65536"},
	})
	if resp.Header.LoginStatusClass() != 0 {
		t.Fatalf("discovery login rejected: class=%d", resp.Header.LoginStatusClass())
	}
	if !resp.Header.LoginTransit() {
		t.Fatal("the target did not enter the full feature phase")
	}

	// SendTargets.
	text := iscsi.NewPDU(iscsi.OpTextReq, iscsi.EncodeText([2]string{"SendTargets", "All"}))
	text.Header.SetFlags(iscsi.FlagAlwaysSet)
	text.Header.SetITT(0x22)
	text.Header.SetTTT(iscsi.ReservedTag)
	text.Header.SetCmdSN(1)
	if err := iscsi.WritePDU(conn, text); err != nil {
		t.Fatalf("send text: %v", err)
	}
	tr, err := iscsi.ReadPDU(conn)
	if err != nil {
		t.Fatalf("read text response: %v", err)
	}
	if tr.Header.Opcode() != iscsi.OpTextResp {
		t.Fatalf("expected Text Response, got %s", iscsi.OpcodeName(tr.Header.Opcode()))
	}
	keys := iscsi.ParseText(tr.Data)
	if keys["targetname"] != targetIQN {
		t.Fatalf("SendTargets returned %q, want %q", keys["targetname"], targetIQN)
	}
}
