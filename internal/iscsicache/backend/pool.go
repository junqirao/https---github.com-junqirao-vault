package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// DefaultSessions is how many sessions a Pool opens eagerly at mount time and
// MaxSessions is the hard cap it grows to while the workload pipelines.
//
// A single session has a queue depth of one: Initiator.Exec holds the session
// for the whole command, so a client that keeps commands in flight (the
// frontend advertises a command window of 64) would have its queue depth
// collapsed to one and pay the backing round trip once per command. The pool
// therefore opens a few sessions up front and adds more while every session is
// busy, so the backing link carries as many commands in flight as the client
// actually pipelines. Each session stays strictly serial, which keeps the
// DataSN / R2T state machine untouched.
//
// MaxSessions has to be at least the frontend's command window: the backing
// queue depth is what the proxy's throughput is worth over a real link, and a
// client at queue depth 32 reaching a pool capped at 16 gets half the
// throughput of the same client talking to the array directly, however cheap
// the proxy's per-command work is.
const (
	DefaultSessions = 4
	MaxSessions     = 64
)

// Pool spreads commands over several independent iSCSI sessions to the same
// backing target.
//
// This is only about reads. The proxy serialises writes and cache invalidation
// against reads, so at most one write is ever outstanding; reads are
// independent and can run on different sessions at the same time.
type Pool struct {
	log *slog.Logger
	cfg Config
	// max is the number of sessions the pool may grow to.
	max int

	// mu guards sess, which only ever grows: a session is dialled and logged in
	// before it is appended. Each session has its own command mutex, so a
	// command holds only the session it runs on.
	mu   sync.RWMutex
	sess []*poolSession

	// next picks the first session a command tries, round robin.
	next atomic.Uint64

	// growMu serialises dialling and noGrow latches a failed growth, so the pool
	// stops growing once the target has refused a session or reports a mismatch
	// instead of retrying on every command.
	growMu sync.Mutex
	noGrow bool
}

type poolSession struct {
	it *Initiator
	// mu serialises the commands on this session, so the backing link sees one
	// command at a time per session exactly as a real initiator's session would.
	mu   sync.Mutex
	dead atomic.Bool
}

var _ Backend = (*Pool)(nil)

// DialPool opens the eager sessions to the backing target and returns them as
// one Backend.
//
// The first session must succeed: without it there is no backend at all. The
// remaining eager sessions are a throughput optimisation, so a target that
// refuses them still yields a working pool with the sessions that did log in,
// and that is logged rather than failing the mount. Further sessions are added
// on demand by Exec, up to the pool's cap.
func DialPool(ctx context.Context, cfg Config) (*Pool, error) {
	cfg.applyDefaults()
	max := cfg.Sessions
	if max <= 0 || max > MaxSessions {
		max = MaxSessions
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	first, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &Pool{log: log, cfg: cfg, max: max, sess: []*poolSession{{it: first}}}

	eager := DefaultSessions
	if eager > max {
		eager = max
	}
	for i := 1; i < eager; i++ {
		it, err := Dial(ctx, cfg)
		if err != nil {
			log.Warn("backend: eager session could not log in, continuing with fewer",
				"opened", len(p.sess), "wanted", eager, "err", err)
			break
		}
		if err := p.checkIdentity(first, it); err != nil {
			_ = it.Close()
			_ = p.Close()
			return nil, err
		}
		p.sess = append(p.sess, &poolSession{it: it})
	}
	log.Info("backend: session pool ready", "sessions", len(p.sess), "max", max)
	return p, nil
}

// Info returns the device parameters discovered by the first session.
func (p *Pool) Info() DeviceInfo { return p.sessions()[0].it.Info() }

// Sessions reports how many sessions the pool is running.
func (p *Pool) Sessions() int { return len(p.sessions()) }

// Exec implements Backend by returning a freshly allocated read payload.
func (p *Pool) Exec(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (*Result, error) {
	return p.exec(ctx, cdb, dataOut, nil, inLen)
}

// ExecInto issues a read whose payload the chosen session writes straight into
// buf. It is the pooled counterpart of Initiator.ExecInto, and it matters
// because the pool is what production wires up: without it the read path falls
// back to Exec, which allocates a buffer as large as the transfer inside the
// session and copies it out again, so every command crossing the pool costs a
// transfer-sized allocation and a transfer-sized copy that the cache and the
// frontend have already provided buffers for. The copy is on top of the one the
// proxy cannot avoid (receiving the bytes off the backing link), and the
// allocation is what puts the proxy's buffers back on the collector.
func (p *Pool) ExecInto(ctx context.Context, cdb []byte, dataOut []byte, buf []byte) (*Result, error) {
	if len(buf) == 0 {
		return nil, errors.New("backend: empty read buffer")
	}
	return p.exec(ctx, cdb, dataOut, buf, len(buf))
}

// exec routes one command to a session, growing the pool while every session is
// busy so the client's queue depth is preserved. buf, when non-nil, is the
// caller-provided destination for a read payload.
//
// It does not retry on another session: a command that failed may have reached
// the device, and reissuing it is only safe because every command the proxy
// sends is idempotent (reads, writes of the same bytes, UNMAP). Retiring the
// failed session and letting the next command use a healthy one keeps the
// failure visible to the SCSI layer without risking a duplicate effect.
func (p *Pool) exec(ctx context.Context, cdb, dataOut, buf []byte, inLen int) (*Result, error) {
	start := p.next.Add(1)
	for {
		sessions := p.sessions()
		n := len(sessions)
		if n == 0 {
			return nil, ErrClosed
		}

		// A session that is free runs the command at once. Until the client
		// actually pipelines more commands than there are sessions, this keeps
		// the pool at its eager size.
		for i := 0; i < n; i++ {
			s := sessions[(int(start)+i)%n]
			if s.dead.Load() || !s.mu.TryLock() {
				continue
			}
			return p.run(s, ctx, cdb, dataOut, buf, inLen)
		}

		// Every session is busy: the client pipelines more commands than the pool
		// has sessions, so grow it instead of collapsing the queue depth.
		if p.grow(ctx) {
			continue
		}

		// At the cap with every session busy: queue on the next live session.
		for i := 0; i < n; i++ {
			s := sessions[(int(start)+i)%n]
			if s.dead.Load() {
				continue
			}
			s.mu.Lock()
			return p.run(s, ctx, cdb, dataOut, buf, inLen)
		}
		return nil, ErrClosed
	}
}

// run executes one command on a session whose command mutex is already held and
// retires the session if the command failed for a reason other than the caller's
// own context.
func (p *Pool) run(s *poolSession, ctx context.Context, cdb, dataOut, buf []byte, inLen int) (*Result, error) {
	defer s.mu.Unlock()
	var res *Result
	var err error
	if buf != nil {
		res, err = s.it.ExecInto(ctx, cdb, dataOut, buf)
	} else {
		res, err = s.it.Exec(ctx, cdb, dataOut, inLen)
	}
	if err == nil {
		return res, nil
	}
	if ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// A real backend failure: retire the session so the rest of the traffic
		// does not keep landing on a desynchronised connection.
		if s.dead.CompareAndSwap(false, true) {
			p.log.Warn("backend: session retired after a failed command",
				"alive", p.alive(), "err", err)
		}
	}
	return nil, err
}

// grow adds one session while every session is busy. It reports whether the pool
// changed, so the caller can retry with the extra session.
func (p *Pool) grow(ctx context.Context) bool {
	if !p.canGrow() {
		return false
	}
	p.growMu.Lock()
	defer p.growMu.Unlock()
	// Re-check under the growth lock: another command may have grown the pool
	// while this one waited.
	if !p.canGrow() {
		return false
	}
	first := p.sessions()[0].it

	// Dial on a context of the pool's own: a command that happens to be
	// cancelled must not look like a target that refuses sessions and latch the
	// pool at its current size for good.
	dialCtx, cancel := context.WithTimeout(context.Background(), p.cfg.DialTimeout)
	defer cancel()
	it, err := Dial(dialCtx, p.cfg)
	if err != nil {
		p.stopGrowing()
		p.log.Warn("backend: could not add a session, keeping the current pool",
			"sessions", p.Sessions(), "err", err)
		return false
	}
	if err := p.checkIdentity(first, it); err != nil {
		_ = it.Close()
		p.stopGrowing()
		p.log.Warn("backend: new session sees a different LUN, not growing the pool", "err", err)
		return false
	}

	p.mu.Lock()
	p.sess = append(p.sess, &poolSession{it: it})
	n := len(p.sess)
	p.mu.Unlock()
	p.log.Info("backend: session added", "sessions", n, "max", p.max)
	return true
}

// canGrow reports whether another session may still be opened.
func (p *Pool) canGrow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.sess) < p.max && !p.noGrow
}

// stopGrowing latches the pool as full: it will not dial again.
func (p *Pool) stopGrowing() {
	p.mu.Lock()
	p.noGrow = true
	p.mu.Unlock()
}

// checkIdentity rejects a session that landed on a different LUN: the cache keys
// and the L2 namespace are derived from the device identity, and a session that
// saw another device would silently serve other data.
func (p *Pool) checkIdentity(first, it *Initiator) error {
	got, want := it.Info(), first.Info()
	if got.BlockSize != want.BlockSize || got.BlockCount != want.BlockCount ||
		got.Serial != want.Serial || got.WWID != want.WWID {
		return fmt.Errorf("backend: session sees a different LUN (serial %q vs %q)", got.Serial, want.Serial)
	}
	return nil
}

// Close logs out and closes every session.
func (p *Pool) Close() error {
	var errs []error
	for _, s := range p.sessions() {
		if err := s.it.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pool) sessions() []*poolSession {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sess
}

func (p *Pool) alive() int {
	n := 0
	for _, s := range p.sessions() {
		if !s.dead.Load() {
			n++
		}
	}
	return n
}
