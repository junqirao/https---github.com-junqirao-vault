package frontend_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
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
	stop func()
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
	storage := e2eStartStorage(b, ctx, link)

	be, err := backend.DialPool(ctx, backend.Config{
		Address: storage.addr, TargetIQN: storage.iqn,
		IOTimeout: 10 * time.Second, DialTimeout: 5 * time.Second, Logger: e2eLogger(),
	})
	if err != nil {
		b.Fatalf("backing dial: %v", err)
	}

	pxCfg := proxy.Config{Backend: be, TargetIQN: "iqn.e2e:proxy", Logger: e2eLogger()}
	if cached {
		c, err := cache.New(cache.Config{
			BlockSize: cache.DefaultBlockSize, SectorSize: 4096, L1Bytes: 16 << 20,
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
		addr: target.Addr().String(), iqn: "iqn.e2e:proxy",
		stop: func() { _ = target.Close(); _ = be.Close(); storage.stop() },
	}
}

func e2eDial(b *testing.B, ctx context.Context, addr, iqn string) *backend.Initiator {
	b.Helper()
	it, err := backend.Dial(ctx, backend.Config{
		Address: addr, TargetIQN: iqn,
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
					start = seq.Add(uint64(blocks)) - uint64(blocks)
					if start+uint64(blocks) > devBlocks {
						start = 0
					}
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
