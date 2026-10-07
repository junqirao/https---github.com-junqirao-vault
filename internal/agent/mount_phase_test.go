package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"vault/internal/lock"
)

// newPhaseTestAgent 构造一个只带状态存储、事件总线与锁表的代理，用于验证挂载阶段推进。
//
// 不启 HTTP、不碰 iSCSI：阶段推进是纯状态机 + 事件广播，测试只需这些。
// 锁表必须给：卸载走 unmount → lock.Keyed，缺了它会在 nil 上 panic，而 panic 被 safeGo
// 吞掉之后只表现为"记录一动不动"，比错误本身更难查。
func newPhaseTestAgent(t *testing.T) *Agent {
	t.Helper()
	store, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"), testLogger())
	if err != nil {
		t.Fatalf("构造状态存储失败：%v", err)
	}
	a := &Agent{logger: testLogger(), store: store, hub: NewEventHub(), locks: lock.NewKeyed()}
	a.engine = &mountEngine{a: a}
	return a
}

// mountPhaseTestServerKey 是阶段用例用的服务端键。
//
// 阶段推进（服务端发来 progress 事件 → 本机挂载阶段）只按分配 ID 认领，与"是哪台服务端"
// 无关；这里仍显式给一台，是因为多服务端下的事件入口按台划分（handleServerEvent(key, …)），
// 事件语义上必须落在某一台上。
const mountPhaseTestServerKey = "https://10.0.0.1:8443"

// takeMountEvent 非阻塞取出一条事件（Publish 是同步写入带缓冲通道的，调用返回后即可读到）。
func takeMountEvent(t *testing.T, events <-chan Event) (Event, bool) {
	t.Helper()
	select {
	case ev := <-events:
		return ev, true
	default:
		return Event{}, false
	}
}

// TestServerMountPhaseEventsAdvanceLocalPhase 锁定"服务端阶段事件 → 本机挂载阶段"的推进。
//
// 真实反馈：挂载在服务端是同步重活（等差异盘 + PowerShell 全量下发 iSCSI 目标，实测数十秒），
// 界面以前只有一个转圈的按钮，用户不知道卡在哪一步。服务端因此在同步请求内发阶段事件
// （topic=mount），代理经 SSE 收到后写进本机挂载状态并广播给界面。
func TestServerMountPhaseEventsAdvanceLocalPhase(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-1"
	a.store.PutMount(&MountState{
		AllocationID: alloc,
		ServerKey:    mountPhaseTestServerKey,
		State:        MountStateMounting,
		Phase:        MountPhaseRequesting,
	}, &mountRuntime{})

	_, events := a.hub.Subscribe()

	phaseEvent := func(phase string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{
			"action":        "progress",
			"allocation_id": alloc,
			"phase":         phase,
		})
		if err != nil {
			t.Fatalf("构造事件失败：%v", err)
		}
		return raw
	}

	// ① 服务端推进 → 本机阶段跟随，并广播给界面。
	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, phaseEvent(MountPhasePreparingDisk))
	ms, _, ok := a.store.GetMount(alloc)
	if !ok {
		t.Fatal("挂载状态丢失")
	}
	if ms.Phase != MountPhasePreparingDisk {
		t.Fatalf("阶段 = %q，期望 %q", ms.Phase, MountPhasePreparingDisk)
	}
	if ev, ok := takeMountEvent(t, events); !ok || ev.Type != "mount" {
		t.Fatalf("应广播一条 mount 事件，实际 %+v (ok=%v)", ev, ok)
	}

	// ② 乱序/迟到的事件不得让阶段回退（界面会出现"阶段倒着走"）。
	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, phaseEvent(MountPhaseAllocating))
	if ms, _, _ := a.store.GetMount(alloc); ms.Phase != MountPhasePreparingDisk {
		t.Fatalf("阶段回退了：%q", ms.Phase)
	}

	// ③ 继续前进。
	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, phaseEvent(MountPhaseConfiguringTarget))
	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, phaseEvent(MountPhaseConnecting))
	if ms, _, _ := a.store.GetMount(alloc); ms.Phase != MountPhaseConnecting {
		t.Fatalf("阶段 = %q，期望 %q", ms.Phase, MountPhaseConnecting)
	}

	// ④ 非进度动作（action != progress）一律忽略。
	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, json.RawMessage(`{"action":"done","allocation_id":"alloc-1"}`))
	if ms, _, _ := a.store.GetMount(alloc); ms.Phase != MountPhaseConnecting {
		t.Fatalf("非进度事件不应改变阶段，实际 %q", ms.Phase)
	}
}

// TestServerMountPhaseIgnoresUnknownAllocation 阶段事件只对本机正在挂载的分配生效。
//
// 服务端的事件是**广播**的（进程内 EventHub），别的客户端在挂载时本机也会收到；
// 若不加判断，界面会凭空多出一条"正在挂载"的假状态。
func TestServerMountPhaseIgnoresUnknownAllocation(t *testing.T) {
	a := newPhaseTestAgent(t)
	_, events := a.hub.Subscribe()

	a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, json.RawMessage(
		`{"action":"progress","allocation_id":"other-client","phase":"preparing_disk"}`))

	if _, _, ok := a.store.GetMount("other-client"); ok {
		t.Fatal("不得为其它客户端的挂载造出本机状态")
	}
	if ev, ok := takeMountEvent(t, events); ok {
		t.Fatalf("不应广播事件，实际收到 %s", ev.Type)
	}
}

// TestServerMountPhaseIgnoresSettledMount 已经结束的挂载不再接受阶段推进。
func TestServerMountPhaseIgnoresSettledMount(t *testing.T) {
	a := newPhaseTestAgent(t)
	const alloc = "alloc-2"

	for _, state := range []string{MountStateMounted, MountStateError, MountStateRevoked} {
		a.store.PutMount(&MountState{AllocationID: alloc, ServerKey: mountPhaseTestServerKey, State: state}, &mountRuntime{})
		a.handleServerEvent(mountPhaseTestServerKey, serverMountEventType, json.RawMessage(
			`{"action":"progress","allocation_id":"alloc-2","phase":"preparing_disk"}`))
		ms, _, _ := a.store.GetMount(alloc)
		if ms.Phase != "" {
			t.Fatalf("state=%s 时不应接受阶段推进，实际 phase=%q", state, ms.Phase)
		}
	}
}

// TestMountPhaseRankIsMonotonic 阶段定序必须严格递增（它保证阶段只前进不回退）。
//
// 顺序即真实流程：申请 → 服务端分配资源 → 服务端准备磁盘 → 服务端下发 iSCSI 目标
// → 本机建立会话 → 磁盘上线 → 挂载点 → 后置脚本。
func TestMountPhaseRankIsMonotonic(t *testing.T) {
	order := []string{
		MountPhaseRequesting,
		MountPhaseAllocating,
		MountPhasePreparingDisk,
		MountPhaseConfiguringTarget,
		MountPhaseConnecting,
		MountPhaseOnline,
		MountPhaseMountPoint,
		MountPhasePostScript,
	}
	for i := 1; i < len(order); i++ {
		if mountPhaseRank(order[i]) <= mountPhaseRank(order[i-1]) {
			t.Fatalf("%s 的序必须大于 %s（否则阶段会回退）", order[i], order[i-1])
		}
	}
	if mountPhaseRank("no_such_phase") != 0 {
		t.Fatal("未知阶段的序应为 0（最低），使任何已知阶段都能覆盖它")
	}
	if mountPhaseRank("") != 0 {
		t.Fatal("空阶段的序应为 0")
	}
}

// TestServerMountPhaseWireContract 锁定线上取值：服务端发的字符串与代理常量必须逐字相同。
//
// 这两组字面量分布在 internal/app 与 internal/agent 两个包（不共享常量以避免反向依赖），
// 所以用测试把"线上契约"钉住：改了一边不改另一边，阶段就永远不会推进。
func TestServerMountPhaseWireContract(t *testing.T) {
	// 与 internal/app/events.go 的 MountPhase* 保持一致。
	cases := map[string]string{
		"allocating":         MountPhaseAllocating,
		"preparing_disk":     MountPhasePreparingDisk,
		"configuring_target": MountPhaseConfiguringTarget,
	}
	for wire, local := range cases {
		if wire != local {
			t.Fatalf("线上取值 %q 与代理常量 %q 不一致", wire, local)
		}
	}
	// 事件名（topic）同理：与 internal/app 的 serverMountEventType 必须逐字一致。
	if serverMountEventType != "mount" {
		t.Fatalf("阶段事件名 = %q，期望 mount", serverMountEventType)
	}
}
