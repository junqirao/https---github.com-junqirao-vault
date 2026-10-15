package iscsiproxy_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsiproxy"
)

// TestLiveProxyAgainstRealBackend 是 iscsiproxy（v2 透传代理）对着**真实后端**的
// 端到端实机测试：代理监听本地，一个真正的 iSCSI 发起端连进来，读/写全部经代理转发的
// 后端真实目标。它不需要管理员权限，也不需要 MSiSCSI（纯 iSCSI-over-TCP）。
//
// 这是为定位「代理挂到一半无声消失」而加的：真机故障里 iscsiproxy 的某个 goroutine
// panic 会带走整个代理进程，而 Electron 丢弃子进程 stderr，日志里什么都不剩。本测试
// 压过读写各分支后断言日志里没有 panic，并借此确认修复（goroutine 兜底）有效。
//
// 默认跳过，需显式配置：
//
//	$env:VAULT_ISCSI_TEST_ADDR="172.18.28.200:3260"
//	$env:VAULT_ISCSI_TEST_IQN="iqn.1991-05.com.microsoft:win-0e595h11jss-test-target"
//	go test ./internal/iscsiproxy -run Live -v
func TestLiveProxyAgainstRealBackend(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_ADDR"))
	iqn := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_IQN"))
	if addr == "" || iqn == "" {
		t.Skip("live target not configured (VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p, err := iscsiproxy.NewPortal(iscsiproxy.PortalConfig{Logger: log})
	if err != nil {
		t.Fatalf("NewPortal: %v", err)
	}
	defer p.Close()

	if err := p.Add(ctx, iscsiproxy.TargetConfig{
		Key: "live",
		// v2 是透传代理：代理对本地发起端宣告的就是后端目标的真实 IQN。
		Backend: iscsiproxy.BackendConfig{Address: addr, TargetIQN: iqn},
		Cache: iscsiproxy.CacheConfig{
			BlockSize: 65536, SectorSize: 4096, L1Bytes: 32 << 20,
			FillWorkers: 2, FillQueueDepth: 64,
		},
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	host, port, ok := p.TargetAddr("live")
	if !ok {
		t.Fatal("TargetAddr: 未返回代理地址")
	}
	t.Logf("proxy listening on %s:%d, backend=%s iqn=%s", host, port, addr, iqn)

	// 用一个真正的 iSCSI 发起端连本地代理，全程走 iSCSI 协议（无需管理员/MSiSCSI）。
	it, err := backend.Dial(ctx, backend.Config{
		Address:   net.JoinHostPort(host, strconv.Itoa(port)),
		TargetIQN: iqn,
		IOTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial proxy: %v（代理日志：%s）", err, logs.String())
	}
	defer it.Close()

	info := it.Info()
	t.Logf("client saw block_size=%d block_count=%d", info.BlockSize, info.BlockCount)
	if info.BlockSize == 0 || info.BlockCount == 0 {
		t.Fatalf("发起端没读到盘几何：%+v（代理日志：%s）", info, logs.String())
	}

	read := func(lba uint32, blocks uint16) []byte {
		t.Helper()
		cdb := make([]byte, 10)
		cdb[0] = iscsi.SCSIRead10
		binary.BigEndian.PutUint32(cdb[2:6], lba)
		binary.BigEndian.PutUint16(cdb[7:9], blocks)
		res, err := it.Exec(ctx, cdb, nil, int(blocks)*int(info.BlockSize))
		if err != nil {
			t.Fatalf("read lba=%d: %v（代理日志：%s）", lba, err, logs.String())
		}
		if !res.OK() {
			t.Fatalf("read lba=%d: %v", lba, res.Error())
		}
		if len(res.Data) != int(blocks)*int(info.BlockSize) {
			t.Fatalf("read lba=%d 返回 %d 字节", lba, len(res.Data))
		}
		return res.Data
	}

	// 多次读（首次穿透回填 + 二次命中）+ 一次写，尽量压到数据泵的各个分支。
	first := read(0, 8)
	time.Sleep(300 * time.Millisecond) // 等异步回填
	second := read(0, 8)
	if !bytes.Equal(first, second) {
		t.Fatal("同一区间两次读结果不一致")
	}

	writeCDB := make([]byte, 10)
	writeCDB[0] = iscsi.SCSIWrite10
	binary.BigEndian.PutUint32(writeCDB[2:6], 0)
	binary.BigEndian.PutUint16(writeCDB[7:9], 8)
	payload := make([]byte, 8*int(info.BlockSize))
	for i := range payload {
		payload[i] = 0xA5
	}
	if res, err := it.Exec(ctx, writeCDB, payload, 0); err != nil || !res.OK() {
		t.Fatalf("write: err=%v res=%v（代理日志：%s）", err, res, logs.String())
	}

	if st, ok := p.Stats("live"); ok {
		t.Logf("stats: block_size=%d", st.BlockSize)
	}

	// 核心断言：代理进程没有因为数据泵 panic 而崩溃（这正是本次修复要保证的）。
	if strings.Contains(logs.String(), "panic") {
		t.Fatalf("代理内部发生了 panic（应被兜住并记录）：\n%s", logs.String())
	}
}

// TestLiveWriteThroughput 对比三种链路下的**写**性能，定位写损失来源：
//
//  1. 直连后端（基线）；
//  2. 经代理但不开缓存（NoCache 纯透传）——量出"转发本身"的开销；
//  3. 经代理且开 L1+L2 缓存——在 2 的基础上量出"写失效"的开销。
//
// 负载与 CDM 同形：顺序 1MiB Q8T1、随机 4K Q32T1。每个负载跑固定时长，
// 用吞吐/IOPS 说话。默认跳过，配置同 TestLiveProxyAgainstRealBackend。
func TestLiveWriteThroughput(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_ADDR"))
	iqn := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_IQN"))
	if addr == "" || iqn == "" {
		t.Skip("live target not configured (VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN)")
	}

	const runTime = 4 * time.Second

	type result struct {
		mbps float64
		iops float64
	}
	// runWorkload 在一条已登录的发起端会话上跑一种写负载。
	runWorkload := func(ctx context.Context, it *backend.Initiator, seq bool, ioBlocks uint16, depth int) result {
		info := it.Info()
		bs := int64(info.BlockSize)
		ioBytes := int64(ioBlocks) * bs
		maxLBA := info.BlockCount - uint64(ioBlocks)
		payload := make([]byte, ioBytes)
		for i := range payload {
			payload[i] = byte(i * 7)
		}

		write10 := func(lba uint64) error {
			cdb := make([]byte, 10)
			cdb[0] = iscsi.SCSIWrite10
			binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
			binary.BigEndian.PutUint16(cdb[7:9], ioBlocks)
			res, err := it.Exec(ctx, cdb, payload, 0)
			if err != nil {
				return err
			}
			if !res.OK() {
				return res.Error()
			}
			return nil
		}

		// 预热：把命令窗口、R2T 流水和目标侧的写缓存都拉起来再计时。
		for i := 0; i < depth; i++ {
			if err := write10(uint64(i) * uint64(ioBlocks) % (maxLBA + 1)); err != nil {
				t.Fatalf("预热写失败: %v", err)
			}
		}

		var cursor atomic.Uint64 // 顺序模式的共享 LBA 游标（块）
		var done atomic.Int64
		var firstErr atomic.Value
		stop := time.Now().Add(runTime)
		var wg sync.WaitGroup
		for g := 0; g < depth; g++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(seed)))
				for time.Now().Before(stop) {
					var lba uint64
					if seq {
						lba = cursor.Add(uint64(ioBlocks)) % (maxLBA + 1)
					} else {
						lba = uint64(rng.Int63n(int64(maxLBA) + 1))
					}
					if err := write10(lba); err != nil {
						firstErr.CompareAndSwap(nil, err)
						return
					}
					done.Add(1)
				}
			}(g + 1)
		}
		wg.Wait()
		if v := firstErr.Load(); v != nil {
			t.Fatalf("写负载出错: %v", v)
		}
		n := done.Load()
		secs := runTime.Seconds()
		return result{
			mbps: float64(n) * float64(ioBytes) / secs / (1 << 20),
			iops: float64(n) / secs,
		}
	}

	type link struct {
		name  string
		dial  func(t *testing.T) *backend.Initiator
		close func()
		stats func() (iscsiproxy.Stats, bool)
	}

	dialDirect := func(t *testing.T) *backend.Initiator {
		it, err := backend.Dial(context.Background(), backend.Config{
			Address: addr, TargetIQN: iqn, IOTimeout: 60 * time.Second,
		})
		if err != nil {
			t.Fatalf("直连后端失败: %v", err)
		}
		return it
	}

	// startProxy 起一个本机代理目标并返回经它的发起端会话。
	startProxy := func(t *testing.T, key string, cacheOn bool) link {
		var logs strings.Builder
		log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		p, err := iscsiproxy.NewPortal(iscsiproxy.PortalConfig{Logger: log})
		if err != nil {
			t.Fatalf("NewPortal: %v", err)
		}
		cc := iscsiproxy.CacheConfig{}
		if cacheOn {
			cc = iscsiproxy.CacheConfig{
				L1Bytes: 64 << 20,
				L2: iscsiproxy.L2Config{
					Enabled:   true,
					Path:      t.TempDir() + "\\l2.bin",
					SizeBytes: 256 << 20,
				},
				ScanEnabled: true, ScanThreshold: 32,
			}
		}
		if err := p.Add(context.Background(), iscsiproxy.TargetConfig{
			Key:     key,
			Backend: iscsiproxy.BackendConfig{Address: addr, TargetIQN: iqn},
			Cache:   cc,
			NoCache: !cacheOn,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		host, port, ok := p.TargetAddr(key)
		if !ok {
			t.Fatal("TargetAddr: 未返回代理地址")
		}
		l := link{
			close: func() { _ = p.Close() },
			stats: func() (iscsiproxy.Stats, bool) { return p.Stats(key) },
		}
		l.dial = func(t *testing.T) *backend.Initiator {
			it, err := backend.Dial(context.Background(), backend.Config{
				Address:   net.JoinHostPort(host, strconv.Itoa(port)),
				TargetIQN: iqn,
				IOTimeout: 60 * time.Second,
			})
			if err != nil {
				t.Fatalf("连代理失败（%s）: %v\n代理日志：%s", key, err, logs.String())
			}
			return it
		}
		return l
	}

	// startSplice 起一个**纯 TCP 字节转发**（零 iSCSI 解析）作为对照：它和代理一样
	// 是"发起端→本机→后端"两跳中继，但不拆帧、不拷贝、不观测。用它能把"中继结构本身"
	// 的损失与"代理解析/转发"的损失分开。若它也掉到代理的水平，说明瓶颈是中继天花板
	// （R2T 往返经两跳翻倍），而非代理实现。
	startSplice := func(t *testing.T) link {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("splice listen: %v", err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(ic net.Conn) {
					bc, err := net.DialTimeout("tcp", addr, 10*time.Second)
					if err != nil {
						_ = ic.Close()
						return
					}
					done := make(chan struct{}, 2)
					cp := func(dst, src net.Conn) {
						_, _ = io.Copy(dst, src)
						done <- struct{}{}
					}
					go cp(bc, ic)
					go cp(ic, bc)
					<-done
					_ = ic.Close()
					_ = bc.Close()
				}(c)
			}
		}()
		host, port, _ := net.SplitHostPort(ln.Addr().String())
		l := link{
			close: func() { _ = ln.Close() },
			stats: func() (iscsiproxy.Stats, bool) { return iscsiproxy.Stats{}, false },
		}
		l.dial = func(t *testing.T) *backend.Initiator {
			it, err := backend.Dial(context.Background(), backend.Config{
				Address:   net.JoinHostPort(host, port),
				TargetIQN: iqn,
				IOTimeout: 60 * time.Second,
			})
			if err != nil {
				t.Fatalf("连 splice 失败: %v", err)
			}
			return it
		}
		return l
	}

	type workload struct {
		name     string
		seq      bool
		ioBlocks uint16
		depth    int
		settle   time.Duration // 跑前等磁盘把上一轮的刷盘消化掉
	}
	// 块数按发起端登录时读到的真实块大小折算：1MiB / 4KiB。
	probe := dialDirect(t)
	pbs := int64(probe.Info().BlockSize)
	_ = probe.Close()
	// Q1/Q4/Q8 标度：区分"每命令固定开销"与"有效队列深度被掐断"。
	// 每个负载前的 settle 让磁盘把上一轮的写缓存刷盘消化掉，避免状态漂移污染数据。
	workloads := []workload{
		{name: "顺序 1MiB Q1T1", seq: true, ioBlocks: uint16(1 << 20 / pbs), depth: 1, settle: 6 * time.Second},
		{name: "顺序 1MiB Q4T1", seq: true, ioBlocks: uint16(1 << 20 / pbs), depth: 4, settle: 6 * time.Second},
		{name: "顺序 1MiB Q8T1", seq: true, ioBlocks: uint16(1 << 20 / pbs), depth: 8, settle: 6 * time.Second},
		{name: "随机 4K Q32T1", seq: false, ioBlocks: uint16(4096 / pbs), depth: 32, settle: 12 * time.Second},
	}

	type cell struct{ direct, splice, passthrough, cached result }
	results := make(map[string]cell, len(workloads))

	for _, wb := range workloads {
		if wb.settle > 0 {
			t.Logf("（等待 %s 让磁盘消化上一轮写缓存）", wb.settle)
			time.Sleep(wb.settle)
		}
		t.Logf("=== 负载：%s（块大小 %d B）===", wb.name, pbs)

		it := dialDirect(t)
		rDirect := runWorkload(context.Background(), it, wb.seq, wb.ioBlocks, wb.depth)
		_ = it.Close()
		t.Logf("  直连          : %8.1f MB/s  %8.0f IOPS", rDirect.mbps, rDirect.iops)

		ls := startSplice(t)
		it = ls.dial(t)
		rSplice := runWorkload(context.Background(), it, wb.seq, wb.ioBlocks, wb.depth)
		_ = it.Close()
		ls.close()
		t.Logf("  纯TCP转发     : %8.1f MB/s  %8.0f IOPS", rSplice.mbps, rSplice.iops)

		lp := startProxy(t, "bench-passthrough", false)
		it = lp.dial(t)
		rPass := runWorkload(context.Background(), it, wb.seq, wb.ioBlocks, wb.depth)
		_ = it.Close()
		if st, ok := lp.stats(); ok && st.Proxy.Degraded > 0 {
			t.Fatalf("代理会话降级为纯透传（degraded=%d），基准无效", st.Proxy.Degraded)
		}
		lp.close()
		t.Logf("  代理(纯透传)  : %8.1f MB/s  %8.0f IOPS", rPass.mbps, rPass.iops)

		lc := startProxy(t, "bench-cached", true)
		it = lc.dial(t)
		// 绑定是观测到 LUN 身份后在泵里异步完成的，轮询等待。
		bound := false
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if st, ok := lc.stats(); ok && st.BlockSize != 0 {
				bound = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !bound {
			t.Fatalf("代理缓存未绑定（BlockSize 恒为 0），基准无效")
		}
		rCached := runWorkload(context.Background(), it, wb.seq, wb.ioBlocks, wb.depth)
		_ = it.Close()
		if st, ok := lc.stats(); ok {
			if st.Proxy.Degraded > 0 {
				t.Fatalf("代理会话降级为纯透传（degraded=%d），基准无效", st.Proxy.Degraded)
			}
			t.Logf("  代理统计: writes=%d invalidations=%d local=%d",
				st.Proxy.Writes, st.Proxy.Invalidations, st.Proxy.LocalCmds)
		}
		lc.close()
		t.Logf("  代理(L1+L2)   : %8.1f MB/s  %8.0f IOPS", rCached.mbps, rCached.iops)

		results[wb.name] = cell{direct: rDirect, splice: rSplice, passthrough: rPass, cached: rCached}
	}

	t.Logf("=== 写损失分解（相对直连）===")
	for _, wb := range workloads {
		c := results[wb.name]
		spliceLoss := (1 - c.splice.mbps/c.direct.mbps) * 100
		t.Logf("  %s: 两跳中继本身损失 %.1f%%", wb.name, spliceLoss)
		passLoss := (1 - c.passthrough.mbps/c.direct.mbps) * 100
		cacheLoss := (1 - c.cached.mbps/c.direct.mbps) * 100
		t.Logf("  %s: 转发开销损失 %.1f%%，写失效再损失 %.1f%%（合计 %.1f%%）",
			wb.name, passLoss, cacheLoss-passLoss, cacheLoss)
	}
}

// ---------------------------------------------------------------------------
// CrystalDiskMark 口径的三链路对比：裸连 / 代理(纯透传) / 代理+L1+L2
// ---------------------------------------------------------------------------

// 口径取自 CrystalDiskMark 9 的 Default profile（与那份 CrystalDiskMark_*.txt 一致）：
// SEQ1M Q8T1、SEQ1M Q1T1、RND4K Q32T16、RND4K Q1T1 读写各四项，测试区 512 MiB，
// 每个负载测 3 次（报告取 3 次的平均），负载之间静 5 秒。
//
// 三条链路就是客户端 agent 的三种真实形态（见 internal/agent/cacheproxy.go）：
//
//	裸连      发起端直连后端目标，没有代理，基线
//	代理      走本机代理，但该库没开缓存（NoCache 纯透传）
//	代理+缓存 走本机代理且开了缓存（L1/L2 + 顺序扫描准入，agent 的默认配额）
//
// ⚠️ 写测试是破坏性的：测试区取 LUN **末尾**的 512 MiB（与 vault-iscsi-bench
// -allow-write 写的是同一侧，不碰盘首）。默认不跑，要跑就显式指定：
//
//	$env:VAULT_ISCSI_TEST_ADDR="172.18.28.200:3260"
//	$env:VAULT_ISCSI_TEST_IQN="iqn.1991-05.com.microsoft:win-0e595h11jss-test-target"
//	go test ./internal/iscsiproxy -run TestLiveCDMProfile -v -count=1 -timeout 90m
//
// VAULT_CDM_MB / VAULT_CDM_ITERS 可覆盖测试区与次数（调试时缩小用；日志会写明实际口径），
// VAULT_CDM_L1_MB / VAULT_CDM_L2_MB 覆盖"开缓存那一列"的缓存预算。L1 必须大于测试区，
// 否则缓存列量的是后端而不是缓存（见下面的注释）。
const (
	cdmDefaultMB    = 512
	cdmDefaultIters = 3
	// cdmInterval 对应 CDM 的 "Interval 5 sec"：负载之间静 5 秒，顺带把后端在上一相末尾
	// 吞下的写缓存消化掉，前一个负载的尾流不算进下一个负载。
	cdmInterval = 5 * time.Second
	// cdmL1Bytes/cdmL2Bytes 是"开缓存那一列"两级缓存的预算。⚠️ L1 必须显著大于测试区：
	// CDM 每一轮都把整个测试区重新读一遍，工作集就是测试区本身，L1 只要不大于它，第二轮
	// 仍然要回到后端（或落到 L2），缓存列就永远等于裸连，量的是后端的脸而不是缓存的。
	// 所以默认给 2 GiB（agent 推荐值的上限），而不是 agent 的总预算默认值 512 MiB —— 后者
	// 配上 CDM 默认的 512 MiB 测试区刚好相等，是"缓存列没有意义"的配置。
	// VAULT_CDM_L1_MB / VAULT_CDM_L2_MB 可覆盖；L2 生产默认 4 GiB，默认照样给 4 GiB，
	// 但 L1 一旦装得下工作集，L2 就只是兜底，跑不出差别。
	cdmDefaultL1MB = 2048
	cdmDefaultL2MB = 4096
)

// cdmWorkload 是一个 CDM 负载。depth 是同时在飞的命令数（CDM 标题里的 Q=），threads 是
// CDM 自己的线程数（T=）——两者只决定它内部的划分，在飞的命令条数都是 Q，所以这里按
// depth 起 worker，threads 只用来打印与 CDM 报告一致的标题。
type cdmWorkload struct {
	size    int64
	depth   int
	threads int
	random  bool
	write   bool
}

func (w cdmWorkload) label() string {
	kind, size := "SEQ", "1MiB"
	if w.random {
		kind = "RND"
	}
	if w.size < 1<<20 {
		size = "4KiB"
	}
	return fmt.Sprintf("%s %5s (Q=%3d, T=%2d)", kind, size, w.depth, w.threads)
}

func (w cdmWorkload) phase() string {
	if w.write {
		return "写"
	}
	return "读"
}

// cdmWorkloads 按 CDM 报告的顺序给出八个负载：先四个读，再四个写。
func cdmWorkloads() []cdmWorkload {
	base := []cdmWorkload{
		{size: 1 << 20, depth: 8, threads: 1},
		{size: 1 << 20, depth: 1, threads: 1},
		{size: 4 << 10, depth: 32, threads: 16, random: true},
		{size: 4 << 10, depth: 1, threads: 1, random: true},
	}
	out := make([]cdmWorkload, 0, len(base)*2)
	out = append(out, base...)
	for _, w := range base {
		w.write = true
		out = append(out, w)
	}
	return out
}

// cdmResult 是一个负载一轮的结果。mbps 用 CDM 的十进制口径（1 MB = 1e6 字节），lat 是
// 平均单命令时延——即 CDM 那个 <> 里的数字（总时长 / 命令数）。
type cdmResult struct {
	mbps float64
	iops float64
	lat  time.Duration
}

func cdmAvg(rs []cdmResult) cdmResult {
	if len(rs) == 0 {
		return cdmResult{}
	}
	var mbps, iops float64
	var lat time.Duration
	for _, r := range rs {
		mbps += r.mbps
		iops += r.iops
		lat += r.lat
	}
	n := float64(len(rs))
	return cdmResult{mbps: mbps / n, iops: iops / n, lat: lat / time.Duration(len(rs))}
}

// cdmPass 跑一轮完整的测试区：把 span 按 size 切成连续的（顺序）或打乱的（随机）偏移，
// 用 depth 个 worker 发出去。返回这一轮的耗时与命令数。
func cdmPass(ctx context.Context, it *backend.Initiator, w cdmWorkload, start, span int64, blockSize int, seed int64) (time.Duration, int, error) {
	steps := int(span / w.size)
	offs := make([]int64, steps)
	for i := range offs {
		offs[i] = start + int64(i)*w.size
	}
	if w.random {
		// CDM 的随机测试是"每个块恰好碰一次"的乱序，不是带重复的随机采样：这样一轮走过
		// 的字节数正好等于测试区，各负载之间才可比。
		rand.New(rand.NewSource(seed)).Shuffle(steps, func(i, j int) { offs[i], offs[j] = offs[j], offs[i] })
	}

	depth := w.depth
	if depth < 1 {
		depth = 1
	}
	shards := make([][]int64, depth)
	for i, off := range offs {
		shards[i%depth] = append(shards[i%depth], off)
	}

	blocks := uint32(w.size / int64(blockSize))
	var (
		mu       sync.Mutex
		firstErr error
	)
	record := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	t0 := time.Now()
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		wg.Add(1)
		go func(shard []int64) {
			defer wg.Done()
			buf := make([]byte, w.size)
			cdb := make([]byte, 10)
			if w.write {
				cdb[0] = iscsi.SCSIWrite10
				for i := range buf {
					buf[i] = byte(i*31 + 7)
				}
			} else {
				cdb[0] = iscsi.SCSIRead10
			}
			for _, off := range shard {
				binary.BigEndian.PutUint32(cdb[2:6], uint32(off/int64(blockSize)))
				binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
				var (
					res *backend.Result
					err error
				)
				if w.write {
					res, err = it.Exec(ctx, cdb, buf, 0)
				} else {
					res, err = it.ExecInto(ctx, cdb, nil, buf)
				}
				if err != nil {
					record(err)
					return
				}
				if !res.OK() {
					record(res.Error())
					return
				}
				if !w.write && len(res.Data) != len(buf) {
					record(fmt.Errorf("短读：%d 字节，期望 %d", len(res.Data), len(buf)))
					return
				}
			}
		}(shard)
	}
	wg.Wait()
	dur := time.Since(t0)
	mu.Lock()
	err := firstErr
	mu.Unlock()
	if err != nil {
		return dur, 0, err
	}
	return dur, steps, nil
}

// cdmMeasure 按 CDM 的口径把一个负载重复 iters 轮，并把每轮的数字打进日志。
func cdmMeasure(ctx context.Context, t *testing.T, it *backend.Initiator, w cdmWorkload, start, span int64, blockSize, iters int) ([]cdmResult, error) {
	out := make([]cdmResult, 0, iters)
	for k := 0; k < iters; k++ {
		time.Sleep(cdmInterval)
		dur, ops, err := cdmPass(ctx, it, w, start, span, blockSize, int64(k)+1)
		if err != nil {
			return out, fmt.Errorf("第 %d 轮: %w", k+1, err)
		}
		r := cdmResult{
			mbps: float64(ops) * float64(w.size) / dur.Seconds() / 1e6,
			iops: float64(ops) / dur.Seconds(),
			lat:  dur / time.Duration(ops),
		}
		out = append(out, r)
		t.Logf("    %s %s 第%d轮: %9.3f MB/s [%9.1f IOPS] < %8.2f us>",
			w.phase(), w.label(), k+1, r.mbps, r.iops, float64(r.lat.Nanoseconds())/1e3)
	}
	return out, nil
}

// cdmLink 是一条被对比的链路：一个名字、一个发起端会话的工厂、清理与统计。
type cdmLink struct {
	name  string
	dial  func(t *testing.T) *backend.Initiator
	close func()
	stats func() (iscsiproxy.Stats, bool)
	// bindWait 表示这条链路的缓存是异步绑定的，跑负载之前要等它绑上。
	bindWait bool
}

func cdmEnvInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func TestLiveCDMProfile(t *testing.T) {
	addr := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_ADDR"))
	iqn := strings.TrimSpace(os.Getenv("VAULT_ISCSI_TEST_IQN"))
	if addr == "" || iqn == "" {
		t.Skip("live target not configured (VAULT_ISCSI_TEST_ADDR / VAULT_ISCSI_TEST_IQN)")
	}
	spanMB := cdmEnvInt("VAULT_CDM_MB", cdmDefaultMB)
	iters := cdmEnvInt("VAULT_CDM_ITERS", cdmDefaultIters)
	l1Bytes := int64(cdmEnvInt("VAULT_CDM_L1_MB", cdmDefaultL1MB)) << 20
	l2Bytes := int64(cdmEnvInt("VAULT_CDM_L2_MB", cdmDefaultL2MB)) << 20

	// 4K 随机写在这块盘上只有一千多 IOPS，八个负载 × 三轮 × 三条链路要跑二三十分钟，
	// 超时给足（-timeout 也要跟上，Go 默认的 10 分钟会把它掐了）。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	probe, err := backend.Dial(ctx, backend.Config{Address: addr, TargetIQN: iqn, IOTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("直连后端失败: %v", err)
	}
	info := probe.Info()
	_ = probe.Close()

	blockSize := int(info.BlockSize)
	devBytes := int64(info.BlockCount) * int64(blockSize)
	span := int64(spanMB) << 20
	if span > devBytes/2 {
		span = devBytes / 2
	}
	span -= span % (1 << 20) // 1MiB 的负载要能整除整块测试区
	start := devBytes - span

	t.Logf("目标: %s %s（%s %s，%d 字节/块，%.2f GiB）",
		addr, iqn, info.Vendor, info.Product, blockSize, float64(devBytes)/(1<<30))
	t.Logf("口径: CrystalDiskMark 9 Default profile（SEQ1M Q8T1/Q1T1、RND4K Q32T16/Q1T1，读+写）")
	t.Logf("测试区: LUN 末尾 %d MiB（偏移 %d 起）× %d 轮，负载之间静 %s", span>>20, start, iters, cdmInterval)
	t.Logf("开缓存那一列: L1=%d MiB、L2=%d MiB、顺序扫描准入开", l1Bytes>>20, l2Bytes>>20)
	if l1Bytes <= span {
		t.Logf("⚠️ L1(%d MiB) 不大于测试区(%d MiB)：缓存列的工作集装不进 L1，这一列会接近裸连，"+
			"量的是后端而不是缓存。要看缓存的增益请把 VAULT_CDM_L1_MB 抬到测试区的 2 倍以上，"+
			"或把 VAULT_CDM_MB 缩到 L1 之内。", l1Bytes>>20, spanMB)
	}
	t.Logf("⚠️ 写相会覆盖测试区所在的 %d MiB", span>>20)

	dialBackend := func(t *testing.T, address string) *backend.Initiator {
		it, err := backend.Dial(context.Background(), backend.Config{
			Address: address, TargetIQN: iqn, IOTimeout: 60 * time.Second,
		})
		if err != nil {
			t.Fatalf("直连后端失败（%s）: %v", address, err)
		}
		return it
	}

	// startProxyLink 起一个代理链路：cacheOn=false 时该库用 NoCache 纯透传，也就是
	// 客户端 agent 里"用户没给这个卷开缓存"的形态。
	startProxyLink := func(name, key string, cacheOn bool) cdmLink {
		var logs strings.Builder
		log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		p, err := iscsiproxy.NewPortal(iscsiproxy.PortalConfig{Logger: log})
		if err != nil {
			t.Fatalf("NewPortal: %v", err)
		}
		cc := iscsiproxy.CacheConfig{}
		if cacheOn {
			cc = iscsiproxy.CacheConfig{
				L1Bytes: l1Bytes,
				L2: iscsiproxy.L2Config{
					Enabled:   true,
					Path:      t.TempDir() + "\\l2.bin",
					SizeBytes: l2Bytes,
				},
				ScanEnabled: true, ScanThreshold: 32,
			}
		}
		if err := p.Add(context.Background(), iscsiproxy.TargetConfig{
			Key:     key,
			Backend: iscsiproxy.BackendConfig{Address: addr, TargetIQN: iqn},
			Cache:   cc,
			NoCache: !cacheOn,
		}); err != nil {
			t.Fatalf("Add(%s): %v", key, err)
		}
		host, port, ok := p.TargetAddr(key)
		if !ok {
			t.Fatal("TargetAddr: 未返回代理地址")
		}
		l := cdmLink{name: name, bindWait: cacheOn}
		l.close = func() { _ = p.Close() }
		l.stats = func() (iscsiproxy.Stats, bool) { return p.Stats(key) }
		l.dial = func(t *testing.T) *backend.Initiator {
			it, err := backend.Dial(context.Background(), backend.Config{
				Address:   net.JoinHostPort(host, strconv.Itoa(port)),
				TargetIQN: iqn,
				IOTimeout: 60 * time.Second,
			})
			if err != nil {
				t.Fatalf("连代理失败（%s）: %v\n代理日志：%s", key, err, logs.String())
			}
			return it
		}
		return l
	}

	links := []cdmLink{
		{name: "裸连（直连后端）"},
		startProxyLink("代理（纯透传）", "cdm-nocache", false),
		startProxyLink("代理+缓存（L1+L2）", "cdm-cached", true),
	}
	links[0].dial = func(t *testing.T) *backend.Initiator { return dialBackend(t, addr) }
	links[0].close = func() {}
	links[0].stats = func() (iscsiproxy.Stats, bool) { return iscsiproxy.Stats{}, false }
	workloads := cdmWorkloads()
	results := make([][]cdmResult, len(links))
	snaps := make([]iscsiproxy.Stats, len(links))
	haveSnap := make([]bool, len(links))

	for li := range links {
		t.Logf("=== 链路 %d/%d：%s ===", li+1, len(links), links[li].name)
		it := links[li].dial(t)
		if links[li].bindWait {
			// 缓存是在泵里观测到 LUN 身份之后异步绑定的，等它绑上再测。
			bound := false
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				if st, ok := links[li].stats(); ok && st.BlockSize != 0 {
					bound = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !bound {
				_ = it.Close()
				links[li].close()
				t.Fatalf("链路 %s 的缓存没绑上（BlockSize 恒为 0），这一列的数没有意义", links[li].name)
			}
		}
		results[li] = make([]cdmResult, len(workloads))
		for wi, w := range workloads {
			rs, err := cdmMeasure(ctx, t, it, w, start, span, blockSize, iters)
			if err != nil {
				_ = it.Close()
				links[li].close()
				t.Fatalf("链路 %s 的%s %s: %v", links[li].name, w.phase(), w.label(), err)
			}
			results[li][wi] = cdmAvg(rs)
		}
		_ = it.Close()
		if st, ok := links[li].stats(); ok {
			snaps[li], haveSnap[li] = st, true
		}
		links[li].close()
	}

	// 按 CDM 报告的样式，一条链路一块。
	for li := range links {
		t.Logf("[%s]", links[li].name)
		for _, phase := range []string{"读", "写"} {
			t.Logf("  [%s]", map[string]string{"读": "Read", "写": "Write"}[phase])
			for wi, w := range workloads {
				if w.phase() != phase {
					continue
				}
				r := results[li][wi]
				t.Logf("    %s: %9.3f MB/s [%9.1f IOPS] < %8.2f us>",
					w.label(), r.mbps, r.iops, float64(r.lat.Nanoseconds())/1e3)
			}
		}
		if haveSnap[li] {
			st := snaps[li]
			t.Logf("    统计: 观测读=%d 写=%d 缓存代答=%d 透传=%d 降级=%d | 后端读=%d L1命中=%d L2命中=%d 填充=%d 失效=%d 扫描跳过=%d",
				st.Proxy.Reads, st.Proxy.Writes, st.Proxy.LocalCmds, st.Proxy.Passthrough, st.Proxy.Degraded,
				st.Cache.BackendReads, st.Cache.L1Hits, st.Cache.L2Hits,
				st.Cache.Fills, st.Cache.Invalidations, st.Cache.ScanBypass)
		}
	}

	if len(links) != 3 {
		t.Fatalf("对比表按三条链路排版，实际 %d 条", len(links))
	}
	t.Logf("=== 汇总（MB/s，%d 轮平均；CDM 口径 1MB=1e6 字节）===", iters)
	t.Logf("%-24s %11s %11s %13s %10s", "负载", "裸连", "代理", "代理+缓存", "缓存/裸连")
	for _, phase := range []string{"读", "写"} {
		for wi, w := range workloads {
			if w.phase() != phase {
				continue
			}
			d, p, c := results[0][wi].mbps, results[1][wi].mbps, results[2][wi].mbps
			t.Logf("%-24s %11.3f %11.3f %13.3f %9.2fx", w.label(), d, p, c, c/d)
		}
	}

	// 代理链路一旦降级成纯字节转发，缓存就根本没参与，这一列的数没有意义。
	for li := 1; li < len(links); li++ {
		if haveSnap[li] && snaps[li].Proxy.Degraded > 0 {
			t.Fatalf("链路 %s 的会话降级为纯透传（degraded=%d），对比无效", links[li].name, snaps[li].Proxy.Degraded)
		}
	}
	// 松口径兜底：任何一列都不该掉到直连的 20% 以下。真掉下去说明链路坏了，而不是
	// "慢"——每条链路的损失看上面的汇总。
	for wi, w := range workloads {
		d := results[0][wi].mbps
		if d <= 0 {
			t.Fatalf("%s %s 的直连基线是 0，设备可能没在干活", w.phase(), w.label())
		}
		for li := 1; li < len(links); li++ {
			if r := results[li][wi].mbps; r < d*0.2 {
				t.Errorf("%s 的%s %s 掉到直连的 %.1f%%（%.3f vs %.3f MB/s），链路可能坏了",
					links[li].name, w.phase(), w.label(), r/d*100, r, d)
			}
		}
	}
}
