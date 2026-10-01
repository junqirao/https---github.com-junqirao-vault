package agent

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vault/internal/apperr"
)

// 客户端证书会话自动续期的时间参数。
const (
	// sessionRenewCheckInterval 后台检查间隔。
	sessionRenewCheckInterval = 60 * time.Second
	// sessionRenewMinInterval 两次续期尝试之间的最小间隔（避免短 TTL 下反复续期）。
	sessionRenewMinInterval = 5 * time.Minute
	// sessionRenewWindowFloor 续期窗口下限：剩余时间小于它即进入续期。
	sessionRenewWindowFloor = 5 * time.Minute
	// sessionRenewWindowDivisor 续期窗口系数：窗口 = max(下限, 观测寿命 / 4)，即寿命的 25%。
	sessionRenewWindowDivisor = 4
)

// 401 触发的"证书重登录"参数（与上面的定时续期相互独立）。
const (
	// authRetryMinInterval 两次"401 触发的证书重登录"之间的最小间隔。
	//
	// 服务端重启后令牌立即失效，心跳/事件流/挂载等多个调用会**同时**撞上 401；
	// 没有这道闸门就会并发重登录 N 次。
	authRetryMinInterval = 10 * time.Second
)

// authRetryTooSoon 表示刚重登录过，本次跳过（调用方沿用原错误）。
func authRetryTooSoon() error {
	return apperr.New("agent.auth_retry_throttled", http.StatusTooManyRequests)
}

// errCertRenewBlocked 表示证书已被判定为不可用（吊销/过期/未登记），不再自动重登录。
func errCertRenewBlocked() error {
	return apperr.New("agent.cert_unusable", http.StatusUnauthorized)
}

// refreshSessionToken 用本地客户端证书重新登录一次，返回新令牌。
//
// 触发场景：服务端重启清空了内存会话表，此前签发的 Bearer 令牌全部失效（401）。
// 此时证书登录的客户端应**自愈**——用本地证书重新换一个会话，而不是把用户踢回登录页。
//
// 并发与节流：
//   - 单飞：并发调用共享同一次重登录（只发一个 cert-login 请求）；
//   - 节流：两次重登录至少间隔 authRetryMinInterval，服务端持续 401 时不会被打爆；
//   - 确定性错误（证书被吊销/过期等）走 handleRenewFailure 停止后续自动续期。
//
// 前置条件：必须已安装本地证书身份；否则直接返回"未安装身份"（调用方回落为要求登录）。
func (a *Agent) refreshSessionToken(ctx context.Context) (string, error) {
	if _, ok := a.identity.Get(); !ok {
		return "", errIdentityNotInstalled()
	}

	a.renewMu.Lock()
	if a.renewBlocked {
		a.renewMu.Unlock()
		// 证书已被认定为不可用（吊销/过期/未登记）：重试无意义。
		return "", errCertRenewBlocked()
	}
	if call := a.authRetryInFlight; call != nil {
		a.renewMu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	now := time.Now().UnixMilli()
	if a.authRetryLastAt > 0 && now-a.authRetryLastAt < authRetryMinInterval.Milliseconds() {
		a.renewMu.Unlock()
		return "", authRetryTooSoon()
	}
	a.authRetryLastAt = now
	call := &authRetryCall{done: make(chan struct{})}
	a.authRetryInFlight = call
	a.renewMu.Unlock()

	session, _, err := a.certLogin(ctx, "")
	if err != nil {
		a.noteAuthRetryFailure(err)
		call.err = err
	} else {
		call.token = session.Token
		a.applyRefreshedSession(session)
	}

	a.renewMu.Lock()
	a.authRetryInFlight = nil
	a.renewMu.Unlock()
	close(call.done)
	return call.token, call.err
}

// authRetryCall 是一次"证书重登录"的单飞句柄。
type authRetryCall struct {
	done  chan struct{}
	token string
	err   error
}

// applyRefreshedSession 把重登录得到的会话写回本地状态并通知渲染进程。
//
// 刻意**不**调用 setSession：那条路径会触发刷新服务端信息、重拉事件订阅与恢复挂载，
// 而这里只是"换一个令牌"，环境都已就绪，重跑一遍只会带来无谓的抖动。
func (a *Agent) applyRefreshedSession(session *Session) {
	// certLogin 的响应不含服务端展示字段：沿用现有会话的名称/实例 ID，避免被清空。
	if prev, ok := a.store.Session(); ok {
		if session.ServerName == "" {
			session.ServerName = prev.ServerName
		}
		if session.ServerInstanceID == "" {
			session.ServerInstanceID = prev.ServerInstanceID
		}
		if session.CertSHA256 == "" {
			session.CertSHA256 = prev.CertSHA256
		}
	}
	a.store.SetSession(session)
	a.store.SetUser(UserState{ID: session.UserID, Username: session.Username})
	a.renewMu.Lock()
	a.renewBlocked = false
	a.renewMu.Unlock()
	// 渲染进程据此更新自己持有的令牌（本机 127.0.0.1 本地接口）。
	a.publishSessionEvent(session)
	// 日志只出现服务端地址与用户名，绝不出现令牌。
	a.logger.Info("服务端会话已失效（可能由服务端重启导致），已用本地证书自动重新登录",
		"server_url", session.ServerURL, "username", session.Username, "new_expires_at", session.ExpiresAt)
}

// noteAuthRetryFailure 记录一次"401 触发的证书重登录"失败。
func (a *Agent) noteAuthRetryFailure(err error) {
	if sess, ok := a.store.Session(); ok {
		a.handleRenewFailure(sess, err)
		return
	}
	a.logger.Warn("用本地证书重新登录失败", "error", err)
}

// runSessionRenewalLoop 后台定期检查并续期客户端证书会话，直到上下文结束。
//
// 为什么由代理（而非渲染进程）做续期：代理进程常驻、界面关掉也能续，
// 是"会话权威方"。仅在【已安装客户端证书身份】且【当前有会话】时才尝试。
func (a *Agent) runSessionRenewalLoop(ctx context.Context) {
	ticker := time.NewTicker(sessionRenewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.maybeRenewSession(ctx)
		}
	}
}

// maybeRenewSession 判断是否需要续期，需要则用本地证书换取新会话并写回本地状态。
//
// 续期时机：剩余时间 < max(5min, 观测到的会话寿命 * 25%)；两次尝试之间至少间隔 5 分钟。
// 失败绝不清除现有会话、绝不影响挂载：确定性错误停止自动续期并要求重新登录，其余退避重试。
func (a *Agent) maybeRenewSession(ctx context.Context) {
	if _, ok := a.identity.Get(); !ok {
		return // 未安装客户端证书身份：不自动续期
	}
	session, ok := a.store.Session()
	if !ok || strings.TrimSpace(session.ServerURL) == "" || session.ExpiresAt <= 0 {
		return
	}

	now := time.Now()
	a.renewMu.Lock()
	blocked := a.renewBlocked
	lastAt := a.renewLastAt
	a.renewMu.Unlock()
	if blocked {
		return
	}
	if lastAt > 0 && now.UnixMilli()-lastAt < sessionRenewMinInterval.Milliseconds() {
		return
	}

	// 观测到的会话寿命 = expires_at - 收到该会话的时刻。
	lifetime := session.ExpiresAt - session.receivedAt
	if lifetime <= 0 {
		return
	}
	window := lifetime / sessionRenewWindowDivisor
	if floor := sessionRenewWindowFloor.Milliseconds(); window < floor {
		window = floor
	}
	if session.ExpiresAt-now.UnixMilli() > window {
		return // 尚未进入续期窗口
	}

	// 记录本次尝试时刻：既是"两次续期至少间隔 5 分钟"的闸门，也避免并发重复续期。
	a.renewMu.Lock()
	a.renewLastAt = now.UnixMilli()
	a.renewMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()

	renewed, _, err := a.certLogin(callCtx, session.ServerURL)
	if err != nil {
		a.handleRenewFailure(session, err)
		return
	}
	// 续期响应不含服务端展示字段：沿用当前会话的名称/实例 ID，避免写回时被清空。
	if renewed.ServerName == "" {
		renewed.ServerName = session.ServerName
	}
	if renewed.ServerInstanceID == "" {
		renewed.ServerInstanceID = session.ServerInstanceID
	}

	// 与 POST /agent/session 一致地写回本地会话状态（token / expires_at / user 全部刷新）。
	a.setSession(renewed)
	a.logger.Info("客户端证书会话已自动续期",
		"server_url", renewed.ServerURL, "username", renewed.Username,
		"old_expires_at", session.ExpiresAt, "new_expires_at", renewed.ExpiresAt)
	// 通知渲染进程更新它持有的令牌（本机 127.0.0.1 本地接口，与 /agent/state 同信任级别）。
	a.publishSessionEvent(renewed)
}

// handleRenewFailure 分类处理续期失败。
//
//   - 确定性错误（证书被吊销/过期/未知、auth.forbidden 等）：停止自动续期并标记"需要重新登录"，
//     绝不静默无限重试；
//   - 其余（网络/服务端暂时不可用）：按既有最小间隔退避重试，不清会话、不影响挂载。
//
// 日志只出现 server_url / username / 错误码，绝不出现 token 原文。
func (a *Agent) handleRenewFailure(session *Session, err error) {
	if isDeterministicRenewalFailure(err) {
		a.renewMu.Lock()
		a.renewBlocked = true
		a.renewMu.Unlock()
		a.logger.Warn("客户端证书会话续期失败，已停止自动续期（需要重新登录）",
			"server_url", session.ServerURL, "username", session.Username, "error", err)
		return
	}
	a.logger.Warn("客户端证书会话续期失败，稍后重试",
		"server_url", session.ServerURL, "username", session.Username, "error", err)
}

// isDeterministicRenewalFailure 判断续期失败是否属"证书类确定性错误"（重试无意义）。
//
// 覆盖 auth.forbidden（cert_not_registered / cert_revoked / user_disabled ...）、
// auth.cert_* 系列与 auth.ca_changed；网络/服务端暂时不可用（agent.server_unreachable、5xx）不在此列。
func isDeterministicRenewalFailure(err error) bool {
	switch code := apperr.CodeOf(err); {
	case code == "auth.forbidden", code == "auth.ca_changed":
		return true
	case strings.HasPrefix(code, "auth.cert_"):
		return true
	}
	return false
}

// publishSessionEvent 广播 session 事件（续期成功后调用），携带最新会话。
func (a *Agent) publishSessionEvent(session *Session) {
	a.hub.Publish(Event{Type: "session", Data: sessionView(session)})
}

// sessionView 是 GET /agent/state 的 session 字段与 session 事件的负载。
//
// 这是本机 127.0.0.1 的本地接口（与 /agent/identity、/agent/state 同信任级别），
// 因此可以返回 token —— 渲染进程据此在续期后更新自己持有的令牌。
func sessionView(s *Session) map[string]any {
	if s == nil {
		return nil
	}
	return map[string]any{
		"server_url": s.ServerURL,
		"token":      s.Token,
		"expires_at": s.ExpiresAt,
		"user_id":    s.UserID,
		"username":   s.Username,
	}
}
