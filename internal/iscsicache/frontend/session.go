package frontend

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// cmdWindow is the command window advertised in MaxCmdSN. The target executes
// commands as they arrive, so a generous window is safe.
const cmdWindow = 64

const maxLoginIterations = 16

type session struct {
	t    *Target
	conn net.Conn
	log  *slog.Logger

	isid [6]byte
	tsih uint16
	cid  uint16

	initiatorName string
	discovery     bool

	// iqn and h are the target this session logged in to, resolved once at login
	// from the portal's registry. Commands are executed by h, so two sessions on
	// the same connection-serving portal can be backed by different targets.
	iqn string
	h   Handler

	statSN uint32
	// expCmdSN is published by the connection reader with a lock-free CAS so
	// that recording an arriving command never contends with a command that is
	// already sending its response. It is the only sequence number that is not
	// owned by sendMu.
	expCmdSN atomic.Uint32

	initiatorMaxRecvDSL int

	// sendMu serializes every PDU the target emits and guards the session
	// sequence numbers above. Commands run concurrently, so without it two
	// responses could stamp the same StatSN and the initiator would reset the
	// session; it also keeps a multi-PDU Data-In burst from interleaving with
	// another command's response.
	sendMu sync.Mutex

	// pending routes SCSI Data-Out PDUs to the R2T that asked for them. The
	// connection reader owns the socket and deposits each burst into the
	// transfer registered under its Target Transfer Tag, which lets a write wait
	// for its data off the reader goroutine.
	pendingMu sync.Mutex
	pending   map[uint32]*dataOut
}

// dataOut is one outstanding R2T: the burst the target asked for and the buffer
// the reader fills. done is closed once the burst is complete or failed.
type dataOut struct {
	buf    []byte // the whole command payload
	base   int    // offset the R2T asked for
	want   int    // burst length
	got    int
	err    error
	done   chan struct{}
	finish sync.Once
}

func (t *dataOut) complete() { t.finish.Do(func() { close(t.done) }) }

func (s *session) readPDU() (*iscsi.PDU, error) {
	if d := s.t.cfg.IdleTimeout; d > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(d))
	}
	p, err := iscsi.ReadPDU(s.conn)
	if err == nil {
		s.logPDU("recv", p)
	}
	return p, err
}

func (s *session) writePDU(p *iscsi.PDU) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	s.logPDU("send", p)
	return iscsi.WritePDU(s.conn, p)
}

// logPDU records one PDU at debug level. Real initiators differ in the command
// set and the PDU shapes they expect, so a verbose trace is the only practical
// way to diagnose a session that is dropped right after login.
//
// The detail is expensive to render (hex and sorting per PDU), so the level is
// checked first: this runs on every PDU of every command.
func (s *session) logPDU(dir string, p *iscsi.PDU) {
	if !s.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	h := &p.Header
	s.log.Debug("frontend: pdu", "dir", dir,
		"opcode", iscsi.OpcodeName(h.Opcode()),
		"itt", h.ITT(), "ttt", h.TTT(),
		"cmdsn", h.CmdSN(), "statsn", h.StatSN(),
		"flags", fmt.Sprintf("0x%02x", h.Flags()), "residual", h.ResidualCount(),
		"len", len(p.Data), "detail", pduDetail(p))
}

// pduDetail renders a PDU for the debug trace: the CDB of a SCSI command,
// negotiation text as key=value pairs, anything else as a short hex prefix.
func pduDetail(p *iscsi.PDU) string {
	switch p.Header.Opcode() {
	case iscsi.OpSCSICommand:
		return "cdb=" + hex.EncodeToString(p.Header.CDB())
	case iscsi.OpLoginReq, iscsi.OpLoginResp, iscsi.OpTextReq, iscsi.OpTextResp:
		keys := iscsi.ParseText(p.Data)
		names := make([]string, 0, len(keys))
		for name, value := range keys {
			names = append(names, name+"="+value)
		}
		sort.Strings(names)
		return strings.Join(names, " ")
	}
	if len(p.Data) == 0 {
		return ""
	}
	n := len(p.Data)
	if n > 16 {
		n = 16
	}
	return hex.EncodeToString(p.Data[:n])
}

// Login --------------------------------------------------------------------------

func (s *session) login(ctx context.Context) error {
	s.tsih = s.t.nextTSIH()
	s.expCmdSN.Store(1)
	s.initiatorMaxRecvDSL = DefaultMaxRecvDataSegmentLength
	s.pending = make(map[uint32]*dataOut)

	for i := 0; i < maxLoginIterations; i++ {
		pdu, err := s.readPDU()
		if err != nil {
			return fmt.Errorf("frontend: read login: %w", err)
		}
		h := &pdu.Header
		if h.Opcode() != iscsi.OpLoginReq {
			return fmt.Errorf("frontend: expected Login Request, got %s", iscsi.OpcodeName(h.Opcode()))
		}
		keys := iscsi.ParseText(pdu.Data)
		copy(s.isid[:], h[8:14])

		csg := h.LoginCSG()
		nsg := h.LoginNSG()
		transit := h.LoginTransit()

		class, detail, respKeys := s.negotiate(csg, keys)
		if class != 0 {
			_ = s.sendLoginResponse(h, csg, nsg, false, nil, class, detail)
			return fmt.Errorf("frontend: login rejected, class=%d detail=%d", class, detail)
		}

		if csg == iscsi.StageOperational && transit {
			// Full feature phase reached.
			if err := s.sendLoginResponse(h, csg, iscsi.StageFullFeature, true, respKeys, 0, 0); err != nil {
				return err
			}
			return nil
		}
		if err := s.sendLoginResponse(h, csg, nsg, transit, respKeys, 0, 0); err != nil {
			return err
		}
	}
	return fmt.Errorf("frontend: login negotiation did not converge")
}

// negotiate applies the login keys of one stage. A non zero class rejects the
// login with the returned status class and detail.
func (s *session) negotiate(csg uint8, keys map[string]string) (class, detail uint8, respKeys [][2]string) {
	switch csg {
	case iscsi.StageSecurity:
		am := strings.TrimSpace(keys["authmethod"])
		if am == "" {
			// Missing AuthMethod: default to None, which is what the MVP
			// supports.
			am = "None"
		}
		if !hasListValue(am, "None") {
			return 0x02, 0x01, nil // initiator error: authentication failure
		}
		// Initiators differ in where they place the session keys: some send
		// them alongside the security keys, others defer them to the
		// operational stage. Microsoft's initiator sends them early, so accept
		// them here too.
		if c, d := s.applySessionKeys(keys); c != 0 {
			return c, d, nil
		}
		respKeys = append(respKeys, [2]string{"AuthMethod", "None"})
		if _, ok := keys["headerdigest"]; ok {
			respKeys = append(respKeys, [2]string{"HeaderDigest", "None"})
		}
		if _, ok := keys["datadigest"]; ok {
			respKeys = append(respKeys, [2]string{"DataDigest", "None"})
		}
		respKeys = append(respKeys, [2]string{
			"MaxRecvDataSegmentLength", strconv.Itoa(s.t.cfg.MaxRecvDataSegmentLength)})
		if v, err := strconv.Atoi(strings.TrimSpace(keys["maxrecvdatasegmentlength"])); err == nil && v > 0 {
			s.initiatorMaxRecvDSL = v
		}
		return 0, 0, respKeys

	case iscsi.StageOperational:
		if c, d := s.applySessionKeys(keys); c != 0 {
			return c, d, nil
		}
		// A normal session that never named a registered target cannot be
		// routed anywhere, so it is refused here rather than failing later with
		// no handler to execute its commands.
		if !s.discovery && s.h == nil {
			return 0x02, 0x03, nil // initiator error: target not found
		}
		if v, err := strconv.Atoi(strings.TrimSpace(keys["maxrecvdatasegmentlength"])); err == nil && v > 0 {
			s.initiatorMaxRecvDSL = v
		}
		return 0, 0, s.operationalResponse(keys)

	default:
		return 0x02, 0x00, nil
	}
}

// applySessionKeys records the session identity keys (InitiatorName,
// SessionType, TargetName) and resolves the target. Either negotiation stage
// may carry them: initiators that send only the auth keys in the security stage
// defer SessionType and TargetName to the operational one, so a stage with no
// target name resolves nothing and lets the next stage decide.
func (s *session) applySessionKeys(keys map[string]string) (class, detail uint8) {
	if v := strings.TrimSpace(keys["initiatorname"]); v != "" {
		s.initiatorName = v
	}
	switch strings.ToLower(strings.TrimSpace(keys["sessiontype"])) {
	case "", "normal":
		name := strings.TrimSpace(keys["targetname"])
		if name == "" {
			return 0, 0
		}
		h, iqn, ok := s.t.lookup(name)
		if !ok {
			return 0x02, 0x03 // initiator error: target not found
		}
		s.h, s.iqn = h, iqn
	case "discovery":
		s.discovery = true
	default:
		return 0x02, 0x09 // initiator error: session type not supported
	}
	return 0, 0
}

// operationalResponse echoes the keys the initiator offered, pinned to the
// values the MVP target accepts.
func (s *session) operationalResponse(keys map[string]string) [][2]string {
	c := s.t.cfg
	out := make([][2]string, 0, 12)
	add := func(key, value string) {
		if _, ok := keys[strings.ToLower(key)]; ok {
			out = append(out, [2]string{key, value})
		}
	}
	if s.discovery {
		add("SessionType", "Discovery")
		return out
	}
	add("TargetName", s.iqn)
	add("SessionType", "Normal")
	add("InitialR2T", "Yes")
	// ImmediateData must stay "No": the write path always drives R2T for the
	// whole payload and has no handling for unsolicited data arriving with the
	// command PDU, so a real initiator that is told "Yes" would desync.
	add("ImmediateData", "No")
	add("MaxConnections", "1")
	add("MaxBurstLength", strconv.Itoa(c.MaxBurstLength))
	add("FirstBurstLength", strconv.Itoa(c.FirstBurstLength))
	add("ErrorRecoveryLevel", "0")
	add("DataPDUInOrder", "Yes")
	add("DataSequenceInOrder", "Yes")
	return out
}

// Emission -----------------------------------------------------------------------
//
// Every PDU the target sends goes through withSeq or withRaw, so the session
// sequence numbers stay consistent even though several commands are in flight
// at once. withSeq reserves a StatSN; withRaw leaves it alone for PDUs that are
// not a status (an R2T).

func (s *session) withSeq(build func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	exp := s.expCmdSN.Load()
	if err := s.writePDU(build(s.statSN, exp, exp+cmdWindow)); err != nil {
		return err
	}
	s.statSN++
	return nil
}

func (s *session) withRaw(build func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	exp := s.expCmdSN.Load()
	return s.writePDU(build(s.statSN, exp, exp+cmdWindow))
}

// noteCommand accepts a command's CmdSN and republishes the command window. The
// connection reader calls it as soon as a command arrives, before the command
// runs, so ExpCmdSN follows arrival order rather than the order in which
// concurrent commands happen to finish.
//
// It is deliberately lock free: taking sendMu here would make the reader
// contend with a command that is already writing its response, which costs more
// than the bookkeeping is worth when commands arrive one at a time.
func (s *session) noteCommand(sn uint32) {
	for {
		cur := s.expCmdSN.Load()
		if sn+1 <= cur {
			return
		}
		if s.expCmdSN.CompareAndSwap(cur, sn+1) {
			return
		}
	}
}

func (s *session) sendLoginResponse(req *iscsi.BHS, csg, nsg uint8, transit bool, keys [][2]string, class, detail uint8) error {
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		resp := iscsi.NewPDU(iscsi.OpLoginResp, iscsi.EncodeText(keys...))
		var flags uint8 = (csg << iscsi.LoginFlagCSGShift) | (nsg & iscsi.LoginFlagNSGMask)
		if transit {
			flags |= iscsi.LoginFlagTransit
		}
		resp.Header.SetFlags(flags)
		resp.Header[2], resp.Header[3] = 0, 0
		copy(resp.Header[8:14], s.isid[:])
		resp.Header.SetLoginTSIH(s.tsih)
		resp.Header.SetITT(req.ITT())
		resp.Header.SetStatSN(statSN)
		resp.Header.SetExpCmdSN(expCmdSN)
		resp.Header.SetMaxCmdSN(maxCmdSN)
		resp.Header[36] = class
		resp.Header[37] = detail
		return resp
	})
}

// Command loop -------------------------------------------------------------------
//
// The connection has a single reader goroutine, which owns the socket for
// reading. It never runs a command itself: it records the CmdSN, hands the
// command to its own goroutine and keeps reading, so several commands are
// outstanding at once instead of the initiator's queue depth being collapsed to
// one. Data-Out PDUs are routed to the write that asked for them.

func (s *session) loop(ctx context.Context) error {
	for {
		pdu, err := s.readPDU()
		if err != nil {
			return err
		}
		if s.t.closed.Load() {
			return io.EOF
		}
		if err := s.route(ctx, pdu); err != nil {
			return err
		}
	}
}

// route handles one PDU received from the initiator.
func (s *session) route(ctx context.Context, pdu *iscsi.PDU) error {
	h := &pdu.Header
	switch h.Opcode() {
	case iscsi.OpSCSICommand:
		s.dispatch(ctx, pdu)
		return nil
	case iscsi.OpNOPOut:
		return s.handleNOPOut(pdu)
	case iscsi.OpTextReq:
		return s.handleText(pdu)
	case iscsi.OpTaskMgmtReq:
		return s.handleTaskMgmt(pdu)
	case iscsi.OpLogoutReq:
		_ = s.handleLogout(pdu)
		return io.EOF
	case iscsi.OpDataOut:
		s.handleDataOut(pdu)
		return nil
	case iscsi.OpLoginReq:
		return s.sendReject(h, iscsi.RejectReasonInvalidPDUField)
	default:
		return s.sendReject(h, iscsi.RejectReasonCmdNotSupported)
	}
}

// dispatch accepts a command and runs it in its own goroutine.
//
// The CmdSN is recorded here, on the reader, because ExpCmdSN must follow the
// order commands arrived in, not the order concurrent commands finish in. A
// command that fails on the connection (a write that cannot be completed, a
// response that cannot be sent) tears the session down rather than leaving the
// initiator waiting for a reply that will never come.
func (s *session) dispatch(ctx context.Context, pdu *iscsi.PDU) {
	s.noteCommand(pdu.Header.CmdSN())
	go func() {
		if err := s.runCommand(ctx, pdu); err != nil {
			s.log.Warn("frontend: command failed, closing session",
				"opcode", iscsi.OpcodeName(pdu.Header.Opcode()), "err", err)
			_ = s.conn.Close()
		}
	}()
}

func (s *session) runCommand(ctx context.Context, pdu *iscsi.PDU) error {
	h := &pdu.Header
	itt := h.ITT()
	cdb := h.CDB()
	flags := h.Flags()
	expLen := int(h.ExpectedDataTransferLength())
	isWrite := flags&iscsi.CmdFlagWrite != 0

	if s.discovery {
		return s.sendResponse(itt, iscsi.StatusCheckCondition,
			iscsi.IllegalRequestSense(iscsi.ASCInvalidCommandOperationCode), 0, false, false, 0)
	}
	if expLen < 0 {
		expLen = 0
	}
	if capacity := s.h.Device().CapacityBytes(); int64(expLen) > capacity {
		return s.sendResponse(itt, iscsi.StatusCheckCondition,
			iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange), 0, false, false, 0)
	}

	var dataOut []byte
	if isWrite && expLen > 0 {
		dataOut = make([]byte, expLen)
		if err := s.receiveDataOut(ctx, itt, dataOut); err != nil {
			return err
		}
	}
	inLen := 0
	if !isWrite && expLen > 0 {
		inLen = expLen
	}

	res, err := s.h.Execute(ctx, cdb, dataOut, inLen)
	// The handler may have served Data from a pooled buffer; it stays borrowed
	// until the reply has been written (or the command has failed), so the
	// release runs on the way out of this function, after the send below.
	if res.Release != nil {
		defer res.Release()
	}
	if err != nil {
		s.log.Warn("frontend: command failed", "cdb", hex.EncodeToString(cdb[:min(16, len(cdb))]), "err", err)
		res = Result{Status: iscsi.StatusCheckCondition, Sense: iscsi.NotReadySense()}
	}

	// A command that transfers less than the initiator asked for must report the
	// shortfall in the Residual Count with the U bit set. The initiator sizes the
	// transfer from it, and Windows resets the connection when the returned
	// length does not add up. Callers that compute the residual themselves (the
	// cache for a short read) keep it.
	residual, overflow, underflow := res.Residual, res.Overflow, res.Underflow
	if !isWrite && expLen > 0 && !overflow && !underflow {
		switch {
		case len(res.Data) < expLen:
			residual, underflow = uint32(expLen-len(res.Data)), true
		case len(res.Data) > expLen:
			residual, overflow = uint32(len(res.Data)-expLen), true
		}
	}

	if len(res.Data) > 0 {
		// The status rides on the last Data-In (S bit); a separate SCSI Response
		// after an unsolicited Data-In is what the Windows initiator resets on.
		return s.sendDataIn(itt, res.Data, res.Status, residual, overflow, underflow)
	}
	return s.sendResponse(itt, res.Status, res.Sense, residual, overflow, underflow, 0)
}

// receiveDataOut drives the R2T sequence that collects a write payload. It runs
// on the command's own goroutine: each burst is published as an R2T and then
// awaited, while the connection reader routes the matching Data-Out PDUs in.
func (s *session) receiveDataOut(ctx context.Context, itt uint32, buf []byte) error {
	total := len(buf)
	received := 0
	var r2tSN uint32

	for received < total {
		burst := total - received
		if burst > s.t.cfg.MaxBurstLength {
			burst = s.t.cfg.MaxBurstLength
		}
		ttt := s.t.nextTTT()
		tx := &dataOut{buf: buf, base: received, want: burst, done: make(chan struct{})}
		s.addPending(ttt, tx)
		if err := s.withRaw(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
			return iscsi.BuildR2T(itt, ttt, statSN, expCmdSN, maxCmdSN,
				r2tSN, uint32(received), uint32(burst))
		}); err != nil {
			s.dropPending(ttt)
			return err
		}
		r2tSN++

		select {
		case <-tx.done:
		case <-ctx.Done():
			s.dropPending(ttt)
			return ctx.Err()
		}
		s.dropPending(ttt)
		if tx.err != nil {
			return tx.err
		}
		received += burst
	}
	return nil
}

// handleDataOut deposits a Data-Out burst into the transfer its transfer tag
// identifies. It runs on the connection reader, so a write never has to read
// from the socket itself and several commands can be in flight at once.
func (s *session) handleDataOut(pdu *iscsi.PDU) {
	h := &pdu.Header
	ttt := h.TTT()
	tx := s.takePending(ttt)
	if tx == nil {
		s.log.Warn("frontend: Data-Out for unknown transfer tag", "ttt", ttt)
		return
	}
	off := int(h.BufOffset()) - tx.base
	if off < 0 || off+len(pdu.Data) > tx.want {
		tx.err = fmt.Errorf("frontend: Data-Out offset %d out of range (burst of %d at %d)",
			int(h.BufOffset()), tx.want, tx.base)
	} else {
		copy(tx.buf[tx.base+off:], pdu.Data)
	}
	tx.got += len(pdu.Data)
	if tx.err != nil || tx.got >= tx.want {
		tx.complete()
	}
}

func (s *session) addPending(ttt uint32, tx *dataOut) {
	s.pendingMu.Lock()
	s.pending[ttt] = tx
	s.pendingMu.Unlock()
}

func (s *session) takePending(ttt uint32) *dataOut {
	s.pendingMu.Lock()
	tx := s.pending[ttt]
	s.pendingMu.Unlock()
	return tx
}

func (s *session) dropPending(ttt uint32) {
	s.pendingMu.Lock()
	delete(s.pending, ttt)
	s.pendingMu.Unlock()
}

// sendDataIn streams data as unsolicited Data-In PDUs and closes the command
// with the status on the last one.
//
// StatSN is consumed once per command, not once per PDU: the field in a Data-In
// PDU points at the StatSN that closes the command, so advancing it for every
// PDU makes the initiator see a status sequence number it never expected and
// drop the session.
func (s *session) sendDataIn(itt uint32, data []byte, status uint8, residual uint32, overflow, underflow bool) error {
	max := s.initiatorMaxRecvDSL
	if max <= 0 {
		max = DefaultMaxRecvDataSegmentLength
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	// The whole burst shares one StatSN and must not interleave with another
	// command's response, so it is both built and sent under the same lock.
	statSN := s.statSN
	expCmdSN := s.expCmdSN.Load()
	maxCmdSN := expCmdSN + cmdWindow

	// The burst goes out as one scatter-gather write: every PDU contributes its
	// header plus a slice of the payload, so the payload is never copied and the
	// response costs one syscall instead of one per PDU. A streamed read is a
	// single command carrying a large payload, which is exactly where a per-PDU
	// copy and syscall would dominate.
	n := (len(data) + max - 1) / max
	hdrs := make([]iscsi.BHS, n)
	bufs := make(net.Buffers, 0, 2*n+1)
	var pad [3]byte
	var dataSN uint32
	for i := 0; i < n; i++ {
		off := i * max
		end := off + max
		if end > len(data) {
			end = len(data)
		}
		seg := data[off:end]
		last := end == len(data)
		var res uint32
		var ov, un bool
		if last {
			res, ov, un = residual, overflow, underflow
		}
		iscsi.FillDataInHeader(&hdrs[i], itt, iscsi.ReservedTag, statSN, expCmdSN, maxCmdSN,
			dataSN, uint32(off), len(seg), last, last, status, res, ov, un)
		bufs = append(bufs, hdrs[i][:], seg)
		if p := (4 - len(seg)%4) % 4; p > 0 {
			bufs = append(bufs, pad[:p])
		}
		dataSN++
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := bufs.WriteTo(s.conn); err != nil {
		return err
	}
	s.statSN++
	return nil
}

func (s *session) sendResponse(itt uint32, status uint8, sense []byte, residual uint32, overflow, underflow bool, expDataSN uint32) error {
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		return iscsi.BuildSCSIResponse(itt, iscsi.ReservedTag, statSN, expCmdSN, maxCmdSN,
			expDataSN, status, residual, sense, overflow, underflow)
	})
}

func (s *session) handleNOPOut(pdu *iscsi.PDU) error {
	if pdu.Header.TTT() != iscsi.ReservedTag {
		return nil
	}
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		return iscsi.BuildNOPIn(pdu.Header.ITT(), iscsi.ReservedTag, statSN, expCmdSN, maxCmdSN, pdu.Data)
	})
}

// handleText answers SendTargets, which is what the Windows initiator uses to
// discover the targets behind this portal.
func (s *session) handleText(pdu *iscsi.PDU) error {
	keys := iscsi.ParseText(pdu.Data)
	var text []byte
	query := strings.TrimSpace(keys["sendtargets"])
	if query != "" {
		switch {
		case strings.EqualFold(query, "all"):
			names := s.t.TargetNames()
			pairs := make([][2]string, 0, len(names))
			for _, name := range names {
				pairs = append(pairs, [2]string{"TargetName", name})
			}
			text = iscsi.EncodeText(pairs...)
		default:
			// A named query answers with that target only, and with an empty
			// list when the portal does not serve it (which is how a real target
			// reports an unknown name).
			if _, iqn, ok := s.t.lookup(query); ok {
				text = iscsi.EncodeText([2]string{"TargetName", iqn})
			}
		}
	}
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		return iscsi.BuildTextResponse(pdu.Header.ITT(), pdu.Header.TTT(), statSN, expCmdSN, maxCmdSN, text, true)
	})
}

func (s *session) handleTaskMgmt(pdu *iscsi.PDU) error {
	fn := pdu.Header[1] & 0x7f
	response := uint8(0) // function complete
	switch fn {
	case 0, 0x7f:
		response = 5 // function rejected
	}
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		return iscsi.BuildTaskMgmtResponse(pdu.Header.ITT(), statSN, expCmdSN, maxCmdSN, response)
	})
}

func (s *session) handleLogout(pdu *iscsi.PDU) error {
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		resp := iscsi.NewPDU(iscsi.OpLogoutResp, nil)
		resp.Header.SetFlags(iscsi.FlagAlwaysSet)
		resp.Header.SetITT(pdu.Header.ITT())
		resp.Header.SetTTT(iscsi.ReservedTag)
		resp.Header.SetStatSN(statSN)
		resp.Header.SetExpCmdSN(expCmdSN)
		resp.Header.SetMaxCmdSN(maxCmdSN)
		resp.Header[2] = 0 // connection closed successfully
		return resp
	})
}

func (s *session) sendReject(req *iscsi.BHS, reason uint8) error {
	return s.withSeq(func(statSN, expCmdSN, maxCmdSN uint32) *iscsi.PDU {
		return iscsi.BuildReject(req.ITT(), statSN, expCmdSN, maxCmdSN, reason)
	})
}

// helpers ------------------------------------------------------------------------

// hasListValue reports whether a comma separated negotiation list contains v.
func hasListValue(offer, v string) bool {
	for _, item := range strings.Split(offer, ",") {
		if strings.EqualFold(strings.TrimSpace(item), v) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
