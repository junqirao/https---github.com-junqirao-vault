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
	"sync/atomic"
	"time"

	"vault/internal/iscsicache/iscsi"
)

// Defaults for the MVP initiator.
const (
	// DefaultMaxRecvDataSegmentLength is the largest Data-In segment this
	// initiator advertises it can receive. It is the size of the PDUs the
	// backing target sends back, so it is also the granularity at which a
	// streamed read can be forwarded: a segment is handed to the command's own
	// goroutine the moment it is read, so the next segment is read off the
	// backing link while the previous one is being written to the client. One
	// transfer-sized PDU leaves nothing to overlap — the read and the write run
	// back to back — so a larger value is slower, not faster, once the payload
	// is bigger than one PDU. 256 KiB is what production initiators advertise
	// and equals DefaultMaxBurstLength, so a MiB read arrives as four segments:
	// enough to keep the reader ahead of the writer, few enough that the
	// per-PDU cost stays negligible. The key is negotiable, so a target that
	// caps it below this value is handled transparently.
	DefaultMaxRecvDataSegmentLength = 256 << 10
	DefaultMaxBurstLength           = 262144
	DefaultDialTimeout              = 10 * time.Second
	DefaultIOTimeout                = 30 * time.Second
	// DefaultSocketBufferSize is the per-direction socket buffer requested on the
	// backing connection. A Data-Out burst or a streamed Data-In PDU is written in
	// one call, and the kernel only takes what its send buffer has room for, so a
	// small buffer turns one transfer into many partial writes.
	DefaultSocketBufferSize = 4 << 20
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

	// SocketBufferSize is the read/write socket buffer requested on the backing
	// connection. Zero applies DefaultSocketBufferSize.
	SocketBufferSize int

	// Sessions is how many independent sessions DialPool opens and fans commands
	// over (see Pool). Zero applies DefaultSessions; values above MaxSessions are
	// clamped. It is ignored by Dial, which always opens exactly one session.
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
	if c.SocketBufferSize <= 0 {
		c.SocketBufferSize = DefaultSocketBufferSize
	}
	if c.InitiatorIQN == "" {
		c.InitiatorIQN = DefaultInitiatorIQN()
	}
	if c.Sessions <= 0 {
		c.Sessions = DefaultSessions
	}
	if c.Sessions > MaxSessions {
		c.Sessions = MaxSessions
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

// Initiator is a single-connection pure-Go iSCSI initiator that pipelines
// commands: one reader goroutine owns the socket and dispatches every response
// PDU to the command that asked for it by Initiator Task Tag, so any number of
// commands can be outstanding on the one session.
//
// That is how a real initiator spends its command window, and it is what keeps
// the backing link busy. The previous design ran exactly one command per session
// and stacked sessions in a Pool to recover a queue depth; the sessions had to
// be dialled while the workload was already running, so the act of buying queue
// depth competed for the very link that depth was meant to fill.
//
// The reader owns the socket for reading and is the only goroutine that reads;
// every writer goes through writeMu. A command registers a pendingCmd, writes
// its SCSI Command PDU, and waits on the command's channel, which the reader
// closes with the result.
type Initiator struct {
	cfg Config
	log *slog.Logger

	conn net.Conn

	// writeMu serialises every PDU written. The reader writes too (Data-Out
	// bursts answering an R2T, NOP-Out echoes of a NOP-In), so it is the one lock
	// shared between the reader and the command path.
	writeMu sync.Mutex

	// sendMu makes "take the next CmdSN and put its command on the wire" one
	// step. CmdSN is a wire-order number: a target processes commands in CmdSN
	// order and a real one treats a command that arrives with a CmdSN below one
	// it has already seen as a protocol error — Microsoft's target stops
	// answering the session outright. Allocating the CmdSN under a CAS and then
	// writing under writeMu lets two commands swap places (the allocator of
	// sn=N can lose the CPU before it writes while sn=N+1 takes writeMu), so the
	// window can carry CmdSN 6 and only later 5. Holding sendMu across both
	// keeps the socket's CmdSN strictly increasing; it is held only for the
	// allocation and the write, never across a wait.
	sendMu sync.Mutex

	// mu guards the command registry and the shutdown state. It is held only for
	// the map operations, never across I/O.
	mu       sync.Mutex
	closed   bool
	failErr  error
	inflight map[uint32]*pendingCmd
	// windowCh is closed and replaced whenever the target advances MaxCmdSN, to
	// wake commands waiting for room in the command window.
	windowCh  chan struct{}
	closeOnce sync.Once

	// done is closed when the session fails or is closed, so a command never
	// blocks past the death of the session.
	done chan struct{}

	info DeviceInfo
	isid [6]byte
	tsih uint16

	itt       atomic.Uint32
	cmdSN     atomic.Uint32
	expStatSN atomic.Uint32
	maxCmdSN  atomic.Uint32

	// writeSetAt is the unix-nano time the socket write deadline was last
	// rewritten, so setWriteDeadline can keep the rewrite off the per-PDU path.
	writeSetAt atomic.Int64

	// scratch is the reader's reusable buffer for segments that do not land in a
	// command's own destination (sense data, a NOP-In ping, a Data-In beyond the
	// transfer). Only the reader goroutine touches it.
	scratch []byte

	// negotiated parameters
	ourMaxRecvDSL    int
	targetMaxRecvDSL int
	maxBurstLength   int

	// immediateData is a negotiated ImmediateData=Yes: the first slice of a
	// write rides along with the command PDU, so the write does not wait a
	// round trip for an R2T before any data flows. unsolicited is a negotiated
	// InitialR2T=No: further unsolicited Data-Out PDUs may follow, up to the
	// target's firstBurstLength. A target that does not explicitly agree keeps
	// both off, which is the old R2T-only behaviour.
	immediateData    bool
	unsolicited      bool
	firstBurstLength int
}

// streamQueueDepth is how many Data-In segments a streamed command may have
// handed over but not yet written. One segment in flight is enough to overlap a
// command's client-facing write with the next PDU's read; a couple more absorb a
// busy backing link without letting a stalled client pin transfer-sized buffers
// across the whole command window.
const streamQueueDepth = 2

// segment is one Data-In data segment passed from the reader to a streamed
// command's own goroutine, which owns data once it arrives.
type segment struct {
	off  int64
	data []byte
	last bool
	res  *Result
}

// segmentBufs recycles the buffers streamed segments are read into. A buffer is
// stored behind a pointer so returning one does not box a slice header.
var segmentBufs = sync.Pool{New: func() any { return new([]byte) }}

func getSegmentBuf(n int) []byte {
	bp := segmentBufs.Get().(*[]byte)
	if b := *bp; cap(b) >= n {
		*bp = nil
		return b[:n]
	}
	*bp = nil
	segmentBufs.Put(bp)
	return make([]byte, n)
}

func putSegmentBuf(b []byte) {
	if cap(b) == 0 {
		return
	}
	bp := segmentBufs.Get().(*[]byte)
	*bp = b[:0]
	segmentBufs.Put(bp)
}

// pendingCmd is one command in flight. The reader fills res/err and closes done;
// the command goroutine waits on it. The read destination and the write payload
// are owned by the command goroutine and only read by the reader, so they need
// no lock: closing done is the happens-before edge.
type pendingCmd struct {
	itt     uint32
	dataOut []byte
	buf     []byte // read destination, nil when the command reads nothing
	haveIn  int

	// sink, when set, receives the read payload incrementally and buf is unused.
	// Only the reader touches it and sinkErr, so neither needs a lock.
	sink    DataInSink
	sinkErr error
	// inCh carries segments from the reader to the command's goroutine. Only the
	// reader sends; the command goroutine drains it in forwardSegments.
	inCh chan segment

	// dataOutSN is the next DataSN for a Data-Out burst. Only the reader touches
	// it, because only the reader answers an R2T — except during the
	// unsolicited phase below, which the command goroutine owns.
	dataOutSN uint32

	// The unsolicited phase of a write: with InitialR2T=No the command
	// goroutine streams the first burst without an R2T, and the target may
	// already have R2Ted the remainder. The reader must not answer that R2T
	// before the unsolicited PDUs are on the wire, because DataPDUInOrder=Yes
	// forbids putting a task's data PDUs on the wire out of offset order; it
	// stashes the R2T here and the command goroutine answers it right after
	// the unsolicited phase. unsolDone is true unless an unsolicited phase is
	// in progress; the pair is guarded by unsolMu. At most one R2T can arrive
	// early because the negotiated MaxOutstandingR2T is 1.
	unsolMu   sync.Mutex
	unsolDone bool
	earlyR2T  *iscsi.BHS

	res  *Result
	err  error
	once sync.Once
	done chan struct{}
}

func (c *pendingCmd) complete(res *Result, err error) {
	c.once.Do(func() {
		c.res, c.err = res, err
		close(c.done)
	})
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
		// Size the socket buffers so a full Data-Out burst or Data-In PDU is written
		// in one call instead of many partial ones.
		_ = tc.SetReadBuffer(cfg.SocketBufferSize)
		_ = tc.SetWriteBuffer(cfg.SocketBufferSize)
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
		inflight:         make(map[uint32]*pendingCmd),
		windowCh:         make(chan struct{}),
		done:             make(chan struct{}),
		ourMaxRecvDSL:    cfg.MaxRecvDataSegmentLength,
		targetMaxRecvDSL: DefaultMaxRecvDataSegmentLength,
		maxBurstLength:   cfg.MaxBurstLength,
		// RFC 7143 Appendix A default FirstBurstLength; a target that announces
		// its own value in the login response replaces it.
		firstBurstLength: 8192,
	}
	it.cmdSN.Store(1)
	if _, err := rand.Read(it.isid[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("backend: generate isid: %w", err)
	}
	if err := it.login(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	// From here on the socket is owned by the reader, and every command —
	// including the discovery below — goes through the pipelined path.
	go it.readLoop()
	if err := it.discover(ctx); err != nil {
		_ = it.Close()
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
	it.logout()
	it.fail(ErrClosed)
	return nil
}

// Closed reports whether the session has failed or been closed. A Pool uses it
// to retire a session that a command found gone, rather than guessing from the
// error, which cannot tell a dead session from a single failed command.
func (it *Initiator) Closed() bool {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.closed
}

// logout sends a best-effort Logout Request. The session is going away either
// way, so the request is only a courtesy to the target.
func (it *Initiator) logout() {
	it.mu.Lock()
	closed := it.closed
	it.mu.Unlock()
	if closed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), it.cfg.IOTimeout)
	defer cancel()
	it.writeMu.Lock()
	defer it.writeMu.Unlock()
	p := iscsi.NewPDU(iscsi.OpLogoutReq, nil)
	p.Header.SetFlags(iscsi.FlagAlwaysSet)
	p.Header.SetITT(it.nextITT())
	p.Header.SetTTT(iscsi.ReservedTag)
	p.Header.SetCmdSN(it.cmdSN.Load())
	p.Header.SetExpStatSN(it.expStatSN.Load())
	p.Header[2] = 0 // reason code: close session
	_ = it.writePDULocked(ctx, p)
}

// fail tears the session down: it records the error, closes the socket (which
// unblocks the reader), fails every command still in flight and wakes anything
// waiting on the command window. It runs exactly once.
func (it *Initiator) fail(err error) {
	it.closeOnce.Do(func() {
		it.mu.Lock()
		it.closed = true
		it.failErr = err
		cmds := it.inflight
		it.inflight = make(map[uint32]*pendingCmd)
		ch := it.windowCh
		it.windowCh = make(chan struct{})
		it.mu.Unlock()

		close(ch)
		_ = it.conn.Close()
		for _, c := range cmds {
			c.complete(nil, err)
		}
		close(it.done)
	})
}

// shutdownErr returns the error the session died with.
func (it *Initiator) shutdownErr() error {
	it.mu.Lock()
	err := it.failErr
	it.mu.Unlock()
	if err == nil {
		err = ErrClosed
	}
	return err
}

func (it *Initiator) nextITT() uint32 {
	for {
		v := it.itt.Add(1)
		if v != 0 && v != iscsi.ReservedTag {
			return v
		}
	}
}

// register/lookup/unregister manage the command registry. They are the only
// operations that touch it.mu on the hot path.

func (it *Initiator) register(c *pendingCmd) bool {
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.closed {
		return false
	}
	it.inflight[c.itt] = c
	return true
}

func (it *Initiator) lookup(itt uint32) *pendingCmd {
	it.mu.Lock()
	c := it.inflight[itt]
	it.mu.Unlock()
	return c
}

func (it *Initiator) unregister(itt uint32) {
	it.mu.Lock()
	delete(it.inflight, itt)
	it.mu.Unlock()
}

// deadline helpers --------------------------------------------------------------

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

// writeDeadlineRefresh bounds how often the socket write deadline is rewritten.
// Rewriting one takes the runtime poll lock that every read and write on the fd
// shares, and the command path would otherwise pay it once per PDU. The deadline
// is measured in seconds, so a value one refresh stale still bounds a blocked
// write within the configured timeout.
const writeDeadlineRefresh = time.Second

func (it *Initiator) setWriteDeadline(ctx context.Context) {
	d := it.cfg.IOTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && (d == 0 || rem < d) {
			d = rem
		}
	}
	if d <= 0 {
		return
	}
	now := time.Now()
	if prev := it.writeSetAt.Load(); prev != 0 && now.UnixNano()-prev < int64(writeDeadlineRefresh) {
		return
	}
	it.writeSetAt.Store(now.UnixNano())
	_ = it.conn.SetWriteDeadline(now.Add(d))
}

// readPDU reads one PDU straight off the socket. It is used only during login,
// before the reader goroutine takes ownership of the socket.
func (it *Initiator) readPDU(ctx context.Context) (*iscsi.PDU, error) {
	it.setReadDeadline(ctx)
	return iscsi.ReadPDU(it.conn)
}

// writePDU serialises a PDU under the write lock.
func (it *Initiator) writePDU(ctx context.Context, p *iscsi.PDU) error {
	it.writeMu.Lock()
	defer it.writeMu.Unlock()
	return it.writePDULocked(ctx, p)
}

// writePDULocked writes a PDU; the caller must hold it.writeMu.
func (it *Initiator) writePDULocked(ctx context.Context, p *iscsi.PDU) error {
	it.setWriteDeadline(ctx)
	return iscsi.WritePDU(it.conn, p)
}

// Reader ------------------------------------------------------------------------------
//
// The reader goroutine is the only goroutine that reads the socket. It reads the
// BHS, then the data segment (into the command's own destination when it fits,
// so a read pays no per-PDU copy), then dispatches on the opcode.

func (it *Initiator) readLoop() {
	// Login drives the socket through readPDU, which arms a read deadline from
	// the login context. That deadline is absolute and is still set when the
	// reader takes over, so the first read that blocks past it fails with an
	// i/o timeout and the session tears itself down even though the target is
	// healthy — a session that merely went idle for a while dies. The reader
	// owns the socket from here and must not inherit it: a backing session has to
	// survive an idle period (a mounted disk can sit untouched for hours and the
	// pool does not redial a retired session), while every command is bounded by
	// its own context (see exec), so the reader blocks without a deadline and
	// leans on TCP keepalive to notice a peer that has really gone away.
	_ = it.conn.SetReadDeadline(time.Time{})
	for {
		h, err := iscsi.ReadBHS(it.conn)
		if err != nil {
			it.fail(fmt.Errorf("backend: read: %w", err))
			return
		}
		if err := it.handlePDU(&h); err != nil {
			it.log.Warn("backend: session error", "err", err)
			it.fail(err)
			return
		}
	}
}

func (it *Initiator) handlePDU(h *iscsi.BHS) error {
	switch h.Opcode() {
	case iscsi.OpDataIn:
		seg, inPlace, c, err := it.readDataInSegment(h)
		if err != nil {
			return err
		}
		return it.handleDataIn(c, h, seg, inPlace)
	case iscsi.OpSCSIResponse:
		seg, err := it.readScratchSegment(h)
		if err != nil {
			return err
		}
		it.handleSCSIResponse(h, seg)
	case iscsi.OpR2T:
		return it.handleR2T(h)
	case iscsi.OpNOPIn:
		seg, err := it.readScratchSegment(h)
		if err != nil {
			return err
		}
		return it.handleNOPIn(h, seg)
	case iscsi.OpAsyncMsg:
		if _, err := it.readScratchSegment(h); err != nil {
			return err
		}
		it.log.Warn("backend: asynchronous message", "async_event", h[36])
	case iscsi.OpReject:
		if _, err := it.readScratchSegment(h); err != nil {
			return err
		}
		return fmt.Errorf("backend: PDU rejected, reason=%d", h[2])
	default:
		if _, err := it.readScratchSegment(h); err != nil {
			return err
		}
		it.log.Warn("backend: unexpected PDU", "opcode", iscsi.OpcodeName(h.Opcode()))
	}
	return nil
}

// readDataInSegment reads a Data-In data segment. A segment that fits inside the
// destination of the command it belongs to is read straight into that buffer, so
// the read path pays neither a per-PDU allocation nor a copy; inPlace reports
// that case.
//
// The command is looked up here and returned, because the segment can only be
// read once its destination is known. Looking it up again when the segment is
// dispatched would put a second registry lock on every Data-In PDU.
func (it *Initiator) readDataInSegment(h *iscsi.BHS) (seg []byte, inPlace bool, c *pendingCmd, err error) {
	dsl := h.DataSegmentLength()
	c = it.lookup(h.ITT())
	if dsl == 0 {
		return nil, false, c, nil
	}
	if c != nil && c.sink != nil {
		// A streamed segment is handed to the command's own goroutine, which writes
		// it to its client while this reader goes on to the next PDU. It therefore
		// needs a buffer of its own: the reader's scratch buffer would be
		// overwritten under the forwarder.
		seg = getSegmentBuf(dsl)
		if err := iscsi.ReadSegment(it.conn, h, seg); err != nil {
			putSegmentBuf(seg)
			return nil, false, nil, err
		}
		return seg, false, c, nil
	}
	if c != nil && c.buf != nil {
		if off := int(h.BufOffset()); off+dsl <= len(c.buf) {
			seg = c.buf[off : off+dsl]
			return seg, true, c, iscsi.ReadSegment(it.conn, h, seg)
		}
	}
	seg, err = it.readScratchSegment(h)
	return seg, false, c, err
}

// readScratchSegment reads a data segment into the reader's reusable buffer.
func (it *Initiator) readScratchSegment(h *iscsi.BHS) ([]byte, error) {
	dsl := h.DataSegmentLength()
	if dsl == 0 {
		return nil, nil
	}
	if cap(it.scratch) < dsl {
		it.scratch = make([]byte, dsl)
	}
	seg := it.scratch[:dsl]
	return seg, iscsi.ReadSegment(it.conn, h, seg)
}

// handleDataIn dispatches a Data-In PDU to the command the reader already
// resolved for it. The command is nil when it finished or timed out before its
// late reply arrived, in which case the PDU is dropped.
func (it *Initiator) handleDataIn(c *pendingCmd, h *iscsi.BHS, seg []byte, inPlace bool) error {
	if c == nil {
		return nil // command already finished or timed out
	}
	if c.sink != nil {
		return it.streamSegment(c, h, seg)
	}
	off := int(h.BufOffset())
	switch {
	case c.buf == nil:
		it.log.Warn("backend: data-in for a command with no read buffer", "itt", h.ITT(), "len", len(seg))
	case off+len(seg) > len(c.buf):
		// Target sent more than requested: keep what fits and note the residual.
		it.log.Warn("backend: data-in beyond expected length",
			"offset", off, "len", len(seg), "expected", len(c.buf))
		if off < len(c.buf) {
			copy(c.buf[off:], seg)
			c.haveIn = len(c.buf)
		}
	case inPlace:
		// The segment was read straight into c.buf, so only the high water mark
		// has to move.
		if off+len(seg) > c.haveIn {
			c.haveIn = off + len(seg)
		}
	default:
		copy(c.buf[off:], seg)
		if off+len(seg) > c.haveIn {
			c.haveIn = off + len(seg)
		}
	}
	if h.Flags()&iscsi.DataInFlagStatus != 0 {
		it.adoptSequence(h)
		c.complete(&Result{Status: h[3], Residual: h.ResidualCount(), Data: c.buf[:c.haveIn]}, nil)
	}
	return nil
}

// streamSegment hands one Data-In segment to the command's own goroutine, which
// writes it to its client. Doing the client-facing write here instead would put
// every command's socket write on the one reader that serves the whole command
// window, so one slow client would hold up the session for all of them.
//
// A handoff that cannot complete (the command already finished or the session is
// gone) drops the segment. A sink error fails only this command: the rest of its
// transfer is dropped as it arrives and the session stays usable for the other
// commands sharing it.
func (it *Initiator) streamSegment(c *pendingCmd, h *iscsi.BHS, seg []byte) error {
	if c.sinkErr != nil {
		putSegmentBuf(seg)
		return nil
	}
	last := h.Flags()&iscsi.DataInFlagStatus != 0
	res := &Result{}
	if last {
		it.adoptSequence(h)
		res.Status = h[3]
		res.Residual = h.ResidualCount()
		res.Overflow = h.Flags()&iscsi.RespFlagOverflow != 0
		res.Underflow = h.Flags()&iscsi.RespFlagUnderflow != 0
	}
	select {
	case c.inCh <- segment{off: int64(h.BufOffset()), data: seg, last: last, res: res}:
	case <-c.done:
		putSegmentBuf(seg)
	case <-it.done:
		putSegmentBuf(seg)
	}
	return nil
}

// forwardSegments is the streamed command's own goroutine: it drains the
// segments the reader reads off the socket and gives each to the sink, so the
// client-facing write runs beside the reader instead of on it.
//
// It owns the command's completion, because the caller must not be told the
// command is done before the last segment has actually been written out.
func (it *Initiator) forwardSegments(ctx context.Context, c *pendingCmd) (*Result, error) {
	for {
		select {
		case seg, ok := <-c.inCh:
			if !ok {
				return c.res, c.err
			}
			err := c.sink.Segment(seg.off, seg.data, seg.last, seg.res)
			putSegmentBuf(seg.data)
			if err != nil {
				c.sinkErr = err
				c.complete(nil, err)
				return nil, err
			}
			// The last segment has been written out, so the command is complete.
			if seg.last {
				c.complete(seg.res, nil)
				return seg.res, nil
			}
		case <-c.done:
			return c.res, c.err
		case <-ctx.Done():
			c.complete(nil, ctx.Err())
			return nil, fmt.Errorf("backend: command timed out after %s: %w", it.cfg.IOTimeout, ctx.Err())
		case <-it.done:
			return nil, it.shutdownErr()
		}
	}
}

func (it *Initiator) handleSCSIResponse(h *iscsi.BHS, seg []byte) {
	c := it.lookup(h.ITT())
	if c == nil {
		return
	}
	it.adoptSequence(h)
	res := &Result{
		Status:    h[3],
		Residual:  h.ResidualCount(),
		Overflow:  h.Flags()&iscsi.RespFlagOverflow != 0,
		Underflow: h.Flags()&iscsi.RespFlagUnderflow != 0,
		Sense:     extractSense(seg),
	}
	if c.buf != nil {
		res.Data = c.buf[:c.haveIn]
	}
	if h[2] != iscsi.ResponseCompleted {
		c.complete(res, fmt.Errorf("backend: target failure, response=0x%02x", h[2]))
		return
	}
	c.complete(res, nil)
}

// handleR2T answers a Ready-To-Transfer from the reader: an R2T asks for the
// next slice of a write payload, and replying here keeps the command goroutine
// free to wait on its result channel. An R2T that arrives while the command
// goroutine is still streaming its unsolicited first burst is stashed on the
// command instead (see pendingCmd.unsolDone); the command answers it once the
// unsolicited phase is over.
func (it *Initiator) handleR2T(h *iscsi.BHS) error {
	it.adoptStatSN(h.StatSN())
	c := it.lookup(h.ITT())
	if c == nil {
		return nil
	}
	c.unsolMu.Lock()
	defer c.unsolMu.Unlock()
	if c.unsolDone {
		return it.answerR2T(c, h)
	}
	r2t := *h // the header lives in the reader's PDU and is reused
	c.earlyR2T = &r2t
	return nil
}

// answerR2T sends the Data-Out PDUs that satisfy an R2T request.
func (it *Initiator) answerR2T(c *pendingCmd, h *iscsi.BHS) error {
	ttt := h.TTT()
	offset := int(h.BufOffset())
	desired := int(h.DesiredDataTransferLength())
	if desired == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), it.cfg.IOTimeout)
	defer cancel()

	it.writeMu.Lock()
	defer it.writeMu.Unlock()
	for desired > 0 {
		chunk := desired
		if it.targetMaxRecvDSL > 0 && chunk > it.targetMaxRecvDSL {
			chunk = it.targetMaxRecvDSL
		}
		if offset+chunk > len(c.dataOut) {
			return fmt.Errorf("backend: R2T beyond write buffer (offset=%d chunk=%d len=%d)", offset, chunk, len(c.dataOut))
		}
		desired -= chunk
		p := iscsi.NewPDU(iscsi.OpDataOut, c.dataOut[offset:offset+chunk])
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
		p.Header.SetExpStatSN(it.expStatSN.Load())
		p.Header.SetDataSN(c.dataOutSN)
		p.Header.SetBufOffset(uint32(offset))
		if err := it.writePDULocked(ctx, p); err != nil {
			return fmt.Errorf("backend: send data-out: %w", err)
		}
		c.dataOutSN++
		offset += chunk
	}
	return nil
}

// sendUnsolicited streams dataOut[from:firstBurstLength] as unsolicited
// Data-Out PDUs (TTT reserved, RFC 7143 §7.7) and then declares the
// unsolicited phase over, answering any R2T the target sent in the meantime.
// It runs on the command's goroutine. unsolDone must already be false.
//
// writeMu is released before unsolMu is taken, so the lock pair never nests
// with handleR2T (which takes unsolMu and then writeMu inside answerR2T).
func (it *Initiator) sendUnsolicited(ctx context.Context, c *pendingCmd, from int) error {
	end := len(c.dataOut)
	if it.firstBurstLength > 0 && end > it.firstBurstLength {
		end = it.firstBurstLength
	}
	it.writeMu.Lock()
	for from < end {
		chunk := end - from
		if it.targetMaxRecvDSL > 0 && chunk > it.targetMaxRecvDSL {
			chunk = it.targetMaxRecvDSL
		}
		p := iscsi.NewPDU(iscsi.OpDataOut, c.dataOut[from:from+chunk])
		var flags uint8
		if from+chunk >= end {
			// F bit on the last unsolicited PDU of the task (RFC 7143 §7.7).
			flags = iscsi.FlagAlwaysSet
		}
		p.Header.SetFlags(flags)
		p.Header.SetLUN(0)
		p.Header.SetITT(c.itt)
		p.Header.SetTTT(iscsi.ReservedTag)
		p.Header.SetExpStatSN(it.expStatSN.Load())
		p.Header.SetDataSN(c.dataOutSN)
		p.Header.SetBufOffset(uint32(from))
		if err := it.writePDULocked(ctx, p); err != nil {
			it.writeMu.Unlock()
			return err
		}
		c.dataOutSN++
		from += chunk
	}
	it.writeMu.Unlock()

	c.unsolMu.Lock()
	c.unsolDone = true
	early := c.earlyR2T
	c.earlyR2T = nil
	c.unsolMu.Unlock()
	if early != nil {
		return it.answerR2T(c, early)
	}
	return nil
}

func (it *Initiator) handleNOPIn(h *iscsi.BHS, ping []byte) error {
	if h.TTT() == iscsi.ReservedTag {
		return nil // ping echo of our own NOP-Out
	}
	// A NOP-Out that answers a target NOP-In does not consume a CmdSN: the
	// target keeps its ExpCmdSN unchanged for it (verified against the backing
	// Microsoft target). Advancing it here makes every later command carry a
	// CmdSN the target is not waiting for, and such a target holds those
	// commands silently instead of rejecting them, which wedges the session
	// forever. The CmdSN field of this NOP-Out is therefore the value the target
	// expects next.
	ctx, cancel := context.WithTimeout(context.Background(), it.cfg.IOTimeout)
	defer cancel()
	it.writeMu.Lock()
	defer it.writeMu.Unlock()
	p := iscsi.BuildNOPOut(h.ITT(), h.TTT(), it.cmdSN.Load(), it.expStatSN.Load(), ping)
	return it.writePDULocked(ctx, p)
}

// Sequence numbers -------------------------------------------------------------------
//
// A pipelined initiator owns its CmdSN: it allocates one per command, in order,
// and never lets a response move it backwards (that would reorder commands
// already on the wire). It only honours the target's MaxCmdSN window, which is
// the flow control that keeps the target from being overrun.

func (it *Initiator) adoptSequence(h *iscsi.BHS) {
	it.adoptStatSN(h.StatSN())
	it.setMaxCmdSN(h.MaxCmdSN())
}

func (it *Initiator) adoptStatSN(sn uint32) {
	if sn != 0 {
		it.expStatSN.Store(sn + 1)
	}
}

// setMaxCmdSN records that the target will accept commands up to m and wakes any
// command blocked on the window.
func (it *Initiator) setMaxCmdSN(m uint32) {
	if m == 0 {
		return
	}
	for {
		cur := it.maxCmdSN.Load()
		if m <= cur {
			return
		}
		if it.maxCmdSN.CompareAndSwap(cur, m) {
			it.mu.Lock()
			close(it.windowCh)
			it.windowCh = make(chan struct{})
			it.mu.Unlock()
			return
		}
	}
}

// takeCmdSN allocates the next command sequence number and returns with sendMu
// held, so the caller's command is built and written to the socket before any
// other command can take a later CmdSN. That is what keeps CmdSN strictly
// increasing on the wire: a target processes commands in CmdSN order, and one
// that receives a command whose CmdSN is below another it has already seen
// treats the session as broken.
//
// It waits for room in the target's window with sendMu released, because the
// room only appears in a response the reader delivers and the reader must be
// free to make progress. A zero MaxCmdSN means the target has not narrowed the
// window, so nothing blocks.
func (it *Initiator) takeCmdSN(ctx context.Context) (uint32, error) {
	for {
		it.sendMu.Lock()
		if max := it.maxCmdSN.Load(); max != 0 && it.cmdSN.Load() > max {
			it.sendMu.Unlock()
			it.mu.Lock()
			if it.closed {
				err := it.failErr
				it.mu.Unlock()
				if err == nil {
					err = ErrClosed
				}
				return 0, err
			}
			ch := it.windowCh
			it.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-it.done:
				return 0, it.shutdownErr()
			}
			continue
		}
		sn := it.cmdSN.Add(1) - 1
		return sn, nil
	}
}

// Login ------------------------------------------------------------------------------

func (it *Initiator) operationalKeys() [][2]string {
	return [][2]string{
		{"SessionType", "Normal"},
		// R2T-only (InitialR2T=Yes, ImmediateData=No) — the pair the MVP has
		// always run and the one proven against the real Microsoft target.
		// Offering No/Yes made the initiator stream an unsolicited first burst
		// and stop at the target's 8 KiB FirstBurstLength, then wait an R2T for
		// each 256 KiB burst — extra round trips on every 1 MiB write, which is
		// the shape that regressed CDM sequential writes. A target that counters
		// with No/Yes still drives the unsolicited path via the negotiated
		// flags; this only fixes what we OFFER.
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
		it.cmdSN.Store(resp.Header.ExpCmdSN())
		if sn := resp.Header.StatSN(); sn != 0 {
			it.expStatSN.Store(sn + 1)
		}
		it.setMaxCmdSN(resp.Header.MaxCmdSN())
		text := iscsi.ParseText(resp.Data)
		if v, ok := text["maxrecvdatasegmentlength"]; ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				it.targetMaxRecvDSL = n
			}
		}
		// RFC 7143 §11.1.4: unsolicited data is negotiated only when BOTH sides
		// offered InitialR2T=No, and immediate data only when BOTH offered
		// ImmediateData=Yes. We declare Yes/No (see operationalKeys), so these
		// stay false whatever the target echoes — every write is R2T-driven.
		if v, ok := text["initialr2t"]; ok {
			it.unsolicited = it.unsolicited && strings.EqualFold(strings.TrimSpace(v), "no")
		}
		if v, ok := text["immediatedata"]; ok {
			it.immediateData = it.immediateData && strings.EqualFold(strings.TrimSpace(v), "yes")
		}
		if v, ok := text["firstburstlength"]; ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				it.firstBurstLength = n
			}
		}
		if it.unsolicited || it.immediateData {
			it.log.Debug("login write negotiation",
				"initial_r2t", !it.unsolicited, "immediate_data", it.immediateData,
				"first_burst_length", it.firstBurstLength)
		}
		it.log.Debug("login exchange",
			"iter", iter, "csg", resp.Header.LoginCSG(), "nsg", resp.Header.LoginNSG(),
			"transit", resp.Header.LoginTransit(), "continue", resp.Header.LoginContinue(),
			"status_class", resp.Header.LoginStatusClass(), "keys", len(text),
			"expcmdsn", resp.Header.ExpCmdSN(), "maxcmdsn", resp.Header.MaxCmdSN(),
			"statsn", resp.Header.StatSN())

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
	return it.exec(ctx, cdb, dataOut, nil, inLen, nil)
}

// ExecInto issues one SCSI command whose read payload is written straight into
// buf, so a caller that already has a destination pays neither an allocation
// nor a copy for the transfer. Result.Data aliases buf and is not a copy of it,
// which makes this the read path for a cache or proxy that owns its buffers.
func (it *Initiator) ExecInto(ctx context.Context, cdb []byte, dataOut []byte, buf []byte) (*Result, error) {
	if len(buf) == 0 {
		return nil, errors.New("backend: empty read buffer")
	}
	return it.exec(ctx, cdb, dataOut, buf, len(buf), nil)
}

// ExecStream issues a read and delivers its payload to sink as each Data-In
// segment arrives from the target, instead of buffering the transfer whole. It
// is what a proxy uses to forward a read while it is still arriving.
//
// The sink is called on the command's own goroutine, so it may write the segment
// out to the caller's client without holding up the reader that serves the other
// commands sharing the session. It is still called in order, and only one
// segment at a time.
func (it *Initiator) ExecStream(ctx context.Context, cdb []byte, inLen int, sink DataInSink) (*Result, error) {
	if sink == nil {
		return nil, errors.New("backend: stream requires a sink")
	}
	return it.exec(ctx, cdb, nil, nil, inLen, sink)
}

func (it *Initiator) exec(ctx context.Context, cdb []byte, dataOut []byte, buf []byte, inLen int, sink DataInSink) (*Result, error) {
	if len(cdb) == 0 {
		return nil, errors.New("backend: empty CDB")
	}
	it.mu.Lock()
	if it.closed {
		err := it.failErr
		it.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return nil, err
	}
	it.mu.Unlock()

	// Bound the whole command. The wait below selects on this context as well as
	// on the command's channel, so a target that simply never answers cannot
	// leave the caller waiting forever. The reader keeps running, so a command
	// that timed out does not desynchronise the session: any late PDU for it is
	// dropped by the reader.
	ctx, cancel := context.WithTimeout(ctx, it.cfg.IOTimeout)
	defer cancel()

	expLen := inLen
	if len(dataOut) > 0 {
		expLen = len(dataOut)
	}
	// A streamed read owns no destination buffer: the payload is handed out as it
	// arrives.
	if buf == nil && sink == nil && inLen > 0 {
		buf = make([]byte, inLen)
	}

	// takeCmdSN returns with sendMu held: the command is built and written below
	// before any other command can take a CmdSN, so the socket carries CmdSN in
	// ascending order.
	sn, err := it.takeCmdSN(ctx)
	if err != nil {
		return nil, err
	}
	itt := it.nextITT()
	c := &pendingCmd{itt: itt, dataOut: dataOut, buf: buf, sink: sink, done: make(chan struct{}), unsolDone: true}
	if sink != nil {
		c.inCh = make(chan segment, streamQueueDepth)
	}
	if !it.register(c) {
		it.sendMu.Unlock()
		return nil, it.shutdownErr()
	}
	defer it.unregister(itt)

	cmd := iscsi.BuildSCSICommand(itt, sn, it.expStatSN.Load(), 0, cdb, expLen, inLen > 0, len(dataOut) > 0)
	// Immediate data (RFC 7143 §7.5.2): with ImmediateData negotiated to Yes
	// the first slice of the payload rides along with the command PDU, so the
	// write does not wait a round trip for an R2T before any data flows. It is
	// bounded by the target's MaxRecvDataSegmentLength and FirstBurstLength,
	// and it counts as the task's DataSN 0. With InitialR2T=No the rest of the
	// first burst follows as unsolicited Data-Out PDUs; unsolDone is cleared
	// before the command PDU is written, because the target may R2T the
	// remainder the moment it sees the command and the reader must stash that
	// R2T instead of answering before the unsolicited PDUs are on the wire.
	unsent := 0
	if len(dataOut) > 0 && it.immediateData {
		imm := len(dataOut)
		if it.firstBurstLength > 0 && imm > it.firstBurstLength {
			imm = it.firstBurstLength
		}
		if it.targetMaxRecvDSL > 0 && imm > it.targetMaxRecvDSL {
			imm = it.targetMaxRecvDSL
		}
		cmd.Data = dataOut[:imm]
		unsent = imm
		c.dataOutSN = 1
	}
	if len(dataOut) > 0 && it.unsolicited && it.firstBurstLength > unsent && unsent < len(dataOut) {
		c.unsolDone = false
		// Unsolicited Data-Out PDUs will follow this PDU, so its F bit must be
		// clear (RFC 7143 §7.3.2). When the immediate data already covers the
		// whole first burst none follow, and the F bit stays set. This is the
		// only branch that lets the reader stash an early R2T, so it must not
		// be taken when no unsolicited PDUs are coming.
		cmd.Header.SetFlags(cmd.Header.Flags() &^ iscsi.CmdFlagFinal)
	}
	if it.log.Enabled(ctx, slog.LevelDebug) {
		it.log.Debug("backend: pdu send", "opcode", "SCSICommand", "itt", itt, "cmdsn", sn,
			"expstatsn", it.expStatSN.Load(), "expdatalen", expLen, "immediate", unsent,
			"cdb", hex.EncodeToString(cdb[:min(16, len(cdb))]))
	}
	werr := it.writePDU(ctx, cmd)
	it.sendMu.Unlock()
	if werr != nil {
		it.fail(fmt.Errorf("backend: send command: %w", werr))
		return nil, fmt.Errorf("backend: send command: %w", werr)
	}

	// Stream the rest of the first burst without waiting for an R2T. This
	// runs on the command's goroutine, after every other command's CmdSN was
	// already taken, so the command ordering is unaffected.
	if !c.unsolDone {
		if err := it.sendUnsolicited(ctx, c, unsent); err != nil {
			it.fail(fmt.Errorf("backend: send unsolicited data: %w", err))
			return nil, err
		}
	}

	if c.inCh != nil {
		return it.forwardSegments(ctx, c)
	}
	select {
	case <-c.done:
		return c.res, c.err
	case <-ctx.Done():
		return nil, fmt.Errorf("backend: command timed out after %s: %w", it.cfg.IOTimeout, ctx.Err())
	case <-it.done:
		return nil, it.shutdownErr()
	}
}

// extractSense copies the sense data out of a SCSI Response data segment. It
// copies because the segment may live in the reader's reusable scratch buffer,
// which the next PDU overwrites.
func extractSense(data []byte) []byte {
	if len(data) < 2 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(data[0:2]))
	if n == 0 || 2+n > len(data) {
		return nil
	}
	out := make([]byte, n)
	copy(out, data[2:2+n])
	return out
}
