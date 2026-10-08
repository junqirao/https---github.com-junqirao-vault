package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/cache"
	"vault/internal/iscsicache/iscsi"
)

// benchDev is the in-memory LUN the read-overhead benchmarks run against. Each
// command costs a fixed service time plus the time the payload would take to
// cross the link, so the numbers stand in for talking to a real backing target:
// a fast command that carries a large payload is cheap, and a command that
// carries a small one is dominated by the round trip.
type benchDev struct {
	data []byte
	bs   int64
	lat  time.Duration
	bw   int64 // bytes per second
	cmds atomic.Int64
}

func newBenchDev(sizeMB int, bs int64, lat time.Duration) *benchDev {
	return &benchDev{data: make([]byte, int64(sizeMB)<<20), bs: bs, lat: lat, bw: 1200 << 20}
}

func (d *benchDev) Info() backend.DeviceInfo {
	return backend.DeviceInfo{
		Vendor: "BENCH", Product: "Bench Disk", Revision: "0001",
		Serial: "BENCHDEV", WWID: "naa.bench",
		BlockSize: uint32(d.bs), BlockCount: uint64(len(d.data)) / uint64(d.bs),
		TargetIQN: "iqn.bench:dev",
	}
}

func (d *benchDev) Close() error { return nil }

func (d *benchDev) Exec(ctx context.Context, cdb []byte, dataOut []byte, inLen int) (*backend.Result, error) {
	d.cmds.Add(1)
	if len(cdb) == 0 {
		return &backend.Result{Status: iscsi.StatusCheckCondition}, nil
	}
	if cdb[0] != iscsi.SCSIRead10 && cdb[0] != iscsi.SCSIRead16 {
		if d.lat > 0 {
			time.Sleep(d.lat)
		}
		return &backend.Result{Status: iscsi.StatusGood}, nil
	}
	p, err := iscsi.ParseReadWrite(cdb)
	if err != nil {
		return nil, err
	}
	off := int64(p.LBA) * d.bs
	n := int64(p.Blocks) * d.bs
	if off < 0 || off+n > int64(len(d.data)) {
		return &backend.Result{Status: iscsi.StatusCheckCondition,
			Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
	}
	if d.lat > 0 {
		time.Sleep(d.lat)
	}
	if d.bw > 0 {
		time.Sleep(time.Duration(n) * time.Second / time.Duration(d.bw))
	}
	out := make([]byte, n)
	copy(out, d.data[off:off+n])
	return &backend.Result{Status: iscsi.StatusGood, Data: out}, nil
}

// benchReadCDB builds the READ the benchmark issues.
func benchReadCDB(lba uint64, blocks uint32) []byte {
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

const (
	benchSector = 4096
	// benchLatency is the backing round trip plus the target's service time.
	benchLatency = 250 * time.Microsecond
	// benchL1 bounds the cache so the streaming benchmark runs against a cache
	// far smaller than the device and therefore keeps missing.
	benchL1 = 64 << 20
	// benchDeviceMB is comfortably larger than the L1 budget, so a stream
	// wrapped around the device does not turn into a hit run.
	benchDeviceMB = 512
)

func newBenchProxy(tb testing.TB, dev *benchDev, scan bool) *Proxy {
	tb.Helper()
	c, err := cache.New(cache.Config{
		BlockSize:   cache.DefaultBlockSize,
		SectorSize:  benchSector,
		L1Bytes:     benchL1,
		ScanEnabled: scan,
	})
	if err != nil {
		tb.Fatalf("cache.New: %v", err)
	}
	p, err := New(Config{Backend: dev, Cache: c, TargetIQN: "iqn.bench:proxy"})
	if err != nil {
		tb.Fatalf("proxy.New: %v", err)
	}
	return p
}

func sizeName(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// BenchmarkReadOverhead compares one client read against the backing target
// with the same read through the proxy, for the sizes a benchmark tool uses.
//
// "direct" is the reference: the client's read is one command to the storage.
// "proxy-stream" reads a stream far larger than the cache, so the proxy is
// expected to miss and pay for a backing read; "proxy-hot" re-reads one range
// that fits the cache, so it measures the overhead of serving a hit. The
// backend-cmds/read metric is the point of the benchmark: a proxy that fetches
// one cache block per command turns a streamed read into one backing round trip
// per 64 KiB.
func BenchmarkReadOverhead(b *testing.B) {
	for _, size := range []int64{4 << 10, 128 << 10, 1 << 20} {
		blocks := uint32(size / 512)
		b.Run("direct/"+sizeName(size), func(b *testing.B) {
			dev := newBenchDev(benchDeviceMB, 512, benchLatency)
			benchDirect(b, dev, benchReadCDB(0, blocks), size)
		})
		b.Run("proxy-stream/"+sizeName(size), func(b *testing.B) {
			dev := newBenchDev(benchDeviceMB, 512, benchLatency)
			benchProxyRead(b, newBenchProxy(b, dev, true), dev, blocks, size, true)
		})
		b.Run("proxy-hot/"+sizeName(size), func(b *testing.B) {
			dev := newBenchDev(benchDeviceMB, 512, benchLatency)
			benchProxyRead(b, newBenchProxy(b, dev, true), dev, blocks, size, false)
		})
	}
}

func benchDirect(b *testing.B, dev *benchDev, cdb []byte, size int64) {
	b.SetBytes(size)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := dev.Exec(ctx, cdb, nil, int(size))
		if err != nil || len(res.Data) != int(size) {
			b.Fatalf("read: err=%v len=%d", err, len(res.Data))
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(dev.cmds.Load())/float64(b.N), "backend-cmds/read")
}

// benchProxyRead runs b.N reads through the proxy. stream walks the device so
// each read starts where the previous one ended, which is what a sequential
// benchmark does; otherwise every read repeats one cached range.
func benchProxyRead(b *testing.B, p *Proxy, dev *benchDev, blocks uint32, size int64, stream bool) {
	b.SetBytes(size)
	ctx := context.Background()
	devBlocks := uint64(dev.Info().BlockCount)
	step := uint64(blocks)
	start := uint64(0)
	var cdb []byte
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if stream {
			if start+step > devBlocks {
				start = 0
			}
			cdb = benchReadCDB(start, blocks)
			start += step
		} else {
			cdb = benchReadCDB(0, blocks)
		}
		res, err := p.Execute(ctx, cdb, nil, int(size))
		if err != nil {
			b.Fatalf("execute: %v", err)
		}
		if len(res.Data) != int(size) {
			b.Fatalf("read returned %d of %d bytes", len(res.Data), size)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(dev.cmds.Load())/float64(b.N), "backend-cmds/read")
}

// TestStreamedReadCostsOneBackendCommand pins the defect the merging in the
// cache fixes: a streamed read through the proxy must not charge the backing
// target one command per cache block, because each of those commands carries a
// full round trip and caps throughput at the round trip time.
func TestStreamedReadCostsOneBackendCommand(t *testing.T) {
	const size = 1 << 20
	dev := newBenchDev(benchDeviceMB, 512, 0)
	p := newBenchProxy(t, dev, true)
	before := dev.cmds.Load()

	res, err := p.Execute(context.Background(), benchReadCDB(0, uint32(size/512)), nil, size)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Data) != size {
		t.Fatalf("read returned %d of %d bytes", len(res.Data), size)
	}
	if got := dev.cmds.Load() - before; got != 1 {
		t.Fatalf("a %s cold read cost %d backend commands, want 1", sizeName(size), got)
	}
}
