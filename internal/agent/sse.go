package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// errServerEventsUnsupported 表示服务端未提供事件流通道（/v1/system/events 未实现）。
//
// 此时代理**不得因此失败**，应降级为「仅心跳」：靠心跳返回 lease.revoked 触发卸载。
var errServerEventsUnsupported = errors.New("agent: 服务端未提供 SSE 事件通道")

// 事件流读取上限（单行/单帧）。
const (
	sseInitialBuffer = 64 << 10
	sseMaxBuffer     = 1 << 20
)

// streamEvents 订阅服务端 GET /v1/system/events，逐条回调事件。
//
// 返回语义：
//   - errServerEventsUnsupported：服务端未实现该通道（调用方应降级为仅心跳）；
//   - nil：连接被服务端正常关闭（调用方可短暂等待后重连）；
//   - 其他错误：网络/认证错误（调用方可退避重试）。
func (c *serverClient) streamEvents(ctx context.Context, handle func(eventType string, data json.RawMessage)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/system/events", nil)
	if err != nil {
		return errServerUnreachable(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.stream.Do(req)
	if err != nil {
		return errServerUnreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return errServerEventsUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxServerResponseBytes))
		return parseServerError(resp.StatusCode, data)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, sseInitialBuffer), sseMaxBuffer)

	var eventType string
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// 一帧结束
			if len(dataLines) > 0 {
				dispatchServerEvent(eventType, strings.Join(dataLines, "\n"), handle)
			}
			eventType = ""
			dataLines = nil
		case strings.HasPrefix(line, ":"):
			// 注释（keepalive），忽略
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errServerUnreachable(err)
	}
	return nil
}

// dispatchServerEvent 解析一帧事件并回调（同时兼容 data 内的 type 字段）。
func dispatchServerEvent(eventType, payload string, handle func(string, json.RawMessage)) {
	eventType = strings.TrimSpace(eventType)

	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		// 不是服务端 Event 结构：按 SSE 的 event 名直接回调原始负载。
		if eventType != "" {
			handle(eventType, json.RawMessage(payload))
		}
		return
	}
	if envelope.Type != "" {
		eventType = envelope.Type
	}
	if eventType == "" {
		eventType = "message"
	}
	handle(eventType, envelope.Data)
}

// runServerEvents 持续消费服务端事件流，直到服务端不再支持或上下文结束。
func (a *Agent) runServerEvents(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		client, err := a.serverClient()
		if err != nil {
			// 无会话：等待前端推送会话（onSessionEstablished 会重新拉起订阅）。
			return
		}

		err = client.streamEvents(ctx, a.handleServerEvent)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errServerEventsUnsupported):
			a.eventsUnsupported.Store(true)
			a.logger.Warn("服务端未提供事件通道，降级为仅心跳模式（靠心跳错误 lease.revoked 触发卸载）")
			return
		case err == nil:
			a.setServerConnected(false, "events_closed")
		default:
			a.setServerConnected(false, describeError("events", err))
			a.logger.Warn("服务端事件流中断，稍后重试", "error", err)
			// 服务端重启后会话表被清空，事件流会一直 401；这里主动用本地证书重登录，
			// 下次循环拿到新客户端即可恢复订阅（refreshSessionToken 自带节流与单飞）。
			if isSessionExpiredError(err) {
				retryCtx, retryCancel := context.WithTimeout(ctx, serverRequestTimeout)
				if _, rErr := a.refreshSessionToken(retryCtx); rErr != nil {
					a.logger.Warn("事件流因会话失效中断，用本地证书重新登录失败", "error", rErr)
				}
				retryCancel()
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// serverMountEventType 是服务端**挂载阶段事件**的 SSE 事件名（即 topic）。
//
// 与 internal/app 的 MountEventTopic 必须逐字一致：这是跨进程的线上契约。
// 两个包不共享常量（app 不依赖 agent，也不该被 agent 反向依赖），因此各留一份，
// 并由 mount_phase_test.go 的合约测试钉住。
const serverMountEventType = "mount"

// handleServerEvent 处理单条服务端事件。
func (a *Agent) handleServerEvent(eventType string, data json.RawMessage) {
	switch eventType {
	case "revoke", "lease.revoked", "lease_revoked":
		var payload struct {
			AllocationID string `json:"allocation_id"`
			LeaseID      string `json:"lease_id"`
			Reason       string `json:"reason"`
		}
		_ = json.Unmarshal(data, &payload)

		allocationID := strings.TrimSpace(payload.AllocationID)
		if allocationID == "" && payload.LeaseID != "" {
			if ms, ok := a.store.MountByLease(payload.LeaseID); ok {
				allocationID = ms.AllocationID
			}
		}
		if allocationID == "" {
			a.logger.Warn("收到踢下线事件但无法定位分配", "lease_id", payload.LeaseID)
			return
		}
		reason := strings.TrimSpace(payload.Reason)
		if reason == "" {
			reason = "server_revoked"
		}
		safeGo(a.logger, "handle_revoke", func() { a.handleRevoke(a.bgContext(), allocationID, reason) })
	case serverMountEventType:
		// 服务端在挂载过程中推送的**阶段事件**（见 internal/app 的 emitMountPhase）。
		//
		// 挂载在服务端是同步重活：代理这期间阻塞在 HTTP 响应上，只有这条 SSE 通道能把
		// "服务端现在做到哪一步"带回来（界面因此能看到"正在准备虚拟磁盘/正在创建 iSCSI 目标"）。
		var payload struct {
			Action       string `json:"action"`
			AllocationID string `json:"allocation_id"`
			Phase        string `json:"phase"`
		}
		_ = json.Unmarshal(data, &payload)
		allocationID := strings.TrimSpace(payload.AllocationID)
		phase := strings.TrimSpace(payload.Phase)
		if payload.Action != "progress" || allocationID == "" || phase == "" {
			return
		}
		// setPhase 只对"本机正处于 mounting"的分配生效：其它客户端的挂载、已经结束或
		// 从未开始的挂载都会被忽略，不会凭空造出一条状态。
		a.engine.setPhase(allocationID, phase)
	default:
		// 其余事件（任务进度、租约变更等）对代理无动作需求：权威状态以心跳与显式查询为准。
		a.logger.Debug("忽略服务端事件", "type", eventType)
	}
}
