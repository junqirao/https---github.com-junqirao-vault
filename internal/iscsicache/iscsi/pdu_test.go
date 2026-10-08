package iscsi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestBHSAccessorsRoundTrip guards the field offsets of the basic header
// segment, which the whole proxy depends on.
func TestBHSAccessorsRoundTrip(t *testing.T) {
	var h BHS
	h.SetOpcode(OpSCSICommand)
	if got := h.Opcode(); got != OpSCSICommand {
		t.Fatalf("opcode = 0x%02x, want 0x%02x", got, OpSCSICommand)
	}
	if h.Immediate() {
		t.Fatal("immediate should default to false")
	}
	h.SetImmediate(true)
	if !h.Immediate() {
		t.Fatal("immediate not set")
	}
	// The F bit lives in byte 1 and must not collide with Immediate in byte 0.
	if h.Final() {
		t.Fatal("final should default to false")
	}
	h.SetFinal(true)
	if !h.Final() || !h.Immediate() || h.Opcode() != OpSCSICommand {
		t.Fatal("setting the final bit clobbered another field")
	}

	h.SetFlags(0xff)
	if h.Flags() != 0xff {
		t.Fatal("flags round trip failed")
	}
	if h.AHSLength() != 0 {
		t.Fatal("AHS length should default to zero")
	}
	h.SetDataSegmentLength(0x123456)
	if got := h.DataSegmentLength(); got != 0x123456 {
		t.Fatalf("data segment length = %#x", got)
	}
	h.SetLUN(0x0102030405060708)
	if got := h.LUN(); got != 0x0102030405060708 {
		t.Fatalf("lun = %#x", got)
	}
	h.SetITT(0xdeadbeef)
	if got := h.ITT(); got != 0xdeadbeef {
		t.Fatalf("itt = %#x", got)
	}
	h.SetTTT(ReservedTag)
	if got := h.TTT(); got != ReservedTag {
		t.Fatalf("ttt = %#x", got)
	}
	h.SetCmdSN(7)
	h.SetExpStatSN(9)
	if h.CmdSN() != 7 || h.ExpStatSN() != 9 {
		t.Fatal("command sequence numbers overlap")
	}
	h.SetStatSN(11)
	h.SetExpCmdSN(13)
	h.SetMaxCmdSN(15)
	if h.StatSN() != 11 || h.ExpCmdSN() != 13 || h.MaxCmdSN() != 15 {
		t.Fatal("response sequence numbers overlap")
	}
	h.SetBufOffset(4096)
	if h.BufOffset() != 4096 {
		t.Fatal("buffer offset round trip failed")
	}
}

// TestLoginFields checks the login specific overlays, which reuse bytes 8..15
// and 36..37.
func TestLoginFields(t *testing.T) {
	var h BHS
	h[1] = LoginFlagTransit | (StageOperational << LoginFlagCSGShift) | StageFullFeature
	if !h.LoginTransit() {
		t.Fatal("transit bit not decoded")
	}
	if got := h.LoginCSG(); got != StageOperational {
		t.Fatalf("csg = %d", got)
	}
	if got := h.LoginNSG(); got != StageFullFeature {
		t.Fatalf("nsg = %d", got)
	}
	h.SetLoginTSIH(0x1234)
	if h.LoginTSIH() != 0x1234 {
		t.Fatal("tsih round trip failed")
	}
	h[36] = 0x02
	h[37] = 0x03
	if h.LoginStatusClass() != 0x02 || h.LoginStatusDetail() != 0x03 {
		t.Fatal("login status not decoded")
	}
}

// TestPDURoundTrip checks framing including the 4-byte data padding.
func TestPDURoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 4, 7, 8, 48, 4096} {
		data := bytes.Repeat([]byte{0xab}, n)
		p := NewPDU(OpDataIn, data)
		p.Header.SetFlags(0x80)
		p.Header.SetITT(42)

		var buf bytes.Buffer
		if err := WritePDU(&buf, p); err != nil {
			t.Fatalf("write pdu: %v", err)
		}
		want := BHSLen + n + pad4(n)
		if buf.Len() != want {
			t.Fatalf("data %d: encoded %d bytes, want %d", n, buf.Len(), want)
		}
		got, err := ReadPDU(&buf)
		if err != nil {
			t.Fatalf("read pdu: %v", err)
		}
		if got.Header.Opcode() != OpDataIn || got.Header.ITT() != 42 {
			t.Fatalf("header not preserved: %s", got)
		}
		if !bytes.Equal(got.Data, data) {
			t.Fatalf("data %d not preserved", n)
		}
		if buf.Len() != 0 {
			t.Fatalf("data %d: %d trailing bytes", n, buf.Len())
		}
	}
}

// TestBuildSCSICommandDirection checks the read/write flags land in byte 1.
func TestBuildSCSICommandDirection(t *testing.T) {
	cdb := make([]byte, 10)
	cdb[0] = SCSIRead10
	read := BuildSCSICommand(1, 2, 3, 0, cdb, 4096, true, false)
	if read.Header.Flags()&CmdFlagRead == 0 {
		t.Fatal("read flag not set")
	}
	if read.Header.Flags()&CmdFlagWrite != 0 {
		t.Fatal("write flag set on a read")
	}
	if got := read.Header.ExpectedDataTransferLength(); got != 4096 {
		t.Fatalf("expected data transfer length = %d", got)
	}

	write := BuildSCSICommand(1, 2, 3, 0, cdb, 4096, false, true)
	if write.Header.Flags()&CmdFlagWrite == 0 || write.Header.Flags()&CmdFlagRead != 0 {
		t.Fatal("write direction flags wrong")
	}
	if !bytes.Equal(write.Header.CDB()[:len(cdb)], cdb) {
		t.Fatal("cdb not copied into bytes 32..47")
	}
}

// TestBuildDataInStatusBit checks the S bit, status byte and residual placement.
func TestBuildDataInStatusBit(t *testing.T) {
	p := BuildDataIn(1, ReservedTag, 5, 6, 7, 0, 0, []byte("hello"), true, false, 0, 0, false, false)
	if p.Header.Flags()&DataInFlagFinal == 0 {
		t.Fatal("final flag missing")
	}
	if p.Header.Flags()&DataInFlagStatus != 0 {
		t.Fatal("status bit set when withStatus is false")
	}
	p = BuildDataIn(1, ReservedTag, 5, 6, 7, 0, 0, []byte("x"), true, true, StatusCheckCondition, 512, false, true)
	if p.Header.Flags()&DataInFlagStatus == 0 {
		t.Fatal("status bit missing")
	}
	if p.Header[3] != StatusCheckCondition {
		t.Fatalf("status byte = 0x%02x", p.Header[3])
	}
	if p.Header.Flags()&DataInFlagUnderflow == 0 {
		t.Fatal("underflow flag missing")
	}
	if got := p.Header.ResidualCount(); got != 512 {
		t.Fatalf("residual = %d", got)
	}
}

// TestBuildSCSIResponseResidual checks residual count and overflow flags.
func TestBuildSCSIResponseResidual(t *testing.T) {
	sense := FixedSense(SenseIllegalReq, ASCInvalidFieldInCDB, 0)
	p := BuildSCSIResponse(1, ReservedTag, 2, 3, 4, 1, StatusCheckCondition, 512, sense, true, false)
	if p.Header[2] != ResponseCompleted {
		t.Fatalf("response code = 0x%02x", p.Header[2])
	}
	if p.Header[3] != StatusCheckCondition {
		t.Fatalf("status = 0x%02x", p.Header[3])
	}
	if p.Header.ResidualCount() != 512 {
		t.Fatal("residual count not encoded")
	}
	if p.Header.Flags()&RespFlagOverflow == 0 {
		t.Fatal("overflow flag missing")
	}
	if p.Header.Flags()&RespFlagUnderflow != 0 {
		t.Fatal("underflow flag set unexpectedly")
	}
	// The SCSI Response data segment is a 2-byte sense length followed by the
	// sense bytes.
	if len(p.Data) != 2+len(sense) {
		t.Fatalf("response data segment length = %d, want %d", len(p.Data), 2+len(sense))
	}
	if binary.BigEndian.Uint16(p.Data[0:2]) != uint16(len(sense)) {
		t.Fatal("sense length prefix wrong")
	}
	if !bytes.Equal(p.Data[2:], sense) {
		t.Fatal("sense data missing from the response")
	}
}
