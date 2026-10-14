package frontend_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsicache/proxy"
)

// The end-to-end benchmark runs the whole module over its own two protocol
// endpoints, the way the product is deployed:
//
//	client initiator -> frontend target -> proxy (+cache) -> backend initiator
//	                 -> storage target -> storage proxy -> in-memory LUN
//
// "direct" dials the storage target instead, so the difference between the two
// is exactly what the proxy adds: the cache, the second protocol hop and the
// backing initiator's PDU handling. That is the overhead the product must keep
// under a few percent, and the reason this benchmark exists: the proxy-level
// benchmark cannot see the PDU cost because it calls Execute directly.
const (
	e2eBlockSize = 512
	e2eDeviceMB  = 64
)

// e2eLUN is an in-memory block device. Its Exec is deliberately allocation-cheap
// so the measurement reflects the proxy, not the fake storage.
//
// e2eLink is the storage link the backing target sits behind: a round trip
// charged to every command and a byte-serial bandwidth shared by every
// connection crossing it.
//
// It is the missing half of the local-only benchmark. With both the proxy and
// the storage on loopback the local TCP stack is the bottleneck and the two-hop
// design measures only its own copy cost. A real mount is the opposite: the
// array is behind a link that is far slower than the local stack, so the link
// is the floor for both a direct mount and a proxied one, and the question the
// benchmark has to answer is whether the proxy keeps the link busy. A direct
// mount does that by keeping its command window in flight on one session; a
// proxy that cannot keep as many commands in flight leaves the same link idle
// and reports a fraction of direct even though its per-command work is cheap.
type e2eLink struct {
	rtt time.Duration
	bw  int64 // bytes per second, 0 = unlimited

	// mu serialises the bytes on the wire. A link carries one transfer at a
	// time whoever is sending, so the aggregate bandwidth is exactly bw however
	// many connections share it. Propagation (rtt) is charged outside the lock,
	// because it overlaps across commands.
	mu sync.Mutex
}

// roundTrip charges one command: propagation first, then its bytes on the wire.
func (l *e2eLink) roundTrip(n int) {
	if l == nil {
		return
	}
	if l.rtt > 0 {
		time.Sleep(l.rtt)
	}
	if l.bw <= 0 || n <= 0 {
		return
	}
	l.mu.Lock()
	time.Sleep(time.Duration(float64(n) / float64(l.bw) * float64(time.Second)))
	l.mu.Unlock()
}

type e2eLUN struct {
	data []byte
	link *e2eLink
}

func newE2ELUN(link *e2eLink) *e2eLUN {
	d := make([]byte, e2eDeviceMB<<20)
	for i := range d {
		d[i] = byte(i / e2eBlockSize)
	}
	return &e2eLUN{data: d, link: link}
}

func (m *e2eLUN) Info() backend.DeviceInfo {
	return backend.DeviceInfo{
		Vendor: "E2E", Product: "Bench Disk", Revision: "0001",
		Serial: "E2ELUN", BlockSize: e2eBlockSize,
		BlockCount: uint64(len(m.data)) / e2eBlockSize,
	}
}

func (m *e2eLUN) Close() error { return nil }

func (m *e2eLUN) Exec(_ context.Context, cdb []byte, _ []byte, _ int) (*backend.Result, error) {
	switch cdb[0] {
	case iscsi.SCSIRead10, iscsi.SCSIRead16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		off := int64(p.LBA) * e2eBlockSize
		n := int64(p.Blocks) * e2eBlockSize
		m.link.roundTrip(int(n))
		if off < 0 || off+n > int64(len(m.data)) {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
		}
		// The LUN hands back a view of its own immutable contents rather than a
		// fresh copy: allocating a transfer-sized buffer per read here would put
		// garbage on the collector during the measurement and drain the proxy's
		// buffer pool, which is exactly the effect under test. Nothing writes to
		// the returned bytes (writes are no-ops), so sharing them is safe.
		return &backend.Result{Status: iscsi.StatusGood, Data: m.data[off : off+n]}, nil
	case iscsi.SCSIWrite10, iscsi.SCSIWrite16:
		m.link.roundTrip(0)
		return &backend.Result{Status: iscsi.StatusGood}, nil
	default:
		m.link.roundTrip(0)
		return &backend.Result{Status: iscsi.StatusCheckCondition,
			Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidCommandOperationCode)}, nil
	}
}

func e2eReadCDB(lba uint64, blocks uint32) []byte {
	cdb := make([]byte, 16)
	if lba <= 0xffffffff && blocks <= 0xffff {
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
		binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
		return cdb[:10]
	}
	cdb[0] = iscsi.SCSIRead16
	binary.BigEndian.PutUint64(cdb[2:10], lba)
	binary.BigEndian.PutUint32(cdb[10:14], blocks)
	return cdb
}

// e2eStack is one client session and everything it talks to.
type e2eStack struct {
	client *backend.Initiator
	stop   func()
}

func (s *e2eStack) close() {
	_ = s.client.Close()
	s.stop()
}

func e2eLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// e2eEndpoint is a target the benchmark dials. The concurrent benchmark gives
// every worker its own client session, so it needs the address rather than one
// client: a single client session is strictly serial and hides the client's own
// queue depth.
type e2eEndpoint struct {
	addr string
	iqn  string
	// cache is the middle proxy's L1, when it has one, so a benchmark can report
	// what the fills actually bought.
	cache *cache.Cache
	stop  func()
}

// e2eStartStorage brings up the backing target the product would talk to: the
// in-memory LUN behind a passthrough proxy and its own target.
func e2eStartStorage(b *testing.B, ctx context.Context, link *e2eLink) *e2eEndpoint {
	b.Helper()
	lun := newE2ELUN(link)
	storageProxy, err := proxy.New(proxy.Config{
		Backend: lun, TargetIQN: "iqn.e2e:storage", Logger: e2eLogger(),
	})
	if err != nil {
		b.Fatalf("storage proxy: %v", err)
	}
	target, err := frontend.New(frontend.Config{
		ListenAddr: "127.0.0.1:0", TargetIQN: "iqn.e2e:storage",
	}, storageProxy)
	if err != nil {
		b.Fatalf("storage target: %v", err)
	}
	go func() { _ = target.Serve(ctx) }()
	return &e2eEndpoint{
		addr: target.Addr().String(), iqn: "iqn.e2e:storage",
		stop: func() { _ = target.Close() },
	}
}

// e2eStartProxy puts the module under test in front of the storage endpoint.
// The backing initiator is a fixed pool of pipelining sessions, so the proxy
// fans commands over several sockets exactly as the product does and the local
// hop is not pinned to one core.
func e2eStartProxy(b *testing.B, ctx context.Context, cached bool, link *e2eLink) *e2eEndpoint {
	b.Helper()
	l1 := int64(0)
	if cached {
		l1 = 16 << 20
	}
	return e2eStartProxySize(b, ctx, l1, 0, link)
}

// e2eStartProxySize builds the middle proxy with an explicit L1 budget and shard
// count; l1 == 0 leaves the cache off. The size is a knob because the cache's
// usefulness is a question of geometry: a sequential stream revisits a shard
// only every `shards` blocks, so at 64 KiB blocks over a 64 MiB region a
// small-shard L1 is refilled with blocks that are evicted before anything reads
// them again, while a large enough L1 turns the same fills into hits.
//
// Sizing L1 downwards is not free: New floors a shard at eight blocks, so the
// smallest L1 the defaults allow is 256 shards * 8 blocks * 64 KiB = 128 MiB of
// shards of capacity no matter what budget is asked for. Fewer shards is what
// actually shrinks the cache.
func e2eStartProxySize(b *testing.B, ctx context.Context, l1 int64, shards int, link *e2eLink) *e2eEndpoint {
	b.Helper()
	storage := e2eStartStorage(b, ctx, link)

	be, err := backend.DialPool(ctx, backend.Config{
		Address: storage.addr, TargetIQN: storage.iqn,
		IOTimeout: 10 * time.Second, DialTimeout: 5 * time.Second, Logger: e2eLogger(),
	})
	if err != nil {
		b.Fatalf("backing dial: %v", err)
	}

	pxCfg := proxy.Config{Backend: be, TargetIQN: "iqn.e2e:proxy", Logger: e2eLogger()}
	var c *cache.Cache
	if l1 > 0 {
		// ScanEnabled is off by default so these numbers stay comparable with the
		// ones recorded before the knob existed. The service turns scan admission
		// on (CacheConfig.applyDefaults), so VAULT_BENCH_SCAN=1 measures what a
		// deployment actually runs and is also how the stream admission rule is
		// A/B'd: the large reads below are streamed, which never consults the run
		// detector, so with it on the only admission that differs is the stream
		// span rule.
		c, err = cache.New(cache.Config{
			BlockSize: cache.DefaultBlockSize, SectorSize: 4096, L1Bytes: l1, Shards: shards,
			ScanEnabled: os.Getenv("VAULT_BENCH_SCAN") != "",
		})
		if err != nil {
			b.Fatalf("cache: %v", err)
		}
		pxCfg.Cache = c
	}
	px, err := proxy.New(pxCfg)
	if err != nil {
		b.Fatalf("proxy: %v", err)
	}
	target, err := frontend.New(frontend.Config{
		ListenAddr: "127.0.0.1:0", TargetIQN: "iqn.e2e:proxy",
	}, px)
	if err != nil {
		b.Fatalf("frontend target: %v", err)
	}
	go func() { _ = target.Serve(ctx) }()
	return &e2eEndpoint{
		addr: target.Addr().String(), iqn: "iqn.e2e:proxy", cache: c,
		stop: func() { _ = target.Close(); _ = be.Close(); storage.stop() },
	}
}

// e2eReport fills logs what the cache did during the timed run, so a throughput
// number can be read against the fills that produced it.
func e2eReportFills(b *testing.B, ep *e2eEndpoint) {
	b.Helper()
	if ep.cache == nil {
		return
	}
	s := ep.cache.Stats()
	b.Logf("cache: reads=%d l1hits=%d requesthits=%d fills=%d evictions=%d scanbypass=%d fillskips=%d l1used=%d/%d",
		s.Reads, s.L1Hits, s.RequestHits, s.Fills, s.Evictions, s.ScanBypass, s.FillSkips, s.L1UsedBytes, s.L1LimitBytes)
}

func e2eDial(b *testing.B, ctx context.Context, addr, iqn string) *backend.Initiator {
	b.Helper()
	return e2eDialWith(b, ctx, addr, iqn, 0)
}

// e2eDialWith dials with an explicit MaxRecvDataSegmentLength, so a run can
// announce the 64 KiB segments a Windows initiator announces instead of this
// initiator's 256 KiB default.
func e2eDialWith(b *testing.B, ctx context.Context, addr, iqn string, mrdsl int) *backend.Initiator {
	b.Helper()
	it, err := backend.Dial(ctx, backend.Config{
		Address: addr, TargetIQN: iqn, MaxRecvDataSegmentLength: mrdsl,
		IOTimeout: 10 * time.Second, DialTimeout: 5 * time.Second, Logger: e2eLogger(),
	})
	if err != nil {
		b.Fatalf("dial %s: %v", iqn, err)
	}
	return it
}

// startE2E builds the stack. cached adds the read cache to the proxy in the
// middle; when false the middle proxy is a plain passthrough, which makes the
// benchmark measure pure protocol overhead.
func startE2E(b *testing.B, cached bool) *e2eStack {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ep := e2eStartProxy(b, ctx, cached, nil)
	return &e2eStack{
		client: e2eDial(b, ctx, ep.addr, ep.iqn),
		stop:   func() { cancel(); ep.stop() },
	}
}

// startE2EDirect dials the storage target with no middle proxy: the reference
// the proxy is measured against.
func startE2EDirect(b *testing.B) *e2eStack {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ep := e2eStartStorage(b, ctx, nil)
	return &e2eStack{
		client: e2eDial(b, ctx, ep.addr, ep.iqn),
		stop:   func() { cancel(); ep.stop() },
	}
}

func e2eSizeName(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// e2eNextStart hands out the next 1 MiB-aligned offset of a stream that cycles
// the whole device. The position has to be reduced modulo the device on every
// call: an earlier version reset the offset to zero once the counter ran past
// the device, which left every later read pinned at offset zero, so a "stream"
// benchmark quietly became a hot-read benchmark that the cache served from L1
// without ever filling or evicting.
func e2eNextStart(seq *atomic.Uint64, blocks, devBlocks uint64) uint64 {
	span := devBlocks / blocks * blocks
	if span == 0 {
		return 0
	}
	return (seq.Add(blocks) - blocks) % span
}

func e2eRunReads(b *testing.B, s *e2eStack, size int64, stream bool) {
	b.Helper()
	blocks := uint32(size / e2eBlockSize)
	devBlocks := uint64(e2eDeviceMB<<20) / e2eBlockSize
	ctx := context.Background()
	start := uint64(0)

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cdb []byte
		if stream {
			if start+uint64(blocks) > devBlocks {
				start = 0
			}
			cdb = e2eReadCDB(start, blocks)
			start += uint64(blocks)
		} else {
			cdb = e2eReadCDB(0, blocks)
		}
		res, err := s.client.Exec(ctx, cdb, nil, int(size))
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		if len(res.Data) != int(size) {
			b.Fatalf("read returned %d of %d bytes", len(res.Data), size)
		}
	}
	b.StopTimer()
}

// BenchmarkEndToEndRead is the throughput/CPU yardstick for the whole module.
// Compare the proxy rows against direct: the gap is the proxy's total cost,
// including the protocol layer, which the proxy-level benchmark cannot see.
func BenchmarkEndToEndRead(b *testing.B) {
	for _, size := range []int64{128 << 10, 1 << 20} {
		name := e2eSizeName(size)
		b.Run("direct/"+name, func(b *testing.B) {
			s := startE2EDirect(b)
			defer s.close()
			e2eRunReads(b, s, size, true)
		})
		b.Run("proxy-stream/"+name, func(b *testing.B) {
			s := startE2E(b, true)
			defer s.close()
			e2eRunReads(b, s, size, true)
		})
		b.Run("proxy-hot/"+name, func(b *testing.B) {
			s := startE2E(b, true)
			defer s.close()
			e2eRunReads(b, s, size, false)
		})
	}
}

// e2eRunReadsParallel issues reads from workers sessions at once.
//
// A session is strictly serial, so a benchmark that drives one client session
// (everything above) measures queue depth one and cannot see the backing
// concurrency at all. This one dials one session per worker, which is what a
// real initiator's command window looks like from the target's side, and is the
// only way the proxy's own queue depth shows up in the numbers.
func e2eRunReadsParallel(b *testing.B, ctx context.Context, ep *e2eEndpoint, size int64, stream bool, workers int) {
	b.Helper()
	if workers > b.N {
		workers = b.N
	}
	if workers < 1 {
		workers = 1
	}
	blocks := uint32(size / e2eBlockSize)
	devBlocks := uint64(e2eDeviceMB<<20) / e2eBlockSize

	// Dial up front, on the benchmark goroutine: a worker must not call b.Fatal,
	// and login does not belong inside the timed region.
	clients := make([]*backend.Initiator, workers)
	for i := range clients {
		clients[i] = e2eDial(b, ctx, ep.addr, ep.iqn)
		defer clients[i].Close()
	}

	per, extra := b.N/workers, b.N%workers
	var seq atomic.Uint64

	b.SetBytes(size)
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		n := per
		if w < extra {
			n++
		}
		wg.Add(1)
		go func(client *backend.Initiator, n int) {
			defer wg.Done()
			// One buffer per worker, reused: the client end of a real initiator
			// owns its transfer buffers too, and allocating 64 KiB per read here
			// would put garbage on the collector that drains the proxy's own
			// buffer pool and inflates its measured cost.
			buf := make([]byte, size)
			for i := 0; i < n; i++ {
				var start uint64
				if stream {
					start = e2eNextStart(&seq, uint64(blocks), devBlocks)
				}
				res, err := client.ExecInto(ctx, e2eReadCDB(start, blocks), nil, buf)
				if err != nil {
					b.Errorf("read: %v", err)
					return
				}
				if len(res.Data) != int(size) {
					b.Errorf("read returned %d of %d bytes", len(res.Data), size)
					return
				}
			}
		}(clients[w], n)
	}
	wg.Wait()
	b.StopTimer()
}

// BenchmarkEndToEndReadConcurrent is the queue depth the product actually sees.
// The frontend advertises a command window of 64, so a real mount keeps dozens
// of reads in flight, and the proxy's backing queue depth is what decides the
// throughput. Run it beside BenchmarkEndToEndRead: the serial one reports near
// parity, this one reports what a real client gets.
func BenchmarkEndToEndReadConcurrent(b *testing.B) {
	const size = 1 << 20
	for _, workers := range []int{16, 32, 64} {
		name := fmt.Sprintf("qd%d", workers)
		b.Run("direct/"+name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ep := e2eStartStorage(b, ctx, nil)
			defer ep.stop()
			e2eRunReadsParallel(b, ctx, ep, size, true, workers)
		})
		b.Run("proxy-stream/"+name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ep := e2eStartProxy(b, ctx, false, nil)
			defer ep.stop()
			e2eRunReadsParallel(b, ctx, ep, size, true, workers)
		})
	}
}

// e2eRunReadsOneSession drives one client session with depth commands
// outstanding, which is the shape a real initiator presents: a Windows mount
// puts its whole queue depth on the one connection it has. The per-worker
// benchmark above buys depth with extra sessions, so it cannot show what a deep
// queue on one connection does to the proxy, and that is exactly the case a
// CrystalDiskMark sequential run exercises.
func e2eRunReadsOneSession(b *testing.B, ctx context.Context, ep *e2eEndpoint, size int64, mrdsl int, depth int, stream bool) {
	b.Helper()
	if depth < 1 {
		depth = 1
	}
	if depth > b.N {
		depth = b.N
	}
	blocks := uint32(size / e2eBlockSize)
	devBlocks := uint64(e2eDeviceMB<<20) / e2eBlockSize

	client := e2eDialWith(b, ctx, ep.addr, ep.iqn, mrdsl)
	defer client.Close()

	per, extra := b.N/depth, b.N%depth
	var seq atomic.Uint64

	b.SetBytes(size)
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < depth; w++ {
		n := per
		if w < extra {
			n++
		}
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			buf := make([]byte, size)
			for i := 0; i < n; i++ {
				var start uint64
				if stream {
					start = e2eNextStart(&seq, uint64(blocks), devBlocks)
				}
				res, err := client.ExecInto(ctx, e2eReadCDB(start, blocks), nil, buf)
				if err != nil {
					b.Errorf("read: %v", err)
					return
				}
				if len(res.Data) != int(size) {
					b.Errorf("read returned %d of %d bytes", len(res.Data), size)
					return
				}
			}
		}(n)
	}
	wg.Wait()
	b.StopTimer()
}

// BenchmarkEndToEndReadQueueDepth is the CrystalDiskMark sequential profile on
// the connection shape a real mount has: one session, one MiB reads, QD1 against
// QD8. QD8 must not be slower than QD1 — the client is offering the proxy more
// work to overlap, and the only thing the extra depth may cost is bookkeeping.
//
// The mrdsl sub-benchmarks split the client segment size, because that is the
// granularity at which the frontend interleaves several commands' Data-In on the
// one connection: 64 KiB is what the Microsoft initiator announces, 256 KiB is
// this initiator's default. The streams are kept cold: the region cycles the
// whole device, which is four times the 16 MiB L1 the harness builds.
func BenchmarkEndToEndReadQueueDepth(b *testing.B) {
	const size = 1 << 20
	caches := []struct {
		name   string
		l1     int64
		shards int
	}{
		{"nocache", 0, 0},
		// 4 shards * 4 blocks floors the L1 at 1 MiB against a 64 MiB region, so
		// every block a fill installs has been evicted by the time the stream comes
		// round again: the fills are pure overhead. This is the shape a 256 MiB L1
		// under a 1 GiB sequential region has.
		{"l1-thrash-1m", 1 << 20, 4},
		// The whole region fits, so the same fills are read back.
		{"l1-fits-16m", 16 << 20, 0},
	}
	for _, client := range []struct {
		name  string
		mrdsl int
	}{
		{"win64k", 64 << 10},
		{"dsl256k", 256 << 10},
	} {
		for _, cc := range caches {
			for _, depth := range []int{1, 8} {
				qd := fmt.Sprintf("qd%d", depth)
				b.Run(cc.name+"/"+client.name+"/"+qd, func(b *testing.B) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					ep := e2eStartProxySize(b, ctx, cc.l1, cc.shards, nil)
					defer ep.stop()
					e2eRunReadsOneSession(b, ctx, ep, size, client.mrdsl, depth, true)
					e2eReportFills(b, ep)
				})
			}
		}
	}
}

// e2eBackendLatency stands in for the round trip to a backing target over a real
// link: every command pays it, so the backing throughput is the queue depth
// times the read size over it, and nothing else.
const e2eBackendLatency = 1 * time.Millisecond

// BenchmarkEndToEndReadLatency is the regime a real mount lives in. With a round
// trip on every command, a direct mount's throughput scales with the client's
// queue depth; a proxy that cannot keep that many commands in flight to the
// backing target delivers the ratio its own queue depth allows, no matter how
// cheap its per-command work is. That is the shape of "the proxy is at 60% of
// direct" and this benchmark is where it shows up.
func BenchmarkEndToEndReadLatency(b *testing.B) {
	const size = 64 << 10
	for _, workers := range []int{16, 32, 64} {
		name := fmt.Sprintf("qd%d", workers)
		b.Run("direct/"+name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ep := e2eStartStorage(b, ctx, &e2eLink{rtt: e2eBackendLatency})
			defer ep.stop()
			e2eRunReadsParallel(b, ctx, ep, size, true, workers)
		})
		b.Run("proxy-stream/"+name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ep := e2eStartProxy(b, ctx, false, &e2eLink{rtt: e2eBackendLatency})
			defer ep.stop()
			e2eRunReadsParallel(b, ctx, ep, size, true, workers)
		})
	}
}

// BenchmarkEndToEndLink is the production topology: the array is behind a link
// far slower than the local stack, so the link — not the local TCP stack — is
// what both a direct mount and a proxied one are limited by.
//
// A direct mount keeps its whole command window in flight on one session and
// fills the link. The proxy has to do the same through its backing sessions.
// The ratio proxy/direct here is the number the product is judged on: a proxy
// that keeps the link as busy as a direct mount approaches 1.0 however much its
// per-command work costs, and one that cannot keep the same number of commands
// in flight reports the ratio its backing queue depth allows.
func BenchmarkEndToEndLink(b *testing.B) {
	const linkBW = 1250 << 20 // 10 GbE, ~1.25 GB/s
	newLink := func() *e2eLink { return &e2eLink{rtt: 400 * time.Microsecond, bw: linkBW} }

	for _, size := range []int64{64 << 10, 1 << 20} {
		name := e2eSizeName(size)
		for _, workers := range []int{8, 32} {
			qd := fmt.Sprintf("qd%d", workers)
			b.Run("direct/"+qd+"/"+name, func(b *testing.B) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ep := e2eStartStorage(b, ctx, newLink())
				defer ep.stop()
				e2eRunReadsParallel(b, ctx, ep, size, true, workers)
			})
			b.Run("proxy/"+qd+"/"+name, func(b *testing.B) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ep := e2eStartProxy(b, ctx, false, newLink())
				defer ep.stop()
				e2eRunReadsParallel(b, ctx, ep, size, true, workers)
			})
		}
	}
}
