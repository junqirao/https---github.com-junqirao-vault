// Package frontend implements the pure Go iSCSI target the proxy presents to
// local initiators.
//
// The MVP target is deliberately small: one LUN, ErrorRecoveryLevel 0,
// InitialR2T=Yes, no digests. Several sessions may be logged in at once, which
// is what real initiators do, and each session runs its commands concurrently
// so a single connection can use its full command window. Everything that the
// target cannot serve locally is passed to the Handler, which is the proxy's
// cache aware command executor.
package frontend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"vault/internal/iscsicache/iscsi"
)

// Negotiation defaults.
const (
	DefaultMaxRecvDataSegmentLength = 65536
	DefaultMaxBurstLength           = 262144
	DefaultFirstBurstLength         = 65536
	// DefaultIdleTimeout closes a session whose peer went away without logging
	// out, so the single-session slot is not leaked forever.
	DefaultIdleTimeout = 30 * time.Minute
)

// DeviceInfo is the LUN identity the target reports.
type DeviceInfo struct {
	Vendor     string
	Product    string
	Revision   string
	Serial     string
	DeviceType uint8
	BlockSize  uint32
	BlockCount uint64
}

// CapacityBytes returns the LUN capacity in bytes.
func (d DeviceInfo) CapacityBytes() int64 {
	return int64(d.BlockSize) * int64(d.BlockCount)
}

// Result is the outcome of one SCSI command.
type Result struct {
	Status    uint8
	Sense     []byte
	Data      []byte
	Residual  uint32
	Overflow  bool
	Underflow bool

	// Release, when non-nil, is called by the target once Data has been written
	// out (or the command failed) and the handler may reuse the buffer. It lets
	// a handler serve Data out of a pool instead of allocating a fresh buffer
	// per command, which for a read is the largest allocation on the path.
	// Handlers that return a buffer they do not own leave it nil.
	Release func()
}

// Handler executes SCSI commands for the target. Implementations must be safe
// for concurrent use.
type Handler interface {
	// Device returns the LUN identity reported to initiators.
	Device() DeviceInfo
	// Execute runs a single CDB. dataOut carries the bytes received for write
	// commands; inLen is the number of bytes the initiator expects back.
	Execute(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (Result, error)
}

// Config configures a Target.
//
// TargetIQN is only used by the single target constructor New. A portal that
// serves several targets (see Register) leaves it empty.
type Config struct {
	ListenAddr string
	TargetIQN  string

	Vendor   string
	Product  string
	Revision string

	MaxRecvDataSegmentLength int
	MaxBurstLength           int
	FirstBurstLength         int

	IdleTimeout time.Duration
	Logger      *slog.Logger
}

func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:3260"
	}
	if c.MaxRecvDataSegmentLength <= 0 {
		c.MaxRecvDataSegmentLength = DefaultMaxRecvDataSegmentLength
	}
	if c.MaxBurstLength <= 0 {
		c.MaxBurstLength = DefaultMaxBurstLength
	}
	if c.FirstBurstLength <= 0 {
		c.FirstBurstLength = DefaultFirstBurstLength
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// Target is an iSCSI portal. It serves one or more targets, each identified by
// its IQN and backed by its own Handler; a session picks its Handler from the
// TargetName it logs in with. Any number of sessions is accepted and each
// session runs its commands concurrently, so every Handler must be safe for
// concurrent use.
type Target struct {
	cfg Config
	log *slog.Logger

	// handlersMu guards handlers. A session resolves its Handler once, at login,
	// while the portal may still be gaining and losing targets at runtime.
	handlersMu sync.RWMutex
	handlers   map[string]*targetEntry

	ln     net.Listener
	closed atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	ttt  atomic.Uint32
	tsih atomic.Uint32
}

// targetEntry is one registered target: the IQN as the caller spelled it (for
// display) and the Handler that serves it.
type targetEntry struct {
	iqn string
	h   Handler
}

// New creates a Target bound to a TCP listener.
//
// With a handler it behaves like the single target portal the MVP started as:
// cfg.TargetIQN is required and becomes the only registered target. With a nil
// handler it opens an empty portal that targets are added to with Register.
func New(cfg Config, h Handler) (*Target, error) {
	cfg.normalize()
	t := &Target{
		cfg:      cfg,
		log:      cfg.Logger,
		handlers: make(map[string]*targetEntry),
		conns:    make(map[net.Conn]struct{}),
	}
	if h != nil {
		if cfg.TargetIQN == "" {
			return nil, errors.New("frontend: target IQN is required")
		}
		t.handlers[normalizeIQN(cfg.TargetIQN)] = &targetEntry{iqn: cfg.TargetIQN, h: h}
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("frontend: listen %s: %w", cfg.ListenAddr, err)
	}
	t.ln = ln
	return t, nil
}

// Register adds a target to the portal, replacing any target already registered
// under the same IQN. IQNs are compared case insensitively, which is how
// initiators and the login path treat them.
func (t *Target) Register(iqn string, h Handler) error {
	name := strings.TrimSpace(iqn)
	if name == "" {
		return errors.New("frontend: target IQN is required")
	}
	if h == nil {
		return errors.New("frontend: handler is required")
	}
	t.handlersMu.Lock()
	t.handlers[normalizeIQN(name)] = &targetEntry{iqn: name, h: h}
	t.handlersMu.Unlock()
	return nil
}

// Unregister removes a target. Removing an unknown IQN is a no-op, so cleanup
// does not have to know whether the target was ever registered.
func (t *Target) Unregister(iqn string) {
	t.handlersMu.Lock()
	delete(t.handlers, normalizeIQN(iqn))
	t.handlersMu.Unlock()
}

// lookup resolves the Handler registered for an IQN.
func (t *Target) lookup(iqn string) (Handler, string, bool) {
	t.handlersMu.RLock()
	e, ok := t.handlers[normalizeIQN(iqn)]
	t.handlersMu.RUnlock()
	if !ok {
		return nil, "", false
	}
	return e.h, e.iqn, true
}

// TargetNames returns the registered IQNs, sorted, for SendTargets and logging.
func (t *Target) TargetNames() []string {
	t.handlersMu.RLock()
	names := make([]string, 0, len(t.handlers))
	for _, e := range t.handlers {
		names = append(names, e.iqn)
	}
	t.handlersMu.RUnlock()
	sort.Strings(names)
	return names
}

// normalizeIQN is the map key form of an IQN.
func normalizeIQN(iqn string) string {
	return strings.ToLower(strings.TrimSpace(iqn))
}

// nextTSIH hands out a TSIH that is unique among the sessions of this portal.
// Reusing one value for a discovery session and a normal session confuses
// initiators that keep both open.
func (t *Target) nextTSIH() uint16 {
	return uint16(t.tsih.Add(1))
}

// Addr returns the address the target listens on.
func (t *Target) Addr() net.Addr { return t.ln.Addr() }

// Close stops accepting connections and drops the active ones.
func (t *Target) Close() error {
	t.closed.Store(true)
	err := t.ln.Close()
	t.mu.Lock()
	for c := range t.conns {
		_ = c.Close()
	}
	t.conns = make(map[net.Conn]struct{})
	t.mu.Unlock()
	return err
}

// Serve accepts connections until the target is closed or ctx is cancelled.
//
// Cancelling ctx must unblock Accept, otherwise a caller that stops the service
// by cancelling a context (a signal handler, for instance) hangs forever on a
// listener nobody is connecting to.
func (t *Target) Serve(ctx context.Context) error {
	t.log.Info("frontend listening", "addr", t.ln.Addr().String(), "targets", strings.Join(t.TargetNames(), ","))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = t.Close()
		case <-stop:
		}
	}()
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			if t.closed.Load() {
				return nil
			}
			return fmt.Errorf("frontend: accept: %w", err)
		}
		go t.serveConn(ctx, conn)
	}
}

func (t *Target) nextTTT() uint32 {
	v := t.ttt.Add(1)
	if v == 0 || v == iscsi.ReservedTag {
		v = t.ttt.Add(1)
	}
	return v
}

func (t *Target) serveConn(ctx context.Context, conn net.Conn) {
	t.mu.Lock()
	t.conns[conn] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.conns, conn)
		t.mu.Unlock()
		_ = conn.Close()
	}()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		// Detect a peer that vanished without closing the session.
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	peer := conn.RemoteAddr().String()
	// A per-session context lets the commands still running when the session
	// ends abort instead of lingering on a dead connection.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &session{t: t, conn: conn, log: t.log.With("peer", peer)}
	if err := s.login(sctx); err != nil {
		s.log.Warn("frontend: login failed", "err", err)
		return
	}
	s.log.Info("frontend: session established",
		"initiator", s.initiatorName, "tsih", s.tsih, "discovery", s.discovery)
	err := s.loop(sctx)
	s.log.Info("frontend: session closed", "tsih", s.tsih, "discovery", s.discovery, "err", err)
}
