// Command vault-iscsi-bench measures the read cache of the iSCSI cache proxy.
//
// AS SSD Benchmark cannot show whether this cache helps: it is write heavy and
// its reads run once over a working set far larger than L1, so nothing is ever
// re-read. The cache is a writearound read cache, so its effect only appears
// when the same addresses are read twice: the first read misses and goes to the
// backend, the second is served from L1 memory. Sequential reads do not benefit
// either, because scan detection deliberately serves them without filling the
// cache.
//
// This command therefore runs four phases and reports the cache counters around
// each one:
//
//	random 4K cold   every 4K slot of the hot window, read once, in random order
//	random 4K warm   the same addresses again, repeated - this is the cached path
//	sequential 64K   a control: shows that scan detection bypasses the fill
//	write 4K         optional (-allow-write), the path the cache does not help
//
// It runs against an in-process proxy, so the cache counters are visible, and
// -direct runs the same phases against the backing target for a baseline.
//
// Usage:
//
//	go run ./cmd/vault-iscsi-bench -config cmd/vault-iscsi-cache/iscsi-cache.example.yaml
//	go run ./cmd/vault-iscsi-bench -config ... -direct
//	go run ./cmd/vault-iscsi-bench -config ... -qd 8
//
// -qd opens that many connections to the proxy and splits every phase across
// them, so the target sees N commands in flight instead of one: it shows what
// the proxy does with the queue depth a real initiator would use.
//
// The benchmark is read only unless -allow-write is given.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"vault/internal/iscsicache"
	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/iscsi"
)

const mib = 1 << 20

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vault-iscsi-bench: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	configPath string
	direct     bool
	qd         int
	hotMB      int
	seqMB      int
	reps       int
	allowWrite bool
	writeMB    int
	verbose    bool
}

func run() error {
	var o options
	flag.StringVar(&o.configPath, "config", "iscsi-cache.yaml", "path to the proxy YAML configuration")
	flag.BoolVar(&o.direct, "direct", false, "benchmark the backing target directly, without the proxy (baseline)")
	flag.IntVar(&o.qd, "qd", 1, "queue depth: how many connections to open, each keeping one command outstanding")
	flag.IntVar(&o.hotMB, "hot-mb", 64, "size of the random read working set, in MiB (keep it below the L1 size)")
	flag.IntVar(&o.seqMB, "seq-mb", 256, "size of the sequential read control phase, in MiB")
	flag.IntVar(&o.reps, "reps", 4, "how many times the warm phase replays the hot address list")
	flag.BoolVar(&o.allowWrite, "allow-write", false, "add a destructive 4K write phase")
	flag.IntVar(&o.writeMB, "write-mb", 64, "size of the write phase, written to the end of the LUN (destructive)")
	flag.BoolVar(&o.verbose, "verbose", false, "log proxy internals")
	flag.Parse()

	raw, err := os.ReadFile(o.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg iscsicache.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse config %s: %w", o.configPath, err)
	}
	cfg.Logger = logger(o.verbose)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		clients  []*backend.Initiator
		svc      *iscsicache.Service
		serveErr chan error
		label    string
		dialAddr string
		dialIQN  string
	)
	if o.direct {
		label = "direct (no proxy)"
		dialAddr, dialIQN = cfg.Backend.Address, cfg.Backend.TargetIQN
	} else {
		label = "through proxy"
		// Bind an ephemeral port so the benchmark never fights with a running
		// proxy, and keep L2 off so a stale L2 file cannot turn a "cold" read
		// into a hit and spoil the measurement.
		cfg.ListenAddr = "127.0.0.1:0"
		cfg.Cache.L2.Enabled = false
		svc, err = iscsicache.New(ctx, cfg)
		if err != nil {
			return fmt.Errorf("start proxy: %w", err)
		}
		defer svc.Close()
		serveErr = make(chan error, 1)
		go func() { serveErr <- svc.Start(ctx) }()
		dialAddr, dialIQN = svc.Addr().String(), cfg.TargetIQN
	}

	// Open one connection per queue slot. Each connection keeps a single command
	// outstanding, so N connections put N commands in flight at the proxy; that
	// is the queue depth the proxy sees.
	qd := o.qd
	if qd < 1 {
		qd = 1
	}
	for i := 0; i < qd; i++ {
		cl, err := dial(ctx, dialAddr, dialIQN)
		if err != nil {
			return fmt.Errorf("dial %s: %w", dialAddr, err)
		}
		defer cl.Close()
		clients = append(clients, cl)
	}

	info := clients[0].Info()
	blockSize := int(info.BlockSize)
	if blockSize == 0 {
		return fmt.Errorf("backend reported a zero block size")
	}
	if 65536%blockSize != 0 || 4096%blockSize != 0 {
		return fmt.Errorf("block size %d does not divide the 4K/64K benchmark reads", blockSize)
	}
	devBlocks := info.BlockCount
	fmt.Printf("mode            : %s\n", label)
	fmt.Printf("queue depth     : %d connection(s)\n", qd)
	fmt.Printf("backend         : %s (%s)\n", cfg.Backend.Address, cfg.Backend.TargetIQN)
	fmt.Printf("device          : %s %s  %d bytes/block  %d blocks  %.1f GiB\n",
		info.Vendor, info.Product, blockSize, devBlocks, float64(devBlocks)*float64(blockSize)/(1<<30))
	if !o.direct {
		dev := svc.DeviceInfo()
		fmt.Printf("proxy           : listen=%s iqn=%s cap=%d bytes\n", svc.Addr(), cfg.TargetIQN, dev.CapacityBytes())
		fmt.Printf("cache           : L1=%d MiB  block=%d  sector=%d  scan_detection=%v(threshold=%d)\n",
			cfg.Cache.L1.SizeBytes/mib, cfg.Cache.BlockSize, cfg.Cache.SectorSize,
			derefBool(cfg.Cache.ScanDetection.Enabled), cfg.Cache.ScanDetection.Threshold)
	}
	fmt.Println()

	hot := o.hotMB * mib
	if cap := int(devBlocks) * blockSize; hot <= 0 || hot > cap {
		hot = cap / 4
	}
	// The random phase walks every 4K slot of the hot window exactly once, in a
	// fixed shuffled order, so the address list is reproducible and the whole
	// window ends up cached.
	n := hot / 4096
	order := make([]uint64, n)
	for i := range order {
		order[i] = uint64(i) * 4096
	}
	rand.New(rand.NewSource(1)).Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })

	fmt.Printf("hot window      : %d MiB  (%d distinct 4K addresses, working set must fit L1)\n", hot/mib, n)
	fmt.Println()

	var phases []phase

	phases = append(phases, runRandom(ctx, clients, svc, order, 1, blockSize, "random 4K cold (fill)"))
	phases = append(phases, runRandom(ctx, clients, svc, order, o.reps, blockSize, "random 4K warm (replay)"))

	// The control reads a region disjoint from the hot window, twice, so cache
	// hits are impossible on the first pass and the second pass can only be
	// faster if the first one filled the cache.
	seqStart := int64(hot)
	seqLen := int64(o.seqMB) * mib
	if room := int64(devBlocks)*int64(blockSize) - seqStart; seqLen > room {
		seqLen = room
	}
	if seqLen > 0 {
		phases = append(phases, runSeq(ctx, clients, svc, seqStart, seqLen, blockSize, "sequential 64K (control x2)"))
	}

	if o.allowWrite {
		start := (int64(devBlocks) - int64(o.writeMB)*mib/int64(blockSize)) * int64(blockSize)
		if start < 0 {
			start = 0
		}
		fmt.Printf("WARNING: write phase is destructive, it overwrites the last %d MiB of the LUN (from offset %d)\n", o.writeMB, start)
		phases = append(phases, runWrite(ctx, clients, svc, start, int64(o.writeMB)*mib, blockSize, "random 4K write (not cached)"))
	}

	report(phases)
	interpret(phases)

	if serveErr != nil {
		_ = svc.Close()
		_ = <-serveErr
	}
	return nil
}

type phase struct {
	name  string
	ops   int
	bytes int64
	dur   time.Duration
	lat   []time.Duration
	stat  cache.Snapshot // cache counter delta over the phase
}

// readOp is one transfer in a phase. Reads and writes are laid out the same way
// (an LBA, a block count and a byte length), so both phases share the runner.
type readOp struct {
	lba    uint64
	blocks uint32
	size   int
}

// passResult is the work one connection completed.
type passResult struct {
	ops   int
	bytes int64
	lat   []time.Duration
}

// split hands the ops to n connections. Strided splitting gives every
// connection an even mix of the phase's addresses; contiguous splitting gives
// each connection one run of consecutive addresses, which is what a sequential
// control needs for the cache's scan detection to see a stream.
func split(ops []readOp, n int, contiguous bool) [][]readOp {
	out := make([][]readOp, n)
	if contiguous {
		size := (len(ops) + n - 1) / n
		for i := range out {
			lo := min(i*size, len(ops))
			hi := min(lo+size, len(ops))
			out[i] = ops[lo:hi]
		}
		return out
	}
	for i := 0; i < n; i++ {
		for j := i; j < len(ops); j += n {
			out[i] = append(out[i], ops[j])
		}
	}
	return out
}

// parallel spreads ops across the connections and runs them: every connection
// keeps one command outstanding, so N connections give the proxy N commands in
// flight.
func parallel(ctx context.Context, clients []*backend.Initiator, ops []readOp, contiguous bool,
	do func(context.Context, *backend.Initiator, readOp) error) (passResult, error) {
	if len(clients) == 0 {
		return passResult{}, fmt.Errorf("no connections")
	}
	outs := make([]passResult, len(clients))
	chunks := split(ops, len(clients), contiguous)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w, cl := range clients {
		wg.Add(1)
		go func(w int, cl *backend.Initiator, chunk []readOp) {
			defer wg.Done()
			var r passResult
			for _, op := range chunk {
				t0 := time.Now()
				if err := do(ctx, cl, op); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					break
				}
				r.lat = append(r.lat, time.Since(t0))
				r.ops++
				r.bytes += int64(op.size)
			}
			outs[w] = r
		}(w, cl, chunks[w])
	}
	wg.Wait()
	if firstErr != nil {
		return passResult{}, firstErr
	}
	var total passResult
	for _, r := range outs {
		total.ops += r.ops
		total.bytes += r.bytes
		total.lat = append(total.lat, r.lat...)
	}
	return total, nil
}

// runRandom reads every address in order, reps times. The first pass fills the
// cache; the remaining passes should be served from L1.
func runRandom(ctx context.Context, clients []*backend.Initiator, svc *iscsicache.Service, order []uint64, reps, blockSize int, name string) phase {
	blocks := uint32(4096 / blockSize)
	ops := make([]readOp, len(order))
	for i, off := range order {
		ops[i] = readOp{lba: off / uint64(blockSize), blocks: blocks, size: 4096}
	}
	read := func(ctx context.Context, cl *backend.Initiator, op readOp) error {
		_, err := readAt(ctx, cl, op.lba, op.blocks, op.size)
		return err
	}

	before := snapshot(svc)
	p := phase{name: name}
	start := time.Now()
	for r := 0; r < reps; r++ {
		res, err := parallel(ctx, clients, ops, false, read)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", name, err)
			break
		}
		p.ops += res.ops
		p.bytes += res.bytes
		p.lat = append(p.lat, res.lat...)
	}
	p.dur = time.Since(start)
	p.stat = diff(before, snapshot(svc))
	return p
}

// runSeq reads a contiguous region twice in cache block steps. Scan detection
// serves such a stream without filling the cache, so the second pass is as slow
// as the first and the counters show backend reads with no L1 hits.
func runSeq(ctx context.Context, clients []*backend.Initiator, svc *iscsicache.Service, start, length int64, blockSize int, name string) phase {
	step := int64(65536)
	var ops []readOp
	for off := start; off+step <= start+length; off += step {
		ops = append(ops, readOp{
			lba:    uint64(off / int64(blockSize)),
			blocks: uint32(step / int64(blockSize)),
			size:   int(step),
		})
	}
	read := func(ctx context.Context, cl *backend.Initiator, op readOp) error {
		_, err := readAt(ctx, cl, op.lba, op.blocks, op.size)
		return err
	}

	before := snapshot(svc)
	p := phase{name: name}
	start0 := time.Now()
	for pass := 0; pass < 2; pass++ {
		res, err := parallel(ctx, clients, ops, true, read)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", name, err)
			break
		}
		p.ops += res.ops
		p.bytes += res.bytes
		p.lat = append(p.lat, res.lat...)
	}
	p.dur = time.Since(start0)
	p.stat = diff(before, snapshot(svc))
	return p
}

// runWrite issues aligned 4K writes and invalidates the cache like any real
// write. It exists to show the workload the cache cannot accelerate.
func runWrite(ctx context.Context, clients []*backend.Initiator, svc *iscsicache.Service, start int64, length int64, blockSize int, name string) phase {
	var ops []readOp
	for off := start; off+4096 <= start+length; off += 4096 {
		ops = append(ops, readOp{
			lba:    uint64(off / int64(blockSize)),
			blocks: uint32(4096 / blockSize),
			size:   4096,
		})
	}
	write := func(ctx context.Context, cl *backend.Initiator, op readOp) error {
		buf := make([]byte, op.size)
		rand.Read(buf)
		_, err := cl.Exec(ctx, writeCDB(op.lba, op.blocks), buf, 0)
		return err
	}

	before := snapshot(svc)
	p := phase{name: name}
	start0 := time.Now()
	res, err := parallel(ctx, clients, ops, false, write)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", name, err)
	}
	p.ops += res.ops
	p.bytes += res.bytes
	p.lat = append(p.lat, res.lat...)
	p.dur = time.Since(start0)
	p.stat = diff(before, snapshot(svc))
	return p
}

// readAt issues a READ(10) or READ(16) and returns the payload.
func readAt(ctx context.Context, cl *backend.Initiator, lba uint64, blocks uint32, length int) ([]byte, error) {
	cdb := readCDB(lba, blocks)
	res, err := cl.Exec(ctx, cdb, nil, length)
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, res.Error()
	}
	return res.Data, nil
}

func readCDB(lba uint64, blocks uint32) []byte {
	c := make([]byte, 16)
	if lba <= 0xffffffff && blocks <= 0xffff {
		c[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(c[2:6], uint32(lba))
		binary.BigEndian.PutUint16(c[7:9], uint16(blocks))
		return c[:10]
	}
	c[0] = iscsi.SCSIRead16
	binary.BigEndian.PutUint64(c[2:10], lba)
	binary.BigEndian.PutUint32(c[10:14], blocks)
	return c
}

func writeCDB(lba uint64, blocks uint32) []byte {
	c := make([]byte, 16)
	if lba <= 0xffffffff && blocks <= 0xffff {
		c[0] = iscsi.SCSIWrite10
		binary.BigEndian.PutUint32(c[2:6], uint32(lba))
		binary.BigEndian.PutUint16(c[7:9], uint16(blocks))
		return c[:10]
	}
	c[0] = iscsi.SCSIWrite16
	binary.BigEndian.PutUint64(c[2:10], lba)
	binary.BigEndian.PutUint32(c[10:14], blocks)
	return c
}

func report(phases []phase) {
	fmt.Printf("%-30s %7s %10s %9s %9s %9s %9s\n",
		"phase", "ops", "MiB/s", "IOPS", "p50 ms", "p95 ms", "avg ms")
	for _, p := range phases {
		fmt.Printf("%-30s %7d %10.1f %9.0f %9.2f %9.2f %9.2f\n",
			p.name, p.ops, mbps(p), iops(p), pct(p.lat, 0.50), pct(p.lat, 0.95), avgMS(p.lat))
	}

	fmt.Println()
	fmt.Printf("%-30s %8s %8s %8s %8s %8s %8s %8s\n",
		"cache counters (delta)", "reads", "L1hit", "partial", "L2hit", "backend", "fills", "scanbyp")
	for _, p := range phases {
		s := p.stat
		fmt.Printf("%-30s %8d %8d %8d %8d %8d %8d %8d\n",
			p.name, s.Reads, s.L1Hits, s.PartialHits, s.L2Hits, s.BackendReads, s.Fills, s.ScanBypass)
	}
	fmt.Println()
}

func interpret(phases []phase) {
	if len(phases) >= 2 {
		cold, warm := phases[0], phases[1]
		fmt.Printf("random 4K: cold %.0f IOPS -> warm %.0f IOPS (%.1fx)\n",
			iops(cold), iops(warm), safeDiv(iops(warm), iops(cold)))
		if warm.stat.BackendReads == 0 && warm.ops > 0 {
			fmt.Println("warm phase reached the backend 0 times: every read was served from the cache")
		} else {
			fmt.Printf("warm phase still reached the backend %d times (%.1f%% of %d reads): the working set does not fit L1\n",
				warm.stat.BackendReads, 100*float64(warm.stat.BackendReads)/float64(max(warm.ops, 1)), warm.ops)
		}
	}
	for _, p := range phases {
		if !strings.HasPrefix(p.name, "sequential") {
			continue
		}
		if p.stat.ScanBypass > 0 {
			fmt.Printf("%s: %d of %d reads bypassed the cache (scan detection), only %d L1 hits and %d backend reads counted - a sequential stream is served directly, so its second pass is no faster\n",
				p.name, p.stat.ScanBypass, p.ops, p.stat.L1Hits, p.stat.BackendReads)
		} else if p.stat.L1Hits > 0 {
			fmt.Printf("%s: %d L1 hits of %d reads - this stream was cached\n", p.name, p.stat.L1Hits, p.ops)
		} else {
			fmt.Printf("%s: %d L1 hits and %d backend reads of %d - the stream was not cached (the region does not fit L1), so its second pass is no faster\n",
				p.name, p.stat.L1Hits, p.stat.BackendReads, p.ops)
		}
	}
}

// Helpers -------------------------------------------------------------------------

func dial(ctx context.Context, addr, iqn string) (*backend.Initiator, error) {
	return backend.Dial(ctx, backend.Config{
		Address:     addr,
		TargetIQN:   iqn,
		Auth:        "none",
		IOTimeout:   30 * time.Second,
		DialTimeout: 10 * time.Second,
	})
}

func snapshot(svc *iscsicache.Service) cache.Snapshot {
	if svc == nil {
		return cache.Snapshot{}
	}
	return svc.Stats().Cache
}

func diff(before, after cache.Snapshot) cache.Snapshot {
	return cache.Snapshot{
		Reads:         after.Reads - before.Reads,
		L1Hits:        after.L1Hits - before.L1Hits,
		PartialHits:   after.PartialHits - before.PartialHits,
		L2Hits:        after.L2Hits - before.L2Hits,
		BackendReads:  after.BackendReads - before.BackendReads,
		Fills:         after.Fills - before.Fills,
		Evictions:     after.Evictions - before.Evictions,
		Invalidations: after.Invalidations - before.Invalidations,
		ScanBypass:    after.ScanBypass - before.ScanBypass,
	}
}

func logger(verbose bool) *slog.Logger {
	if !verbose {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func mbps(p phase) float64 {
	if p.dur <= 0 {
		return 0
	}
	return float64(p.bytes) / p.dur.Seconds() / mib
}

func iops(p phase) float64 {
	if p.dur <= 0 {
		return 0
	}
	return float64(p.ops) / p.dur.Seconds()
}

func avgMS(lat []time.Duration) float64 {
	if len(lat) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	return float64(sum) / float64(len(lat)) / float64(time.Millisecond)
}

// pct returns the q-th percentile (0..1) of the latencies, in milliseconds.
func pct(lat []time.Duration, q float64) float64 {
	if len(lat) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(lat))
	copy(sorted, lat)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(q * float64(len(sorted)-1))
	return float64(sorted[idx]) / float64(time.Millisecond)
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func derefBool(p *bool) bool { return p != nil && *p }
