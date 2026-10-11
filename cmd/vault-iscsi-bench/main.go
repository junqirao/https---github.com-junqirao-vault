// Command vault-iscsi-bench measures the read cache of the iSCSI cache proxy.
//
// AS SSD Benchmark cannot show whether this cache helps: it is write heavy and
// its reads run once over a working set far larger than L1, so nothing is ever
// re-read. The cache is a writearound read cache, so its effect only appears
// when the same addresses are read twice: the first read misses and goes to the
// backend, the second is served from L1 memory. A sequential stream is a
// separate case: at the default 64K step scan detection serves it without
// filling, so its second pass is no faster, while a step of -seq-step 1m is
// large enough to be cached.
//
// This command therefore runs these phases and reports the cache counters
// around each one:
//
//	random 4K cold   every 4K slot of the hot window, read once, in random order
//	random 4K warm   the same addresses again, repeated - this is the cached path
//	sequential cold  a contiguous region read in -seq-step units, first pass
//	sequential warm  the same region again - cached unless a scan is bypassed
//	write 4K         optional (-allow-write), the path the cache does not help
//
// The sequential step decides which way the control goes: at the default 64K a
// stream trips scan detection and is served without filling, while at -seq-step
// 1m it is filled and the warm pass comes out of L1.
//
// It runs against an in-process proxy, so the cache counters are visible, and
// -direct runs the same phases against the backing target for a baseline.
//
// Usage:
//
//	go run ./cmd/vault-iscsi-bench -config cmd/vault-iscsi-cache/iscsi-cache.example.yaml
//	go run ./cmd/vault-iscsi-bench -config ... -direct
//	go run ./cmd/vault-iscsi-bench -config ... -qd 8 -seq-step 1m -seq-mb 128 -seq-write
//
// -qd keeps that many commands outstanding, spread over -conns connections
// (default 1, the shape a real Windows initiator presents), and the benchmark
// initiator announces -client-mrdsl sized Data-In segments (default 64k, what
// the Microsoft initiator announces), so the numbers line up with a
// CrystalDiskMark run on the mounted disk.
//
// The benchmark is read only unless -allow-write or -seq-write is given.
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
	"strconv"
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
	configPath  string
	direct      bool
	qd          int
	conns       int
	clientMRDSL string
	hotMB       int
	seqMB       int
	seqStep     string
	reps        int
	allowWrite  bool
	seqWrite    bool
	writeMB     int
	verbose     bool
}

func run() error {
	var o options
	flag.StringVar(&o.configPath, "config", "iscsi-cache.yaml", "path to the proxy YAML configuration")
	flag.BoolVar(&o.direct, "direct", false, "benchmark the backing target directly, without the proxy (baseline)")
	flag.IntVar(&o.qd, "qd", 1, "total queue depth: how many commands are outstanding across the client connections")
	flag.IntVar(&o.conns, "conns", 1, "how many client connections share the queue depth (1 matches a real Windows initiator)")
	flag.StringVar(&o.clientMRDSL, "client-mrdsl", "64k", "MaxRecvDataSegmentLength the benchmark initiator announces (64k matches the Windows initiator)")
	flag.IntVar(&o.hotMB, "hot-mb", 64, "size of the random read working set, in MiB (keep it below the L1 size)")
	flag.IntVar(&o.seqMB, "seq-mb", 256, "size of the sequential read control phase, in MiB")
	flag.StringVar(&o.seqStep, "seq-step", "64k", "sequential read size, e.g. 64k or 1m")
	flag.IntVar(&o.reps, "reps", 4, "how many times the warm phase replays the hot address list")
	flag.BoolVar(&o.allowWrite, "allow-write", false, "add a destructive 4K write phase")
	flag.BoolVar(&o.seqWrite, "seq-write", false, "add a destructive sequential write phase over the sequential read region")
	flag.IntVar(&o.writeMB, "write-mb", 64, "size of the write phase, written to the end of the LUN (destructive)")
	flag.BoolVar(&o.verbose, "verbose", false, "log proxy internals")
	flag.Parse()

	seqStep, err := parseSize(o.seqStep)
	if err != nil {
		return err
	}
	clientMRDSL, err := parseSize(o.clientMRDSL)
	if err != nil {
		return err
	}
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

	// The client shape is what a benchmark sees through the queue depth: -qd
	// commands outstanding, spread over -conns connections. A real Windows
	// initiator runs the whole depth on one connection, so the default is one.
	qd := o.qd
	if qd < 1 {
		qd = 1
	}
	conns := o.conns
	if conns < 1 {
		conns = 1
	}
	if conns > qd {
		conns = qd
	}
	for i := 0; i < conns; i++ {
		cl, err := dial(ctx, dialAddr, dialIQN, int(clientMRDSL))
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
	if seqStep <= 0 || seqStep%int64(blockSize) != 0 {
		return fmt.Errorf("seq-step %d is not a positive multiple of the %d byte block size", seqStep, blockSize)
	}
	devBlocks := info.BlockCount
	fmt.Printf("mode            : %s\n", label)
	fmt.Printf("queue depth     : %d over %d connection(s), client segments %s\n", qd, conns, o.clientMRDSL)
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

	phases = append(phases, runRandom(ctx, clients, qd, svc, order, 1, blockSize, "random 4K cold (fill)"))
	phases = append(phases, runRandom(ctx, clients, qd, svc, order, o.reps, blockSize, "random 4K warm (replay)"))

	// The control reads a region disjoint from the hot window, twice, so cache
	// hits are impossible on the first pass and the second pass can only be
	// faster if the first one filled the cache.
	seqStart := int64(hot)
	seqLen := int64(o.seqMB) * mib
	if room := int64(devBlocks)*int64(blockSize) - seqStart; seqLen > room {
		seqLen = room
	}
	if seqLen > 0 {
		phases = append(phases, runSeq(ctx, clients, qd, svc, seqStart, seqLen, seqStep, blockSize,
			"sequential "+stepLabel(seqStep))...)
	}

	if o.seqWrite && seqLen > 0 {
		fmt.Printf("WARNING: sequential write phase is destructive, it overwrites the %d MiB control region (from offset %d)\n",
			int(seqLen/mib), seqStart)
		phases = append(phases, runSeqWrite(ctx, clients, qd, svc, seqStart, seqLen, seqStep, blockSize,
			"seq write "+stepLabel(seqStep)+" (cold)"))
	}

	if o.allowWrite {
		start := (int64(devBlocks) - int64(o.writeMB)*mib/int64(blockSize)) * int64(blockSize)
		if start < 0 {
			start = 0
		}
		fmt.Printf("WARNING: write phase is destructive, it overwrites the last %d MiB of the LUN (from offset %d)\n", o.writeMB, start)
		phases = append(phases, runWrite(ctx, clients, qd, svc, start, int64(o.writeMB)*mib, blockSize, "random 4K write (not cached)"))
	}

	report(phases)
	interpret(phases, svc != nil)

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

// parallel spreads ops across depth workers and runs them: worker j is pinned
// to connection j modulo the number of connections, so the whole depth is in
// flight while the sockets are shared the way a real initiator shares them.
func parallel(ctx context.Context, clients []*backend.Initiator, depth int, ops []readOp, contiguous bool,
	do func(context.Context, *backend.Initiator, readOp) error) (passResult, error) {
	if len(clients) == 0 {
		return passResult{}, fmt.Errorf("no connections")
	}
	if depth < len(clients) {
		depth = len(clients)
	}
	outs := make([]passResult, depth)
	chunks := split(ops, depth, contiguous)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for j := 0; j < depth; j++ {
		if len(chunks[j]) == 0 {
			continue
		}
		wg.Add(1)
		go func(j int, cl *backend.Initiator, chunk []readOp) {
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
			outs[j] = r
		}(j, clients[j%len(clients)], chunks[j])
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
func runRandom(ctx context.Context, clients []*backend.Initiator, depth int, svc *iscsicache.Service, order []uint64, reps, blockSize int, name string) phase {
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
		res, err := parallel(ctx, clients, depth, ops, false, read)
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

// runSeq reads a contiguous region twice in step sized commands and reports the
// two passes separately, so the second one shows whether the first filled the
// cache. A 64K step trips scan detection and is served without filling; a step
// of 1m is large enough for the proxy to stream and fill.
func runSeq(ctx context.Context, clients []*backend.Initiator, depth int, svc *iscsicache.Service, start, length, step int64, blockSize int, name string) []phase {
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

	var out []phase
	for _, pass := range []string{"cold", "warm"} {
		before := snapshot(svc)
		p := phase{name: name + " " + pass}
		start0 := time.Now()
		res, err := parallel(ctx, clients, depth, ops, true, read)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", p.name, err)
			break
		}
		p.ops += res.ops
		p.bytes += res.bytes
		p.lat = append(p.lat, res.lat...)
		p.dur = time.Since(start0)
		p.stat = diff(before, snapshot(svc))
		out = append(out, p)
	}
	return out
}

// runSeqWrite overwrites the sequential control region once, in step sized
// commands, and reports the write path the proxy cannot accelerate. Contiguous
// splitting keeps each worker on its own run of consecutive blocks.
func runSeqWrite(ctx context.Context, clients []*backend.Initiator, depth int, svc *iscsicache.Service, start, length, step int64, blockSize int, name string) phase {
	var ops []readOp
	for off := start; off+step <= start+length; off += step {
		ops = append(ops, readOp{
			lba:    uint64(off / int64(blockSize)),
			blocks: uint32(step / int64(blockSize)),
			size:   int(step),
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
	res, err := parallel(ctx, clients, depth, ops, true, write)
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

// stepLabel renders a read size the way the report names read sizes.
func stepLabel(step int64) string {
	if step%mib == 0 {
		return fmt.Sprintf("%dM", step/mib)
	}
	return fmt.Sprintf("%dK", step/1024)
}

// parseSize parses a byte size written as "64k", "1m", "1g" or a plain number.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "k"):
		mult, t = 1024, strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		mult, t = mib, strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "g"):
		mult, t = 1<<30, strings.TrimSuffix(t, "g")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", s, err)
	}
	return n * mult, nil
}

// runWrite issues aligned 4K writes and invalidates the cache like any real
// write. It exists to show the workload the cache cannot accelerate.
func runWrite(ctx context.Context, clients []*backend.Initiator, depth int, svc *iscsicache.Service, start int64, length int64, blockSize int, name string) phase {
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
	res, err := parallel(ctx, clients, depth, ops, false, write)
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

// interpret turns the phase table into a verdict. proxied reports whether the
// phases ran through the cache: in -direct mode there is no cache, so every
// counter is zero and the cache verdicts below would be meaningless.
func interpret(phases []phase, proxied bool) {
	if !proxied {
		fmt.Println("direct mode: no cache counters to interpret; this run is the baseline")
		return
	}
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
	for i := 0; i+1 < len(phases); i++ {
		cold, warm := phases[i], phases[i+1]
		if !strings.HasSuffix(cold.name, " cold") || !strings.HasPrefix(cold.name, "sequential ") {
			continue
		}
		name := strings.TrimSuffix(cold.name, " cold")
		fmt.Printf("%s: cold %.1f MiB/s -> warm %.1f MiB/s (%.1fx); warm counters: %d L1 hits, %d partial, %d backend reads, %d fills, %d scan bypass\n",
			name, mbps(cold), mbps(warm), safeDiv(mbps(warm), mbps(cold)),
			warm.stat.L1Hits, warm.stat.PartialHits, warm.stat.BackendReads, warm.stat.Fills, warm.stat.ScanBypass)
		switch {
		case warm.stat.ScanBypass > 0:
			fmt.Println("  served directly: scan detection treated the stream as sequential, so nothing was filled")
		case warm.stat.BackendReads == 0 && warm.ops > 0:
			fmt.Println("  every warm read was served from L1")
		case warm.stat.L1Hits > 0:
			fmt.Printf("  partly cached: %.1f%% of the warm reads still reached the backend (the region does not fit L1)\n",
				100*float64(warm.stat.BackendReads)/float64(max(warm.ops, 1)))
		default:
			fmt.Println("  not cached: the region is larger than L1")
		}
	}
}

// Helpers -------------------------------------------------------------------------

func dial(ctx context.Context, addr, iqn string, mrdsl int) (*backend.Initiator, error) {
	return backend.Dial(ctx, backend.Config{
		Address:     addr,
		TargetIQN:   iqn,
		Auth:        "none",
		IOTimeout:   30 * time.Second,
		DialTimeout: 10 * time.Second,
		// The segment size the benchmark initiator announces decides how big
		// the Data-In PDUs it receives are, on both the direct and the proxy
		// leg. 64k matches the Microsoft software initiator, so the numbers
		// line up with a CrystalDiskMark run on the mounted disk.
		MaxRecvDataSegmentLength: mrdsl,
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
