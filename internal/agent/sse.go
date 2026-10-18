package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
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

// 事件流重连与日志策略（见 runServerEvents）。
const (
	// serverEventsBackoffMax 重连退避上限。
	serverEventsBackoffMax = 30 * time.Second

	// serverEventsFailLogLimit 一轮中断里最多记几条 WARN，之后静默重试。
	//
	// 服务端连不上时重连一直不成功，同一句话每 serverEventsBackoffMax 刷一条会把日志刷满
	// （真实反馈：一台服务端下线一天就 900+ 条同一句话）。重试本身**不停**——服务端回来
	// 仍能在 30s 内自动恢复，只是后续尝试降到 debug 级别，打开 log.level=debug 仍看得到现场。
	serverEventsFailLogLimit = 3

	// serverEventsStableWindow 一次订阅活过这么久才算"真的连上过"。
	//
	// 用来区分两种中断：稳定运行后断掉 = 新一轮故障（计数清零，重新开始报日志）；
	// 刚连上就被掐断 = 仍处在"连不上"里（继续累计，很快转入静默）。
	serverEventsStableWindow = 30 * time.Second
)

// streamEvents 订阅服务端 GET /v1/system/events，逐条回调事件。
//
// 返回语义：
//   - errServerEventsUnsupported：服务端未实现该通道（调用方应降级为仅心跳）；
//   - nil：连接被服务端正常关闭（调用方可短暂等待后重连）；
//   - 其他错误：网络/认证错误（调用方可退避重试）。
//
// onOpen 在事件通道**真正建立**（HTTP 200、开始读流）时回调一次，可为 nil；调用方据此确认
// "服务端可达且会话有效"。
func (c *serverClient) streamEvents(
	ctx context.Context,
	handle func(eventType string, data json.RawMessage),
	onOpen func(),
) error {
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

	// 通道建立即回调：这是"服务端可达且会话有效"的直接证据。
	if onOpen != nil {
		onOpen()
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

// runServerEvents 持续消费**某台**服务端的事件流，直到该台不再支持或上下文结束。
//
// 多服务端下每台各有一条订阅协程（见 ensureEventSubscriber）：一台断了重连不影响其它台。
//
// 日志策略：一轮中断只报前 serverEventsFailLogLimit 次，之后**静默重试**；恢复时补一条
// INFO，排障时据此看出"断了多久、重试了多少次"。
func (a *Agent) runServerEvents(ctx context.Context, key string) {
	backoff := time.Second
	// failStreak 数连续失败次数；outageWarned 记本轮中断是否已经报过（含"已恢复"）。
	// 两者一起把日志限制在"一轮中断的前几次"，见上面的日志策略。
	failStreak := 0
	outageWarned := false
	for {
		if ctx.Err() != nil {
			return
		}
		// 该台会话已被清掉（退出登录/移除服务端）：结束订阅，不必空转重连。
		if _, ok := a.store.Session(key); !ok {
			return
		}
		client, err := a.serverClientFor(key)
		if err != nil {
			// 无会话：等待前端推送会话（onSessionEstablished 会重新拉起订阅）。
			return
		}

		// 事件流就绪即确认连接：SystemInfo 可能失败（10s 超时），而心跳只在**有挂载**时才发
		// （见 lease.go 的 sweepHeartbeats），两条路都不通时界面会一直卡在"未连接"，
		// 即使服务端其实完全可达。
		opened := false
		var openedAt time.Time
		err = client.streamEvents(ctx, func(eventType string, data json.RawMessage) {
			a.handleServerEvent(key, eventType, data)
		}, func() {
			opened = true
			openedAt = time.Now()
			a.setServerConnected(key, true, "")
			// 中断过之后又能连上：报一条"已恢复"，本轮中断到此了结。
			if outageWarned {
				a.logger.Info("服务端事件流已恢复", "server", key, "failed_attempts", failStreak)
				outageWarned = false
			}
		})

		if opened && time.Since(openedAt) >= serverEventsStableWindow {
			// 稳定连上过：退避与失败计数复位，下一次中断按"新一轮故障"从头报。
			backoff = time.Second
			failStreak = 0
			outageWarned = false
		}

		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errServerEventsUnsupported):
			a.setEventsSupported(key, false)
			a.logger.Warn("服务端未提供事件通道，降级为仅心跳模式（靠心跳错误 lease.revoked 触发卸载）",
				"server", key)
			return
		case err == nil:
			a.setServerConnected(key, false, "events_closed")
		default:
			a.setServerConnected(key, false, describeError("events", err))
			failStreak++
			// 前几次报 WARN，之后静默重试：同一句话反复刷屏没有信息量（见 serverEventsFailLogLimit）。
			if failStreak <= serverEventsFailLogLimit {
				outageWarned = true
				a.logger.Warn("服务端事件流中断，稍后重试",
					"server", key, "error", err, "attempt", failStreak)
			} else {
				a.logger.Debug("服务端事件流仍未恢复，继续静默重试",
					"server", key, "error", err, "attempt", failStreak)
			}
			// 服务端重启后会话表被清空，事件流会一直 401；这里主动用本地证书重登录，
			// 下次循环拿到新客户端即可恢复订阅（refreshSessionTokenFor 自带节流与单飞）。
			if isSessionExpiredError(err) {
				retryCtx, retryCancel := context.WithTimeout(ctx, serverRequestTimeout)
				_, rErr := a.refreshSessionTokenFor(retryCtx, key)
				retryCancel()
				// 撞上节流闸门是预期结果（说明刚重登录过），不是新故障，不记日志。
				if rErr != nil && !isAuthRetryThrottled(rErr) && failStreak <= serverEventsFailLogLimit {
					a.logger.Warn("事件流因会话失效中断，用本地证书重新登录失败",
						"server", key, "error", rErr)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < serverEventsBackoffMax {
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

// serverJobEventType 是服务端**任务事件**的 SSE 事件名（即 topic）。
//
// 与 cmd/vault-server/main.go 里 jobs.OnProgress / jobs.OnFinished 用的 topic 同名（"job"），
// 两者形状一致（job_id/type/ref_id/state/progress）；同样是跨进程的线上契约，改要两端一起改。
const serverJobEventType = "job"

// jobEventBuffer 是单个订阅者的进度事件缓冲：装得下"突发几条"就够 —— 满了宁可丢，
// 也不能阻塞事件流读取（见 jobWatch 的注释）。
const jobEventBuffer = 8

// localJobEventType 是推给**渲染层**的任务事件名（本地事件流 GET /agent/events 的 topic）。
//
// 与服务端的 serverJobEventType 同名（都是 "job"）但用途不同：那条是代理↔服务端的线上契约，
// 这条是代理↔界面的本地契约。刻意同名 + 同形状，是为了让"同一件事"在两侧长得一样，
// 边界（主进程转发、渲染层解析）不需要再翻译一次。
const localJobEventType = "job"

// jobEvent 是任务事件在两个方向上的共同形状（服务端推来的 data，也就是转发给界面的 data）。
//
// 字段名沿用服务端那一版（job_id 而不是 serverJob 的 id）：这个形状是**线上契约**，
// 不是内部 DTO —— 由 job_event_test.go 钉住。
type jobEvent struct {
	JobID    string `json:"job_id"`
	Type     string `json:"type"`
	RefID    string `json:"ref_id"`
	State    string `json:"state"`
	Progress int    `json:"progress"`
}

// jobWatch 是"服务端建库任务进度"的订阅中心。
//
// 为什么需要它：建库（JobUploadComplete）动辄几分钟到几十分钟，而进度过去靠代理**每 2 秒**
// 轮询一次 GET /v1/jobs/{id} —— 服务端访问日志因此被同一件事的流水刷满（真实反馈："上传时
// 会因为客户端拉上传状态而刷屏服务日志"）。服务端本来就在推 job 事件，代理接住即可：进度
// 有变化才推送，没变化一条请求都不发。
//
// 投递是**尽力而为**的（订阅通道满了就丢掉这一条）：进度是幂等快照，丢一条只会晚一次刷新，
// 而阻塞在这里会拖住整条事件流读取协程 —— 那代价大得多。因此订阅方必须自带兜底查询
// （见 waitUploadJob），丢事件不会让等待方卡死。
//
// 所有方法都对 nil 接收者安全：零值 Agent（测试里常见）不会因此 panic，只是退化为纯兜底轮询。
type jobWatch struct {
	mu   sync.Mutex
	subs map[string]map[int]chan serverJob
	next int
}

// newJobWatch 构造订阅中心。
func newJobWatch() *jobWatch {
	return &jobWatch{subs: make(map[string]map[int]chan serverJob)}
}

// subscribe 订阅某个任务的事件，返回订阅 ID（交给 unsubscribe）与接收通道。
func (w *jobWatch) subscribe(jobID string) (int, <-chan serverJob) {
	if w == nil || strings.TrimSpace(jobID) == "" {
		return 0, nil
	}
	ch := make(chan serverJob, jobEventBuffer)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subs == nil {
		w.subs = make(map[string]map[int]chan serverJob)
	}
	w.next++
	id := w.next
	if w.subs[jobID] == nil {
		w.subs[jobID] = make(map[int]chan serverJob)
	}
	w.subs[jobID][id] = ch
	return id, ch
}

// unsubscribe 退订（幂等）。
func (w *jobWatch) unsubscribe(jobID string, id int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	subs := w.subs[jobID]
	if subs == nil {
		return
	}
	delete(subs, id)
	if len(subs) == 0 {
		delete(w.subs, jobID)
	}
}

// publish 把一条任务事件投给该任务的订阅者（尽力而为，见类型注释）。
func (w *jobWatch) publish(j serverJob) {
	if w == nil || strings.TrimSpace(j.ID) == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs[j.ID] {
		select {
		case ch <- j:
		default:
		}
	}
}

// handleServerEvent 处理某台服务端发来的单条事件。
//
// key 是事件来源服务端：分配 ID 全局唯一（UUID），因此这里不需要按来源过滤，只用它做日志与事件
// 归属标注（界面据此把事件落到对应那台）。
func (a *Agent) handleServerEvent(key, eventType string, data json.RawMessage) {
	switch eventType {
	case "revoke", "lease.revoked", "lease_revoked", "lease":
		// 服务端"踢下线"可能以两种形态到达：
		//   - 独立 topic：revoke / lease.revoked / lease_revoked；
		//   - 通用 lease 事件里带 action=revoke（管理员强制下线、维护清理差异盘都走这条）。
		// 旧代理只认前一种，结果"管理员点了强制下线、客户端却要等心跳超时"——
		// 维护清理差异盘更等不起：客户端不先断开，盘文件会因占用删不掉。
		var payload struct {
			Action       string `json:"action"`
			AllocationID string `json:"allocation_id"`
			LeaseID      string `json:"lease_id"`
			Reason       string `json:"reason"`
		}
		_ = json.Unmarshal(data, &payload)
		if eventType == "lease" && strings.TrimSpace(payload.Action) != "revoke" {
			return
		}

		allocationID := strings.TrimSpace(payload.AllocationID)
		if allocationID == "" && payload.LeaseID != "" {
			if ms, ok := a.store.MountByLease(payload.LeaseID); ok {
				allocationID = ms.AllocationID
			}
		}
		if allocationID == "" {
			a.logger.Warn("收到踢下线事件但无法定位分配", "server", key, "lease_id", payload.LeaseID)
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
	case serverJobEventType:
		// 任务的进度/终态（见 job.Worker.OnProgress / OnFinished）走两条路：
		//   ① 交给正在等这个任务的订阅者（waitUploadJob，上传建库用）；
		//   ② 原样广播给渲染层（本地事件流 event: job），替代界面过去的 1.5s 轮询。
		//
		// 这是"上传/装依赖时不再刷屏服务端日志"的关键一环：进度变化由服务端推来，代理与界面
		// 都不必再定时轮询 GET /v1/jobs/{id}。
		var payload jobEvent
		_ = json.Unmarshal(data, &payload)
		jobID := strings.TrimSpace(payload.JobID)
		if jobID == "" {
			// 没有 job_id 就不知道是谁的任务，投给谁都是错的。
			return
		}
		payload.JobID = jobID
		payload.State = strings.TrimSpace(payload.State)
		a.jobWatch.publish(serverJob{
			ID:       payload.JobID,
			Type:     payload.Type,
			RefID:    payload.RefID,
			State:    payload.State,
			Progress: payload.Progress,
		})
		// 尽力而为：渲染层没订阅或缓冲满时丢弃（见 EventHub）。
		a.hub.Publish(Event{Type: localJobEventType, Data: payload})
	default:
		// 其余事件（租约变更等）对代理无动作需求：权威状态以心跳与显式查询为准。
		a.logger.Debug("忽略服务端事件", "type", eventType)
	}
}
