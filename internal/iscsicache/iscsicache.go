// Package iscsicache is the standalone iSCSI read-cache proxy module.
//
// It presents a local iSCSI target to initiators on this machine and serves
// reads from an L1 memory cache and an optional L2 file cache filled from a
// remote iSCSI backing target:
//
//	initiator --iSCSI--> [frontend target] --+--> L1/L2 cache --+
//	                                         +--> [backend initiator] --iSCSI--> target
//
// The package is self contained: it depends only on the standard library, the
// iscsicache sub-packages and, on Windows, golang.org/x/sys/windows. Nothing in
// the vault agent/client is referenced, so a host application wires it up by
// constructing a Service and calling Start.
//
// Platform support: the protocol, cache, frontend and backend layers are pure
// Go and build everywhere. The L2 file store (unbuffered IO) and the mlock
// based memory locker are platform specific; Windows is implemented and other
// platforms are stubs that report ErrUnsupported.
package iscsicache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/l2"
	"vault/internal/iscsicache/proxy"
)

// Config is the proxy configuration. It maps one to one onto the YAML file the
// bundled command reads.
type Config struct {
	// ListenAddr is the local address the target listens on. Defaults to
	// 127.0.0.1:3260.
	ListenAddr string `yaml:"listen_addr"`
	// TargetIQN is the IQN this proxy announces to local initiators.
	TargetIQN string `yaml:"target_iqn"`

	Vendor   string `yaml:"vendor"`
	Product  string `yaml:"product"`
	Revision string `yaml:"revision"`

	Backend BackendConfig `yaml:"backend"`
	Cache   CacheConfig   `yaml:"cache"`

	Logger *slog.Logger `yaml:"-"`
}

// BackendConfig describes the remote iSCSI target.
type BackendConfig struct {
	Address      string `yaml:"address"`
	TargetIQN    string `yaml:"target_iqn"`
	InitiatorIQN string `yaml:"initiator_iqn"`
	Auth         string `yaml:"auth"`
	Username     string `yaml:"username"`
	Secret       string `yaml:"secret"`

	DialTimeout string `yaml:"dial_timeout"`
	IOTimeout   string `yaml:"io_timeout"`

	MaxRecvDataSegmentLength int `yaml:"max_recv_data_segment_length"`
	MaxBurstLength           int `yaml:"max_burst_length"`

	// Sessions caps how many independent backing sessions the proxy may hold. Zero
	// selects backend.MaxSessions. The proxy opens backend.DefaultSessions eagerly
	// at mount time and grows towards this cap only while the client pipelines
	// more commands than there are sessions, which is what keeps the backing
	// queue depth from being collapsed to one.
	Sessions int `yaml:"sessions"`
}

// CacheConfig describes the read cache.
type CacheConfig struct {
	// Mode must be "writearound"; the MVP has no writeback.
	Mode string `yaml:"mode"`
	// BlockSize/SectorSize are the cache geometry.
	BlockSize  int `yaml:"block_size"`
	SectorSize int `yaml:"sector_size"`
	// IdentityBinding drops the cache when the backing LUN identity changes.
	// It defaults to true and the MVP does not support turning it off.
	IdentityBinding *bool `yaml:"identity_binding"`

	L1 struct {
		SizeBytes int64 `yaml:"size_bytes"`
	} `yaml:"l1"`

	L2 struct {
		Enabled   bool   `yaml:"enabled"`
		Path      string `yaml:"path"`
		SizeBytes int64  `yaml:"size_bytes"`
	} `yaml:"l2"`

	Eviction struct {
		Policy        string  `yaml:"policy"`
		NewQuotaPct   int     `yaml:"new_quota_pct"`
		HighWatermark float64 `yaml:"high_watermark"`
		LowWatermark  float64 `yaml:"low_watermark"`
	} `yaml:"eviction"`

	ScanDetection struct {
		Enabled   *bool `yaml:"enabled"`
		Threshold int   `yaml:"threshold"`
	} `yaml:"scan_detection"`

	Prefetch struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"prefetch"`
}

func (c *CacheConfig) applyDefaults() {
	if c.Mode == "" {
		c.Mode = "writearound"
	}
	if c.BlockSize == 0 {
		c.BlockSize = cache.DefaultBlockSize
	}
	if c.SectorSize == 0 {
		c.SectorSize = cache.DefaultSectorSize
	}
	if c.L1.SizeBytes == 0 {
		c.L1.SizeBytes = 256 << 20
	}
	if c.Eviction.Policy == "" {
		c.Eviction.Policy = "s3-fifo"
	}
	if c.ScanDetection.Enabled == nil {
		on := true
		c.ScanDetection.Enabled = &on
	}
	if c.ScanDetection.Threshold == 0 {
		c.ScanDetection.Threshold = 32
	}
}

// validateCacheConfig applies the defaults and rejects the configurations the
// MVP does not implement. Shared by Config.validate and the portal.
func validateCacheConfig(cc *CacheConfig) error {
	cc.applyDefaults()
	if cc.Mode != "writearound" {
		return fmt.Errorf("iscsicache: cache mode %q is not supported (only writearound)", cc.Mode)
	}
	if cc.Eviction.Policy != "s3-fifo" {
		return fmt.Errorf("iscsicache: eviction policy %q is not supported (only s3-fifo)", cc.Eviction.Policy)
	}
	if cc.Prefetch.Enabled {
		return errors.New("iscsicache: prefetch is not supported by the MVP")
	}
	if cc.IdentityBinding != nil && !*cc.IdentityBinding {
		return errors.New("iscsicache: identity_binding cannot be disabled")
	}
	if cc.L2.Enabled && cc.L2.Path == "" {
		return errors.New("iscsicache: cache.l2.path is required when l2 is enabled")
	}
	return nil
}

func (c *Config) validate() error {
	if c.TargetIQN == "" {
		return errors.New("iscsicache: target_iqn is required")
	}
	if c.Backend.Address == "" {
		return errors.New("iscsicache: backend.address is required")
	}
	if c.Backend.TargetIQN == "" {
		return errors.New("iscsicache: backend.target_iqn is required")
	}
	return validateCacheConfig(&c.Cache)
}

// Stats aggregates the counters of the layers.
type Stats struct {
	Cache    cache.Snapshot
	Proxy    proxy.StatsSnapshot
	L2Slots  int
	L2Used   int
	Sessions int64
	// BlockSize is the cache block size, which is also the L2 slot size; callers
	// use it to turn L2Slots/L2Used into bytes.
	BlockSize int
}

// Service is a running proxy instance.
type Service struct {
	cfg Config
	log *slog.Logger

	be  *backend.Pool
	l2  *l2.File
	c   *cache.Cache
	px  *proxy.Proxy
	tgt *frontend.Target

	mu   sync.Mutex
	done bool
}

// New dials the backing target, derives the cache namespace, opens the L2 file
// and builds the frontend target. It does not start listening; call Start.
func New(ctx context.Context, cfg Config) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	rt, err := buildTarget(ctx, log, TargetConfig{
		TargetIQN: cfg.TargetIQN,
		Backend:   cfg.Backend,
		Cache:     cfg.Cache,
		Vendor:    cfg.Vendor,
		Product:   cfg.Product,
		Revision:  cfg.Revision,
	})
	if err != nil {
		return nil, err
	}

	s := &Service{cfg: cfg, log: log, be: rt.be, l2: rt.l2, c: rt.c, px: rt.px}
	tgt, err := frontend.New(frontend.Config{
		ListenAddr: cfg.ListenAddr,
		TargetIQN:  cfg.TargetIQN,
		Vendor:     rt.px.Device().Vendor,
		Product:    rt.px.Device().Product,
		Revision:   rt.px.Device().Revision,
		Logger:     log.With("component", "frontend"),
	}, rt.px)
	if err != nil {
		_ = rt.close()
		return nil, err
	}
	s.tgt = tgt
	return s, nil
}

// Addr returns the address the frontend listens on.
func (s *Service) Addr() net.Addr { return s.tgt.Addr() }

// Start serves the target until ctx is cancelled or Close is called.
func (s *Service) Start(ctx context.Context) error {
	err := s.tgt.Serve(ctx)
	if err != nil && s.closed() {
		return nil
	}
	return err
}

// Cache returns the underlying cache, exposed for metrics and tests.
func (s *Service) Cache() *cache.Cache { return s.c }

// Proxy returns the command executor, exposed for metrics and tests.
func (s *Service) Proxy() *proxy.Proxy { return s.px }

// DeviceInfo reports the LUN as the proxy presents it.
func (s *Service) DeviceInfo() frontend.DeviceInfo { return s.px.Device() }

// Stats returns an aggregated counter snapshot.
func (s *Service) Stats() Stats {
	st := Stats{}
	if s.c != nil {
		st.Cache = s.c.Stats()
		st.BlockSize = s.c.BlockSize()
	}
	if s.px != nil {
		st.Proxy = s.px.Stats()
	}
	if s.l2 != nil {
		st.L2Slots = s.l2.Slots()
		st.L2Used = s.l2.UsedSlots()
	}
	return st
}

func (s *Service) closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Close stops the target, flushes and closes the L2 file and logs out of the
// backing target.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	s.mu.Unlock()

	var errs []error
	if s.tgt != nil {
		if err := s.tgt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.l2 != nil {
		if err := s.l2.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := s.l2.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.be != nil {
		if err := s.be.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
