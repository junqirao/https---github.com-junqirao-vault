package agent

import (
	"context"
	"strings"
	"time"
)

// 心跳与租约的时间常量（见 docs/implementation.md 5.7）。
const (
	// defaultHeartbeatSeconds 默认心跳间隔（服务端未下发 heartbeat_seconds 时使用）。
	defaultHeartbeatSeconds = 30 * time.Second
	// defaultLeaseTTL 默认租约 TTL（服务端未下发 lease_ttl_seconds 时使用）。
	defaultLeaseTTL = 120 * time.Second
	// heartbeatSweepInterval 心跳调度扫描间隔。
	heartbeatSweepInterval = 2 * time.Second
	// heartbeatCallTimeout 单次心跳调用的超时。
	heartbeatCallTimeout = 15 * time.Second
	// maxHeartbeatBackoff 心跳失败退避上限。
	maxHeartbeatBackoff = 5 * time.Minute
)

// runHeartbeatLoop 周期扫描并发送租约心跳，直到上下文结束。
//
// 即使界面关闭，代理也持续维持心跳（见 5.7）。
func (a *Agent) runHeartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(heartbeatSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweepHeartbeats(ctx)
		}
	}
}

// sweepHeartbeats 对所有到期的活跃挂载发送心跳。
//
// 覆盖面（此前只发 state=mounted，是"挂载一两分钟后失效"的关键成因之一）：
//   - mounted：常规续租；
//   - mounting：挂载可能是几十秒到几分钟的重活（服务端等差异盘 + 下发 iSCSI 目标、
//     本机建会话 + 磁盘上线）。这段窗口里若不续租，租约会在**挂载完成之前**就过期
//     （服务端 TTL 默认 120s），随后过期被判离线、客户端下一次心跳被拒 → 刚挂上就被卸载；
//   - error 且 rt.HeartbeatError：因心跳连续失败被置 error（磁盘仍在本机）的记录，
//     继续尝试心跳，服务端恢复后自动回到 mounted（见 heartbeatOne）。
//     挂载失败保留的 error 记录不在此列 —— 那些没有"正在挂载的东西"要维持。
func (a *Agent) sweepHeartbeats(ctx context.Context) {
	now := time.Now()
	for _, ms := range a.store.ListMounts() {
		_, rt, ok := a.store.GetMount(ms.AllocationID)
		if !ok {
			continue
		}
		if !heartbeatEligible(ms.State, rt) {
			continue
		}
		// 占位记录（调用服务端前写入，尚无 lease_id）：没有租约可续，跳过而不是发一个空心跳
		// —— 那会拿到 lease.revoked 并把正在进行的挂载自己卸载掉。
		if strings.TrimSpace(ms.LeaseID) == "" {
			continue
		}
		if rt.NextHeartbeatAt > 0 && now.UnixMilli() < rt.NextHeartbeatAt {
			continue
		}
		a.heartbeatOne(ctx, ms, now)
	}
}

// heartbeatEligible 判断某条挂载记录当前是否应该发心跳。
func heartbeatEligible(state string, rt mountRuntime) bool {
	switch state {
	case MountStateMounting, MountStateMounted:
		return true
	case MountStateError:
		// 只有"心跳失败导致的 error"还能靠心跳自愈；挂载失败留下的 error 不该被心跳复位。
		return rt.HeartbeatError
	}
	return false
}

// heartbeatOne 对单个租约发送一次心跳，并按结果更新状态。
func (a *Agent) heartbeatOne(ctx context.Context, ms MountState, now time.Time) {
	client, err := a.serverClient()
	if err != nil {
		// 无会话：不视为心跳失败，等待前端推送会话（不写 last_error）。
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, heartbeatCallTimeout)
	defer cancel()

	result, hbErr := client.Heartbeat(callCtx, ms.LeaseID, a.store.ClientID())
	if hbErr != nil {
		if isLeaseRevoked(hbErr) {
			// 服务端已撤销租约（终态）：说明是管理员强制下线或该租约已被释放，按踢下线处理。
			// 注意：租约**过期**不会走到这里 —— 过期可恢复，服务端返回成功并把它续回 active。
			// reason 用服务端同样的稳定码（lease.revoked），界面据此翻译成"该租约已被强制下线"。
			a.logger.Warn("心跳返回租约已撤销，执行主动卸载", "allocation_id", ms.AllocationID)
			a.handleRevoke(ctx, ms.AllocationID, leaseRevokedCode)
			return
		}
		a.recordHeartbeatFailure(ms.AllocationID, now, hbErr)
		return
	}

	var recovered *MountState
	a.store.UpdateMount(ms.AllocationID, func(m *MountState, r *mountRuntime) {
		if result.LeaseTTLSeconds > 0 {
			r.LeaseTTLSeconds = result.LeaseTTLSeconds
		}
		r.HeartbeatFailCount = 0
		r.HeartbeatFailSince = 0
		r.NextHeartbeatAt = now.Add(r.heartbeatInterval()).UnixMilli()
		m.LastHeartbeatAt = now.UnixMilli()
		m.LastError = ""
		m.LastErrorDetail = ""
		// 心跳恢复：此前因心跳失败被置为 error 的挂载**复位为 mounted**。
		//
		// 磁盘从未被卸载（心跳失败不影响挂载），所以这里只是把界面状态纠正回来；
		// 没有这一步，"心跳连续失败 > TTL"就是一条单向门：状态永远停在 error，
		// 而 sweepHeartbeats 又只扫 mounted，于是永远不再发心跳、永远无法自愈。
		if m.State == MountStateError && r.HeartbeatError {
			m.State = MountStateMounted
			m.Phase = ""
			r.HeartbeatError = false
			cp := *m
			recovered = &cp
		}
	})
	if recovered != nil {
		a.logger.Info("心跳已恢复，挂载状态复位为已挂载", "allocation_id", ms.AllocationID)
		a.publishMount(recovered)
	}
	a.setServerConnected(true, "")
	a.hub.Publish(Event{Type: "heartbeat", Data: map[string]any{
		"at":            now.UnixMilli(),
		"allocation_id": ms.AllocationID,
	}})
}

// recordHeartbeatFailure 记录心跳失败，按指数退避重试；连续失败超过租约 TTL 时置为 error。
//
// 刻意**不**把单次心跳失败写进挂载记录的 last_error（真实反馈："挂载成功时就不要展示最后错误了"）：
// 挂载依然好好地挂在机器上，只是"和说话的人断了线"，往挂载上贴一个红色感叹号纯属误导
// ——服务端连通性由代理的 server.last_error 与界面横幅统一反映。只有真正降级
// （连续失败超过 TTL，本地判定 error）时才写 last_error，那次会同时把状态推给界面。
func (a *Agent) recordHeartbeatFailure(allocationID string, now time.Time, cause error) {
	var errorState *MountState

	a.store.UpdateMount(allocationID, func(m *MountState, r *mountRuntime) {
		r.HeartbeatFailCount++
		if r.HeartbeatFailSince == 0 {
			r.HeartbeatFailSince = now.UnixMilli()
		}
		backoff := time.Duration(r.HeartbeatFailCount) * r.heartbeatInterval()
		if backoff > maxHeartbeatBackoff {
			backoff = maxHeartbeatBackoff
		}
		r.NextHeartbeatAt = now.Add(backoff).UnixMilli()

		// 连续失败超过租约 TTL：本地判定为 error 并通过 SSE 通知（见任务规格第 4 条）。
		if now.UnixMilli()-r.HeartbeatFailSince > r.leaseTTL().Milliseconds() {
			m.State = MountStateError
			m.LastError = describeError("heartbeat", cause)
			m.LastErrorDetail = mountErrorDetailOf(cause)
			// 标记这条 error 的来由：磁盘还挂着，心跳恢复后要自动复位为 mounted
			// （见 heartbeatOne 与 sweepHeartbeats 的 heartbeatEligible）。
			r.HeartbeatError = true
			cp := *m
			errorState = &cp
		}
	})

	a.setServerConnected(false, describeError("heartbeat", cause))
	a.logger.Warn("租约心跳失败", "allocation_id", allocationID, "error", cause)

	if errorState != nil {
		a.publishMount(errorState)
		a.logger.Error("租约心跳连续失败超过 TTL，本地状态置为 error", "allocation_id", allocationID)
	}
}

// handleRevoke 处理服务端踢下线：主动卸载并推送 revoked 事件。
func (a *Agent) handleRevoke(ctx context.Context, allocationID, reason string) {
	if _, _, ok := a.store.GetMount(allocationID); !ok {
		return
	}

	// 先标记为 revoked，便于界面在卸载完成前就能看到原因。
	var revoked MountState
	a.store.UpdateMount(allocationID, func(m *MountState, _ *mountRuntime) {
		m.State = MountStateRevoked
		m.LastError = reason
		cp := *m
		revoked = cp
	})
	a.publishMount(&revoked)

	if err := a.engine.unmount(ctx, allocationID, true); err != nil {
		a.logger.Error("响应踢下线时卸载失败", "allocation_id", allocationID, "error", err)
	}

	a.hub.Publish(Event{Type: "revoked", Data: map[string]any{
		"allocation_id": allocationID,
		"reason":        reason,
	}})
}
