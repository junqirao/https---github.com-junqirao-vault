package iscsiproxy

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/identity"
	"vault/internal/iscsicache/l2"
)

const (
	// defaultDialTimeout bounds the TCP dial to the backing target.
	defaultDialTimeout = 10 * time.Second
	// maxTeeBytes bounds how large a read the proxy will collect for a cache
	// fill. A larger read is forwarded without filling: the collection buffer
	// would dominate memory and a streaming read is a one-off scan anyway.
	maxTeeBytes = 4 << 20
	// maxPending bounds the per-session in-flight command table. A full table
	// drops the observation, never the command.
	maxPending = 256
	// socketBuffer is applied to both sockets, following the v1 tuning.
	socketBuffer = 4 << 20
	// scanBuckets mirrors the cache package's sequential-scan detector fan-out.
	scanBuckets = 64
)

// L2Config describes the optional file backed second level cache.
type L2Config struct {
	Enabled   bool
	Path      string
	SizeBytes int64
}

// CacheConfig describes the per-target read cache. Unset geometry takes the
// cache package's defaults.
type CacheConfig struct {
	BlockSize      int
	SectorSize     int
	L1Bytes        int64
	L2             L2Config
	ScanEnabled    bool
	ScanThreshold  int
	FillWorkers    int
	FillQueueDepth int
}

// BackendConfig describes the remote iSCSI target the proxy forwards to. The
// backend session is established by the initiator itself (the proxy only dials
// TCP), so no credentials live here: CHAP runs end to end through the splice.
type BackendConfig struct {
	// Address is the backend target's host:port.
	Address string
	// TargetIQN is the backing target's IQN. It feeds the cache identity and the
	// logs; the proxy never announces it.
	TargetIQN string
	// DialTimeout is a Go duration string; empty takes the default.
	DialTimeout string
}

// TargetConfig describes one cached target behind a Portal.
type TargetConfig struct {
	// Key identifies the target inside the portal, normally the allocation.
	Key     string
	Backend BackendConfig
	Cache   CacheConfig
	// NoCache turns the target into a pure pass-through: the proxy still forwards
	// every command and counts it, but never admits anything into L1/L2. It is how
	// a volume that the user did not enable caching for still goes through the
	// proxy, so its read/write IOPS stay observable.
	NoCache bool
}

// counters are the per-target forwarding counters.
type counters struct {
	reads         atomic.Int64
	writes        atomic.Int64
	invalidations atomic.Int64
	localCmds     atomic.Int64
	passthrough   atomic.Int64
	degraded      atomic.Int64
}

// target serves one allocation: a listener the initiator connects to, a backend
// it dials per connection, and the cache they share.
type target struct {
	key     string
	iqn     string
	backend string
	dial    time.Duration
	cc      CacheConfig
	// noCache makes the target a pure pass-through: no cache is ever bound.
	noCache bool
	log     *slog.Logger

	ln   net.Listener
	addr net.Addr

	mu          sync.Mutex
	closed      bool
	cache       *cache.Cache
	l2          *l2.File
	blockSize   uint32
	blockCount  uint64
	serial      string
	wwid        string
	bindStarted bool

	sessMu   sync.Mutex
	sessions map[*session]struct{}
	scan     scanDetector

	stats counters
}

func newTarget(log *slog.Logger, tc TargetConfig) (*target, error) {
	if strings.TrimSpace(tc.Backend.Address) == "" {
		return nil, errors.New("iscsiproxy: backend.address is required")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("iscsiproxy: listen: %w", err)
	}
	t := &target{
		key:      strings.TrimSpace(tc.Key),
		iqn:      strings.TrimSpace(tc.Backend.TargetIQN),
		backend:  strings.TrimSpace(tc.Backend.Address),
		dial:     defaultDialTimeout,
		cc:       tc.Cache,
		noCache:  tc.NoCache,
		log:      log,
		ln:       ln,
		addr:     ln.Addr(),
		sessions: make(map[*session]struct{}),
		scan:     newScanDetector(tc.Cache.ScanEnabled, tc.Cache.ScanThreshold),
	}
	if d, err := time.ParseDuration(strings.TrimSpace(tc.Backend.DialTimeout)); err == nil && d > 0 {
		t.dial = d
	}
	return t, nil
}

// serve accepts initiator connections until the listener is closed.
func (t *target) serve() {
	defer func() {
		// Accept 循环本身 panic（例如监听套接字被异常关闭）同样会带走整个代理进程，
		// 与数据泵同理：兜住并记日志，绝不能无声杀死代理（见 session.guardedPump）。
		if r := recover(); r != nil {
			t.log.Error("iscsiproxy: 接受连接循环 panic，该目标停止服务（代理继续运行）",
				"engine", Version, "key", t.key, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.guardedHandle(c)
	}
}

// guardedHandle 处理一条发起端连接并兜住 panic：单条连接的异常只影响它自己，
// 不允许升级为整个代理进程的崩溃（理由见 session.guardedPump）。
func (t *target) guardedHandle(ic net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			t.log.Error("iscsiproxy: 处理发起端连接 panic，已隔离该连接（代理继续运行）",
				"engine", Version, "key", t.key, "panic", r, "stack", string(debug.Stack()))
			_ = ic.Close()
		}
	}()
	t.handle(ic)
}

// handle dials the backend for a fresh initiator connection and runs the splice
// session. A backend that cannot be reached rejects the local connection: the
// agent then falls back to a direct mount.
func (t *target) handle(ic net.Conn) {
	// 发起端连到本机代理的**第一条**证据。健康会话在下面虽然也会打日志，但只有这条能
	// 区分"发起端压根没连代理（走成了直连，指标必然全 0）"和"连上了但没绑定缓存"。
	t.log.Info("iscsiproxy: 发起端已连接本机缓存代理",
		"engine", Version, "key", t.key, "remote", ic.RemoteAddr().String(), "backend", t.backend)
	tuneSocket(ic)
	bc, err := net.DialTimeout("tcp", t.backend, t.dial)
	if err != nil {
		t.log.Warn("iscsiproxy: 后端不可达，拒绝本地连接", "backend", t.backend, "error", err)
		_ = ic.Close()
		return
	}
	tuneSocket(bc)
	s := newSession(t, ic, bc)
	t.addSession(s)
	s.run()
	t.removeSession(s)
}

func (t *target) addSession(s *session) {
	t.sessMu.Lock()
	t.sessions[s] = struct{}{}
	t.sessMu.Unlock()
}

func (t *target) removeSession(s *session) {
	t.sessMu.Lock()
	delete(t.sessions, s)
	t.sessMu.Unlock()
}

func (t *target) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// getCache returns the bound cache, or nil while the target is unbound (all
// traffic is a pure pass-through until the backing LUN's identity is seen).
func (t *target) getCache() *cache.Cache {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cache
}

func (t *target) getL2() *l2.File {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.l2
}

// lunBlockSize returns the backing LUN's logical block size, or 0 until a READ
// CAPACITY response has been observed.
func (t *target) lunBlockSize() uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.blockSize
}

// identitySeen reports whether the LUN has been described well enough to bind a
// cache namespace.
func (t *target) identitySeen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.blockSize != 0 && (t.serial != "" || t.wwid != "")
}

func (t *target) setSerial(s string) {
	t.mu.Lock()
	if t.serial == "" {
		t.serial = strings.TrimSpace(s)
	}
	t.mu.Unlock()
}

func (t *target) setWWID(s string) {
	t.mu.Lock()
	if t.wwid == "" {
		t.wwid = strings.TrimSpace(s)
	}
	t.mu.Unlock()
}

func (t *target) setCapacity(blockSize uint32, blockCount uint64) {
	t.mu.Lock()
	if t.blockSize == 0 && blockSize > 0 {
		t.blockSize = blockSize
		t.blockCount = blockCount
	}
	t.mu.Unlock()
}

// maybeBind builds the cache once the LUN geometry and at least one identity
// page have been observed. It runs at most once successfully; a failure releases
// the reservation so a later observation retries.
func (t *target) maybeBind() {
	t.mu.Lock()
	// noCache 目标永不建缓存：纯透传，只观测 IOPS（未开缓存的库也经代理的形态）。
	if t.closed || t.bindStarted || t.noCache {
		t.mu.Unlock()
		return
	}
	if t.blockSize == 0 || (t.serial == "" && t.wwid == "") {
		t.mu.Unlock()
		return
	}
	t.bindStarted = true
	desc := identity.Descriptor{
		TargetIQN:  t.iqn,
		Serial:     t.serial,
		WWID:       t.wwid,
		BlockCount: t.blockCount,
		BlockSize:  t.blockSize,
	}
	cc := t.cc
	t.mu.Unlock()

	ns := desc.Namespace()
	if !desc.Full() {
		t.log.Warn("iscsiproxy: 后端 LUN 未报告序列号与 WWID，缓存无法识别换盘")
	}
	c, l2f, err := buildCache(cc, ns, t.log)
	if err != nil {
		t.log.Warn("iscsiproxy: 建立缓存失败，继续纯透传", "error", err)
		t.mu.Lock()
		t.bindStarted = false
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = closeCache(c, l2f)
		return
	}
	t.cache, t.l2 = c, l2f
	t.mu.Unlock()
	t.log.Info("iscsiproxy: 缓存已绑定后端盘指纹",
		"engine", Version, "key", t.key, "namespace", ns[:16],
		"serial", desc.Serial, "wwid", desc.WWID,
		"block_size", desc.BlockSize, "blocks", desc.BlockCount)
}

// close stops the listener, drops every session and releases the cache.
func (t *target) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	c, l2f := t.cache, t.l2
	t.cache, t.l2 = nil, nil
	t.mu.Unlock()

	if t.ln != nil {
		_ = t.ln.Close()
	}
	t.sessMu.Lock()
	ss := make([]*session, 0, len(t.sessions))
	for s := range t.sessions {
		ss = append(ss, s)
	}
	t.sessMu.Unlock()
	for _, s := range ss {
		s.close()
	}
	return closeCache(c, l2f)
}

func (t *target) statsSnapshot() Stats {
	st := Stats{Proxy: StatsSnapshot{
		Reads:         t.stats.reads.Load(),
		Writes:        t.stats.writes.Load(),
		Invalidations: t.stats.invalidations.Load(),
		LocalCmds:     t.stats.localCmds.Load(),
		Passthrough:   t.stats.passthrough.Load(),
		Degraded:      t.stats.degraded.Load(),
	}}
	if c := t.getCache(); c != nil {
		st.Cache = c.Stats()
		st.BlockSize = c.BlockSize()
	}
	if f := t.getL2(); f != nil {
		st.L2Slots = f.Slots()
		st.L2Used = f.UsedSlots()
	}
	return st
}

// buildCache opens the L2 file and builds the L1+L2 cache.
func buildCache(cc CacheConfig, ns string, log *slog.Logger) (*cache.Cache, *l2.File, error) {
	var store cache.L2
	var l2f *l2.File
	if cc.L2.Enabled && strings.TrimSpace(cc.L2.Path) != "" {
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
			l2f, store = f, f
		case errors.Is(err, l2.ErrUnsupported):
			log.Warn("iscsiproxy: L2 文件存储在本平台不可用，仅使用 L1")
		default:
			return nil, nil, err
		}
	}
	c, err := cache.New(cache.Config{
		BlockSize:      cc.BlockSize,
		SectorSize:     cc.SectorSize,
		L1Bytes:        cc.L1Bytes,
		ScanEnabled:    cc.ScanEnabled,
		ScanThreshold:  cc.ScanThreshold,
		FillWorkers:    cc.FillWorkers,
		FillQueueDepth: cc.FillQueueDepth,
		L2:             store,
		Logger:         log.With("component", "cache"),
	})
	if err != nil {
		if l2f != nil {
			_ = l2f.Close()
		}
		return nil, nil, err
	}
	return c, l2f, nil
}

// closeCache stops the fill workers before the L2 file, then flushes it.
func closeCache(c *cache.Cache, f *l2.File) error {
	var errs []error
	if c != nil {
		c.Close()
	}
	if f != nil {
		if err := f.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// tuneSocket applies the latency and buffer tuning shared by the design.
func tuneSocket(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetReadBuffer(socketBuffer)
		_ = tc.SetWriteBuffer(socketBuffer)
	}
}

// scanDetector implements sequential-scan admission control for the tee path: a
// run of consecutive reads is recognised as a scan and not admitted into the
// cache, so a one-off large sequential read does not flush the hot set.
type scanDetector struct {
	enabled   bool
	threshold int
	buckets   []scanBucket
}

type scanBucket struct {
	mu      sync.Mutex
	lastEnd int64
	run     int
}

func newScanDetector(enabled bool, threshold int) scanDetector {
	if threshold <= 0 {
		threshold = 32
	}
	return scanDetector{enabled: enabled, threshold: threshold, buckets: make([]scanBucket, scanBuckets)}
}

// scan reports whether the request continues a scan that has crossed the
// threshold.
func (d *scanDetector) scan(off, length int64) bool {
	if !d.enabled {
		return false
	}
	b := &d.buckets[(uint64(off)>>20)%uint64(len(d.buckets))]
	b.mu.Lock()
	defer b.mu.Unlock()
	if off == b.lastEnd {
		b.run++
	} else {
		b.run = 0
	}
	b.lastEnd = off + length
	return b.run >= d.threshold
}

// parseCapacity decodes a READ CAPACITY(10/16) response into the LUN's block
// size and block count.
func parseCapacity(data []byte) (blockSize uint32, blockCount uint64, ok bool) {
	switch len(data) {
	case 8:
		last := binary.BigEndian.Uint32(data[0:4])
		bs := binary.BigEndian.Uint32(data[4:8])
		return bs, uint64(last) + 1, bs > 0
	default:
		if len(data) < 32 {
			return 0, 0, false
		}
		last := binary.BigEndian.Uint64(data[0:8])
		bs := binary.BigEndian.Uint32(data[8:12])
		return bs, last + 1, bs > 0
	}
}

// hexIdentity renders a VPD 0x83 payload into a stable identifier string.
func hexIdentity(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	return hex.EncodeToString(payload)
}
