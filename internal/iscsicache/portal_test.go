package iscsicache_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"vault/internal/iscsicache"
	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsicache/proxy"
)

// 门户（Portal）是客户端缓存的集成接缝：**一个客户端一个门户，一个门户下挂多个目标**。
// 这些用例用"自己起一个服务端目标 → 门户代理它"的真实拓扑（下发 → 代理 → 本地建联代理）
// 验证：多个目标能同时挂在同一个门户下、按 IQN 各自路由、各自的缓存计数互不干扰。

const (
	portalBlockSize  = 512
	portalBlockCount = 2048

	portalServerIQNA = "iqn.2024-01.com.vault:repo-a-alloc-a"
	portalServerIQNB = "iqn.2024-01.com.vault:repo-b-alloc-b"
	// 本地代理 IQN 短名（vcache-*）与服务端短名（repo-*-alloc-*）不得互为子串，
	// 否则 Windows 侧按短名包含匹配找会话会串台（见 internal/agent/cacheproxy.go）。
	portalLocalIQNA = "iqn.2024-01.local.vault:vcache-alloc-a"
	portalLocalIQNB = "iqn.2024-01.local.vault:vcache-alloc-b"
)

// memLUN 是"服务端"那一侧的极简内存 LUN（只支持 READ(10)，够缓存读路径用）。
type memLUN struct {
	mu     sync.Mutex
	data   []byte
	serial string
	iqn    string
	reads  int
}

func newMemLUN(serial, iqn string) *memLUN {
	l := &memLUN{data: make([]byte, portalBlockSize*portalBlockCount), serial: serial, iqn: iqn}
	for i := range l.data {
		l.data[i] = byte(i/portalBlockSize) ^ byte(serial[len(serial)-1])
	}
	return l
}

func (l *memLUN) Info() backend.DeviceInfo {
	return backend.DeviceInfo{
		Vendor: "SRV", Product: "Server Disk", Revision: "0002",
		Serial: l.serial, WWID: "naa.6001405" + l.serial,
		BlockSize: portalBlockSize, BlockCount: portalBlockCount,
		TargetIQN: l.iqn,
	}
}

func (l *memLUN) Exec(_ context.Context, cdb []byte, _ []byte, _ int) (*backend.Result, error) {
	switch cdb[0] {
	case iscsi.SCSIRead10, iscsi.SCSIRead16:
		p, err := iscsi.ParseReadWrite(cdb)
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.reads++
		off := int64(p.LBA) * portalBlockSize
		n := int64(p.Blocks) * portalBlockSize
		if off+n > int64(len(l.data)) {
			return &backend.Result{Status: iscsi.StatusCheckCondition,
				Sense: iscsi.IllegalRequestSense(iscsi.ASCLogicalBlockAddressOutOfRange)}, nil
		}
		return &backend.Result{Status: iscsi.StatusGood, Data: append([]byte(nil), l.data[off:off+n]...)}, nil
	default:
		return &backend.Result{Status: iscsi.StatusCheckCondition,
			Sense: iscsi.IllegalRequestSense(iscsi.ASCInvalidCommandOperationCode)}, nil
	}
}

func (l *memLUN) Close() error { return nil }

func (l *memLUN) readCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// startServerTarget 起一个"服务端"iSCSI 目标（门户要去连的就是它）。
func startServerTarget(t *testing.T, iqn, serial string) (*memLUN, string) {
	t.Helper()
	lun := newMemLUN(serial, iqn)
	px, err := proxy.New(proxy.Config{
		Backend: lun, TargetIQN: iqn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("proxy.New(%s): %v", serial, err)
	}
	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0", TargetIQN: iqn}, px)
	if err != nil {
		t.Fatalf("frontend.New(%s): %v", serial, err)
	}
	t.Cleanup(func() { _ = tgt.Close() })
	go tgt.Serve(context.Background())
	return lun, tgt.Addr().String()
}

// readBlock 通过本机发起端读一个块（门户的读路径走的就是它）。
func readBlock(t *testing.T, ctx context.Context, it *backend.Initiator, lba uint64, blocks uint32) []byte {
	t.Helper()
	cdb := make([]byte, 10)
	cdb[0] = iscsi.SCSIRead10
	binary.BigEndian.PutUint32(cdb[2:6], uint32(lba))
	binary.BigEndian.PutUint16(cdb[7:9], uint16(blocks))
	res, err := it.Exec(ctx, cdb, nil, int(blocks)*portalBlockSize)
	if err != nil {
		t.Fatalf("read lba=%d: %v", lba, err)
	}
	if !res.OK() {
		t.Fatalf("read lba=%d status: %v", lba, res.Error())
	}
	return res.Data
}

// TestPortalServesMultipleTargets 锁定"一个门户下多个 iSCSI 目标"的核心行为。
func TestPortalServesMultipleTargets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lunA, addrA := startServerTarget(t, portalServerIQNA, "SRV-SN-A")
	lunB, addrB := startServerTarget(t, portalServerIQNB, "SRV-SN-B")

	// 门户先起来，再逐个添加目标 —— 这正是客户端的实际顺序（懒启动门户，按库挂载时注册）。
	p, err := iscsicache.NewPortal(iscsicache.PortalConfig{
		ListenAddr: "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPortal: %v", err)
	}
	defer p.Close()
	go p.Start(ctx)

	add := func(key, localIQN, serverAddr, serverIQN string) {
		t.Helper()
		cc := iscsicache.CacheConfig{BlockSize: 16 << 10, SectorSize: 4 << 10}
		cc.L1.SizeBytes = 16 << 20
		if err := p.Add(ctx, iscsicache.TargetConfig{
			Key:       key,
			TargetIQN: localIQN,
			Backend: iscsicache.BackendConfig{
				Address:   serverAddr,
				TargetIQN: serverIQN,
			},
			Cache:   cc,
			Vendor:  "VAULT",
			Product: "Vault Cache Disk",
		}); err != nil {
			t.Fatalf("Add(%s): %v", key, err)
		}
	}
	add("alloc-a", portalLocalIQNA, addrA, portalServerIQNA)
	add("alloc-b", portalLocalIQNB, addrB, portalServerIQNB)

	if keys := p.Keys(); len(keys) != 2 {
		t.Fatalf("门户下应有 2 个目标，实际 %v", keys)
	}
	// 幂等：同一分配重复挂载不该重建缓存（会丢命中计数与已预热的 L2）。
	if !p.Has("alloc-a") {
		t.Fatal("Has(alloc-a) 应为 true")
	}
	if err := p.Add(ctx, iscsicache.TargetConfig{
		Key: "alloc-a", TargetIQN: portalLocalIQNA,
		Backend: iscsicache.BackendConfig{Address: addrA, TargetIQN: portalServerIQNA},
	}); err != nil {
		t.Fatalf("重复 Add 应幂等，实际 %v", err)
	}

	// 本机发起端连**门户**的本地 IQN，拿到的必须是各自后端的内容。
	gotSerial := make(map[string]string, 2)
	for key, iqn := range map[string]string{"alloc-a": portalLocalIQNA, "alloc-b": portalLocalIQNB} {
		it, err := backend.Dial(ctx, backend.Config{
			Address: p.Addr().String(), TargetIQN: iqn,
			IOTimeout: 10 * time.Second, DialTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("dial portal %s: %v", iqn, err)
		}
		info := it.Info()
		// 厂商/型号被显式覆盖（保证代理盘与直连盘在 INQUIRY 上可区分，见 cacheproxy.go），
		// 而序列号透传后端 —— 后者正是这里用来判断"连到了哪个后端"的依据。
		if info.Vendor != "VAULT" || info.Product != "Vault Cache Disk" {
			t.Fatalf("%s 的 INQUIRY 未被覆盖：vendor=%q product=%q", key, info.Vendor, info.Product)
		}
		gotSerial[key] = info.Serial

		// 同一个块读两次：第二次必须由缓存满足（后端只读了一次）。
		first := readBlock(t, ctx, it, 0, 32)
		second := readBlock(t, ctx, it, 0, 32)
		if string(first) != string(second) {
			t.Fatalf("%s 两次读结果不一致", key)
		}
		_ = it.Close()
	}
	if gotSerial["alloc-a"] != "SRV-SN-A" || gotSerial["alloc-b"] != "SRV-SN-B" {
		t.Fatalf("按 IQN 路由失败：实际 %v", gotSerial)
	}

	// 后端各自的读计数只受自己的会话影响（未注册时 A/B 各因首读回源一次）。
	if lunA.readCount() != 1 || lunB.readCount() != 1 {
		t.Fatalf("后端读计数 = A:%d B:%d，期望各 1", lunA.readCount(), lunB.readCount())
	}

	// 每个目标有**独立**的缓存计数与用量上报（客户端存储库页面按库展示）。
	stA, ok := p.Stats("alloc-a")
	if !ok {
		t.Fatal("Stats(alloc-a) 未找到")
	}
	stB, ok := p.Stats("alloc-b")
	if !ok {
		t.Fatal("Stats(alloc-b) 未找到")
	}
	if stA.Cache.RequestHits == 0 || stB.Cache.RequestHits == 0 {
		t.Fatalf("两个目标都应有整命令命中：A=%+v B=%+v", stA.Cache, stB.Cache)
	}
	if stA.Cache.Reads != 2 || stB.Cache.Reads != 2 {
		t.Fatalf("两个目标各自应记 2 条读命令：A=%d B=%d", stA.Cache.Reads, stB.Cache.Reads)
	}
	if stA.Cache.L1UsedBytes == 0 || stA.BlockSize == 0 {
		t.Fatalf("A 的 L1 用量/块大小未上报：%+v", stA)
	}
	if iqn, _ := p.TargetIQN("alloc-a"); iqn != portalLocalIQNA {
		t.Fatalf("TargetIQN(alloc-a) = %q，期望 %q", iqn, portalLocalIQNA)
	}

	// 移除一个目标：另一个不受影响（一个库卸载不该影响别的库）。
	if err := p.Remove("alloc-a"); err != nil {
		t.Fatalf("Remove(alloc-a): %v", err)
	}
	if p.Has("alloc-a") {
		t.Fatal("移除后不应还认为 alloc-a 在门户里")
	}
	if !p.Has("alloc-b") {
		t.Fatal("移除 alloc-a 不应影响 alloc-b")
	}
	if it, err := backend.Dial(ctx, backend.Config{
		Address: p.Addr().String(), TargetIQN: portalLocalIQNA,
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second,
	}); err == nil {
		_ = it.Close()
		t.Fatal("已移除的目标不应还能登录")
	}
	// 幂等：重复移除不报错。
	if err := p.Remove("alloc-a"); err != nil {
		t.Fatalf("重复 Remove 应幂等，实际 %v", err)
	}
}

// TestPortalAddUnknownBackendFails 后端连不上时 Add 必须**立刻报错**。
//
// 调用方（agent 的挂载流程）据此回退直连服务端目标 —— 缓存永远不能变成挂载的前置条件。
func TestPortalAddUnknownBackendFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 占一个端口再关掉：该地址上确定没有人接。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p, err := iscsicache.NewPortal(iscsicache.PortalConfig{
		ListenAddr: "127.0.0.1:0",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPortal: %v", err)
	}
	defer p.Close()
	go p.Start(ctx)

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	cc := iscsicache.CacheConfig{}
	cc.L1.SizeBytes = 16 << 20
	if err := p.Add(ctx, iscsicache.TargetConfig{
		Key: "alloc-x", TargetIQN: "iqn.2024-01.local.vault:vcache-alloc-x",
		Backend: iscsicache.BackendConfig{
			Address:   net.JoinHostPort(host, strconv.Itoa(port)),
			TargetIQN: "iqn.2024-01.com.vault:repo-x-alloc-x",
		},
		Cache: cc,
	}); err == nil {
		t.Fatal("后端不可达时 Add 必须失败（调用方据此回退直连）")
	}
	if p.Has("alloc-x") {
		t.Fatal("Add 失败的目标不应留在门户里")
	}
}
