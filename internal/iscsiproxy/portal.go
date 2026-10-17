package iscsiproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
)

// PortalConfig configures a Portal.
type PortalConfig struct {
	Logger *slog.Logger
}

// Portal owns the cached targets of one client.
//
// Unlike a terminating proxy there is no single listener: every target binds its
// own 127.0.0.1 port, because the target the initiator logs in to is the backend
// target itself (its real IQN), and two targets cannot share a port. The mapping
// is static, handed down by the agent per allocation, so no TargetName routing
// is needed.
type Portal struct {
	log *slog.Logger

	opsMu   sync.Mutex
	mu      sync.Mutex
	targets map[string]*target
	closed  bool
}

// NewPortal builds an empty portal. Targets are added with Add.
func NewPortal(cfg PortalConfig) (*Portal, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Portal{log: log, targets: make(map[string]*target)}, nil
}

// Start blocks until ctx is cancelled. Listening is per target, so there is
// nothing global to serve.
func (p *Portal) Start(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	<-ctx.Done()
	return nil
}

// Add registers a target and starts its listener. It is idempotent: an already
// registered key is left untouched, so a re-mount does not rebuild the cache and
// lose its counters or its warm L2 mapping.
func (p *Portal) Add(ctx context.Context, tc TargetConfig) error {
	key := strings.TrimSpace(tc.Key)
	if key == "" {
		return errors.New("iscsiproxy: target key is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.opsMu.Lock()
	defer p.opsMu.Unlock()

	p.mu.Lock()
	closed := p.closed
	_, exists := p.targets[key]
	p.mu.Unlock()
	if closed {
		return errors.New("iscsiproxy: portal is closed")
	}
	if exists {
		return nil
	}

	t, err := newTarget(p.log.With("target", key, "engine", Version), tc)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = t.close()
		return errors.New("iscsiproxy: portal is closed")
	}
	p.targets[key] = t
	p.mu.Unlock()

	t.start()
	p.log.Info("iscsiproxy: 缓存目标已注册",
		"engine", Version, "key", key, "target_iqn", t.iqn,
		"listen", t.addr.String(), "backend", t.backend)
	return nil
}

// Remove unregisters a target, drops its sessions and releases its cache. The
// caller must have disconnected the initiator first. An unknown key is a no-op.
func (p *Portal) Remove(key string) error {
	p.opsMu.Lock()
	p.mu.Lock()
	t := p.targets[strings.TrimSpace(key)]
	delete(p.targets, strings.TrimSpace(key))
	p.mu.Unlock()
	p.opsMu.Unlock()
	if t == nil {
		return nil
	}
	err := t.close()
	p.log.Info("iscsiproxy: 缓存目标已移除", "engine", Version, "key", t.key, "err", err)
	return err
}

// DropCache empties the local read cache of one registered target and leaves the
// target and its sessions alone. Reports false when the key is not registered
// (the library is not mounted, or is not going through the proxy).
func (p *Portal) DropCache(key string) bool {
	p.mu.Lock()
	t := p.targets[strings.TrimSpace(key)]
	p.mu.Unlock()
	if t == nil {
		return false
	}
	return t.dropCache()
}

// Has reports whether a key is registered.
func (p *Portal) Has(key string) bool {
	p.mu.Lock()
	_, ok := p.targets[strings.TrimSpace(key)]
	p.mu.Unlock()
	return ok
}

// Keys returns the registered keys in no particular order.
func (p *Portal) Keys() []string {
	p.mu.Lock()
	keys := make([]string, 0, len(p.targets))
	for k := range p.targets {
		keys = append(keys, k)
	}
	p.mu.Unlock()
	return keys
}

// TargetIQN returns the IQN a registered key is served under. For v2 that is the
// backend target's real IQN: the initiator logs in to the backend end to end.
func (p *Portal) TargetIQN(key string) (string, bool) {
	p.mu.Lock()
	t, ok := p.targets[strings.TrimSpace(key)]
	p.mu.Unlock()
	if !ok {
		return "", false
	}
	return t.iqn, true
}

// TargetAddr returns the loopback host and port the initiator must connect to
// for a registered key.
func (p *Portal) TargetAddr(key string) (string, int, bool) {
	p.mu.Lock()
	t, ok := p.targets[strings.TrimSpace(key)]
	p.mu.Unlock()
	if !ok || t.addr == nil {
		return "", 0, false
	}
	host, port, err := net.SplitHostPort(t.addr.String())
	if err != nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, false
	}
	return host, n, true
}

// Stats returns the counters of one registered target.
func (p *Portal) Stats(key string) (Stats, bool) {
	p.mu.Lock()
	t, ok := p.targets[strings.TrimSpace(key)]
	p.mu.Unlock()
	if !ok {
		return Stats{}, false
	}
	return t.statsSnapshot(), true
}

// TargetSnapshot is one registered target's counters together with the identity
// a status page shows next to them.
type TargetSnapshot struct {
	Stats Stats
	// IQN is the target's IQN, the same value TargetIQN reports for its key.
	IQN string
}

// Snapshots returns every registered target's counters, keyed the way the portal
// is, in no particular order.
//
// It exists for a caller that reports them all at once (the agent's cache status
// endpoint): asking per key costs the portal table lock once for the counters and
// once for the IQN, and the counters themselves walk the target's cache index, so
// one pass over the table is the cheap half of that to halve — but it is the half
// that scales with the number of mounted libraries.
//
// The table is copied under the lock and the counters are sampled outside it:
// statsSnapshot takes the target's own locks, and sampling it while holding the
// portal lock would order the two.
func (p *Portal) Snapshots() map[string]TargetSnapshot {
	type registered struct {
		key string
		t   *target
		iqn string
	}
	p.mu.Lock()
	list := make([]registered, 0, len(p.targets))
	for k, t := range p.targets {
		list = append(list, registered{key: k, t: t, iqn: t.iqn})
	}
	p.mu.Unlock()

	out := make(map[string]TargetSnapshot, len(list))
	for _, r := range list {
		out[r.key] = TargetSnapshot{Stats: r.t.statsSnapshot(), IQN: r.iqn}
	}
	return out
}

// Addr returns the address of the first registered target, or nil when there is
// none. Per target ports mean there is no single portal address.
func (p *Portal) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.targets {
		return t.addr
	}
	return nil
}

// Close drops every target and releases its cache.
func (p *Portal) Close() error {
	p.opsMu.Lock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.opsMu.Unlock()
		return nil
	}
	p.closed = true
	ts := make([]*target, 0, len(p.targets))
	for _, t := range p.targets {
		ts = append(ts, t)
	}
	p.targets = make(map[string]*target)
	p.mu.Unlock()
	p.opsMu.Unlock()

	var errs []error
	for _, t := range ts {
		if err := t.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
