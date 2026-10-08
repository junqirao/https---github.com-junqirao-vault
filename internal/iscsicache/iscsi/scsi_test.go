package iscsi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestFixedSenseLayout(t *testing.T) {
	s := FixedSense(SenseIllegalReq, ASCInvalidFieldInCDB, 0x02)
	if len(s) != 18 {
		t.Fatalf("sense length = %d, want 18", len(s))
	}
	if s[0] != 0x70 {
		t.Fatalf("response code = 0x%02x, want 0x70", s[0])
	}
	if s[2]&0x0f != SenseIllegalReq {
		t.Fatalf("sense key = 0x%02x", s[2]&0x0f)
	}
	if s[7] != 10 {
		t.Fatalf("additional sense length = %d, want 10", s[7])
	}
	if s[12] != ASCInvalidFieldInCDB || s[13] != 0x02 {
		t.Fatal("asc/ascq not encoded")
	}
	if got := SenseFromResult(s); got == "" {
		t.Fatal("SenseFromResult returned an empty summary for a valid sense buffer")
	}
}

func TestParseReadWrite10(t *testing.T) {
	cdb := make([]byte, 10)
	cdb[0] = SCSIRead10
	cdb[1] = 0x08 // FUA
	binary.BigEndian.PutUint32(cdb[2:6], 0x1234)
	binary.BigEndian.PutUint16(cdb[7:9], 128)

	p, err := ParseReadWrite(cdb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.LBA != 0x1234 || p.Blocks != 128 || !p.FUA || p.IsWrite || p.CDBLen != 10 {
		t.Fatalf("decoded %+v", p)
	}
	p.BlockSize = 512
	if got := p.TransferLength(); got != 128*512 {
		t.Fatalf("transfer length = %d", got)
	}
}

func TestParseReadWrite16(t *testing.T) {
	cdb := make([]byte, 16)
	cdb[0] = SCSIWrite16
	binary.BigEndian.PutUint64(cdb[2:10], 0x1_0000_0000)
	binary.BigEndian.PutUint32(cdb[10:14], 7)

	p, err := ParseReadWrite(cdb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.LBA != 0x1_0000_0000 || p.Blocks != 7 || !p.IsWrite {
		t.Fatalf("decoded %+v", p)
	}
	if _, err := ParseReadWrite(make([]byte, 6)); err == nil {
		t.Fatal("expected a short CDB to be rejected")
	}
	if _, err := ParseReadWrite([]byte{SCSIInquiry}); err == nil {
		t.Fatal("expected a non READ/WRITE CDB to be rejected")
	}
}

func TestParseInquiry(t *testing.T) {
	cdb := []byte{SCSIInquiry, 0x01, 0x83, 0x00, 0xff, 0x00}
	p, err := ParseInquiry(cdb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !p.EVPD || p.PageCode != 0x83 || p.AllocLength != 255 {
		t.Fatalf("decoded %+v", p)
	}
	// A zero allocation length means "as much as available".
	p, _ = ParseInquiry([]byte{SCSIInquiry, 0x00, 0x00, 0x00, 0x00, 0x00})
	if p.AllocLength != 255 {
		t.Fatalf("allocation length = %d, want 255", p.AllocLength)
	}
}

func TestStandardInquiryAndVPD(t *testing.T) {
	inq := StandardInquiry(0x00, false, "VAULT", "iSCSI Read Cache", "0001")
	if len(inq) != 36 {
		t.Fatalf("inquiry length = %d", len(inq))
	}
	if inq[0] != 0x00 || inq[4] != 31 {
		t.Fatal("inquiry header fields wrong")
	}
	if !bytes.Equal(inq[8:16], ASCIIField("VAULT", 8)) {
		t.Fatal("vendor field wrong")
	}

	vpd := VPDSerial(0x00, "SN12345")
	if vpd[1] != 0x80 || int(binary.BigEndian.Uint16(vpd[2:4])) != 7 {
		t.Fatal("VPD 0x80 header wrong")
	}
	if string(vpd[4:]) != "SN12345" {
		t.Fatal("VPD 0x80 payload wrong")
	}

	devID := VPDDeviceID(0x00, T10VendorDescriptor("VAULT", "SN12345"), SCSINameDescriptor("iqn.test"))
	if devID[1] != 0x83 {
		t.Fatal("VPD 0x83 page code wrong")
	}
	// Descriptor payloads: 8-byte padded vendor + 7-byte serial, and the
	// 8-byte IQN. Each descriptor carries a 4-byte header.
	d0 := 4 + 8 + len("SN12345")
	d1 := 4 + len("iqn.test")
	want := 4 + d0 + d1
	if got := int(binary.BigEndian.Uint16(devID[2:4])) + 4; got != want {
		t.Fatalf("VPD 0x83 payload length = %d, want %d", got, want)
	}
}

func TestBuildReportLunsAndReadCapacity(t *testing.T) {
	luns := BuildReportLuns(0)
	if int(binary.BigEndian.Uint32(luns[0:4])) != 8 {
		t.Fatalf("report luns list length = %d", binary.BigEndian.Uint32(luns[0:4]))
	}
	if binary.BigEndian.Uint64(luns[8:16]) != 0 {
		t.Fatal("lun 0 not reported")
	}

	rc10 := BuildReadCapacity10(0xffff, 512)
	if binary.BigEndian.Uint32(rc10[0:4]) != 0xffff || binary.BigEndian.Uint32(rc10[4:8]) != 512 {
		t.Fatal("read capacity 10 wrong")
	}

	rc16 := BuildReadCapacity16(0x1_0000_0000, 4096, 1)
	if len(rc16) != 32 {
		t.Fatalf("read capacity 16 length = %d", len(rc16))
	}
	if binary.BigEndian.Uint64(rc16[0:8]) != 0x1_0000_0000 {
		t.Fatal("last LBA wrong")
	}
	if binary.BigEndian.Uint32(rc16[8:12]) != 4096 {
		t.Fatal("block length wrong")
	}
	if rc16[13]>>4 != 0 {
		t.Fatalf("logical per physical exponent = %d, want 0", rc16[13]>>4)
	}
}

func TestTextEncodeParseRoundTrip(t *testing.T) {
	raw := EncodeText([2]string{"SendTargets", "All"}, [2]string{"TargetName", "iqn.test"})
	if !bytes.HasSuffix(raw, []byte{0}) {
		t.Fatal("text blob must be NUL terminated")
	}
	got := ParseText(raw)
	if got["sendtargets"] != "All" {
		t.Fatalf("sendtargets = %q", got["sendtargets"])
	}
	if got["targetname"] != "iqn.test" {
		t.Fatalf("targetname = %q", got["targetname"])
	}
	if v := ParseText([]byte{}); len(v) != 0 {
		t.Fatalf("empty text parsed to %v", v)
	}

	// The PDU layer is responsible for the 4-byte data segment padding.
	p := NewPDU(OpTextReq, raw)
	p.Header.SetFinal(true)
	encoded := p.Bytes()
	if len(encoded)%4 != 0 || p.Header.DataSegmentLength()%4 != 0 {
		t.Fatalf("pdu not padded to 4 bytes: len=%d dsl=%d", len(encoded), p.Header.DataSegmentLength())
	}
	decoded, err := ReadPDU(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if decoded.Header.DataSegmentLength() != len(raw) {
		t.Fatalf("data segment length = %d, want %d", decoded.Header.DataSegmentLength(), len(raw))
	}
}

func TestParseSynchronizeCache(t *testing.T) {
	cdb := make([]byte, 10)
	cdb[0] = SCSISynchronizeCache10
	binary.BigEndian.PutUint32(cdb[2:6], 64)
	binary.BigEndian.PutUint16(cdb[7:9], 8)
	p, err := ParseSynchronizeCache(cdb)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.LBA != 64 || p.Blocks != 8 {
		t.Fatalf("decoded %+v", p)
	}
}
