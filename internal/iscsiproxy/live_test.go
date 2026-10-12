package iscsiproxy_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
