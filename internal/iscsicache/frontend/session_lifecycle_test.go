package frontend_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsicache/proxy"
)

func startLifecycleTarget(t *testing.T, targetIQN string) (*frontend.Target, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	be := newLoopBackend()
	c, err := cache.New(cache.Config{BlockSize: 16 << 10, SectorSize: 4 << 10, L1Bytes: 1 << 20, Shards: 4})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	px, err := proxy.New(proxy.Config{Backend: be, Cache: c, TargetIQN: targetIQN,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0", TargetIQN: targetIQN}, px)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- tgt.Serve(ctx) }()
	return tgt, cancel, serveErr
}

// rawLogin sends one Login Request and returns the response.
func rawLogin(t *testing.T, conn net.Conn, isid [6]byte, csg, nsg uint8, transit bool, keys [][2]string) *iscsi.PDU {
	t.Helper()
	p := iscsi.NewPDU(iscsi.OpLoginReq, iscsi.EncodeText(keys...))
	var flags uint8 = csg<<iscsi.LoginFlagCSGShift | (nsg & iscsi.LoginFlagNSGMask)
	if transit {
		flags |= iscsi.LoginFlagTransit
	}
	p.Header.SetFlags(flags)
	p.Header.SetImmediate(true)
	copy(p.Header[8:14], isid[:])
	p.Header.SetITT(0x11)
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
	return resp
}

// TestLogoutThenRelogin guards the logout path: the single normal session slot
// must be released so the initiator can log in again.
func TestLogoutThenRelogin(t *testing.T) {
	const iqn = "iqn.test:lifecycle-relogin"
	tgt, cancel, serveErr := startLifecycleTarget(t, iqn)
	defer cancel()
	defer func() { tgt.Close(); <-serveErr }()

	addr := tgt.Addr().String()
	ctx := context.Background()

	it, err := backend.Dial(ctx, backend.Config{Address: addr, TargetIQN: iqn,
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("logout: %v", err)
	}

	it2, err := backend.Dial(ctx, backend.Config{Address: addr, TargetIQN: iqn,
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("re-dial after logout: %v", err)
	}
	it2.Close()
}

// TestDiscoverySessionDoesNotBlockNormalSession covers the Windows initiator
// flow where a SendTargets probe stays open next to the real session. A
// discovery session must not consume the normal session slot.
func TestDiscoverySessionDoesNotBlockNormalSession(t *testing.T) {
	const iqn = "iqn.test:lifecycle-discovery"
	tgt, cancel, serveErr := startLifecycleTarget(t, iqn)
	defer cancel()
	defer func() { tgt.Close(); <-serveErr }()

	conn, err := net.DialTimeout("tcp", tgt.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var isid [6]byte
	copy(isid[:], "disc03")
	resp := rawLogin(t, conn, isid, iscsi.StageSecurity, iscsi.StageOperational, true, [][2]string{
		{"AuthMethod", "None"}, {"MaxRecvDataSegmentLength", "65536"},
	})
	resp = rawLogin(t, conn, isid, resp.Header.LoginNSG(), iscsi.StageFullFeature, true, [][2]string{
		{"SessionType", "Discovery"}, {"MaxRecvDataSegmentLength", "65536"},
	})
	if class := resp.Header.LoginStatusClass(); class != 0 {
		t.Fatalf("discovery login rejected: class=%d detail=%d", class, resp.Header.LoginStatusDetail())
	}
	if !resp.Header.LoginTransit() {
		t.Fatal("the discovery session did not enter the full feature phase")
	}

	// The normal session must still be accepted while discovery is held open.
	it, err := backend.Dial(context.Background(), backend.Config{
		Address: tgt.Addr().String(), TargetIQN: iqn,
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("normal session rejected while a discovery session is held: %v", err)
	}
	it.Close()

	// The discovery connection must stay usable, and its TSIH must differ from
	// the normal session's.
	text := iscsi.NewPDU(iscsi.OpTextReq, iscsi.EncodeText([2]string{"SendTargets", "All"}))
	text.Header.SetFlags(iscsi.FlagAlwaysSet)
	text.Header.SetITT(0x22)
	text.Header.SetTTT(iscsi.ReservedTag)
	text.Header.SetCmdSN(1)
	if err := iscsi.WritePDU(conn, text); err != nil {
		t.Fatalf("send text after the normal session: %v", err)
	}
	tr, err := iscsi.ReadPDU(conn)
	if err != nil {
		t.Fatalf("read text response after the normal session: %v", err)
	}
	if got := iscsi.ParseText(tr.Data)["targetname"]; got != iqn {
		t.Fatalf("SendTargets returned %q, want %q", got, iqn)
	}
}

// TestConcurrentNormalSessions checks that a second normal session is served
// rather than refused. The Windows initiator opens several connections and
// sessions; refusing one leaves it stuck in a state where it can neither use
// nor release the target.
func TestConcurrentNormalSessions(t *testing.T) {
	const iqn = "iqn.test:lifecycle-concurrent"
	tgt, cancel, serveErr := startLifecycleTarget(t, iqn)
	defer cancel()
	defer func() { tgt.Close(); <-serveErr }()

	ctx := context.Background()
	dial := func() *backend.Initiator {
		t.Helper()
		it, err := backend.Dial(ctx, backend.Config{
			Address: tgt.Addr().String(), TargetIQN: iqn,
			IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("normal session: %v", err)
		}
		return it
	}

	first := dial()
	defer first.Close()
	second := dial()
	defer second.Close()

	// Both sessions must be able to read the LUN.
	read := func(it *backend.Initiator, lba uint32) {
		t.Helper()
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], 1)
		res, err := it.Exec(ctx, cdb, nil, loopBlockSize)
		if err != nil {
			t.Fatalf("read lba=%d: %v", lba, err)
		}
		if !res.OK() {
			t.Fatalf("read lba=%d status: %v", lba, res.Error())
		}
		if len(res.Data) != loopBlockSize {
			t.Fatalf("read lba=%d returned %d bytes", lba, len(res.Data))
		}
	}
	read(first, 1)
	read(second, 2)
}

// TestShortReadReportsUnderflow covers a command whose allocation length is
// larger than the data the device returns. The shortfall must be reported in the
// Residual Count with the U bit set, on the Data-In PDU that also carries the
// status (the S bit). Ending such a command with a separate SCSI Response after
// an unsolicited Data-In is what made the Windows initiator reset the session.
func TestShortReadReportsUnderflow(t *testing.T) {
	const iqn = "iqn.test:short-read-residual"
	tgt, cancel, serveErr := startLifecycleTarget(t, iqn)
	defer cancel()
	defer func() { tgt.Close(); <-serveErr }()

	conn, err := net.DialTimeout("tcp", tgt.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var isid [6]byte
	copy(isid[:], "short1")
	resp := rawLogin(t, conn, isid, iscsi.StageSecurity, iscsi.StageOperational, true, [][2]string{
		{"AuthMethod", "None"}, {"MaxRecvDataSegmentLength", "65536"},
	})
	resp = rawLogin(t, conn, isid, resp.Header.LoginNSG(), iscsi.StageFullFeature, true, [][2]string{
		{"SessionType", "Normal"}, {"TargetName", iqn}, {"MaxRecvDataSegmentLength", "65536"},
	})
	if class := resp.Header.LoginStatusClass(); class != 0 {
		t.Fatalf("normal login rejected: class=%d detail=%d", class, resp.Header.LoginStatusDetail())
	}
	loginStatSN := resp.Header.StatSN()

	// INQUIRY with EVPD=1, page 0x00 and a 255 byte allocation length. The VPD
	// page list is 7 bytes, so the transfer underflows.
	const alloc = 255
	cdb := make([]byte, 16)
	cdb[0] = iscsi.SCSIInquiry
	cdb[1] = 0x01
	cdb[2] = 0x00
	binary.BigEndian.PutUint16(cdb[3:5], alloc)
	if err := iscsi.WritePDU(conn, iscsi.BuildSCSICommand(0x33, 1, 1, 0, cdb, alloc, true, false)); err != nil {
		t.Fatalf("send inquiry: %v", err)
	}

	dataIn, err := iscsi.ReadPDU(conn)
	if err != nil {
		t.Fatalf("read data-in: %v", err)
	}
	if dataIn.Header.Opcode() != iscsi.OpDataIn {
		t.Fatalf("expected Data-In, got %s", iscsi.OpcodeName(dataIn.Header.Opcode()))
	}
	if len(dataIn.Data) >= alloc {
		t.Fatalf("the VPD page list is %d bytes, expected fewer than %d", len(dataIn.Data), alloc)
	}
	if dataIn.Header.Flags()&iscsi.DataInFlagFinal == 0 {
		t.Fatal("F bit not set on the last Data-In")
	}
	if dataIn.Header.Flags()&iscsi.DataInFlagStatus == 0 {
		t.Fatalf("S bit not set, flags=0x%02x", dataIn.Header.Flags())
	}
	if dataIn.Header[3] != iscsi.StatusGood {
		t.Fatalf("status byte = 0x%02x", dataIn.Header[3])
	}
	if dataIn.Header.Flags()&iscsi.DataInFlagUnderflow == 0 {
		t.Fatalf("underflow bit not set, flags=0x%02x", dataIn.Header.Flags())
	}
	if want, got := alloc-len(dataIn.Data), int(dataIn.Header.ResidualCount()); got != want {
		t.Fatalf("residual count = %d, want %d", got, want)
	}
	// One command consumes one StatSN: the Data-In that closes it advances the
	// counter exactly one step past the login response.
	if want, got := loginStatSN+1, dataIn.Header.StatSN(); got != want {
		t.Fatalf("Data-In StatSN = %d, want %d", got, want)
	}
}

// TestServeStopsOnContextCancel covers service shutdown: cancelling the context
// must unblock Serve, otherwise a caller that stops the service through a
// context (a Ctrl+C handler, for instance) hangs on a listener nobody is using.
func TestServeStopsOnContextCancel(t *testing.T) {
	const iqn = "iqn.test:serve-shutdown"
	tgt, cancel, serveErr := startLifecycleTarget(t, iqn)
	defer tgt.Close()

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}
