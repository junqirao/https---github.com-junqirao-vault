package backend

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// Defaults for the MVP initiator.
const (
	// DefaultMaxRecvDataSegmentLength is the largest Data-In segment this
	// initiator advertises it can receive. It is the size of the PDUs the
	// backing target sends back, so a small value turns one streamed read into
	// one socket read (and one segment allocation) per chunk: at 64 KiB a MiB
	// cost sixteen of each. The key is negotiable, so a target that caps it below
	// this value is handled transparently.
	DefaultMaxRecvDataSegmentLength = 1 << 20
	DefaultMaxBurstLength           = 262144
	DefaultDialTimeout              = 10 * time.Second
	DefaultIOTimeout                = 30 * time.Second
)

// Config configures an Initiator connection to a backing iSCSI target.
type Config struct {
	// Address is the host:port of the backing target, e.g. "172.18.28.200:3260".
	Address string
	// TargetIQN is the backing target name announced during login.
	TargetIQN string
	// InitiatorIQN is our own name; generated from the host name when empty.
	InitiatorIQN string
	// Auth is "none" (default) or "chap" (one-way CHAP, RFC 7143 §11.2.2).
	Auth string
	// Username/Secret are the CHAP N (name) and shared secret. They are required
	// when Auth is "chap" and ignored otherwise.
	Username string
	Secret   string

	DialTimeout time.Duration
	IOTimeout   time.Duration

	// MaxRecvDataSegmentLength is the largest PDU data segment we accept.
	MaxRecvDataSegmentLength int
	// MaxBurstLength is the largest burst we accept.
	MaxBurstLength int

	// Sessions is the maximum number of independent sessions a Pool may run; it
	// is ignored by Dial, which always opens exactly one. Zero (or a value above
	// MaxSessions) selects MaxSessions. The pool opens DefaultSessions eagerly
	// and grows towards this cap only while every session is busy.
	Sessions int

	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.DialTimeout <= 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.IOTimeout <= 0 {
		c.IOTimeout = DefaultIOTimeout
	}
	if c.MaxRecvDataSegmentLength <= 0 {
		c.MaxRecvDataSegmentLength = DefaultMaxRecvDataSegmentLength
	}
	if c.MaxBurstLength <= 0 {
		c.MaxBurstLength = DefaultMaxBurstLength
	}
	if c.InitiatorIQN == "" {
		c.InitiatorIQN = DefaultInitiatorIQN()
	}
}

// DefaultInitiatorIQN builds an initiator IQN for this host.
func DefaultInitiatorIQN() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "vault-client"
	}
	return "iqn.2024-01.local.vault-iscsi-cache:" + h
}

// ProtocolName is the iSCSI name prefix.
const ProtocolName = "iqn"

// Initiator is a single-connection pure-Go iSCSI initiator. Commands are
// serialised: one outstanding command per connection, which keeps the DataSN /
// R2T state machine simple and matches the MVP single-session design.
type Initiator struct {
	cfg Config
	log *slog.Logger

	conn   net.Conn
	closed bool

	mu sync.Mutex

	info DeviceInfo

	isid [6]byte
	tsih uint16

	itt       uint32
	cmdSN     uint32
	expStatSN uint32
	maxCmdSN  uint32

	// negotiated parameters
	ourMaxRecvDSL    int
	targetMaxRecvDSL int
	maxBurstLength   int
}

var _ Backend = (*Initiator)(nil)

// Dial connects and logs in to the configured backing target, then discovers
// the LUN parameters.
func Dial(ctx context.Context, cfg Config) (*Initiator, error) {
	cfg.applyDefaults()
	if cfg.Address == "" {
		return nil, errors.New("backend: address is required")
	}
	if cfg.TargetIQN == "" {
		return nil, errors.New("backend: target_iqn is required")
	}
	switch {
	case cfg.Auth == "", strings.EqualFold(cfg.Auth, "none"):
		// No authentication.
	case strings.EqualFold(cfg.Auth, "chap"):
		// One-way CHAP needs both halves of the credential; a missing half would
		// otherwise fail deep inside the login exchange with a bare auth error.
		if cfg.Username == "" || cfg.Secret == "" {
			return nil, errors.New("backend: auth \"chap\" requires username and secret")
		}
	default:
		return nil, fmt.Errorf("backend: auth %q is not supported (only none or chap)", cfg.Auth)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	d := net.Dialer{Timeout: cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("backend: dial %s: %w", cfg.Address, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		// iSCSI is a request/response protocol with small PDUs: a delayed ACK
		// combined with Nagle would hold a command or an R2T Data-Out burst for
		// tens of milliseconds on every command. The frontend already disables
		// Nagle on the initiator side; the backing side needs it too.
		_ = tc.SetNoDelay(true)
		// Detect a backing target that vanished without logging out, so a dead
		// session is not held open by commands waiting on a socket nobody will
		// answer on.
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	it := &Initiator{
		cfg:              cfg,
		log:              log,
		conn:             conn,
		cmdSN:            1,
		ourMaxRecvDSL:    cfg.MaxRecvDataSegmentLength,
		targetMaxRecvDSL: DefaultMaxRecvDataSegmentLength,
		maxBurstLength:   cfg.MaxBurstLength,
	}
	if _, err := rand.Read(it.isid[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("backend: generate isid: %w", err)
	}

	if err := it.login(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	if err := it.discover(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	log.Info("backend session established",
		"address", cfg.Address, "target", cfg.TargetIQN,
		"vendor", it.info.Vendor, "product", it.info.Product,
		"block_size", it.info.BlockSize, "block_count", it.info.BlockCount,
		"capacity_bytes", it.info.CapacityBytes(), "serial", it.info.Serial)
	return it, nil
}

// Info returns the discovered device parameters.
func (it *Initiator) Info() DeviceInfo { return it.info }

// Close logs out and closes the connection.
func (it *Initiator) Close() error {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.conn == nil {
		return nil
	}
	it.closed = true
	p := iscsi.NewPDU(iscsi.OpLogoutReq, nil)
	p.Header.SetFlags(iscsi.FlagAlwaysSet)
	p.Header.SetITT(it.nextITT())
	p.Header.SetTTT(iscsi.ReservedTag)
	p.Header.SetCmdSN(it.cmdSN)
	p.Header.SetExpStatSN(it.expStatSN)
	p.Header[2] = 0 // reason code: close session
	// Bounded: a target that stopped reading must not be able to wedge Close,
	// which holds it.mu and would then block every other command forever.
	_ = it.writePDU(context.Background(), p)
	err := it.conn.Close()
	it.conn = nil
	return err
}

func (it *Initiator) nextITT() uint32 {
	it.itt++
	if it.itt == 0 || it.itt == iscsi.ReservedTag {
		it.itt = 1
	}
	return it.itt
}

func (it *Initiator) setReadDeadline(ctx context.Context) {
	d := it.cfg.IOTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && (d == 0 || rem < d) {
			d = rem
		}
	}
	if d > 0 {
		_ = it.conn.SetReadDeadline(time.Now().Add(d))
	}
}

func (it *Initiator) setWriteDeadline(ctx context.Context) {
	d := it.cfg.IOTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && (d == 0 || rem < d) {
			d = rem
		}
	}
	if d > 0 {
		_ = it.conn.SetWriteDeadline(time.Now().Add(d))
	}
}

func (it *Initiator) readPDU(ctx context.Context) (*iscsi.PDU, error) {
	it.setReadDeadline(ctx)
	return iscsi.ReadPDU(it.conn)
}

// readResponse reads one response PDU for Exec. A Data-In segment that lies
// inside transfer is read straight into it, so the caller operates on the buffer
// it is going to return and pays neither a segment allocation nor a copy per
// PDU; inPlace reports that case. Any other segment (R2T, NOP-In, a Data-In
// beyond the transfer, sense data) goes to scratch, which is reused across the
// PDUs of one command.
func (it *Initiator) readResponse(transfer []byte, scratch *[]byte) (*iscsi.PDU, bool, error) {
	h, err := iscsi.ReadBHS(it.conn)
	if err != nil {
		return nil, false, err
	}
	dsl := h.DataSegmentLength()
	var seg []byte
	inPlace := false
	if dsl > 0 {
		if off := int(h.BufOffset()); h.Opcode() == iscsi.OpDataIn && off+dsl <= len(transfer) {
			seg, inPlace = transfer[off:off+dsl], true
		} else {
			if cap(*scratch) < dsl {
				*scratch = make([]byte, dsl)
			}
			seg = (*scratch)[:dsl]
		}
	}
	if err := iscsi.ReadSegment(it.conn, &h, seg); err != nil {
		return nil, false, err
	}
	return &iscsi.PDU{Header: h, Data: seg}, inPlace, nil
}

func (it *Initiator) writePDU(ctx context.Context, p *iscsi.PDU) error {
	it.setWriteDeadline(ctx)
	return iscsi.WritePDU(it.conn, p)
}

// Login ------------------------------------------------------------------------------

func (it *Initiator) operationalKeys() [][2]string {
	return [][2]string{
		{"SessionType", "Normal"},
		{"InitialR2T", "Yes"},
		{"ImmediateData", "No"},
		{"MaxConnections", "1"},
		{"MaxBurstLength", strconv.Itoa(it.cfg.MaxBurstLength)},
		{"ErrorRecoveryLevel", "0"},
		{"DataPDUInOrder", "Yes"},
		{"DataSequenceInOrder", "Yes"},
	}
}

// chapAlgorithmMD5 is the CHAP_A identifier for MD5. RFC 7143 §11.2.2 only
// defines MD5 for iSCSI, so it is the only algorithm we offer or accept.
const chapAlgorithmMD5 = "5"

// chapEnabled reports whether one-way CHAP authentication is configured.
func (it *Initiator) chapEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(it.cfg.Auth), "chap")
}

// securityKeys builds the security-stage login keys: the session identity plus
// the AuthMethod offer. With CHAP configured we offer "CHAP,None" (let the
// target choose) and advertise MD5; otherwise we offer only None, which is what
// the MVP target historically received.
func (it *Initiator) securityKeys() [][2]string {
	keys := [][2]string{
		{"InitiatorName", it.cfg.InitiatorIQN},
		{"TargetName", it.cfg.TargetIQN},
		{"SessionType", "Normal"},
	}
	if it.chapEnabled() {
		keys = append(keys,
			[2]string{"AuthMethod", "CHAP,None"},
			[2]string{"CHAP_A", chapAlgorithmMD5})
	} else {
		keys = append(keys, [2]string{"AuthMethod", "None"})
	}
	return append(keys,
		[2]string{"HeaderDigest", "None"},
		[2]string{"DataDigest", "None"},
		[2]string{"MaxRecvDataSegmentLength", strconv.Itoa(it.cfg.MaxRecvDataSegmentLength)})
}

// chapChallenge is the challenge a target sends in the security stage.
type chapChallenge struct {
	algorithm string
	id        uint8
	challenge []byte
}

// parseCHAPChallenge extracts a CHAP challenge from a login response's text
// keys. The second return value is false when the response carries no challenge
// (a target that accepted None, for instance), which is not an error.
func parseCHAPChallenge(text map[string]string) (chapChallenge, bool, error) {
	rawC, hasC := text["chap_c"]
	rawI, hasI := text["chap_i"]
	if !hasC && !hasI {
		return chapChallenge{}, false, nil
	}
	c := chapChallenge{algorithm: strings.TrimSpace(text["chap_a"])}
	if c.algorithm == "" {
		c.algorithm = chapAlgorithmMD5
	}
	id, err := strconv.Atoi(strings.TrimSpace(rawI))
	if err != nil || id < 0 || id > 0xff {
		return chapChallenge{}, false, fmt.Errorf("backend: invalid CHAP_I %q", rawI)
	}
	c.id = uint8(id)
	challenge, err := decodeHexValue(rawC)
	if err != nil {
		return chapChallenge{}, false, fmt.Errorf("backend: invalid CHAP_C %q: %w", rawC, err)
	}
	c.challenge = challenge
	return c, true, nil
}

// chapResponseKeys computes CHAP_N (the user name) and CHAP_R (the answer) for
// the given challenge.
//
// The digest is MD5 over the concatenation of the one-byte CHAP_I value, the
// shared secret and the challenge, per RFC 7143 §11.2.2 (which references the
// CHAP procedure of RFC 1994 §4.1). The answer is sent as "0x" prefixed hex.
func (it *Initiator) chapResponseKeys(c chapChallenge) ([][2]string, error) {
	if !strings.EqualFold(c.algorithm, chapAlgorithmMD5) {
		return nil, fmt.Errorf("backend: target selected unsupported CHAP algorithm %q", c.algorithm)
	}
	h := md5.New()
	h.Write([]byte{c.id})
	h.Write([]byte(it.cfg.Secret))
	h.Write(c.challenge)
	return [][2]string{
		{"CHAP_N", it.cfg.Username},
		{"CHAP_R", "0x" + hex.EncodeToString(h.Sum(nil))},
	}, nil
}

// decodeHexValue decodes an iSCSI CHAP hex string, tolerating the optional "0x"
// prefix, and treats an empty value as an empty challenge.
func decodeHexValue(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	if s == "" {
		return nil, nil
	}
	return hex.DecodeString(s)
}

func (it *Initiator) login(ctx context.Context) error {
	itt := it.nextITT()
	stage := uint8(iscsi.StageSecurity)
	nsg := uint8(iscsi.StageOperational)
	transit := true
	keys := it.securityKeys()
	var tsih uint16
	// chapSent flips once the CHAP_R answer has gone out, so a target that echoes
	// the challenge in a later iteration cannot make us answer twice.
	chapSent := false

	for iter := 0; iter < 16; iter++ {
		req := iscsi.NewPDU(iscsi.OpLoginReq, iscsi.EncodeText(keys...))
		var flags uint8 = (stage << iscsi.LoginFlagCSGShift) | (nsg & iscsi.LoginFlagNSGMask)
		if transit {
			flags |= iscsi.LoginFlagTransit
		}
		req.Header.SetFlags(flags)
		req.Header.SetImmediate(true)
		req.Header[2] = 0 // VersionMax
		req.Header[3] = 0 // VersionMin
		copy(req.Header[8:14], it.isid[:])
		req.Header.SetLoginTSIH(tsih)
		req.Header.SetITT(itt)
		binary.BigEndian.PutUint16(req.Header[20:22], 1) // CID
		req.Header.SetCmdSN(0)
		req.Header.SetExpStatSN(0)

		if err := it.writePDU(ctx, req); err != nil {
			return fmt.Errorf("backend: send login: %w", err)
		}
		resp, err := it.readPDU(ctx)
		if err != nil {
			return fmt.Errorf("backend: read login response: %w", err)
		}
		if resp.Header.Opcode() != iscsi.OpLoginResp {
			return fmt.Errorf("backend: expected login response, got %s", iscsi.OpcodeName(resp.Header.Opcode()))
		}
		if class := resp.Header.LoginStatusClass(); class != 0 {
			return fmt.Errorf("backend: login rejected: class=%d detail=%d text=%v",
				class, resp.Header.LoginStatusDetail(), iscsi.ParseText(resp.Data))
		}
		if t := resp.Header.LoginTSIH(); t != 0 {
			tsih = t
			it.tsih = t
		}
		// The initiator's first command must use the CmdSN the target expects,
		// which it advertises as ExpCmdSN in the login response. Targets differ:
		// some expect 0, others 1.
		it.cmdSN = resp.Header.ExpCmdSN()
		if sn := resp.Header.StatSN(); sn != 0 {
			it.expStatSN = sn + 1
			it.maxCmdSN = resp.Header.MaxCmdSN()
		}
		text := iscsi.ParseText(resp.Data)
		if v, ok := text["maxrecvdatasegmentlength"]; ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				it.targetMaxRecvDSL = n
			}
		}
		it.log.Debug("login exchange",
			"iter", iter, "csg", resp.Header.LoginCSG(), "nsg", resp.Header.LoginNSG(),
			"transit", resp.Header.LoginTransit(), "continue", resp.Header.LoginContinue(),
			"status_class", resp.Header.LoginStatusClass(), "keys", len(text))

		// CHAP (RFC 7143 §11.2.2, one-way): the target answers our
		// AuthMethod=CHAP,None / CHAP_A offer with its challenge — CHAP_I (a
		// one-byte identifier) and CHAP_C (the challenge, hex) — during the
		// security stage. Answer in the same stage with CHAP_N + CHAP_R and offer
		// to move on; the target then decides whether the digest was accepted.
		if stage == iscsi.StageSecurity && !chapSent && it.chapEnabled() {
			challenge, ok, err := parseCHAPChallenge(text)
			if err != nil {
				return err
			}
			if ok {
				respKeys, err := it.chapResponseKeys(challenge)
				if err != nil {
					return err
				}
				chapSent = true
				it.log.Debug("login CHAP challenge answered",
					"iter", iter, "algorithm", challenge.algorithm,
					"id", challenge.id, "challenge_len", len(challenge.challenge))
				stage, nsg, transit, keys = iscsi.StageSecurity, iscsi.StageOperational, true, respKeys
				continue
			}
		}

		rcsg := resp.Header.LoginCSG()
		rnsg := resp.Header.LoginNSG()
		if resp.Header.LoginContinue() {
			// The target has more keys to send in this stage; acknowledge with
			// an empty request and keep reading.
			stage, nsg, transit, keys = rcsg, rnsg, false, nil
			continue
		}
		if resp.Header.LoginTransit() {
			stage = rnsg
			if stage == iscsi.StageFullFeature {
				return nil
			}
			nsg, transit, keys = iscsi.StageFullFeature, true, it.operationalKeys()
			continue
		}
		// No transit offered yet: ask to move to full feature phase.
		stage, nsg, transit, keys = rcsg, iscsi.StageFullFeature, true, it.operationalKeys()
	}
	return errors.New("backend: login negotiation did not converge")
}

// Discovery --------------------------------------------------------------------------

func (it *Initiator) discover(ctx context.Context) error {
	info := DeviceInfo{TargetIQN: it.cfg.TargetIQN, TargetMaxRecvDSL: it.targetMaxRecvDSL}
	info.BlockSize = 512
	info.BlockCount = 0

	res, err := it.Exec(ctx, inquiryCDB(false, 0, 36), nil, 36)
	if err != nil {
		return fmt.Errorf("backend: INQUIRY: %w", err)
	}
	if !res.OK() {
		return fmt.Errorf("backend: INQUIRY: %w", res.Error())
	}
	if len(res.Data) >= 36 {
		info.DeviceType = res.Data[0] & 0x1f
		info.Vendor = strings.TrimSpace(string(res.Data[8:16]))
		info.Product = strings.TrimSpace(string(res.Data[16:32]))
		info.Revision = strings.TrimSpace(string(res.Data[32:36]))
	}

	if r, err := it.Exec(ctx, inquiryCDB(true, 0x80, 255), nil, 255); err == nil && r.OK() && len(r.Data) > 4 {
		info.Serial = strings.TrimSpace(string(r.Data[4:]))
	}
	if r, err := it.Exec(ctx, inquiryCDB(true, 0x83, 255), nil, 255); err == nil && r.OK() {
		info.WWID = firstDesignator(r.Data)
	}

	// READ CAPACITY(16) first, fall back to READ CAPACITY(10).
	rc16 := make([]byte, 16)
	rc16[0] = iscsi.SCSIServiceActionIn16
	rc16[1] = 0x10
	binary.BigEndian.PutUint32(rc16[10:14], 32)
	if r, err := it.Exec(ctx, rc16, nil, 32); err == nil && r.OK() && len(r.Data) >= 12 {
		last := binary.BigEndian.Uint64(r.Data[0:8])
		bs := binary.BigEndian.Uint32(r.Data[8:12])
		if bs != 0 {
			info.BlockSize = bs
			info.BlockCount = last + 1
		}
	} else {
		rc10 := make([]byte, 10)
		rc10[0] = iscsi.SCSIReadCapacity10
		r, err := it.Exec(ctx, rc10, nil, 8)
		if err != nil {
			return fmt.Errorf("backend: READ CAPACITY: %w", err)
		}
		if !r.OK() {
			return fmt.Errorf("backend: READ CAPACITY: %w", r.Error())
		}
		if len(r.Data) >= 8 {
			last := binary.BigEndian.Uint32(r.Data[0:4])
			bs := binary.BigEndian.Uint32(r.Data[4:8])
			if bs != 0 {
				info.BlockSize = bs
				info.BlockCount = uint64(last) + 1
			}
		}
	}
	it.info = info
	return nil
}

func inquiryCDB(evpd bool, page uint8, alloc uint16) []byte {
	c := make([]byte, 16)
	c[0] = iscsi.SCSIInquiry
	if evpd {
		c[1] = 0x01
	}
	c[2] = page
	binary.BigEndian.PutUint16(c[3:5], alloc)
	return c
}

// firstDesignator extracts the first VPD page 0x83 designator payload as hex.
func firstDesignator(vpd []byte) string {
	if len(vpd) < 8 {
		return ""
	}
	payload := vpd[4:]
	var parts []string
	for len(payload) >= 4 {
		l := int(payload[3])
		if 4+l > len(payload) {
			break
		}
		d := payload[4 : 4+l]
		if payload[0]&0x0f == 1 { // ASCII
			parts = append(parts, strings.TrimSpace(string(d)))
		} else {
			parts = append(parts, hex.EncodeToString(d))
		}
		payload = payload[4+l:]
	}
	return strings.Join(parts, "|")
}

// Exec -------------------------------------------------------------------------------

// Exec issues one SCSI command over the session. A command that reads data
// returns it in a freshly allocated Result.Data.
func (it *Initiator) Exec(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (*Result, error) {
	return it.exec(ctx, cdb, dataOut, nil, inLen)
}

// ExecInto issues one SCSI command whose read payload is written straight into
// buf, so a caller that already has a destination pays neither an allocation
// nor a copy for the transfer. Result.Data aliases buf and is not a copy of it,
// which makes this the read path for a cache or proxy that owns its buffers.
func (it *Initiator) ExecInto(ctx context.Context, cdb []byte, dataOut []byte, buf []byte) (*Result, error) {
	if len(buf) == 0 {
		return nil, errors.New("backend: empty read buffer")
	}
	return it.exec(ctx, cdb, dataOut, buf, len(buf))
}

func (it *Initiator) exec(ctx context.Context, cdb []byte, dataOut []byte, buf []byte, inLen int) (*Result, error) {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.conn == nil || it.closed {
		return nil, ErrClosed
	}
	if len(cdb) == 0 {
		return nil, errors.New("backend: empty CDB")
	}

	// Bound the whole command, not just each individual read. The read loop below
	// resets its per-read deadline on every PDU, so a target that keeps the
	// connection alive with NOP-In pings while silently holding the command never
	// lets the loop return. Exec runs under the proxy's global command lock, so a
	// command that never comes back freezes the whole proxy (which is exactly why
	// Windows disk management / VDS stopped responding). IOTimeout is therefore
	// used as the budget for the entire command.
	ctx, cancel := context.WithTimeout(ctx, it.cfg.IOTimeout)
	defer cancel()

	itt := it.nextITT()
	expLen := inLen
	if len(dataOut) > 0 {
		expLen = len(dataOut)
	}
	cmd := iscsi.BuildSCSICommand(itt, it.cmdSN, it.expStatSN, 0, cdb, expLen, inLen > 0, len(dataOut) > 0)
	it.cmdSN++
	if it.log.Enabled(ctx, slog.LevelDebug) {
		it.log.Debug("backend: pdu send", "opcode", "SCSICommand", "itt", itt, "cmdsn", cmd.Header.CmdSN(),
			"expstatsn", it.expStatSN, "expdatalen", expLen, "cdb", hex.EncodeToString(cdb[:min(16, len(cdb))]))
	}
	if err := it.writePDU(ctx, cmd); err != nil {
		return nil, fmt.Errorf("backend: send command: %w", err)
	}

	res := &Result{}
	var dataBuf []byte
	switch {
	case buf != nil:
		// The caller supplied the destination, so the transfer is read into it.
		dataBuf = buf
	case inLen > 0:
		dataBuf = make([]byte, inLen)
	}
	var dataOutSN uint32
	var haveIn int
	var scratch []byte

	// The command is bounded as a whole by ctx, so the socket read deadline is
	// the ctx deadline: arming it once here is equivalent to re-arming it before
	// every PDU, minus the netpoll timer update that a streamed read otherwise
	// paid on each Data-In.
	it.setReadDeadline(ctx)

	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("backend: command timed out after %s: %w", it.cfg.IOTimeout, err)
		}
		pdu, inPlace, err := it.readResponse(dataBuf, &scratch)
		if err != nil {
			return nil, fmt.Errorf("backend: read response: %w", err)
		}
		h := &pdu.Header
		// Formatting the trace costs a Printf and a hex dump per PDU, and the
		// arguments are evaluated whether or not the level is enabled, so the
		// level is checked before building them.
		if it.log.Enabled(ctx, slog.LevelDebug) {
			it.log.Debug("backend: pdu recv", "opcode", iscsi.OpcodeName(h.Opcode()), "itt", h.ITT(), "ttt", h.TTT(),
				"statsn", h.StatSN(), "expcmdsn", h.ExpCmdSN(), "flags", fmt.Sprintf("0x%02x", h.Flags()),
				"len", len(pdu.Data), "bufoff", h.BufOffset(), "residual", h.ResidualCount())
		}
		switch h.Opcode() {
		case iscsi.OpDataIn:
			off := int(h.BufOffset())
			switch {
			case off+len(pdu.Data) > len(dataBuf):
				// Target sent more than requested: keep what fits and note the residual.
				it.log.Warn("backend: data-in beyond expected length", "offset", off, "len", len(pdu.Data), "expected", len(dataBuf))
				if off < len(dataBuf) {
					copy(dataBuf[off:], pdu.Data)
					haveIn = len(dataBuf)
				}
			case inPlace:
				// The segment was read straight into dataBuf, so only the high
				// water mark has to move.
				if off+len(pdu.Data) > haveIn {
					haveIn = off + len(pdu.Data)
				}
			default:
				copy(dataBuf[off:], pdu.Data)
				if off+len(pdu.Data) > haveIn {
					haveIn = off + len(pdu.Data)
				}
			}
			if h.Flags()&iscsi.DataInFlagStatus != 0 {
				res.Status = h[3]
				res.Residual = h.ResidualCount()
				res.Data = dataBuf[:haveIn]
				it.adoptSequence(h)
				return res, nil
			}
		case iscsi.OpR2T:
			if err := it.answerR2T(ctx, h, dataOut, &dataOutSN); err != nil {
				return nil, err
			}
			if sn := h.StatSN(); sn != 0 {
				it.expStatSN = sn + 1
			}
		case iscsi.OpSCSIResponse:
			res.Status = h[3]
			res.Residual = h.ResidualCount()
			res.Overflow = h.Flags()&iscsi.RespFlagOverflow != 0
			res.Underflow = h.Flags()&iscsi.RespFlagUnderflow != 0
			res.Sense = extractSense(pdu.Data)
			res.Data = dataBuf[:haveIn]
			it.adoptSequence(h)
			if h[2] != iscsi.ResponseCompleted {
				return nil, fmt.Errorf("backend: target failure, response=0x%02x", h[2])
			}
			return res, nil
		case iscsi.OpNOPIn:
			if err := it.handleNOPIn(ctx, h, pdu.Data); err != nil {
				return nil, err
			}
		case iscsi.OpAsyncMsg:
			it.log.Warn("backend: asynchronous message", "async_event", h[36])
		case iscsi.OpReject:
			return nil, fmt.Errorf("backend: PDU rejected, reason=%d", h[2])
		default:
			return nil, fmt.Errorf("backend: unexpected PDU %s", iscsi.OpcodeName(h.Opcode()))
		}
	}
}

// answerR2T sends the Data-Out PDUs that satisfy an R2T request.
func (it *Initiator) answerR2T(ctx context.Context, h *iscsi.BHS, dataOut []byte, dataSN *uint32) error {
	ttt := h.TTT()
	offset := int(h.BufOffset())
	desired := int(h.DesiredDataTransferLength())

	for desired > 0 {
		chunk := desired
		if it.targetMaxRecvDSL > 0 && chunk > it.targetMaxRecvDSL {
			chunk = it.targetMaxRecvDSL
		}
		if offset+chunk > len(dataOut) {
			return fmt.Errorf("backend: R2T beyond write buffer (offset=%d chunk=%d len=%d)", offset, chunk, len(dataOut))
		}
		desired -= chunk
		p := iscsi.NewPDU(iscsi.OpDataOut, dataOut[offset:offset+chunk])
		// Byte 1 of a Data-Out PDU is the F bit only (RFC 7143 section 11.7):
		// set it on the last PDU answering this R2T.
		var flags uint8
		if desired == 0 {
			flags = iscsi.FlagAlwaysSet
		}
		p.Header.SetFlags(flags)
		p.Header.SetLUN(0)
		p.Header.SetITT(h.ITT())
		p.Header.SetTTT(ttt)
		p.Header.SetExpStatSN(it.expStatSN)
		p.Header.SetDataSN(*dataSN)
		p.Header.SetBufOffset(uint32(offset))
		if err := it.writePDU(ctx, p); err != nil {
			return fmt.Errorf("backend: send data-out: %w", err)
		}
		*dataSN = *dataSN + 1
		offset += chunk
	}
	return nil
}

func (it *Initiator) handleNOPIn(ctx context.Context, h *iscsi.BHS, ping []byte) error {
	if h.TTT() == iscsi.ReservedTag {
		return nil // ping echo of our own NOP-Out
	}
	// A NOP-Out that answers a target NOP-In does not consume a CmdSN: the
	// target keeps its ExpCmdSN unchanged for it (verified against the backing
	// Microsoft target). Advancing it here makes every later command carry a
	// CmdSN the target is not waiting for, and such a target holds those
	// commands silently instead of rejecting them, which wedges the backend
	// session forever. The CmdSN field of this NOP-Out is not significant, so
	// it stays at the value the target expects next.
	p := iscsi.BuildNOPOut(h.ITT(), h.TTT(), it.cmdSN, it.expStatSN, ping)
	return it.writePDU(ctx, p)
}

// adoptSequence resyncs the CmdSN / ExpStatSN / MaxCmdSN counters with the
// values the target reports in a response. A strictly serial initiator must
// always send the CmdSN the target expects next, so trusting the target is both
// correct and self healing: any drift is corrected on the next response instead
// of silently wedging the session.
func (it *Initiator) adoptSequence(h *iscsi.BHS) {
	if sn := h.StatSN(); sn != 0 {
		it.expStatSN = sn + 1
	}
	if sn := h.ExpCmdSN(); sn != 0 {
		it.cmdSN = sn
	}
	if m := h.MaxCmdSN(); m != 0 {
		it.maxCmdSN = m
	}
}

func extractSense(data []byte) []byte {
	if len(data) < 2 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(data[0:2]))
	if n == 0 || 2+n > len(data) {
		return nil
	}
	return data[2 : 2+n]
}
