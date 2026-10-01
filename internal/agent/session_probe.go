package agent

import (
	"context"
	"strings"
	"time"
)

// 会话实测（MountState.session_active）的时间常量。
const (
	// mountSessionProbeInterval 是已挂载记录的会话实测间隔。
	//
	// 低频是刻意的：每次实测要起一个 PowerShell（Get-IscsiSession，约 0.5~1.5s），
	// 而"会话断掉"这件事本身不紧急（盘已经不可用了），界面上晚 20s 变过来完全够用。
	mountSessionProbeInterval = 20 * time.Second
	// mountSessionProbeTimeout 是单次实测的超时。
	mountSessionProbeTimeout = 15 * time.Second
)

// runMountSessionProbeLoop 后台低频实测"已挂载记录对应的 iSCSI 会话是否真的还在"，直到上下文结束。
//
// 真实诉求："在客户端挂载和卸载的按钮应以实际为准，看对应的 iSCSI 连接是否在活动中的"。
// 挂载记录只是**代理的意图**：会话被系统或用户断开（MSiSCSI 服务重启、长时间断网、
// 手动在 iSCSI 发起程序里断开）之后，记录仍写着 mounted，界面于是显示"已挂载 + 卸载"，
// 而这块盘其实根本不在。实测结果写进 MountState.session_active 并推 mount 事件，
// 界面据此把按钮翻回"挂载"（见 docs/implementation.md 5.5）。
func (a *Agent) runMountSessionProbeLoop(ctx context.Context) {
	// 启动时先跑一轮：从状态文件加载出来的记录一律是"尚未核对"（见 loadLocked），
	// 早一轮得到结论，界面就不会在这 20s 里按记录状态展示，也少一次"其实还挂着却被
	// 判为未挂载"的重复挂载。发起端尚未就绪时这一轮会自己跳过，等下一个 tick。
	a.probeSessionStates(ctx)

	ticker := time.NewTicker(mountSessionProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.probeSessionStates(ctx)
		}
	}
}

// probeSessionStates 对每条"可能还挂着东西"的记录实测一次会话状态。
//
// 实测 mounted 与 error 两种记录：
//   - mounted：记录声称已挂载，但会话可能早被断了 —— 这是主要场景；
//   - error：语义就是"本机还有残留待清理"或"自动挂载失败保留了现场"，盘可能还挂着，
//     而且 Windows 的持久会话（-IsPersistent）会自己把会话连回来，不实测就永远不知道。
//
// 不实测 mounting/unmounting（正在转场，会话本来就在建/在拆）与 revoked（记录随即被卸载删除）。
func (a *Agent) probeSessionStates(ctx context.Context) {
	// 发起端未就绪（服务没跑 / 模块缺失）时实测没有意义：此时 Get-IscsiSession 的结果
	// 反映的是"发起端不可用"，不是"盘没挂着"。保持上次结论，绝不猜 ——
	// 猜错的代价是把一块好端端挂着的盘显示成"已断开"（界面另有横幅提示发起端未就绪）。
	if !a.hostState().ISCSIReady {
		return
	}
	for _, ms := range a.store.ListMounts() {
		if ms.State != MountStateMounted && ms.State != MountStateError {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.probeMountSession(ctx, ms.AllocationID)
	}
}

// probeMountSession 实测某条挂载记录对应的会话是否活动，并把结论写回记录（变化时广播）。
//
// 返回 (是否活动, 是否得到结论)：探测失败或无从实测时 verified=false ——
// **绝不猜**（一次 PowerShell 失败不该让界面把挂着的盘显示成"已断开"）。
func (a *Agent) probeMountSession(ctx context.Context, allocationID string) (bool, bool) {
	ms, _, ok := a.store.GetMount(allocationID)
	if !ok {
		return false, false
	}
	targetIQN := strings.TrimSpace(ms.TargetIQN)
	if targetIQN == "" {
		// 占位记录（会话还没建立）或记录损坏：无从实测。
		return false, false
	}

	probeCtx, cancel := context.WithTimeout(ctx, mountSessionProbeTimeout)
	defer cancel()
	connected, err := a.iscsi.IsConnected(probeCtx, targetIQN)
	if err != nil {
		a.noteSessionProbeFailure(allocationID, targetIQN, err)
		return false, false
	}

	changed := a.recordSessionProbe(allocationID, connected, time.Now().UnixMilli())
	if changed {
		a.logger.Info("实测 iSCSI 会话状态变化",
			"allocation_id", allocationID, "target_iqn", targetIQN, "session_active", connected)
	}
	return connected, true
}

// recordSessionProbe 写入一次实测结论，并在结论变化时广播 mount 事件；返回是否发生变化。
//
// 广播收敛在这里：结论变化正是"界面该翻按钮"的信号，让写状态和推事件同一个入口，
// 调用方就无法漏推（漏推的后果是界面一直显示旧结论，得用户手动刷新才对）。
//
// 只在**结论变化**时才写盘与广播：这条路径每 20s 跑一轮，无条件写入会变成
// "状态文件写个不停 + 事件风暴"（心跳那条路径同理）。
func (a *Agent) recordSessionProbe(allocationID string, connected bool, at int64) bool {
	ms, rt, ok := a.store.GetMount(allocationID)
	if !ok {
		return false
	}
	if ms.SessionCheckedAt > 0 && ms.SessionActive == connected && rt.SessionProbeFailures == 0 {
		return false
	}

	var out MountState
	changed := false
	a.store.UpdateMount(allocationID, func(m *MountState, r *mountRuntime) {
		r.SessionProbeFailures = 0
		if m.SessionCheckedAt > 0 && m.SessionActive == connected {
			return
		}
		m.SessionActive = connected
		m.SessionCheckedAt = at
		cp := *m
		out = cp
		changed = true
	})
	if changed {
		a.publishMount(&out)
	}
	return changed
}

// noteSessionProbeFailure 记录一次实测失败：只在**首次**失败时记日志。
//
// 为什么不在每次失败时都记：探测失败往往是宿主机上 PowerShell 出了问题，那会每 20s
// 失败一次，日志被同一句话刷满（结论本身不变 —— 失败不改状态，见 probeMountSession）。
func (a *Agent) noteSessionProbeFailure(allocationID, targetIQN string, cause error) {
	first := false
	a.store.UpdateMount(allocationID, func(_ *MountState, r *mountRuntime) {
		r.SessionProbeFailures++
		first = r.SessionProbeFailures == 1
	})
	if first {
		a.logger.Warn("实测 iSCSI 会话失败（保持上次结论，不猜）",
			"allocation_id", allocationID, "target_iqn", targetIQN, "error", cause)
	}
}

// mountIdempotent 判断挂载请求能否直接复用既有挂载记录（幂等命中）。
//
// 只有"记录是已挂载"**且**"实测确认会话还活着"才算命中：未核对（刚启动）与实测失败都
// 按未命中处理 —— 宁可多做一次幂等的挂载，也不谎报"已经挂好了"（真实诉求见 session_probe.go 顶部）。
func mountIdempotent(ms MountState, active, verified bool) bool {
	return ms.State == MountStateMounted && verified && active
}

// mountPointRemoved 判断"移除挂载点报错"是否可以视为挂载点已经不存在（卸载的幂等兜底）。
//
//   - pointGone：磁盘号已知，且查当前挂载点为空；
//   - 本机已**实测**无活动会话：盘随会话一起消失，挂载点同样不可能还在。这条专治
//     "会话被断后记录仍是已挂载"的场景 —— 进程重启后磁盘号无从得知（pointGone 查不出来），
//     没有这条兜底用户就会既挂不上、又卸不掉。
func mountPointRemoved(pointGone, sessionActive, sessionVerified bool) bool {
	if pointGone {
		return true
	}
	return sessionVerified && !sessionActive
}
