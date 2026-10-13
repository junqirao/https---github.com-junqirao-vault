package iscsiproxy

import "vault/internal/iscsicache/iscsi"

// The proxy answers a cached read locally, so the backend never sees that
// command, and it computes CmdSN/StatSN differently from the initiator. Two
// monotonic counters reconcile the two views:
//
//   - offset is the number of commands the proxy answered locally. It equals the
//     number of injected Data-In sequences, because the proxy only ever swallows
//     a read it can answer.
//
//   - Target→Initiator: every target PDU (opcodes 0x20..0x3f) carries StatSN,
//     ExpCmdSN and MaxCmdSN in bytes 24..40. StatSN gains the injected answers;
//     ExpCmdSN/MaxCmdSN regain the command sequence numbers the backend never
//     saw, keeping the advertised window width unchanged. A Data-In PDU without
//     the S bit is the one exception: its StatSN is a reserved field, not a
//     counter, so it is neither rewritten nor collected (see captureTargetSeq).
//
//   - Initiator→Target: ExpStatSN (byte 28) is pulled back by offset, because
//     the initiator's count includes the injected answers the backend knows
//     nothing about. Non-immediate commands also consume a CmdSN, and swallowing
//     a command leaves a hole the backend's ERL=0 check (CmdSN == ExpCmdSN)
//     rejects, so their CmdSN is pulled back too.
//
// Login PDUs are left alone: the offset is zero throughout login.

// rewriteTI adjusts a target→initiator PDU for the proxy's local answers.
func rewriteTI(h *iscsi.BHS, offset uint32) {
	if offset == 0 || h.Opcode() < 0x20 {
		return
	}
	// Data-In 不带 S 位时 StatSN 是保留字段（后端填 0），加 offset 只会往保留字段里
	// 写一个假序号；真正的序号空间只由带 S 位的 PDU 推进。
	if h.Opcode() != iscsi.OpDataIn || h.Flags()&iscsi.DataInFlagStatus != 0 {
		h.SetStatSN(h.StatSN() + offset)
	}
	h.SetExpCmdSN(h.ExpCmdSN() + offset)
	h.SetMaxCmdSN(h.MaxCmdSN() + offset)
}

// rewriteIT adjusts an initiator→target PDU for the proxy's local answers.
func rewriteIT(h *iscsi.BHS, offset uint32) {
	if offset == 0 {
		return
	}
	switch h.Opcode() {
	case iscsi.OpLoginReq, iscsi.OpSNACKReq:
		// Login runs before any local answer; SNACK carries neither CmdSN nor
		// ExpStatSN.
		return
	case iscsi.OpNOPOut, iscsi.OpSCSICommand, iscsi.OpTaskMgmtReq, iscsi.OpTextReq, iscsi.OpLogoutReq:
		if !h.Immediate() {
			h.SetCmdSN(h.CmdSN() - offset)
		}
	case iscsi.OpDataOut:
		// Data-Out carries ExpStatSN but no CmdSN.
	default:
		return
	}
	h.SetExpStatSN(h.ExpStatSN() - offset)
}
