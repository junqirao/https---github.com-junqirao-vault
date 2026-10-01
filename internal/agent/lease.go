package agent

import (
	"context"
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
func (a *Agent) sweepHeartbeats(ctx context.Context) {
	now := time.Now()
	for _, ms := range a.store.ListMounts() {
		if ms.State != MountStateMounted {
			continue
		}
		_, rt, ok := a.store.GetMount(ms.AllocationID)
		if !ok {
			continue
		}
		if rt.NextHeartbeatAt > 0 && now.UnixMilli() < rt.NextHeartbeatAt {
			continue
		}
		a.heartbeatOne(ctx, ms, now)
	}
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
			// 服务端已撤销租约：按踢下线处理。
			a.logger.Warn("心跳返回租约已撤销，执行主动卸载", "allocation_id", ms.AllocationID)
			a.handleRevoke(ctx, ms.AllocationID, "lease_revoked")
			return
		}
		a.recordHeartbeatFailure(ms.AllocationID, now, hbErr)
		return
	}

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
	})
	a.setServerConnected(true, "")
	a.hub.Publish(Event{Type: "heartbeat", Data: map[string]any{
		"at":            now.UnixMilli(),
		"allocation_id": ms.AllocationID,
	}})
}

// recordHeartbeatFailure 记录心跳失败，按指数退避重试；连续失败超过租约 TTL 时置为 error。
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
		m.LastError = describeError("heartbeat", cause)
		m.LastErrorDetail = mountErrorDetailOf(cause)

		// 连续失败超过租约 TTL：本地判定为 error 并通过 SSE 通知（见任务规格第 4 条）。
		if now.UnixMilli()-r.HeartbeatFailSince > r.leaseTTL().Milliseconds() {
			m.State = MountStateError
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
