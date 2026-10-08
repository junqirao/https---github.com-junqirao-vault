package iscsi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

// SCSI operation codes used by the proxy.
const (
	SCSITestUnitReady      uint8 = 0x00
	SCSIRequestSense       uint8 = 0x03
	SCSIInquiry            uint8 = 0x12
	SCSIModeSelect6        uint8 = 0x15
	SCSIModeSense6         uint8 = 0x1a
	SCSIStartStopUnit      uint8 = 0x1b
	SCSIReadCapacity10     uint8 = 0x25
	SCSIRead10             uint8 = 0x28
	SCSIWrite10            uint8 = 0x2a
	SCSISynchronizeCache10 uint8 = 0x35
	SCSIUnmap              uint8 = 0x42
	SCSIModeSelect10       uint8 = 0x55
	SCSIModeSense10        uint8 = 0x5a
	SCSIRead16             uint8 = 0x88
	SCSIWrite16            uint8 = 0x8a
	SCSIServiceActionIn16  uint8 = 0x9e
	SCSIReportLuns         uint8 = 0xa0
)

// Sense keys.
const (
	SenseNoSense     uint8 = 0x0
	SenseRecovered   uint8 = 0x1
	SenseNotReady    uint8 = 0x2
	SenseMediumError uint8 = 0x3
	SenseHardwareErr uint8 = 0x4
	SenseIllegalReq  uint8 = 0x5
	SenseUnitAttn    uint8 = 0x6
	SenseDataProtect uint8 = 0x7
	SenseAborted     uint8 = 0xb
)

// Additional sense codes.
const (
	ASCInvalidCommandOperationCode   uint8 = 0x20
	ASCLogicalBlockAddressOutOfRange uint8 = 0x21
	ASCInvalidFieldInCDB             uint8 = 0x24
	ASCInvalidFieldInParameterList   uint8 = 0x26
	ASCWriteProtected                uint8 = 0x27
	ASCLUNNotSupported               uint8 = 0x25
	ASCParametersChanged             uint8 = 0x2a
	ASCElementNotFound               uint8 = 0x3b
	ASCInvalidMessageError           uint8 = 0x49
	ASCSystemResourceFailure         uint8 = 0x55
)

// FixedSense builds an 18-byte "current error" fixed-format sense buffer
// (SPC-4 4.5.3).
func FixedSense(key, asc, ascq uint8) []byte {
	s := make([]byte, 18)
	s[0] = 0x70
	s[2] = key & 0x0f
	s[7] = 10 // additional sense length
	s[12] = asc
	s[13] = ascq
	return s
}

// IllegalRequestSense is the sense returned for unsupported commands/bad CDBs.
func IllegalRequestSense(asc uint8) []byte { return FixedSense(SenseIllegalReq, asc, 0) }

// NotReadySense is returned when the backend is unavailable.
func NotReadySense() []byte { return FixedSense(SenseNotReady, 0x04, 0x00) }

// SenseFromResult converts a backend sense buffer into a printable summary.
func SenseFromResult(sense []byte) string {
	if len(sense) < 14 {
		return ""
	}
	if sense[0]&0x7f == 0x70 || sense[0]&0x7f == 0x71 {
		return fmt.Sprintf("key=0x%02x asc=0x%02x ascq=0x%02x", sense[2]&0x0f, sense[12], sense[13])
	}
	return fmt.Sprintf("sense[0]=0x%02x", sense[0])
}

// ASCIIField renders a space padded ASCII field of exactly n bytes.
func ASCIIField(s string, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	for i := 0; i < len(s) && i < n; i++ {
		out[i] = s[i]
	}
	return out
}

// StandardInquiry builds a standard INQUIRY response (SPC-4 6.4.2).
func StandardInquiry(deviceType uint8, rmb bool, vendor, product, revision string) []byte {
	buf := make([]byte, 36)
	buf[0] = deviceType & 0x1f
	if rmb {
		buf[1] = 0x80
	}
	buf[2] = 0x06 // SPC-4
	buf[3] = 0x02 // response data format
	buf[4] = 31   // additional length
	copy(buf[8:16], ASCIIField(vendor, 8))
	copy(buf[16:32], ASCIIField(product, 16))
	copy(buf[32:36], ASCIIField(revision, 4))
	return buf
}

// VPDPage wraps a payload into a VPD page structure.
func VPDPage(deviceType, page uint8, payload []byte) []byte {
	buf := make([]byte, 4+len(payload))
	buf[0] = deviceType & 0x1f
	buf[1] = page
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(payload)))
	copy(buf[4:], payload)
	return buf
}

// VPDSupportedPages builds the VPD page 0x00 payload listing supported pages.
func VPDSupportedPages(pages ...uint8) []byte { return pages }

// VPDSerial builds VPD page 0x80 (unit serial number).
func VPDSerial(deviceType uint8, serial string) []byte {
	return VPDPage(deviceType, 0x80, []byte(serial))
}

// VPDDescriptor is a single device identification descriptor (VPD page 0x83).
type VPDDescriptor struct {
	CodeSet    uint8 // 1 = ASCII, 2 = binary
	Designator uint8 // 1 = T10 vendor ID, 2 = EUI-64, 3 = NAA, 8 = SCSI name string
	Payload    []byte
}

// VPDDeviceID builds VPD page 0x83 from descriptors.
func VPDDeviceID(deviceType uint8, descs ...VPDDescriptor) []byte {
	var payload []byte
	for _, d := range descs {
		desc := make([]byte, 4+len(d.Payload))
		desc[0] = d.CodeSet & 0x0f
		desc[1] = (d.Designator & 0x0f) << 4
		desc[3] = byte(len(d.Payload))
		copy(desc[4:], d.Payload)
		payload = append(payload, desc...)
	}
	return VPDPage(deviceType, 0x83, payload)
}

// T10VendorDescriptor builds a T10 vendor identification descriptor payload
// (8 byte space padded vendor followed by the serial).
func T10VendorDescriptor(vendor, serial string) VPDDescriptor {
	payload := append(ASCIIField(vendor, 8), []byte(serial)...)
	return VPDDescriptor{CodeSet: 1, Designator: 1, Payload: payload}
}

// SCSINameDescriptor builds a SCSI name string descriptor payload.
func SCSINameDescriptor(name string) VPDDescriptor {
	return VPDDescriptor{CodeSet: 1, Designator: 8, Payload: []byte(name)}
}

// BuildReportLuns builds a REPORT LUNS response for the given LUNs.
func BuildReportLuns(luns ...uint64) []byte {
	payload := make([]byte, 8*len(luns))
	for i, l := range luns {
		binary.BigEndian.PutUint64(payload[i*8:], l)
	}
	buf := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	copy(buf[8:], payload)
	return buf
}

// BuildReadCapacity10 builds a READ CAPACITY(10) response.
func BuildReadCapacity10(lastLBA uint32, blockLen uint32) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint32(buf[0:4], lastLBA)
	binary.BigEndian.PutUint32(buf[4:8], blockLen)
	return buf
}

// BuildReadCapacity16 builds a READ CAPACITY(16) response (32 bytes).
func BuildReadCapacity16(lastLBA uint64, blockLen uint32, logicalPerPhysical uint32) []byte {
	buf := make([]byte, 32)
	binary.BigEndian.PutUint64(buf[0:8], lastLBA)
	binary.BigEndian.PutUint32(buf[8:12], blockLen)
	if logicalPerPhysical == 0 {
		logicalPerPhysical = 1
	}
	logExp := uint8(0)
	for v := logicalPerPhysical; v > 1; v >>= 1 {
		logExp++
	}
	buf[13] = logExp << 4
	return buf
}

// CDB decoders ---------------------------------------------------------------------

// ReadWriteParams describes a READ/WRITE CDB.
type ReadWriteParams struct {
	LBA       uint64
	Blocks    uint32
	FUA       bool
	BlockSize uint32 // logical block size in bytes, filled in by the caller
	IsWrite   bool
	CDBLen    int
}

// TransferLength returns the byte length implied by the CDB and block size.
func (p ReadWriteParams) TransferLength() int64 { return int64(p.Blocks) * int64(p.BlockSize) }

// ParseReadWrite decodes READ(10/16) and WRITE(10/16) CDBs.
func ParseReadWrite(cdb []byte) (ReadWriteParams, error) {
	var p ReadWriteParams
	if len(cdb) < 10 {
		return p, fmt.Errorf("iscsi: CDB too short (%d)", len(cdb))
	}
	switch cdb[0] {
	case SCSIRead10, SCSIWrite10:
		if len(cdb) < 10 {
			return p, fmt.Errorf("iscsi: READ/WRITE(10) CDB too short")
		}
		p.FUA = cdb[1]&0x08 != 0
		p.LBA = uint64(binary.BigEndian.Uint32(cdb[2:6]))
		p.Blocks = uint32(binary.BigEndian.Uint16(cdb[7:9]))
		p.IsWrite = cdb[0] == SCSIWrite10
		p.CDBLen = 10
	case SCSIRead16, SCSIWrite16:
		if len(cdb) < 16 {
			return p, fmt.Errorf("iscsi: READ/WRITE(16) CDB too short")
		}
		p.FUA = cdb[1]&0x08 != 0
		p.LBA = binary.BigEndian.Uint64(cdb[2:10])
		p.Blocks = binary.BigEndian.Uint32(cdb[10:14])
		p.IsWrite = cdb[0] == SCSIWrite16
		p.CDBLen = 16
	default:
		return p, fmt.Errorf("iscsi: not a READ/WRITE CDB (0x%02x)", cdb[0])
	}
	return p, nil
}

// InquiryParams describes an INQUIRY CDB.
type InquiryParams struct {
	EVPD        bool
	PageCode    uint8
	AllocLength uint16
	CDBLen      int
}

// ParseInquiry decodes an INQUIRY CDB.
func ParseInquiry(cdb []byte) (InquiryParams, error) {
	var p InquiryParams
	if len(cdb) < 6 || cdb[0] != SCSIInquiry {
		return p, fmt.Errorf("iscsi: not an INQUIRY CDB")
	}
	p.EVPD = cdb[1]&0x01 != 0
	p.PageCode = cdb[2]
	p.AllocLength = binary.BigEndian.Uint16(cdb[3:5])
	p.CDBLen = 6
	if p.AllocLength == 0 {
		p.AllocLength = 255
	}
	return p, nil
}

// SynchronizeCacheParams describes a SYNCHRONIZE CACHE CDB.
type SynchronizeCacheParams struct {
	LBA    uint64
	Blocks uint32
}

// ParseSynchronizeCache decodes SYNCHRONIZE CACHE(10/16).
func ParseSynchronizeCache(cdb []byte) (SynchronizeCacheParams, error) {
	var p SynchronizeCacheParams
	switch {
	case len(cdb) >= 10 && cdb[0] == SCSISynchronizeCache10:
		p.LBA = uint64(binary.BigEndian.Uint32(cdb[2:6]))
		p.Blocks = uint32(binary.BigEndian.Uint16(cdb[7:9]))
	case len(cdb) >= 16 && cdb[0] == 0x91: // SYNCHRONIZE CACHE(16)
		p.LBA = binary.BigEndian.Uint64(cdb[2:10])
		p.Blocks = binary.BigEndian.Uint32(cdb[10:14])
	default:
		return p, fmt.Errorf("iscsi: not a SYNCHRONIZE CACHE CDB")
	}
	return p, nil
}

// Text negotiation ------------------------------------------------------------------

// ParseText decodes an iSCSI text blob (NUL separated key=value pairs).
func ParseText(data []byte) map[string]string {
	out := make(map[string]string)
	for _, field := range bytes.Split(data, []byte{0}) {
		if len(field) == 0 {
			continue
		}
		s := string(field)
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		out[key] = strings.TrimSpace(s[eq+1:])
	}
	return out
}

// EncodeText renders key=value pairs into an iSCSI text blob.
func EncodeText(pairs ...[2]string) []byte {
	var buf bytes.Buffer
	for _, kv := range pairs {
		buf.WriteString(kv[0])
		buf.WriteByte('=')
		buf.WriteString(kv[1])
		buf.WriteByte(0)
	}
	return buf.Bytes()
}
