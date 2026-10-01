package agent

import (
	"context"
	"errors"
	"testing"
)

// TestRecordSessionProbeReportsDisconnect 锁定"会话断掉后界面能看到实际状态"。
//
// 真实场景：MSiSCSI 服务重启 / 长时间断网 / 用户手动在发起程序里断开之后，挂载记录
// 仍写着 mounted，界面于是显示"已挂载 + 卸载"，而盘根本不在。实测结论变化时必须写回
// 并广播，界面才会把按钮翻回"挂载"、标签变成"已断开"。
func TestRecordSessionProbeReportsDisconnect(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-probe"
	a.store.PutMount(&MountState{
		AllocationID: alloc, State: MountStateMounted, TargetIQN: "iqn.2024-01.test:repo-1",
		SessionActive: true, SessionCheckedAt: 1,
	}, &mountRuntime{})

	_, events := a.hub.Subscribe()

	// ① 实测无活动会话 → 如实写回并广播。
	if !a.recordSessionProbe(alloc, false, 1700000000000) {
		t.Fatal("结论变化应返回 changed=true")
	}
	ms, _, _ := a.store.GetMount(alloc)
	if ms.SessionActive {
		t.Fatal("实测无会话时 session_active 必须为 false")
	}
	if ms.SessionCheckedAt != 1700000000000 {
		t.Fatalf("核对时刻 = %d，期望 1700000000000", ms.SessionCheckedAt)
	}
	if ev, ok := takeMountEvent(t, events); !ok || ev.Type != "mount" {
		t.Fatalf("应广播一条 mount 事件，实际 %+v (ok=%v)", ev, ok)
	}

	// ② 结论不变 → 不写盘、不广播（这条路径每 20s 一轮，不能变成事件风暴）。
	if a.recordSessionProbe(alloc, false, 1700000009000) {
		t.Fatal("结论未变时不应重复写回与广播")
	}
	if ev, ok := takeMountEvent(t, events); ok {
		t.Fatalf("结论未变不应广播事件，实际收到 %s", ev.Type)
	}
	if ms, _, _ := a.store.GetMount(alloc); ms.SessionCheckedAt != 1700000000000 {
		t.Fatalf("结论未变不应刷新核对时刻，实际 %d", ms.SessionCheckedAt)
	}

	// ③ 会话恢复 → 同样如实广播（界面按钮再翻回"卸载"）。
	if !a.recordSessionProbe(alloc, true, 1700000010000) {
		t.Fatal("会话恢复应返回 changed=true")
	}
	if ms, _, _ := a.store.GetMount(alloc); !ms.SessionActive || ms.SessionCheckedAt != 1700000010000 {
		t.Fatalf("会话恢复应写回 true，实际 %+v", ms)
	}
}

// TestRecordSessionProbeMarksUnverifiedRecord 未核对的记录即使结论是"无会话"也要落核对时刻。
//
// 未核对（session_checked_at=0）与"实测无会话"在界面上是两种含义：前者按记录状态展示，
// 后者才把按钮翻成"挂载"。所以哪怕 session_active 本来就是 false，第一次实测也必须
// 留下核对时刻并广播。
func TestRecordSessionProbeMarksUnverifiedRecord(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-unverified"
	a.store.PutMount(&MountState{
		AllocationID: alloc, State: MountStateMounted, TargetIQN: "iqn.2024-01.test:repo-2",
	}, &mountRuntime{})

	_, events := a.hub.Subscribe()

	if !a.recordSessionProbe(alloc, false, 1700000000000) {
		t.Fatal("未核对的记录首次实测应返回 changed=true")
	}
	if ms, _, _ := a.store.GetMount(alloc); ms.SessionCheckedAt != 1700000000000 {
		t.Fatalf("未核对的记录首次实测应留下核对时刻，实际 %d", ms.SessionCheckedAt)
	}
	if ev, ok := takeMountEvent(t, events); !ok || ev.Type != "mount" {
		t.Fatalf("应广播一条 mount 事件，实际 %+v (ok=%v)", ev, ok)
	}
}

// TestSessionProbeFailureDoesNotGuess 探测失败绝不改结论，也不广播。
//
// 一次 PowerShell 失败不该让界面把一块好端端挂着的盘显示成"已断开"，更不该让挂载请求
// 因此判为"未挂载"而去做重复挂载。
func TestSessionProbeFailureDoesNotGuess(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-fail"
	a.store.PutMount(&MountState{
		AllocationID: alloc, State: MountStateMounted, TargetIQN: "iqn.2024-01.test:repo-3",
		SessionActive: true, SessionCheckedAt: 1,
	}, &mountRuntime{})

	_, events := a.hub.Subscribe()

	a.noteSessionProbeFailure(alloc, "iqn.2024-01.test:repo-3", errors.New("powershell 不可用"))

	ms, rt, _ := a.store.GetMount(alloc)
	if !ms.SessionActive || ms.SessionCheckedAt != 1 {
		t.Fatalf("探测失败不得改动实测结论，实际 %+v", ms)
	}
	if rt.SessionProbeFailures != 1 {
		t.Fatalf("连续失败计数 = %d，期望 1", rt.SessionProbeFailures)
	}
	if ev, ok := takeMountEvent(t, events); ok {
		t.Fatalf("探测失败不应广播事件，实际收到 %s", ev.Type)
	}

	// 连续失败继续计数（日志只在首次失败时记一条，见 noteSessionProbeFailure）。
	a.noteSessionProbeFailure(alloc, "iqn.2024-01.test:repo-3", errors.New("powershell 不可用"))
	if _, rt, _ := a.store.GetMount(alloc); rt.SessionProbeFailures != 2 {
		t.Fatalf("连续失败计数 = %d，期望 2", rt.SessionProbeFailures)
	}
}

// TestMountIdempotentOnlyWhenSessionVerifiedLive 锁定挂载幂等判据：只有"记录已挂载 + 实测会话活动"才命中。
func TestMountIdempotentOnlyWhenSessionVerifiedLive(t *testing.T) {
	mounted := MountState{State: MountStateMounted}
	errState := MountState{State: MountStateError}

	cases := []struct {
		name             string
		ms               MountState
		active, verified bool
		want             bool
	}{
		{"记录已挂载且实测会话活动", mounted, true, true, true},
		{"记录已挂载但实测无会话", mounted, false, true, false},
		{"记录已挂载但尚未核对", mounted, false, false, false},
		{"记录已挂载但探测失败", mounted, true, false, false},
		{"记录不是已挂载", errState, true, true, false},
	}
	for _, tc := range cases {
		if got := mountIdempotent(tc.ms, tc.active, tc.verified); got != tc.want {
			t.Fatalf("%s：mountIdempotent = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestMountPointRemovedTreatsDeadSessionAsRemoved 锁定卸载的幂等兜底。
//
// 关键一条是"磁盘号未知 + 实测无会话"：进程重启后磁盘号无从得知（pointGone 查不出来），
// 没有这条兜底，用户面对会话已断的记录会既挂不上又卸不掉。
func TestMountPointRemovedTreatsDeadSessionAsRemoved(t *testing.T) {
	cases := []struct {
		name                               string
		pointGone, sessionActive, verified bool
		want                               bool
	}{
		{"磁盘号已知且查不到挂载点", true, true, true, true},
		{"磁盘号未知但实测无会话", false, false, true, true},
		{"磁盘号未知且会话还活着", false, true, true, false},
		{"磁盘号未知且尚未核对", false, false, false, false},
		{"磁盘号未知且探测失败", false, true, false, false},
	}
	for _, tc := range cases {
		if got := mountPointRemoved(tc.pointGone, tc.sessionActive, tc.verified); got != tc.want {
			t.Fatalf("%s：mountPointRemoved = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// TestProbeSessionStatesKeepsResultWhenHostNotReady 发起端未就绪时整轮跳过，结论保持不动。
//
// 发起端不可用（MSiSCSI 服务没跑、模块缺失）时，Get-IscsiSession 的结果反映的是"发起端
// 不可用"，不是"盘没挂着"。此时若照样写结论，界面会把所有挂载显示成"已断开"。
func TestProbeSessionStatesKeepsResultWhenHostNotReady(t *testing.T) {
	a := newPhaseTestAgent(t)
	a.store.PutMount(&MountState{
		AllocationID: "alloc-mounting", State: MountStateMounting, TargetIQN: "iqn.2024-01.test:repo-4",
	}, &mountRuntime{})
	a.store.PutMount(&MountState{
		AllocationID: "alloc-mounted", State: MountStateMounted, TargetIQN: "iqn.2024-01.test:repo-5",
		SessionActive: true, SessionCheckedAt: 1,
	}, &mountRuntime{})

	_, events := a.hub.Subscribe()

	if a.hostState().ISCSIReady {
		t.Fatal("测试前提：裸代理的主机状态应为未就绪")
	}
	a.probeSessionStates(context.Background())

	if ms, _, _ := a.store.GetMount("alloc-mounted"); !ms.SessionActive || ms.SessionCheckedAt != 1 {
		t.Fatalf("发起端未就绪时不得改动既有结论，实际 %+v", ms)
	}
	if ms, _, _ := a.store.GetMount("alloc-mounting"); ms.SessionCheckedAt != 0 {
		t.Fatalf("正在挂载的记录不该被实测，核对时刻 = %d", ms.SessionCheckedAt)
	}
	if ev, ok := takeMountEvent(t, events); ok {
		t.Fatalf("不应广播事件，实际收到 %s", ev.Type)
	}
}
