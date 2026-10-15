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
//
// It has to be generous enough that a proxy in front of this target can keep its
// own command window in flight: the proxy's backing initiator is capped by the
// MaxCmdSN this portal advertises, so a window equal to the client's queue depth
// leaves the backing session permanently full and it stops accepting commands.
const cmdWindow = 256

const maxLoginIterations = 16

// writeTimeout bounds a single PDU written to an unresponsive initiator. It only
// has to outlast a stalled peer: a real failure tears the session down.
const writeTimeout = 30 * time.Second

// deadlineRefresh bounds how often a socket deadline is rewritten on the PDU
// path. Rewriting one is not free — it takes the runtime poll lock that every
// read and write on the fd shares — and the PDU path would otherwise pay it on
// every PDU. A deadline is measured in seconds, so a value one refresh stale is
// still within the configured timeout; the cost just leaves the hot path.
const deadlineRefresh = time.Second

// refreshDeadline rewrites a socket deadline at most once per deadlineRefresh.
// last is the unix-nano time of the previous rewrite; set performs the call.
func refreshDeadline(last *atomic.Int64, d time.Duration, set func(time.Time)) {
	if d <= 0 {
		return
	}
	now := time.Now()
	if prev := last.Load(); prev != 0 && now.UnixNano()-prev < int64(deadlineRefresh) {
		return
	}
	last.Store(now.UnixNano())
	set(now.Add(d))
}

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

	// Write-path negotiation. unsolicited is InitialR2T=No and immediateData
	// is ImmediateData=Yes, each granted only when the initiator offered it
	// too; firstBurstLength is the negotiated unsolicited window. They default
	// to R2T-only, which is what an initiator that does not offer the keys
	// gets.
	unsolicited      bool
	immediateData    bool
	firstBurstLength int

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

	// writes routes unsolicited Data-Out PDUs (reserved transfer tag) to the
	// write command their ITT names. The transfer is registered on the
	// connection reader, in dispatch, because unsolicited data can arrive on
	// the very next PDU after the command, before its goroutine has started.
	writesMu sync.Mutex
	writes   map[uint32]*writeTransfer

	// readSetAt/writeSetAt hold the unix-nano time each socket deadline was last
	// rewritten, so refreshDeadline can keep the rewrite off the per-PDU path.
	readSetAt  atomic.Int64
	writeSetAt atomic.Int64
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
	refreshDeadline(&s.readSetAt, s.t.cfg.IdleTimeout, func(t time.Time) {
		_ = s.conn.SetReadDeadline(t)
	})
	h, err := iscsi.ReadBHS(s.conn)
	if err != nil {
		return nil, err
	}
	p := &iscsi.PDU{Header: h}
	// A Data-Out burst is read straight into the buffer that is going to hold it.
	// The alternative — a segment of its own, routed to the transfer afterwards —
	// costs an allocation and a full-length copy for every burst of every write,
	// and the header alone already says where the bytes belong.
	if h.Opcode() == iscsi.OpDataOut {
		if dst := s.dataOutWindow(&h); dst != nil {
			if err := iscsi.ReadSegment(s.conn, &h, dst); err != nil {
				return nil, err
			}
			p.Data, p.InPlace = dst, true
			s.logPDU("recv", p)
			return p, nil
		}
	}
	if dsl := h.DataSegmentLength(); dsl > 0 {
		p.Data = make([]byte, dsl)
	}
	if err := iscsi.ReadSegment(s.conn, &h, p.Data); err != nil {
		return nil, err
	}
	s.logPDU("recv", p)
	return p, nil
}

// dataOutWindow returns the part of the write the next Data-Out segment belongs
// in, or nil when it cannot be placed from the header alone: a transfer that is
// not registered, or a segment that does not fit the window it claims. Such a
// PDU falls back to a segment of its own and handleDataOut reports it exactly as
// it always did, so a placement miss is only ever a lost optimisation.
func (s *session) dataOutWindow(h *iscsi.BHS) []byte {
	dsl := h.DataSegmentLength()
	if dsl <= 0 {
		return nil
	}
	if h.TTT() == iscsi.ReservedTag {
		w := s.takeWrite(h.ITT())
		if w == nil {
			return nil
		}
		off := int(h.BufOffset())
		if !fits(off, dsl, len(w.buf)) {
			return nil
		}
		return w.buf[off : off+dsl]
	}
	tx := s.takePending(h.TTT())
	if tx == nil {
		return nil
	}
	off := int(h.BufOffset()) - tx.base
	if !fits(off, dsl, tx.want) {
		return nil
	}
	return tx.buf[tx.base+off : tx.base+off+dsl]
}

// fits reports whether n bytes at off stay inside a window of size, without the
// wrap that off+n can take for a field an initiator controls.
func fits(off, n, size int) bool {
	return off >= 0 && n >= 0 && off <= size-n
}

func (s *session) writePDU(p *iscsi.PDU) error {
	refreshDeadline(&s.writeSetAt, writeTimeout, func(t time.Time) {
		_ = s.conn.SetWriteDeadline(t)
	})
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
	s.firstBurstLength = s.t.cfg.FirstBurstLength
	s.pending = make(map[uint32]*dataOut)
	s.writes = make(map[uint32]*writeTransfer)

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
	// The write path is R2T-only — the RFC 7143 §11.1.4 defaults (InitialR2T
	// Yes, ImmediateData No). The unsolicited/immediate-data handling (the
	// writeTransfer registered in dispatch, awaitWrite, and the reserved-TTT
	// branch of handleDataOut) is deliberately NOT negotiated: against the
	// real Microsoft initiator it regressed a 1 MiB write from ~8.6 ms to
	// ~24.7 ms per command, and the self-built bench initiator did not
	// reproduce that shape, so the proven R2T sequence stays in charge until
	// the unsolicited path is diagnosed with a real wire trace. A session
	// negotiated Yes/No never sends unsolicited data, so every unsolicited
	// branch remains inert: s.unsolicited and s.immediateData keep their
	// zero values and runCommand always takes the R2T-driven path.
	add("InitialR2T", "Yes")
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
//
// On a session that allows unsolicited write data, the write's transfer is
// registered here too, before the goroutine starts: the first unsolicited
// Data-Out can be the very next PDU on the wire, and the reader must have
// somewhere to put it. The buffer allocation it needs is the same expLen-sized
// one the command would allocate itself a moment later.
func (s *session) dispatch(ctx context.Context, pdu *iscsi.PDU) {
	s.noteCommand(pdu.Header.CmdSN())
	h := &pdu.Header
	var w *writeTransfer
	if !s.discovery && s.unsolicited && h.Flags()&iscsi.CmdFlagWrite != 0 {
		if exp := int(h.ExpectedDataTransferLength()); exp > 0 &&
			int64(exp) <= s.h.Device().CapacityBytes() {
			w = newWriteTransfer(exp)
			s.addWrite(h.ITT(), w)
			if len(pdu.Data) > 0 {
				w.push(0, pdu.Data) // immediate data, RFC 7143 §7.5.2
			}
		}
	}
	go func() {
		if err := s.runCommand(ctx, pdu, w); err != nil {
			s.log.Warn("frontend: command failed, closing session",
				"opcode", iscsi.OpcodeName(pdu.Header.Opcode()), "err", err)
			_ = s.conn.Close()
		}
	}()
}

func (s *session) runCommand(ctx context.Context, pdu *iscsi.PDU, w *writeTransfer) error {
	h := &pdu.Header
	itt := h.ITT()
	if w != nil {
		defer s.dropWrite(itt)
	}
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
		// The write payload arrives in two phases. Unsolicited data (immediate
		// data in the command PDU plus Data-Out PDUs up to the negotiated
		// FirstBurstLength, when InitialR2T=No) is already being collected into
		// the transfer the reader registered; the command waits for that window
		// to fill, then R2Ts the remainder. Without unsolicited data the
		// transfer does not exist and the whole payload is R2T-driven, possibly
		// starting from the immediate data an ImmediateData=Yes initiator sent.
		if w != nil {
			if err := s.awaitWrite(ctx, w, min(s.firstBurstLength, expLen)); err != nil {
				return err
			}
			fill, err := w.state()
			if err != nil {
				return err
			}
			dataOut = w.buf
			if fill < expLen {
				if err := s.receiveDataOut(ctx, itt, dataOut, fill); err != nil {
					return err
				}
			}
		} else {
			dataOut = make([]byte, expLen)
			start := 0
			if s.immediateData && len(pdu.Data) > 0 {
				copy(dataOut, pdu.Data)
				start = len(pdu.Data)
			}
			if start < expLen {
				if err := s.receiveDataOut(ctx, itt, dataOut, start); err != nil {
					return err
				}
			}
		}
	}
	inLen := 0
	if !isWrite && expLen > 0 {
		inLen = expLen
	}

	// A read that the handler can stream is forwarded as it arrives instead of
	// waiting for the whole payload: the handler drives the sink, so this returns
	// only once the command is closed.
	if !isWrite && inLen > 0 {
		if sh, ok := s.h.(StreamHandler); ok {
			return s.runStreamedRead(ctx, itt, cdb, inLen, sh)
		}
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

// receiveDataOut drives the R2T sequence that collects the remainder of a write
// payload, from the first byte still missing. It runs on the command's own
// goroutine: each burst is published as an R2T and then awaited, while the
// connection reader routes the matching Data-Out PDUs in.
func (s *session) receiveDataOut(ctx context.Context, itt uint32, buf []byte, start int) error {
	total := len(buf)
	received := start
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
//
// A Data-Out carrying the reserved transfer tag is unsolicited (InitialR2T=No,
// RFC 7143 §7.5.2): it belongs to the write command its ITT names.
func (s *session) handleDataOut(pdu *iscsi.PDU) {
	h := &pdu.Header
	if h.TTT() == iscsi.ReservedTag {
		w := s.takeWrite(h.ITT())
		if w == nil {
			s.log.Warn("frontend: unsolicited Data-Out for unknown task", "itt", h.ITT())
			return
		}
		if pdu.InPlace {
			// The reader deposited the burst where it belongs, so only the
			// contiguous fill the command waits on has to advance.
			w.arrived(int(h.BufOffset()), len(pdu.Data))
			return
		}
		w.push(int(h.BufOffset()), pdu.Data)
		return
	}
	ttt := h.TTT()
	tx := s.takePending(ttt)
	if tx == nil {
		s.log.Warn("frontend: Data-Out for unknown transfer tag", "ttt", ttt)
		return
	}
	off := int(h.BufOffset()) - tx.base
	if !fits(off, len(pdu.Data), tx.want) {
		tx.err = fmt.Errorf("frontend: Data-Out offset %d out of range (burst of %d at %d)",
			int(h.BufOffset()), tx.want, tx.base)
	} else if !pdu.InPlace {
		copy(tx.buf[tx.base+off:], pdu.Data)
	}
	tx.got += len(pdu.Data)
	if tx.err != nil || tx.got >= tx.want {
		tx.complete()
	}
}

// writeTransfer collects one write command's unsolicited payload: immediate
// data that rode with the command PDU plus the Data-Out PDUs that follow it,
// up to the negotiated FirstBurstLength. The connection reader pushes into it
// and the command goroutine waits on notify; both meet through the mutex and
// the channel edge.
type writeTransfer struct {
	buf    []byte
	mu     sync.Mutex
	fill   int // highest contiguous offset received
	err    error
	notify chan struct{} // one-slot signal of any progress
}

func newWriteTransfer(size int) *writeTransfer {
	return &writeTransfer{buf: make([]byte, size), notify: make(chan struct{}, 1)}
}

// push deposits one data slice. The buffer is expLen sized and the data segment
// is bounded by the negotiated MaxRecvDataSegmentLength, so a well-formed
// initiator cannot overflow; a broken one sets err and the command fails.
func (w *writeTransfer) push(off int, data []byte) {
	w.mu.Lock()
	if fits(off, len(data), len(w.buf)) {
		copy(w.buf[off:], data)
		w.advanceLocked(off, len(data))
	} else {
		w.err = w.outOfRange(off + len(data))
	}
	w.mu.Unlock()
	w.signal()
}

// arrived accounts for a burst the connection reader deposited straight into the
// buffer (see readPDU): the bytes are already in place, so only the contiguous
// fill the command waits on has to advance. It reports the same error push would
// for a burst that does not fit, which the reader's placement check — the very
// same predicate — makes unreachable.
func (w *writeTransfer) arrived(off, n int) {
	w.mu.Lock()
	if fits(off, n, len(w.buf)) {
		w.advanceLocked(off, n)
	} else {
		w.err = w.outOfRange(off + n)
	}
	w.mu.Unlock()
	w.signal()
}

// advanceLocked moves the contiguous fill up to the end of the bytes just
// deposited at off. The caller holds the mutex.
func (w *writeTransfer) advanceLocked(off, n int) {
	if end := off + n; end > w.fill {
		w.fill = end
	}
}

func (w *writeTransfer) outOfRange(end int) error {
	return fmt.Errorf("frontend: write data at %d beyond the %d byte transfer", end, len(w.buf))
}

// signal wakes a command waiting on the transfer. It never blocks: the reader
// must not be held up by a command that is not reading yet.
func (w *writeTransfer) signal() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *writeTransfer) state() (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fill, w.err
}

// unsolicitedWait bounds how long the unsolicited window is waited for. A
// compliant initiator streams its full first burst without pause, so the wait
// ends by progress; the timer is the escape for an initiator that stops early
// and waits for an R2T instead, where the command continues with whatever
// arrived and R2Ts the remainder.
const unsolicitedWait = 200 * time.Millisecond

// awaitWrite waits until the transfer has received window bytes, an error, the
// wait budget, or the context's end. It returns with whatever arrived.
func (s *session) awaitWrite(ctx context.Context, w *writeTransfer, window int) error {
	var timer *time.Timer
	var timeout <-chan time.Time
	for {
		fill, err := w.state()
		if err != nil {
			return err
		}
		if fill >= window {
			return nil
		}
		if timer == nil {
			timer = time.NewTimer(unsolicitedWait)
			defer timer.Stop()
			timeout = timer.C
		}
		select {
		case <-w.notify:
		case <-timeout:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *session) addWrite(itt uint32, w *writeTransfer) {
	s.writesMu.Lock()
	s.writes[itt] = w
	s.writesMu.Unlock()
}

func (s *session) takeWrite(itt uint32) *writeTransfer {
	s.writesMu.Lock()
	w := s.writes[itt]
	s.writesMu.Unlock()
	return w
}

func (s *session) dropWrite(itt uint32) {
	s.writesMu.Lock()
	delete(s.writes, itt)
	s.writesMu.Unlock()
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

// maxDataInSegment caps how much payload one Data-In PDU carries, independently
// of what the initiator is willing to receive.
//
// A one-command-per-session initiator could take a whole MiB in one PDU, but a
// target that serves several commands on one session (the backing initiator now
// keeps the whole command window on a single session) must be able to interleave
// their bursts. PDU-sized chunks are what makes the interleaving meaningful: a
// chunk costs a socket write, and a burst that is one PDU is a burst nobody else
// can get in between.
const maxDataInSegment = 1 << 20

// maxDataInBatch caps how much payload one socket write carries, across
// however many Data-In PDUs it takes. Several commands interleave their bursts
// on one connection; a batch is the granularity at which they take turns, so
// it stays far below a whole transfer. One writev per 256 KiB instead of one
// per PDU is a 4x cut in syscalls and sendMu acquisitions when the initiator
// accepts only 64 KiB segments.
const maxDataInBatch = 256 << 10

// writeDataInRun emits data as consecutive Data-In PDUs in as few socket
// writes as possible: data is chunked to what the initiator accepts and up to
// maxDataInBatch bytes of chunks go out in one write. off is the
// command-relative byte offset of data and dataSN the DataSN of its first
// PDU. last marks the run as the end of the command, in which case the final
// PDU carries the status and reserves the command's StatSN. It returns the
// DataSN of the PDU after the run.
func (s *session) writeDataInRun(itt uint32, data []byte, off uint32, dataSN uint32,
	last bool, status uint8, residual uint32, overflow, underflow bool) (uint32, error) {
	for len(data) > 0 {
		run := data
		if len(run) > maxDataInBatch {
			run = run[:maxDataInBatch]
		}
		data = data[len(run):]
		n, err := s.writeDataInBatch(itt, run, off, dataSN,
			last && len(data) == 0, status, residual, overflow, underflow)
		if err != nil {
			return dataSN, err
		}
		off += uint32(len(run))
		dataSN += uint32(n)
	}
	return dataSN, nil
}

// writeDataInBatch writes one batch of consecutive Data-In PDUs covering data
// in a single socket write under one sendMu acquisition, so a burst costs one
// lock and one writev per maxDataInBatch instead of one per PDU. It returns
// how many PDUs it emitted. StatSN is reserved only by the PDU that closes the
// command, and it is taken at the moment the batch is sent, so statuses keep
// leaving the target in the order they complete.
func (s *session) writeDataInBatch(itt uint32, data []byte, off uint32, dataSN uint32,
	last bool, status uint8, residual uint32, overflow, underflow bool) (int, error) {
	chunk := s.dataInChunk()
	n := (len(data) + chunk - 1) / chunk
	if n == 0 {
		return 0, nil
	}
	var bhs iscsi.BHS
	hdrs := make([]byte, n*len(bhs))
	bufs := make(net.Buffers, 0, n*3)
	var pad [3]byte

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	var statSN uint32
	if last {
		statSN = s.statSN
	}
	// ExpCmdSN is read under sendMu, like statSN and like every other writer of a
	// PDU. It only ever grows (noteCommand raises it as commands arrive), so
	// reading it at the moment the batch is written is what keeps the value the
	// client sees non-decreasing: read before the lock, a batch that reaches the
	// connection later can carry a smaller ExpCmdSN than one already sent, and the
	// command window appears to go backwards on a connection with several
	// commands in flight. One command at a time cannot reorder, which is why this
	// only shows up with queue depth.
	expCmdSN := s.expCmdSN.Load()
	for i := 0; i < n; i++ {
		seg := data[i*chunk:]
		if len(seg) > chunk {
			seg = seg[:chunk]
		}
		isLast := last && i == n-1
		iscsi.FillDataInHeader(&bhs, itt, iscsi.ReservedTag, statSN, expCmdSN, expCmdSN+cmdWindow,
			dataSN+uint32(i), off+uint32(i*chunk), len(seg), isLast, isLast,
			status, residual, overflow, underflow)
		copy(hdrs[i*len(bhs):(i+1)*len(bhs)], bhs[:])
		bufs = append(bufs, hdrs[i*len(bhs):(i+1)*len(bhs)], seg)
		if p := (4 - len(seg)%4) % 4; p > 0 {
			bufs = append(bufs, pad[:p])
		}
	}
	refreshDeadline(&s.writeSetAt, writeTimeout, func(t time.Time) {
		_ = s.conn.SetWriteDeadline(t)
	})
	if _, err := bufs.WriteTo(s.conn); err != nil {
		return 0, err
	}
	if last {
		s.statSN++
	}
	return n, nil
}

// sendDataIn streams data as unsolicited Data-In PDUs and closes the command
// with the status on the last one.
//
// The burst is sent batch by batch, taking sendMu only for the batch in
// flight, so several commands can interleave their Data-In on one connection
// instead of queueing behind a single command's entire transfer.
//
// StatSN is consumed once per command, not once per PDU, and it rides only the
// last PDU: a Data-In PDU without the S bit carries the reserved value. That is
// what makes the interleaving legal — if every PDU of the burst carried the
// closing StatSN, two interleaved commands would present the initiator with a
// status sequence number that moves backwards.
func (s *session) sendDataIn(itt uint32, data []byte, status uint8, residual uint32, overflow, underflow bool) error {
	_, err := s.writeDataInRun(itt, data, 0, 0, true, status, residual, overflow, underflow)
	return err
}

// dataInChunk is the payload one Data-In PDU carries: what the initiator is
// willing to receive, capped so a burst is several PDUs rather than one.
func (s *session) dataInChunk() int {
	max := s.initiatorMaxRecvDSL
	if max <= 0 || max > maxDataInSegment {
		max = maxDataInSegment
	}
	return max
}

// writeDataInPDU emits one Data-In PDU. It takes sendMu only for that PDU, so
// several commands can interleave their bursts on one connection instead of
// queueing behind a single command's entire transfer.
//
// Only the last PDU reserves a StatSN, and it does so at the moment it is sent,
// so the status sequence numbers stay in the order the statuses leave the
// target. A PDU without the S bit carries the reserved value, which is what
// makes the interleaving legal.
func (s *session) writeDataInPDU(itt uint32, seg []byte, bufOffset, dataSN uint32, last bool, status uint8, residual uint32, overflow, underflow bool) error {
	var hdr iscsi.BHS
	var pad [3]byte
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	var statSN uint32
	if last {
		statSN = s.statSN
	}
	expCmdSN := s.expCmdSN.Load()
	iscsi.FillDataInHeader(&hdr, itt, iscsi.ReservedTag, statSN, expCmdSN, expCmdSN+cmdWindow,
		dataSN, bufOffset, len(seg), last, last, status, residual, overflow, underflow)
	bufs := net.Buffers{hdr[:], seg}
	if p := (4 - len(seg)%4) % 4; p > 0 {
		bufs = append(bufs, pad[:p])
	}
	refreshDeadline(&s.writeSetAt, writeTimeout, func(t time.Time) {
		_ = s.conn.SetWriteDeadline(t)
	})
	if _, err := bufs.WriteTo(s.conn); err != nil {
		return err
	}
	if last {
		s.statSN++
	}
	return nil
}

// runStreamedRead drives a read whose payload the handler delivers incrementally
// through a sink, so the target starts forwarding before the handler has seen
// the whole transfer.
func (s *session) runStreamedRead(ctx context.Context, itt uint32, cdb []byte, expLen int, sh StreamHandler) error {
	st := &dataInStream{s: s, itt: itt, expLen: expLen}
	res, err := sh.ExecuteStream(ctx, cdb, expLen, st)
	if err != nil {
		s.log.Warn("frontend: streamed read failed", "err", err)
		if !st.started {
			return s.sendResponse(itt, iscsi.StatusCheckCondition, iscsi.NotReadySense(), 0, false, false, 0)
		}
		// Bytes are already on the wire, so the command can only be closed with a
		// status; the shortfall is reported as an underflow.
		return st.finish(iscsi.StatusCheckCondition, uint32(expLen-st.sent), false, true)
	}
	switch {
	case !st.started:
		// The handler declined to stream and answered with a plain result.
		return s.sendResponse(itt, res.Status, res.Sense, res.Residual, res.Overflow, res.Underflow, 0)
	case st.finished:
		return nil
	default:
		// The handler streamed data but never marked the final segment.
		return st.finish(iscsi.StatusGood, uint32(expLen-st.sent), false, expLen > st.sent)
	}
}

// dataInStream is the sink a streamed read writes through. It re-chunks each
// segment to the initiator's Data-In size, tracks the DataSN and offset, and
// rides the command status on the very last PDU.
type dataInStream struct {
	s      *session
	itt    uint32
	expLen int

	sent     int
	dataSN   uint32
	started  bool
	finished bool
}

func (st *dataInStream) Segment(_ int64, data []byte, last bool, res Result) error {
	st.started = true
	if len(data) == 0 {
		if last {
			return st.finish(res.Status, res.Residual, res.Overflow, res.Underflow)
		}
		return nil
	}
	var status uint8
	var residual uint32
	var ov, un bool
	if last {
		status, residual, ov, un = st.lastStatus(st.sent+len(data), res)
	}
	next, err := st.s.writeDataInRun(st.itt, data, uint32(st.sent), st.dataSN, last, status, residual, ov, un)
	if err != nil {
		return err
	}
	st.sent += len(data)
	st.dataSN = next
	if last {
		st.finished = true
	}
	return nil
}

// lastStatus resolves the status, residual and flags for the final Data-In PDU
// from the bytes delivered through the whole transfer. A handler that reported
// its own shortfall (the backing target's residual) keeps it; otherwise the same
// rule the non-streamed path applies is used, so a handler that answers without
// streaming its payload (an INQUIRY, say) still reports the underflow the
// initiator sizes the transfer from.
func (st *dataInStream) lastStatus(delivered int, res Result) (status uint8, residual uint32, overflow, underflow bool) {
	status = res.Status
	residual, overflow, underflow = res.Residual, res.Overflow, res.Underflow
	if !overflow && !underflow {
		switch {
		case delivered < st.expLen:
			residual, underflow = uint32(st.expLen-delivered), true
		case delivered > st.expLen:
			residual, overflow = uint32(delivered-st.expLen), true
		}
	}
	return status, residual, overflow, underflow
}

// finish closes a streamed read with a zero-length Data-In carrying the status.
func (st *dataInStream) finish(status uint8, residual uint32, overflow, underflow bool) error {
	if err := st.s.writeDataInPDU(st.itt, nil, uint32(st.sent), st.dataSN, true, status, residual, overflow, underflow); err != nil {
		return err
	}
	st.dataSN++
	st.finished = true
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
