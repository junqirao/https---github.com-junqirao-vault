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

// Target is a single LUN iSCSI target. Any number of sessions is accepted and
// each session runs its commands concurrently; the Handler must be safe for
// concurrent use.
type Target struct {
	cfg Config
	h   Handler
	log *slog.Logger

	ln     net.Listener
	closed atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	ttt  atomic.Uint32
	tsih atomic.Uint32
}

// New creates a Target bound to a TCP listener.
func New(cfg Config, h Handler) (*Target, error) {
	cfg.normalize()
	if h == nil {
		return nil, errors.New("frontend: handler is required")
	}
	if cfg.TargetIQN == "" {
		return nil, errors.New("frontend: target IQN is required")
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("frontend: listen %s: %w", cfg.ListenAddr, err)
	}
	return &Target{
		cfg:   cfg,
		h:     h,
		log:   cfg.Logger,
		ln:    ln,
		conns: make(map[net.Conn]struct{}),
	}, nil
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
	t.log.Info("frontend listening", "addr", t.ln.Addr().String(), "target_iqn", t.cfg.TargetIQN)
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
