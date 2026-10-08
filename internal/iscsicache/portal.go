package iscsicache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/identity"
	"vault/internal/iscsicache/l2"
	"vault/internal/iscsicache/proxy"
)

// PortalConfig configures a Portal.
type PortalConfig struct {
	// ListenAddr is the local address the portal listens on. Defaults to
	// 127.0.0.1:3260.
	ListenAddr string
	// IdleTimeout drops a session whose peer went away without logging out.
	IdleTimeout time.Duration
	Logger      *slog.Logger
}

func (c *PortalConfig) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:3260"
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = frontend.DefaultIdleTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// TargetConfig describes one cached target behind a Portal.
type TargetConfig struct {
	// Key identifies the target inside the portal. It is what Remove and Stats
	// are addressed by, and is normally the allocation the target was mounted
	// for. The IQN alone is not enough: the same allocation can be re-mounted
	// against a target whose IQN changed.
	Key string
	// TargetIQN is the IQN the portal announces for this target.
	TargetIQN string

	Backend BackendConfig
	Cache   CacheConfig

	Vendor   string
	Product  string
	Revision string
}

// targetRuntime bundles the layers of one built target.
type targetRuntime struct {
	iqn string

	be *backend.Initiator
	l2 *l2.File
	c  *cache.Cache
	px *proxy.Proxy
}

// stats aggregates the counters of this target's layers.
func (rt *targetRuntime) stats() Stats {
	st := Stats{}
	if rt.c != nil {
		st.Cache = rt.c.Stats()
		st.BlockSize = rt.c.BlockSize()
	}
	if rt.px != nil {
		st.Proxy = rt.px.Stats()
	}
	if rt.l2 != nil {
		st.L2Slots = rt.l2.Slots()
		st.L2Used = rt.l2.UsedSlots()
	}
	return st
}

// close flushes the L2 file and logs out of the backing target. It must not be
// called while a session is still issuing commands through px: the backend
// connection is torn down and in-flight commands fail.
func (rt *targetRuntime) close() error {
	var errs []error
	if rt.l2 != nil {
		if err := rt.l2.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := rt.l2.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if rt.be != nil {
		if err := rt.be.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildTarget dials the backing target and builds the cache and proxy layers.
// The result is not attached to any portal yet; the caller owns closing it.
func buildTarget(ctx context.Context, log *slog.Logger, tc TargetConfig) (*targetRuntime, error) {
	if strings.TrimSpace(tc.TargetIQN) == "" {
		return nil, errors.New("iscsicache: target_iqn is required")
	}
	if strings.TrimSpace(tc.Backend.Address) == "" {
		return nil, errors.New("iscsicache: backend.address is required")
	}
	if strings.TrimSpace(tc.Backend.TargetIQN) == "" {
		return nil, errors.New("iscsicache: backend.target_iqn is required")
	}
	cc := tc.Cache
	if err := validateCacheConfig(&cc); err != nil {
		return nil, err
	}

	var dialTimeout, ioTimeout time.Duration
	var err error
	if tc.Backend.DialTimeout != "" {
		if dialTimeout, err = time.ParseDuration(tc.Backend.DialTimeout); err != nil {
			return nil, fmt.Errorf("iscsicache: backend.dial_timeout: %w", err)
		}
	}
	if tc.Backend.IOTimeout != "" {
		if ioTimeout, err = time.ParseDuration(tc.Backend.IOTimeout); err != nil {
			return nil, fmt.Errorf("iscsicache: backend.io_timeout: %w", err)
		}
	}

	be, err := backend.Dial(ctx, backend.Config{
		Address:                  tc.Backend.Address,
		TargetIQN:                tc.Backend.TargetIQN,
		InitiatorIQN:             tc.Backend.InitiatorIQN,
		Auth:                     tc.Backend.Auth,
		Username:                 tc.Backend.Username,
		Secret:                   tc.Backend.Secret,
		DialTimeout:              dialTimeout,
		IOTimeout:                ioTimeout,
		MaxRecvDataSegmentLength: tc.Backend.MaxRecvDataSegmentLength,
		MaxBurstLength:           tc.Backend.MaxBurstLength,
		Logger:                   log.With("component", "backend"),
	})
	if err != nil {
		return nil, err
	}

	rt := &targetRuntime{iqn: tc.TargetIQN, be: be}
	if err := rt.build(ctx, log, tc, cc); err != nil {
		_ = be.Close()
		return nil, err
	}
	return rt, nil
}

// build fills in the cache and proxy layers over an already dialled backend.
func (rt *targetRuntime) build(_ context.Context, log *slog.Logger, tc TargetConfig, cc CacheConfig) error {
	info := rt.be.Info()

	desc := identity.Descriptor{
		TargetIQN:  info.TargetIQN,
		Serial:     info.Serial,
		WWID:       info.WWID,
		BlockCount: info.BlockCount,
		BlockSize:  info.BlockSize,
	}
	ns := desc.Namespace()
	if !desc.Full() {
		log.Warn("iscsicache: backing LUN reports no serial or WWID; " +
			"the cache namespace cannot detect a replaced disk")
	}
	log.Info("iscsicache: cache namespace bound",
		"namespace", ns[:16], "serial", info.Serial, "wwid", info.WWID)

	var l2Store cache.L2
	if cc.L2.Enabled {
		f, err := l2.Open(l2.Config{
			Path:       cc.L2.Path,
			SizeBytes:  cc.L2.SizeBytes,
			BlockSize:  cc.BlockSize,
			SectorSize: cc.SectorSize,
			Namespace:  ns,
			Logger:     log.With("component", "l2"),
		})
		switch {
		case err == nil:
			rt.l2 = f
			l2Store = f
		case errors.Is(err, l2.ErrUnsupported):
			// Linux placeholder: keep serving from L1 only.
			log.Warn("iscsicache: L2 file store is not implemented on this platform, continuing with L1 only")
		default:
			return err
		}
	}

	c, err := cache.New(cache.Config{
		BlockSize:     cc.BlockSize,
		SectorSize:    cc.SectorSize,
		L1Bytes:       cc.L1.SizeBytes,
		NewQuotaPct:   cc.Eviction.NewQuotaPct,
		HighWatermark: cc.Eviction.HighWatermark,
		LowWatermark:  cc.Eviction.LowWatermark,
		ScanEnabled:   *cc.ScanDetection.Enabled,
		ScanThreshold: cc.ScanDetection.Threshold,
		L2:            l2Store,
		Logger:        log.With("component", "cache"),
	})
	if err != nil {
		return err
	}
	rt.c = c

	px, err := proxy.New(proxy.Config{
		Backend:   rt.be,
		Cache:     c,
		Vendor:    tc.Vendor,
		Product:   tc.Product,
		Revision:  tc.Revision,
		TargetIQN: tc.TargetIQN,
		Logger:    log.With("component", "proxy"),
	})
	if err != nil {
		return err
	}
	rt.px = px
	return nil
}

// Portal is a single iSCSI portal that serves several cached targets.
//
// One portal per client is the design point: the local initiator connects to
// one address, and each mounted repository is registered as its own target
// behind it, so the per-target L1/L2 budgets and hit counters stay separate
// while the client has a single listener to configure.
type Portal struct {
	cfg PortalConfig
	log *slog.Logger

	tgt *frontend.Target

	// opsMu serializes Add and Remove. Both dial or tear down a backend, which
	// takes seconds; without it two Adds for the same key could build two
	// backends while Stats/Has keep working off mu.
	opsMu sync.Mutex
	// mu guards targets and closed for the readers (Stats, Has, TargetIQN).
	mu      sync.Mutex
	targets map[string]*targetRuntime
	closed  bool
}

// NewPortal opens the portal's listener. It does not start serving; call Start.
// Targets are added with Add.
func NewPortal(cfg PortalConfig) (*Portal, error) {
	cfg.normalize()
	log := cfg.Logger
	tgt, err := frontend.New(frontend.Config{
		ListenAddr:  cfg.ListenAddr,
		IdleTimeout: cfg.IdleTimeout,
		Logger:      log.With("component", "frontend"),
	}, nil)
	if err != nil {
		return nil, err
	}
	return &Portal{
		cfg:     cfg,
		log:     log,
		tgt:     tgt,
		targets: make(map[string]*targetRuntime),
	}, nil
}

// Addr returns the address the portal listens on.
func (p *Portal) Addr() net.Addr { return p.tgt.Addr() }

// Start serves the portal until ctx is cancelled or Close is called.
func (p *Portal) Start(ctx context.Context) error {
	err := p.tgt.Serve(ctx)
	if err != nil && p.isClosed() {
		return nil
	}
	return err
}

// Add dials the backing target and registers it under tc.Key and tc.TargetIQN.
//
// It is idempotent: a key that is already registered is left untouched, so a
// re-mount of the same allocation does not rebuild the cache (and does not lose
// its hit counters or its warm L2 mapping).
func (p *Portal) Add(ctx context.Context, tc TargetConfig) error {
	key := strings.TrimSpace(tc.Key)
	if key == "" {
		return errors.New("iscsicache: target key is required")
	}

	p.opsMu.Lock()
	defer p.opsMu.Unlock()

	p.mu.Lock()
	closed := p.closed
	_, exists := p.targets[key]
	p.mu.Unlock()
	if closed {
		return errors.New("iscsicache: portal is closed")
	}
	if exists {
		return nil
	}

	rt, err := buildTarget(ctx, p.log.With("target", key), tc)
	if err != nil {
		return err
	}
	if err := p.tgt.Register(tc.TargetIQN, rt.px); err != nil {
		_ = rt.close()
		return err
	}

	p.mu.Lock()
	p.targets[key] = rt
	p.mu.Unlock()
	p.log.Info("iscsicache: target added",
		"key", key, "target_iqn", tc.TargetIQN, "backend", tc.Backend.Address)
	return nil
}

// Remove unregisters a target and tears down its backend and L2 file. Removing
// an unknown key is a no-op. The caller must have disconnected the local
// initiator first: sessions already logged in keep their channels until they
// are closed, but their commands fail once the backend is gone.
func (p *Portal) Remove(key string) error {
	p.opsMu.Lock()
	defer p.opsMu.Unlock()

	p.mu.Lock()
	rt := p.targets[key]
	delete(p.targets, key)
	p.mu.Unlock()
	if rt == nil {
		return nil
	}
	p.tgt.Unregister(rt.iqn)
	err := rt.close()
	p.log.Info("iscsicache: target removed", "key", key, "target_iqn", rt.iqn, "err", err)
	return err
}

// Has reports whether a key is registered.
func (p *Portal) Has(key string) bool {
	p.mu.Lock()
	_, ok := p.targets[key]
	p.mu.Unlock()
	return ok
}

// TargetIQN returns the IQN a registered key is served under.
func (p *Portal) TargetIQN(key string) (string, bool) {
	p.mu.Lock()
	rt, ok := p.targets[key]
	p.mu.Unlock()
	if !ok {
		return "", false
	}
	return rt.iqn, true
}

// Keys returns the registered keys, in no particular order.
func (p *Portal) Keys() []string {
	p.mu.Lock()
	keys := make([]string, 0, len(p.targets))
	for k := range p.targets {
		keys = append(keys, k)
	}
	p.mu.Unlock()
	return keys
}

// Stats returns the counters of one registered target.
func (p *Portal) Stats(key string) (Stats, bool) {
	p.mu.Lock()
	rt, ok := p.targets[key]
	p.mu.Unlock()
	if !ok {
		return Stats{}, false
	}
	return rt.stats(), true
}

func (p *Portal) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close stops the listener, drops every session and tears down all targets.
//
// It is the only place that shuts a target down, so a caller that stops the
// portal on process exit (a signal handler, for instance) still flushes the L2
// files and logs out of the backends.
func (p *Portal) Close() error {
	p.opsMu.Lock()
	defer p.opsMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	rtss := make([]*targetRuntime, 0, len(p.targets))
	for _, rt := range p.targets {
		rtss = append(rtss, rt)
	}
	p.targets = make(map[string]*targetRuntime)
	p.mu.Unlock()

	var errs []error
	if p.tgt != nil {
		if err := p.tgt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rt := range rtss {
		if err := rt.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
