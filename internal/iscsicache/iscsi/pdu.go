// Package iscsi implements the iSCSI wire protocol primitives used by the
// vault iSCSI read-cache proxy: PDU framing, BHS field access, SCSI CDB
// encoding/decoding, sense data and VPD pages.
//
// The package is pure Go and platform independent. It is shared by the
// frontend (iSCSI target) and the backend (iSCSI initiator).
//
// Field offsets follow RFC 7143 section 11.2.1: the Basic Header Segment is
// 48 bytes, TotalAHSLength lives at byte 4 and DataSegmentLength is a 24-bit
// big-endian field at bytes 5..7.
package iscsi

import (
	"encoding/binary"
	"fmt"
	"io"
)

// BHSLen is the fixed size of the iSCSI Basic Header Segment.
const BHSLen = 48

// ReservedTag is the reserved Initiator/Target Transfer Tag value.
const ReservedTag uint32 = 0xffffffff

// Opcodes (RFC 7143 section 11.2.1.2).
const (
	OpNOPOut       uint8 = 0x00
	OpSCSICommand  uint8 = 0x01
	OpTaskMgmtReq  uint8 = 0x02
	OpLoginReq     uint8 = 0x03
	OpTextReq      uint8 = 0x04
	OpDataOut      uint8 = 0x05
	OpLogoutReq    uint8 = 0x06
	OpSNACKReq     uint8 = 0x10
	OpNOPIn        uint8 = 0x20
	OpSCSIResponse uint8 = 0x21
	OpTaskMgmtResp uint8 = 0x22
	OpLoginResp    uint8 = 0x23
	OpTextResp     uint8 = 0x24
	OpDataIn       uint8 = 0x25
	OpLogoutResp   uint8 = 0x26
	OpR2T          uint8 = 0x31
	OpAsyncMsg     uint8 = 0x32
	OpReject       uint8 = 0x3f
)

// SCSI status codes.
const (
	StatusGood                uint8 = 0x00
	StatusCheckCondition      uint8 = 0x02
	StatusConditionMet        uint8 = 0x04
	StatusBusy                uint8 = 0x08
	StatusReservationConflict uint8 = 0x18
	StatusTaskSetFull         uint8 = 0x28
	StatusACA_ACTIVE          uint8 = 0x24
	StatusTaskAborted         uint8 = 0x40
)

// SCSI Response codes (BHS byte 2 of a SCSI Response PDU).
const (
	ResponseCompleted  uint8 = 0x00
	ResponseTargetFail uint8 = 0x01
	ResponseInvalidPDU uint8 = 0x02
)

// Login stage codes.
const (
	StageSecurity    uint8 = 0
	StageOperational uint8 = 1
	StageFullFeature uint8 = 3
)

// Byte-1 flag masks. Note the RFC numbers bits from the most significant bit
// of each byte, so "bit 0" is 0x80.
const (
	// SCSI Command.
	CmdFlagFinal   uint8 = 0x80 // F
	CmdFlagRead    uint8 = 0x40 // R
	CmdFlagWrite   uint8 = 0x20 // W
	CmdFlagAttrMsk uint8 = 0x07 // Task attribute

	// SCSI Data-In.
	DataInFlagFinal     uint8 = 0x80 // F
	DataInFlagAck       uint8 = 0x40 // A
	DataInFlagOverflow  uint8 = 0x04 // O
	DataInFlagUnderflow uint8 = 0x02 // U
	DataInFlagStatus    uint8 = 0x01 // S

	// SCSI Response / Data-In residual bits.
	RespFlagOverflow  uint8 = 0x04 // O
	RespFlagUnderflow uint8 = 0x02 // U

	// SCSI Data-Out / Text / R2T / NOP / Task Management: mandatory set bit.
	FlagAlwaysSet uint8 = 0x80

	// Text Request/Response.
	TextFlagFinal    uint8 = 0x80 // F
	TextFlagContinue uint8 = 0x40 // C

	// Login Request/Response.
	LoginFlagTransit  uint8 = 0x80 // T
	LoginFlagContinue uint8 = 0x40 // C
	LoginFlagCSGMask  uint8 = 0x0c // CSG occupies bits 4..5
	LoginFlagCSGShift       = 2
	LoginFlagNSGMask  uint8 = 0x03 // NSG occupies bits 6..7
)

// BHS is the raw 48-byte Basic Header Segment.
type BHS [BHSLen]byte

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
func be24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

func put16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func put32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }
func put64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
func put24(b []byte, v uint32) { b[0] = byte(v >> 16); b[1] = byte(v >> 8); b[2] = byte(v) }

// Common BHS accessors ------------------------------------------------------------------

// Opcode returns the 6-bit opcode.
func (h *BHS) Opcode() uint8 { return h[0] & 0x3f }

// SetOpcode writes the opcode, preserving the I and F bits.
func (h *BHS) SetOpcode(op uint8) { h[0] = (h[0] & 0xc0) | (op & 0x3f) }

// Immediate reports the I bit.
func (h *BHS) Immediate() bool { return h[0]&0x40 != 0 }

// SetImmediate sets the I bit.
func (h *BHS) SetImmediate(v bool) {
	if v {
		h[0] |= 0x40
	} else {
		h[0] &^= 0x40
	}
}

// Final reports the F bit of byte 0.
func (h *BHS) Final() bool { return h[0]&0x80 != 0 }

// SetFinal sets the F bit of byte 0.
func (h *BHS) SetFinal(v bool) {
	if v {
		h[0] |= 0x80
	} else {
		h[0] &^= 0x80
	}
}

// Flags returns byte 1 (opcode specific flags).
func (h *BHS) Flags() uint8 { return h[1] }

// SetFlags writes byte 1.
func (h *BHS) SetFlags(v uint8) { h[1] = v }

// AHSLength returns TotalAHSLength in 4-byte words.
func (h *BHS) AHSLength() int { return int(h[4]) }

// DataSegmentLength returns the 24-bit data segment length.
func (h *BHS) DataSegmentLength() int { return int(be24(h[5:8])) }

// SetDataSegmentLength writes the 24-bit data segment length.
func (h *BHS) SetDataSegmentLength(n int) { put24(h[5:8], uint32(n)) }

// LUN returns the 64-bit LUN field.
func (h *BHS) LUN() uint64 { return be64(h[8:16]) }

// SetLUN writes the 64-bit LUN field.
func (h *BHS) SetLUN(v uint64) { put64(h[8:16], v) }

// ITT returns the Initiator Task Tag.
func (h *BHS) ITT() uint32 { return be32(h[16:20]) }

// SetITT writes the Initiator Task Tag.
func (h *BHS) SetITT(v uint32) { put32(h[16:20], v) }

// TTT returns the Target Transfer Tag.
func (h *BHS) TTT() uint32 { return be32(h[20:24]) }

// SetTTT writes the Target Transfer Tag.
func (h *BHS) SetTTT(v uint32) { put32(h[20:24], v) }

// u32 reads a big-endian uint32 at the given offset.
func (h *BHS) u32(off int) uint32 { return be32(h[off : off+4]) }

// setU32 writes a big-endian uint32 at the given offset.
func (h *BHS) setU32(off int, v uint32) { put32(h[off:off+4], v) }

// CmdSN returns the command sequence number (byte 24).
func (h *BHS) CmdSN() uint32 { return h.u32(24) }

// SetCmdSN writes the command sequence number.
func (h *BHS) SetCmdSN(v uint32) { h.setU32(24, v) }

// StatSN returns the status sequence number (byte 24 for responses that use it).
func (h *BHS) StatSN() uint32 { return h.u32(24) }

// SetStatSN writes the status sequence number.
func (h *BHS) SetStatSN(v uint32) { h.setU32(24, v) }

// ExpCmdSN returns the next expected command sequence number.
func (h *BHS) ExpCmdSN() uint32 { return h.u32(28) }

// SetExpCmdSN writes ExpCmdSN.
func (h *BHS) SetExpCmdSN(v uint32) { h.setU32(28, v) }

// MaxCmdSN returns the maximum command sequence number accepted.
func (h *BHS) MaxCmdSN() uint32 { return h.u32(32) }

// SetMaxCmdSN writes MaxCmdSN.
func (h *BHS) SetMaxCmdSN(v uint32) { h.setU32(32, v) }

// ExpStatSN returns the next expected status sequence number.
func (h *BHS) ExpStatSN() uint32 {
	// 28 for request PDUs, 36 for SCSI Response / 28 for login response.
	return h.u32(28)
}

// SetExpStatSN writes ExpStatSN (request PDUs only).
func (h *BHS) SetExpStatSN(v uint32) { h.setU32(28, v) }

// CDB returns the 16-byte CDB embedded in a SCSI Command PDU.
func (h *BHS) CDB() []byte { return h[32:48] }

// DataSN returns the DataSN field (Data-In / Data-Out PDUs, byte 36).
func (h *BHS) DataSN() uint32 { return h.u32(36) }

// SetDataSN writes the DataSN field.
func (h *BHS) SetDataSN(v uint32) { h.setU32(36, v) }

// R2TSN returns the R2TSN field (R2T PDU, byte 36).
func (h *BHS) R2TSN() uint32 { return h.u32(36) }

// BufOffset returns the Buffer Offset field (byte 40).
func (h *BHS) BufOffset() uint32 { return h.u32(40) }

// SetBufOffset writes the Buffer Offset field.
func (h *BHS) SetBufOffset(v uint32) { h.setU32(40, v) }

// ResidualCount returns the Residual Count field (byte 44).
func (h *BHS) ResidualCount() uint32 { return h.u32(44) }

// DesiredDataTransferLength returns the R2T Desired Data Transfer Length (byte 44).
func (h *BHS) DesiredDataTransferLength() uint32 { return h.u32(44) }

// SetDesiredDataTransferLength writes the R2T desired length.
func (h *BHS) SetDesiredDataTransferLength(v uint32) { h.setU32(44, v) }

// ExpectedDataTransferLength returns the SCSI Command expected length (byte 20).
func (h *BHS) ExpectedDataTransferLength() uint32 { return h.u32(20) }

// SetExpectedDataTransferLength writes the SCSI Command expected length.
func (h *BHS) SetExpectedDataTransferLength(v uint32) { h.setU32(20, v) }

// LoginCSG returns the current stage code of a Login PDU.
func (h *BHS) LoginCSG() uint8 { return (h[1] & LoginFlagCSGMask) >> LoginFlagCSGShift }

// LoginNSG returns the next stage code of a Login PDU.
func (h *BHS) LoginNSG() uint8 { return h[1] & LoginFlagNSGMask }

// LoginTransit reports the T bit of a Login PDU.
func (h *BHS) LoginTransit() bool { return h[1]&LoginFlagTransit != 0 }

// LoginContinue reports the C bit of a Login PDU.
func (h *BHS) LoginContinue() bool { return h[1]&LoginFlagContinue != 0 }

// LoginStatusClass returns byte 36 of a Login Response.
func (h *BHS) LoginStatusClass() uint8 { return h[36] }

// LoginStatusDetail returns byte 37 of a Login Response.
func (h *BHS) LoginStatusDetail() uint8 { return h[37] }

// LoginTSIH returns the TSIH field of a Login PDU (bytes 14..15).
func (h *BHS) LoginTSIH() uint16 { return be16(h[14:16]) }

// SetLoginTSIH writes the TSIH field of a Login PDU.
func (h *BHS) SetLoginTSIH(v uint16) { put16(h[14:16], v) }

// PDU is a complete iSCSI PDU (BHS plus data segment). AHS is not used.
type PDU struct {
	Header BHS
	Data   []byte
}

// NewPDU builds a PDU with the given opcode and data segment.
func NewPDU(op uint8, data []byte) *PDU {
	p := &PDU{Data: data}
	p.Header.SetOpcode(op)
	p.Header.SetDataSegmentLength(len(data))
	return p
}

// Bytes serialises the PDU including 4-byte padding.
func (p *PDU) Bytes() []byte {
	dsl := len(p.Data)
	p.Header.SetDataSegmentLength(dsl)
	out := make([]byte, BHSLen+dsl+pad4(dsl))
	copy(out, p.Header[:])
	copy(out[BHSLen:], p.Data)
	return out
}

func pad4(n int) int { return (4 - n%4) % 4 }

// WritePDU serialises and writes a PDU.
func WritePDU(w io.Writer, p *PDU) error {
	buf := p.Bytes()
	_, err := w.Write(buf)
	return err
}

// ReadPDU reads a single PDU (BHS + AHS + data segment + padding).
func ReadPDU(r io.Reader) (*PDU, error) {
	var h BHS
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	ahsLen := h.AHSLength() * 4
	if ahsLen > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(ahsLen)); err != nil {
			return nil, fmt.Errorf("iscsi: read AHS: %w", err)
		}
	}
	dsl := h.DataSegmentLength()
	var data []byte
	if dsl > 0 {
		data = make([]byte, dsl)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("iscsi: read data segment: %w", err)
		}
	}
	if p := pad4(dsl); p > 0 {
		var pad [3]byte
		if _, err := io.ReadFull(r, pad[:p]); err != nil {
			return nil, fmt.Errorf("iscsi: read padding: %w", err)
		}
	}
	return &PDU{Header: h, Data: data}, nil
}

// String renders the opcode name for logging.
func (p *PDU) String() string {
	return fmt.Sprintf("PDU{op=0x%02x flags=0x%02x len=%d itt=0x%08x}",
		p.Header.Opcode(), p.Header.Flags(), len(p.Data), p.Header.ITT())
}

// OpcodeName returns a human readable opcode name.
func OpcodeName(op uint8) string {
	switch op {
	case OpNOPOut:
		return "NOP-Out"
	case OpSCSICommand:
		return "SCSICommand"
	case OpTaskMgmtReq:
		return "TaskMgmtReq"
	case OpLoginReq:
		return "LoginReq"
	case OpTextReq:
		return "TextReq"
	case OpDataOut:
		return "DataOut"
	case OpLogoutReq:
		return "LogoutReq"
	case OpSNACKReq:
		return "SNACKReq"
	case OpNOPIn:
		return "NOP-In"
	case OpSCSIResponse:
		return "SCSIResponse"
	case OpTaskMgmtResp:
		return "TaskMgmtResp"
	case OpLoginResp:
		return "LoginResp"
	case OpTextResp:
		return "TextResp"
	case OpDataIn:
		return "DataIn"
	case OpLogoutResp:
		return "LogoutResp"
	case OpR2T:
		return "R2T"
	case OpAsyncMsg:
		return "AsyncMsg"
	case OpReject:
		return "Reject"
	default:
		return fmt.Sprintf("opcode-0x%02x", op)
	}
}

// BuildSCSICommand builds a SCSI Command PDU for the given CDB.
func BuildSCSICommand(itt, cmdSN, expStatSN uint32, lun uint64, cdb []byte, expDataLen int, read, write bool) *PDU {
	p := NewPDU(OpSCSICommand, nil)
	var flags uint8
	if read {
		flags |= CmdFlagRead
	}
	if write {
		flags |= CmdFlagWrite
	}
	// F=1: no unsolicited data-out PDUs follow this PDU (we answer R2T instead).
	flags |= CmdFlagFinal
	p.Header.SetFlags(flags | 1 /* Simple task attribute */)
	p.Header.SetLUN(lun)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ReservedTag)
	p.Header.SetDataSegmentLength(0)
	p.Header.setU32(20, uint32(expDataLen))
	p.Header.SetCmdSN(cmdSN)
	p.Header.SetExpStatSN(expStatSN)
	copy(p.Header.CDB(), cdb)
	return p
}

// BuildSCSIResponse builds a SCSI Response PDU.
func BuildSCSIResponse(itt, ttt, statSN, expCmdSN, maxCmdSN, expDataSN uint32, status uint8, residual uint32, sense []byte, overflow, underflow bool) *PDU {
	p := NewPDU(OpSCSIResponse, nil)
	flags := FlagAlwaysSet
	if overflow {
		flags |= RespFlagOverflow
	}
	if underflow {
		flags |= RespFlagUnderflow
	}
	p.Header.SetFlags(flags)
	p.Header[2] = ResponseCompleted
	p.Header[3] = status
	p.Header.SetLUN(0)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	p.Header.setU32(36, expDataSN)
	p.Header.setU32(44, residual)
	if len(sense) > 0 {
		seg := make([]byte, 2+len(sense))
		put16(seg, uint16(len(sense)))
		copy(seg[2:], sense)
		p.Data = seg
		p.Header.SetDataSegmentLength(len(seg))
	}
	return p
}

// DataIn PDU field offsets.
const (
	DataInOffStatSN    = 24
	DataInOffExpCmdSN  = 28
	DataInOffMaxCmdSN  = 32
	DataInOffDataSN    = 36
	DataInOffBufOffset = 40
	DataInOffResidual  = 44
)

// BuildDataIn builds a SCSI Data-In PDU. When last is true the F bit is set and
// the PDU ends the data sequence.
//
// withStatus, which is only meaningful on the last PDU, sets the S bit so the
// command's Status travels in this PDU together with the Residual Count and the
// overflow/underflow flags. That is how the Microsoft target ends every command
// that returns data; closing such a command with a separate SCSI Response
// instead makes the Windows initiator reset the connection.
func BuildDataIn(itt, ttt, statSN, expCmdSN, maxCmdSN, dataSN, bufOffset uint32, data []byte, last, withStatus bool, status uint8, residual uint32, overflow, underflow bool) *PDU {
	p := NewPDU(OpDataIn, data)
	var flags uint8
	if last {
		flags |= DataInFlagFinal
	}
	if last && withStatus {
		flags |= DataInFlagStatus
		p.Header[3] = status
		p.Header.setU32(DataInOffResidual, residual)
		if overflow {
			flags |= DataInFlagOverflow
		}
		if underflow {
			flags |= DataInFlagUnderflow
		}
	}
	p.Header.SetFlags(flags)
	p.Header.SetLUN(0)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.setU32(DataInOffStatSN, statSN)
	p.Header.setU32(DataInOffExpCmdSN, expCmdSN)
	p.Header.setU32(DataInOffMaxCmdSN, maxCmdSN)
	p.Header.setU32(DataInOffDataSN, dataSN)
	p.Header.setU32(DataInOffBufOffset, bufOffset)
	return p
}

// BuildR2T builds a Ready-To-Transfer PDU.
func BuildR2T(itt, ttt, statSN, expCmdSN, maxCmdSN, r2tSN, bufOffset, desiredLen uint32) *PDU {
	p := NewPDU(OpR2T, nil)
	p.Header.SetFlags(FlagAlwaysSet)
	p.Header.SetLUN(0)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	p.Header.setU32(36, r2tSN)
	p.Header.setU32(40, bufOffset)
	p.Header.setU32(44, desiredLen)
	return p
}

// BuildNOPIn builds a NOP-In PDU (used to echo a NOP-Out ping).
func BuildNOPIn(itt, ttt, statSN, expCmdSN, maxCmdSN uint32, pingData []byte) *PDU {
	p := NewPDU(OpNOPIn, pingData)
	p.Header.SetFlags(FlagAlwaysSet)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	return p
}

// BuildNOPOut builds a NOP-Out PDU.
func BuildNOPOut(itt, ttt, cmdSN, expStatSN uint32, pingData []byte) *PDU {
	p := NewPDU(OpNOPOut, pingData)
	p.Header.SetFlags(FlagAlwaysSet)
	p.Header.SetImmediate(true)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.SetCmdSN(cmdSN)
	p.Header.SetExpStatSN(expStatSN)
	return p
}

// BuildTextResponse builds a Text Response PDU.
func BuildTextResponse(itt, ttt, statSN, expCmdSN, maxCmdSN uint32, text []byte, final bool) *PDU {
	p := NewPDU(OpTextResp, text)
	var flags uint8
	if final {
		flags |= TextFlagFinal
	}
	p.Header.SetFlags(flags)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ttt)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	return p
}

// BuildTaskMgmtResponse builds a Task Management Function Response PDU.
func BuildTaskMgmtResponse(itt, statSN, expCmdSN, maxCmdSN uint32, response uint8) *PDU {
	p := NewPDU(OpTaskMgmtResp, nil)
	p.Header.SetFlags(FlagAlwaysSet)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ReservedTag)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	p.Header[2] = response
	return p
}

// BuildReject builds a Reject PDU.
func BuildReject(itt, statSN, expCmdSN, maxCmdSN uint32, reason uint8) *PDU {
	p := NewPDU(OpReject, nil)
	p.Header.SetFlags(FlagAlwaysSet)
	p.Header.SetITT(itt)
	p.Header.SetTTT(ReservedTag)
	p.Header.SetStatSN(statSN)
	p.Header.SetExpCmdSN(expCmdSN)
	p.Header.SetMaxCmdSN(maxCmdSN)
	p.Header[2] = reason
	return p
}

// Reject reasons.
const (
	RejectReasonReserved        uint8 = 0x00
	RejectReasonDataDigestErr   uint8 = 0x01
	RejectReasonSnackReq        uint8 = 0x02
	RejectReasonProtocolError   uint8 = 0x03
	RejectReasonCmdNotSupported uint8 = 0x04
	RejectReasonImmediateCmd    uint8 = 0x05
	RejectReasonTaskInProgress  uint8 = 0x06
	RejectReasonInvalidDataSN   uint8 = 0x07
	RejectReasonInvalidR2TSN    uint8 = 0x08
	RejectReasonTooManyConns    uint8 = 0x09
	RejectReasonSessionFail     uint8 = 0x0a
	RejectReasonInvalidPDUField uint8 = 0x0b
	RejectReasonLongOperation   uint8 = 0x0c
	RejectReasonNoResources     uint8 = 0x0d
	RejectReasonNegotiation     uint8 = 0x0e
	RejectReasonFailToBind      uint8 = 0x0f
)
