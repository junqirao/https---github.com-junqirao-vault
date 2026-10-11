package iscsiproxy_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
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
