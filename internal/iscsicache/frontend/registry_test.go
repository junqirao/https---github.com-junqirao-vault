package frontend_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"vault/internal/iscsicache/backend"
	"vault/internal/iscsicache/frontend"
	"vault/internal/iscsicache/iscsi"
	"vault/internal/iscsicache/proxy"
)

// 多目标门户（一个客户端起一个代理，代理下挂多个 iSCSI 目标，见 internal/agent/cacheproxy.go）：
// 门户只监听一个端口，按登录时的 TargetName 把会话路由到各自的 Handler。

const (
	regIQNA = "iqn.2024-01.local.vault:vcache-alloc-a"
	regIQNB = "iqn.2024-01.local.vault:vcache-alloc-b"
)

// taggedBackend 是带独立序列号与 IQN 的回环后端：多目标路由测试要能分辨"连到了哪一个"。
type taggedBackend struct {
	*loopBackend
	serial string
	iqn    string
}

func (b *taggedBackend) Info() backend.DeviceInfo {
	info := b.loopBackend.Info()
	info.Serial = b.serial
	info.TargetIQN = b.iqn
	return info
}

// newTaggedProxy 为给定序列号造一个完整的缓存代理层（后端 → 缓存 → 代理）。
func newTaggedProxy(t *testing.T, serial, iqn string) *proxy.Proxy {
	t.Helper()
	be := &taggedBackend{loopBackend: newLoopBackend(), serial: serial, iqn: iqn}
	px, err := proxy.New(proxy.Config{
		Backend:   be,
		TargetIQN: iqn,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("proxy.New(%s): %v", serial, err)
	}
	return px
}

// TestMultiTargetRegistryRoutesByIQN 锁定"一个门户多个目标"的核心行为：
// 注册多个 IQN 后，按各自 IQN 登录会拿到**各自**的设备（而不是串台到同一个后端）。
func TestMultiTargetRegistryRoutesByIQN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 空门户：不传单目标 IQN，之后逐个 Register。
	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	defer tgt.Close()
	if err := tgt.Register(regIQNA, newTaggedProxy(t, "SN-A", regIQNA)); err != nil {
		t.Fatalf("Register(A): %v", err)
	}
	if err := tgt.Register(regIQNB, newTaggedProxy(t, "SN-B", regIQNB)); err != nil {
		t.Fatalf("Register(B): %v", err)
	}
	go tgt.Serve(ctx)

	names := tgt.TargetNames()
	if len(names) != 2 || names[0] != regIQNA || names[1] != regIQNB {
		t.Fatalf("已注册的目标应为排序后的两个 IQN，实际 %v", names)
	}

	// 按 IQN 分别登录，各自的 INQUIRY 必须来自各自的后端。
	for iqn, want := range map[string]string{regIQNA: "SN-A", regIQNB: "SN-B"} {
		it, err := backend.Dial(ctx, backend.Config{
			Address: tgt.Addr().String(), TargetIQN: iqn,
			IOTimeout: 10 * time.Second, DialTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("dial %s: %v", iqn, err)
		}
		if got := it.Info().Serial; got != want {
			t.Fatalf("%s 连到了序列号 %q 的设备，期望 %q（按 IQN 路由失效）", iqn, got, want)
		}
		_ = it.Close()
	}

	// 未注册的 IQN 必须被拒（而不是落进某个目标）。
	if it, err := backend.Dial(ctx, backend.Config{
		Address: tgt.Addr().String(), TargetIQN: "iqn.2024-01.local.vault:vcache-unknown",
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second,
	}); err == nil {
		_ = it.Close()
		t.Fatal("未注册的 IQN 不应登录成功")
	}

	// 注销其中一个：只剩另一个可用。
	tgt.Unregister(regIQNB)
	if names := tgt.TargetNames(); len(names) != 1 || names[0] != regIQNA {
		t.Fatalf("注销 B 后应只剩 A，实际 %v", names)
	}
	if it, err := backend.Dial(ctx, backend.Config{
		Address: tgt.Addr().String(), TargetIQN: regIQNB,
		IOTimeout: 5 * time.Second, DialTimeout: 5 * time.Second,
	}); err == nil {
		_ = it.Close()
		t.Fatal("已注销的 IQN 不应还能登录")
	}

	// 幂等：重复注册/重复注销都不报错。
	if err := tgt.Register(regIQNA, newTaggedProxy(t, "SN-A2", regIQNA)); err != nil {
		t.Fatalf("重复 Register 应幂等，实际 %v", err)
	}
	tgt.Unregister(regIQNB)
}

// TestMultiTargetRegistryLoginRejection 锁定未注册目标的拒绝语义：
// class=0x02（Initiator Error）detail=0x03（Target not found）。
//
// 为什么必须断言到 class/detail：Windows 发起端把"目标不存在"与"认证失败"区分为完全不同的
// 提示，含糊地断开只会让用户以为是网络问题。
func TestMultiTargetRegistryLoginRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	defer tgt.Close()
	if err := tgt.Register(regIQNA, newTaggedProxy(t, "SN-A", regIQNA)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	go tgt.Serve(ctx)

	conn, err := net.DialTimeout("tcp", tgt.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 一次性把身份键都发出来（本模块的发起端就是这么发的，见 backend/initiator.go）。
	p := iscsi.NewPDU(iscsi.OpLoginReq, iscsi.EncodeText(
		[2]string{"InitiatorName", "iqn.1991-05.com.microsoft:test"},
		[2]string{"TargetName", "iqn.2024-01.local.vault:vcache-unknown"},
		[2]string{"SessionType", "Normal"},
		[2]string{"AuthMethod", "None"},
	))
	p.Header.SetFlags(iscsi.LoginFlagCSGShift | iscsi.LoginFlagTransit | iscsi.StageFullFeature)
	p.Header.SetImmediate(true)
	copy(p.Header[8:14], "reject")
	p.Header.SetITT(0x31)
	binary.BigEndian.PutUint16(p.Header[20:22], 1)
	if err := iscsi.WritePDU(conn, p); err != nil {
		t.Fatalf("send login: %v", err)
	}
	resp, err := iscsi.ReadPDU(conn)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	if resp.Header.Opcode() != iscsi.OpLoginResp {
		t.Fatalf("期望 Login Response，实际 %s", iscsi.OpcodeName(resp.Header.Opcode()))
	}
	if class, detail := resp.Header.LoginStatusClass(), resp.Header.LoginStatusDetail(); class != 0x02 || detail != 0x03 {
		t.Fatalf("未注册目标应回 class=0x02 detail=0x03，实际 class=%#x detail=%#x", class, detail)
	}
}

// TestMultiTargetSendTargetsListsAll SendTargets=all 必须列出门户下**全部**已注册 IQN。
//
// 这是"一个门户多目标"能被本机发起端发现的前提：发现列表里少一个，那个库就永远连不上。
func TestMultiTargetSendTargetsListsAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tgt, err := frontend.New(frontend.Config{ListenAddr: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("frontend.New: %v", err)
	}
	defer tgt.Close()
	for iqn, serial := range map[string]string{regIQNA: "SN-A", regIQNB: "SN-B"} {
		if err := tgt.Register(iqn, newTaggedProxy(t, serial, iqn)); err != nil {
			t.Fatalf("Register(%s): %v", iqn, err)
		}
	}
	go tgt.Serve(ctx)

	conn, err := net.DialTimeout("tcp", tgt.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	login := func(csg, nsg uint8, keys [][2]string) *iscsi.PDU {
		t.Helper()
		p := iscsi.NewPDU(iscsi.OpLoginReq, iscsi.EncodeText(keys...))
		p.Header.SetFlags(csg<<iscsi.LoginFlagCSGShift | (nsg & iscsi.LoginFlagNSGMask) | iscsi.LoginFlagTransit)
		p.Header.SetImmediate(true)
		copy(p.Header[8:14], "disc01")
		p.Header.SetITT(0x41)
		binary.BigEndian.PutUint16(p.Header[20:22], 1)
		if err := iscsi.WritePDU(conn, p); err != nil {
			t.Fatalf("send login: %v", err)
		}
		resp, err := iscsi.ReadPDU(conn)
		if err != nil {
			t.Fatalf("read login response: %v", err)
		}
		if class := resp.Header.LoginStatusClass(); class != 0 {
			t.Fatalf("login rejected: class=%d detail=%d", class, resp.Header.LoginStatusDetail())
		}
		return resp
	}

	resp := login(iscsi.StageSecurity, iscsi.StageOperational, [][2]string{
		{"AuthMethod", "None"}, {"HeaderDigest", "None"}, {"DataDigest", "None"},
	})
	login(resp.Header.LoginNSG(), iscsi.StageFullFeature, [][2]string{
		{"SessionType", "Discovery"}, {"MaxRecvDataSegmentLength", "65536"},
	})

	text := iscsi.NewPDU(iscsi.OpTextReq, iscsi.EncodeText([2]string{"SendTargets", "All"}))
	text.Header.SetFlags(iscsi.FlagAlwaysSet)
	text.Header.SetITT(0x42)
	text.Header.SetTTT(iscsi.ReservedTag)
	text.Header.SetCmdSN(1)
	if err := iscsi.WritePDU(conn, text); err != nil {
		t.Fatalf("send text: %v", err)
	}
	tr, err := iscsi.ReadPDU(conn)
	if err != nil {
		t.Fatalf("read text response: %v", err)
	}
	if tr.Header.Opcode() != iscsi.OpTextResp {
		t.Fatalf("期望 Text Response，实际 %s", iscsi.OpcodeName(tr.Header.Opcode()))
	}
	// 多目标时 "targetname" 键会重复出现，用 map 只能留最后一个，因此直接在原文里找。
	body := string(tr.Data)
	for _, iqn := range []string{regIQNA, regIQNB} {
		if !strings.Contains(body, iqn) {
			t.Fatalf("SendTargets 未列出 %q：%q", iqn, body)
		}
	}
}
