package backend

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
)

// Session pool sizing.
//
// One backing session is one socket, and on a fast local stack one socket costs
// a single core: every Data-In segment is copied kernel to user on the one
// reader goroutine, so a single session tops out near 3.5 GB/s however deep the
// client pipelines. That is above a 10GbE link, so one session is enough when
// the array is the bottleneck — but a client reading in large blocks at depth
// puts the proxy's local hop on the critical path, and it then reports a
// fraction of a direct mount, which fans out over as many sockets as it has
// commands in flight.
//
// The pool therefore opens a fixed number of independent sessions before the
// mount is handed back and spreads commands over them, so the local hop runs on
// as many cores as there are sessions. It never dials while a workload is
// running and it never grows: a session that dies is retired rather than
// replaced, so a session the target refuses cannot become a per-command retry
// loop, and there is no growth latch to collapse the pool under load.
const (
	DefaultSessions = 4
	MaxSessions     = 32
)

// Pool spreads commands over a fixed set of independent iSCSI sessions to the
// same backing target. It implements Backend and Streamer, so it is a drop-in
// replacement for a single Initiator.
//
// Every session is a full pipelining initiator with its own reader, socket,
// command window and sequence numbers, so several commands can be outstanding
// on it even though its own PDUs stay strictly ordered on the wire. A new
// command goes to the least loaded live session.
type Pool struct {
	log  *slog.Logger
	info DeviceInfo
	sess []*poolSession
}

// poolSession is one session plus the pool's bookkeeping about it. load is the
// number of commands placed on it that have not finished, which is what places
// the next command: a plain round robin would stack a run of the slower command
// type on one session.
type poolSession struct {
	it   *Initiator
	load atomic.Int64
	dead atomic.Bool
}

var (
	_ Backend  = (*Pool)(nil)
	_ Streamer = (*Pool)(nil)
)

// DialPool logs in cfg.Sessions independent sessions to the backing target and
// returns them as one Backend.
//
// The first session must succeed: without it there is no backend at all. The
// rest are throughput, so a target that refuses one leaves a working pool with
// the sessions that did log in — that is logged rather than fatal, because a
// target with a per-initiator session limit still serves a smaller pool
// correctly.
func DialPool(ctx context.Context, cfg Config) (*Pool, error) {
	cfg.applyDefaults()
	n := cfg.Sessions
	if n <= 0 {
		n = DefaultSessions
	}
	if n > MaxSessions {
		n = MaxSessions
	}

	first, err := Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &Pool{
		log:  first.log,
		info: first.Info(),
		sess: []*poolSession{{it: first}},
	}
	for len(p.sess) < n {
		it, err := Dial(ctx, cfg)
		if err != nil {
			p.log.Warn("backend: backing session refused, continuing with fewer",
				"have", len(p.sess), "want", n, "err", err)
			break
		}
		p.sess = append(p.sess, &poolSession{it: it})
	}
	p.log.Info("backend: session pool ready",
		"sessions", len(p.sess), "address", cfg.Address, "target", cfg.TargetIQN)
	return p, nil
}

// Sessions returns how many sessions logged in.
func (p *Pool) Sessions() int { return len(p.sess) }

// Info returns the LUN parameters discovered on login. Every session logs in to
// the same LUN, so one of them is representative.
func (p *Pool) Info() DeviceInfo { return p.info }

// Exec issues one SCSI command on the least loaded live session.
func (p *Pool) Exec(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (*Result, error) {
	s := p.pick()
	if s == nil {
		return nil, ErrClosed
	}
	defer s.load.Add(-1)
	res, err := s.it.Exec(ctx, cdb, dataOut, inLen)
	if err != nil && s.it.Closed() {
		p.retire(s)
	}
	return res, err
}

// ExecStream issues a streamed read on the least loaded live session.
func (p *Pool) ExecStream(ctx context.Context, cdb []byte, inLen int, sink DataInSink) (*Result, error) {
	s := p.pick()
	if s == nil {
		return nil, ErrClosed
	}
	defer s.load.Add(-1)
	res, err := s.it.ExecStream(ctx, cdb, inLen, sink)
	if err != nil && s.it.Closed() {
		p.retire(s)
	}
	return res, err
}

// Close logs out of every session.
func (p *Pool) Close() error {
	var errs []error
	for _, s := range p.sess {
		if err := s.it.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pick returns the least loaded live session and records the command on it. It
// returns nil when every session has died, which is what makes a pool with no
// session left fail its commands instead of hanging.
func (p *Pool) pick() *poolSession {
	var best *poolSession
	var bestLoad int64
	for _, s := range p.sess {
		if s.dead.Load() {
			continue
		}
		l := s.load.Load()
		if best == nil || l < bestLoad {
			best, bestLoad = s, l
		}
	}
	if best != nil {
		best.load.Add(1)
	}
	return best
}

// retire marks a session dead after a command found it gone, so the pool stops
// placing commands on it. A retired session is not replaced: the pool is fixed
// at mount time, and a remount is what rebuilds it.
func (p *Pool) retire(s *poolSession) {
	if s.dead.Swap(true) {
		return
	}
	live := 0
	for _, other := range p.sess {
		if !other.dead.Load() {
			live++
		}
	}
	p.log.Warn("backend: backing session retired", "live", live, "total", len(p.sess))
}
