package agent

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	// serverConnectInitialDelay 是启动后第一次自动重连前的等待时间。
	//
	// 只留一个很短的窗口：客户端启动时会主动推会话（POST /agent/session），那是首选路径，
	// 让它先落地即可。此前这里是 20 秒，于是"客户端不会推会话"的启动路径（未勾"记住我"、
	// 或停在登录页且没触发免密登录）必须干等 20 秒才可能自愈，用户观感是
	// "重开客户端要半分钟才连上服务端"（真实反馈）。而"抢推会话"的顾虑本身由
	// needsServerReconnect 的会话判定兜住：有会话时这条循环根本不会出手。
	serverConnectInitialDelay = 2 * time.Second
	// serverConnectCheckInterval 是首次尝试失败后的复检间隔。
	serverConnectCheckInterval = 30 * time.Second
)

// runServerConnectLoop 在没有会话时用本地客户端证书身份自动重连服务端。
//
// 修复的真实问题：代理重启后客户端**不会**重新推会话（客户端只在自身启动/登录时推一次），
// 而 ensureEventSubscriber 遇到"无会话"是直接 return 的、没有任何重试入口 —— 代理于是永远
// 停在 Connected=false，界面只能显示"未连接"，用户读到的是"连不上服务端"，且等多久都不会
// 自己好（真实反馈："前一次更改之后会一直处在未连接的状态，没法连接到服务端"）。
// 这条循环补上这个缺口：无会话时自己用证书换一个回来。
//
// 连续失败达 maxServerConnectAttempts 即停手（原因见 state.go 里该常量的说明），
// 之后只有界面手动重试（POST /agent/server/reconnect）才会再尝试。
func (a *Agent) runServerConnectLoop(ctx context.Context) {
	timer := time.NewTimer(serverConnectInitialDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	a.maybeAutoReconnectServer(ctx)

	ticker := time.NewTicker(serverConnectCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.maybeAutoReconnectServer(ctx)
		}
	}
}

// maybeAutoReconnectServer 逐台检查"确实需要、且还有机会"的服务端并各尝试一次自动重连。
//
// 多服务端：每台各自独立计数与退避（见 needsServerReconnect），一台失败不影响另一台。
func (a *Agent) maybeAutoReconnectServer(ctx context.Context) {
	for _, key := range a.store.ServerKeys() {
		if ctx.Err() != nil {
			return
		}
		if !a.needsServerReconnect(key) {
			continue
		}
		a.reconnectServerIfNeeded(ctx, key)
	}
}

// reconnectServerIfNeeded 对**某台**尝试一次自动重连（含失败计数与事件广播）。
func (a *Agent) reconnectServerIfNeeded(ctx context.Context, key string) {
	callCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()

	before := a.serverStateBefore(key)

	// 先亮出"连接中"：界面据此显示转圈的连接中标签，而不是"未连接"（真实诉求）。
	// 这里用会广播的 setServerPhase：一次证书登录最长要等 sessionProbeTimeout，
	// 界面必须在这之前就看到"连接中"，而不是等失败结果回来。
	a.setServerPhase(key, ServerPhaseConnecting)

	if err := a.reconnectServer(callCtx, key); err != nil {
		exhausted := a.store.RecordServerConnectFailure(key, describeError("server_reconnect", err))
		a.publishServerIfChanged(key, before)
		state, _ := a.store.Server(key)
		a.logger.Warn("自动重连服务端失败",
			"server", key, "error", err, "fail_count", state.FailCount, "give_up", exhausted)
		if exhausted {
			a.logger.Warn("自动重连已放弃，等待界面手动重试",
				"server", key, "attempts", maxServerConnectAttempts)
		}
		return
	}
	a.publishServerIfChanged(key, before)
	a.logger.Info("已用本地客户端证书自动重连服务端", "server", key, "server_url", a.serverDisplayName(key))
}

// needsServerReconnect 判断**某台**是否该由自动重连循环出手：
// 无会话、当前未连接、失败次数没用完，且本机装好了这个服务端的客户端证书身份。
func (a *Agent) needsServerReconnect(key string) bool {
	if _, ok := a.store.Session(key); ok {
		// 有会话：连接状态归事件订阅与心跳负责，不在这里抢活（抢了会重复登录）。
		return false
	}
	server, ok := a.store.Server(key)
	if !ok || server.Connected || server.FailCount >= maxServerConnectAttempts {
		return false
	}
	// 按服务端挑身份：多服务端下别的服务端的证书不能用来连它。
	id, ok := a.resolveIdentity(server.InstanceID, server.URL)
	if !ok {
		// 还没装这个服务端的身份（用户从未在此服务端免密登录过）：重试没有意义。
		return false
	}
	// 连目标地址都没有（服务端状态为空且身份里也没记地址）：重试同样没有意义。
	return strings.TrimSpace(server.URL) != "" || strings.TrimSpace(id.ServerURL) != ""
}

// reconnectServer 用本地证书身份为**某台**换一个新会话并写回本地状态（语义等价于客户端推会话）。
func (a *Agent) reconnectServer(ctx context.Context, key string) error {
	server, ok := a.store.Server(key)
	if !ok {
		return errServerUnknown()
	}
	session, _, err := a.certLogin(ctx, server.URL, server.InstanceID)
	if err != nil {
		return err
	}
	// 与 POST /agent/session 完全一致：setSession 会顺带刷新服务端信息、拉起事件订阅、
	// 恢复自动挂载（见 onSessionEstablished），因此连接状态与界面都会跟着回到"已连接"。
	a.setSession(key, session, false)
	a.logger.Info("未收到会话且连接已断开，已用本地客户端证书自动登录",
		"server", key, "server_url", session.ServerURL, "username", session.Username)
	return nil
}

// handleServerReconnect 处理 POST /agent/server/reconnect —— 界面上的"重试"按钮。
//
// 语义：清零失败计数 → 立即尝试一次（不等下一个 tick）。成功即保持 0，自动重连循环因此
// 重新获得 maxServerConnectAttempts 次机会（对应"手动重试会刷新计数"）。
//
// 多服务端：要重试**哪一台**由 server_key 指定（请求体或查询参数均可）；省略时按主服务端
// （老客户端不带服务端标识）。给了却匹配不上任何已登记服务端则返回 agent.server_unknown。
func (a *Agent) handleServerReconnect(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ServerKey string `json:"server_key"`
	}
	// 请求体可空：老客户端不带任何字段，空体视为零值结构。
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, err)
		return
	}
	key := a.store.ResolveKey(in.ServerKey, r.URL.Query().Get("server_key"))
	if key == "" {
		a.writeError(w, errServerUnknown())
		return
	}
	server, ok := a.store.Server(key)
	if !ok {
		a.writeError(w, errServerUnknown())
		return
	}
	ctx, cancel := contextWithTimeout(r, sessionProbeTimeout)
	defer cancel()

	if server.Connected {
		// 已经连上了：原样返回成功，别把好状态改坏（界面可能连点了两次）。
		a.writeJSON(w, http.StatusOK, server)
		return
	}
	if _, ok := a.resolveIdentity(server.InstanceID, server.URL); !ok {
		a.writeError(w, errIdentityNotInstalled())
		return
	}

	// before 必须取在清零之前：清零本身（disconnected → connecting）也要推给界面，
	// 否则标签会一直停在"未连接"，用户以为点了没反应。
	before := a.serverStateBefore(key)
	a.store.ResetServerConnectFailures(key)
	a.publishServerIfChanged(key, before)

	if err := a.reconnectServer(ctx, key); err != nil {
		a.store.RecordServerConnectFailure(key, describeError("server_reconnect", err))
		a.publishServerIfChanged(key, before)
		a.logger.Warn("手动重连服务端失败", "server", key, "error", err)
		a.writeError(w, err)
		return
	}
	// 连接成功：setSession → onSessionEstablished → setServerConnected(true) 已负责广播，
	// 这里再比一次是幂等的（值未变不会重复发）。
	a.publishServerIfChanged(key, before)
	result, _ := a.store.Server(key)
	a.writeJSON(w, http.StatusOK, result)
}
