package agent

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vault/internal/apperr"
)

// handleGetIdentity 返回本地身份元数据（**不含私钥**）。
//
// 带 server_url / instance_id 查询参数时返回**该服务端**的身份（没装就是 installed=false），
// 便于前端在多个服务端之间切换时判断"这个服务端能不能用证书免密登录"；
// 不带参数时返回"活动"身份（旧版语义）。
func (a *Agent) handleGetIdentity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	serverURL := strings.TrimSpace(query.Get("server_url"))
	instanceID := strings.TrimSpace(query.Get("instance_id"))
	if serverURL == "" && instanceID == "" {
		a.writeJSON(w, http.StatusOK, a.identity.Meta())
		return
	}
	a.writeJSON(w, http.StatusOK, a.identity.MetaFor(instanceID, serverURL))
}

// identityListResponse 是本机全部客户端身份的元数据（按服务端区分）。
type identityListResponse struct {
	// Active 是"未指定服务端"时使用的身份键（见 serverKeyOf）；无身份时为空。
	Active string `json:"active,omitempty"`
	// Identities 各服务端的身份元数据（**不含私钥**）。
	Identities []identityMeta `json:"identities"`
}

// handleListIdentities 列出本机保存的全部客户端身份（GET /agent/identities）。
//
// 多服务端下用户需要看到"哪几个服务端装了证书"并单独撤销某一个，
// 因此这里返回全部条目而不是当前那一条。
func (a *Agent) handleListIdentities(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, identityListResponse{
		Active:     a.identity.ActiveKey(),
		Identities: a.identity.Metas(),
	})
}

// identityInstallRequest 是安装本地身份的请求。
//
// token 是当前会话的 Bearer；cert_sha256 是服务端证书指纹（用于固定校验）；
// user_id/username 由前端传入，用于 CSR 主体与落盘元数据。
type identityInstallRequest struct {
	ServerURL  string `json:"server_url"`
	Token      string `json:"token"`
	UserID     string `json:"user_id"`
	Username   string `json:"username"`
	CertSHA256 string `json:"cert_sha256"`
}

// clientCertHTTPResponse 是服务端 POST /v1/auth/client-certificate 的响应（不含私钥）。
type clientCertHTTPResponse struct {
	CertPEM           string `json:"cert_pem"`
	CAPEM             string `json:"ca_pem"`
	Serial            string `json:"serial"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	SPKISHA256        string `json:"spki_sha256"`
	NotBefore         int64  `json:"not_before"`
	NotAfter          int64  `json:"not_after"`
}

// handleInstallIdentity 本地生成密钥对与 CSR，用当前会话向服务端换取证书并落盘。
func (a *Agent) handleInstallIdentity(w http.ResponseWriter, r *http.Request) {
	var in identityInstallRequest
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, err)
		return
	}
	in.ServerURL = strings.TrimSpace(in.ServerURL)
	in.Token = strings.TrimSpace(in.Token)
	in.UserID = strings.TrimSpace(in.UserID)
	in.Username = strings.TrimSpace(in.Username)
	switch {
	case in.ServerURL == "":
		a.writeError(w, apperr.InvalidParam("server_url"))
		return
	case in.Token == "":
		a.writeError(w, apperr.InvalidParam("token"))
		return
	case in.UserID == "":
		a.writeError(w, apperr.InvalidParam("user_id"))
		return
	case in.Username == "":
		a.writeError(w, apperr.InvalidParam("username"))
		return
	}

	// 私钥在本地生成，绝不外传；只把 CSR 送给服务端。
	keyPEM, csrPEM, err := generateClientKeyAndCSR(in.Username)
	if err != nil {
		a.writeError(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, serverRequestTimeout)
	defer cancel()

	client, err := newServerClient(in.ServerURL, in.Token, in.CertSHA256, a.logger)
	if err != nil {
		a.writeError(w, err)
		return
	}

	var issued clientCertHTTPResponse
	if err := client.do(ctx, http.MethodPost, "/v1/auth/client-certificate",
		map[string]any{"csr": csrPEM}, &issued); err != nil {
		// 服务端结构化错误（如 auth.forbidden / system.invalid_param）原样透出，便于前端展示。
		a.writeError(w, err)
		return
	}
	if strings.TrimSpace(issued.CertPEM) == "" {
		a.writeError(w, errBadResponse(nil))
		return
	}

	// 尽力补全服务端实例 ID（失败不影响安装；登录后仍会刷新服务端信息）。
	instanceID := ""
	if info, infoErr := client.SystemInfo(ctx); infoErr == nil {
		instanceID = info.ServerInstanceID
	}

	id := &Identity{
		ServerURL:         in.ServerURL,
		ServerInstanceID:  instanceID,
		ServerCertSHA256:  strings.ToLower(strings.TrimSpace(in.CertSHA256)),
		UserID:            in.UserID,
		Username:          in.Username,
		CertPEM:           issued.CertPEM,
		KeyPEM:            keyPEM,
		CAPEM:             issued.CAPEM,
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore,
		NotAfter:          issued.NotAfter,
		InstalledAt:       time.Now().UnixMilli(),
	}
	if err := a.identity.Install(id); err != nil {
		a.writeError(w, err)
		return
	}
	// 只记录证书指纹，绝不记录私钥。
	a.logger.Info("本地客户端身份已安装",
		"server_url", id.ServerURL, "username", id.Username,
		"cert_serial", id.Serial, "cert_fingerprint", id.FingerprintSHA256)
	a.writeJSON(w, http.StatusOK, id.meta())
}

// handleDeleteIdentity 删除本地身份（幂等）。
//
// 不带参数时删除"活动"身份（旧版语义）；带 server_url / instance_id 时只删该服务端的身份；
// all=true 删除全部。**只删指定服务端**是多服务端下的关键语义：撤销 A 的证书不能顺手把 B 的也删掉。
func (a *Agent) handleDeleteIdentity(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if strings.EqualFold(strings.TrimSpace(query.Get("all")), "true") {
		if err := a.identity.RemoveAll(); err != nil {
			a.writeError(w, err)
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if err := a.identity.Remove(query.Get("instance_id"), query.Get("server_url")); err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// identityLoginRequest 是证书登录请求；server_url 与 instance_id 都可选。
//
// 两者都为空时用"活动"身份；给了任意一个就按该服务端挑证书（多服务端下必须给，
// 否则可能拿另一个服务端的证书去登录）。instance_id 比地址更准：同一实例换地址后依然能命中。
type identityLoginRequest struct {
	ServerURL  string `json:"server_url"`
	InstanceID string `json:"instance_id"`
}

// certLoginHTTPResponse 是服务端 POST /v1/auth/cert-login 的响应（与口令登录结构一致）。
//
// 注意：Role 必须原样透出，否则证书登录拿到的会话在界面上会被当作普通用户看待
// （超级管理员会丢掉管理端入口）。
type certLoginHTTPResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
}

// handleIdentityLogin 用本地证书发起 mTLS 换取服务端会话，并写回本地状态。
//
// 该 handler 与后台自动续期循环（见 session_renew.go）共用 certLogin，
// 以确保"用本地证书换会话"的逻辑只有一份。
func (a *Agent) handleIdentityLogin(w http.ResponseWriter, r *http.Request) {
	var in identityLoginRequest
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, err)
		return
	}

	ctx, cancel := contextWithTimeout(r, serverRequestTimeout)
	defer cancel()

	session, out, err := a.certLogin(ctx, in.ServerURL, in.InstanceID)
	if err != nil {
		a.writeError(w, err)
		return
	}

	// 与 POST /agent/session 一致地写回本地状态，供心跳/事件订阅等后续能力复用。
	// key 留空：按会话自带的实例 ID/地址推导归属于哪一台。
	a.setSession("", session, false)
	a.logger.Info("已用本地客户端证书免密登录", "server_url", session.ServerURL, "username", session.Username)
	// 原样透出服务端响应。
	a.writeJSON(w, http.StatusOK, out)
}

// certLogin 用本地客户端证书向服务端换取新会话（mTLS），返回可直接写回本地状态的 Session。
//
// serverURL / instanceID 为空时用"活动"身份；给了任意一个就挑该服务端的证书
// （见 resolveIdentity）——多服务端下这一步是"用对了证书"的前提。
//
// 供 handleIdentityLogin（用户点击/自动免密登录）、自动重连与后台自动续期循环共用，
// 避免复制粘贴。失败返回服务端结构化错误（如 auth.cert_invalid / auth.forbidden /
// agent.server_unreachable）。
func (a *Agent) certLogin(ctx context.Context, serverURL, instanceID string) (*Session, *certLoginHTTPResponse, error) {
	id, ok := a.resolveIdentity(instanceID, serverURL)
	if !ok {
		a.logger.Warn("本机没有该服务端的客户端证书身份，无法免密登录",
			"server_url", strings.TrimSpace(serverURL), "server_instance_id", strings.TrimSpace(instanceID))
		return nil, nil, errIdentityNotInstalled()
	}
	url := strings.TrimSpace(serverURL)
	if url == "" {
		url = id.ServerURL
	}
	if url == "" {
		return nil, nil, apperr.InvalidParam("server_url")
	}

	// 服务端证书信任：优先身份文件记录的指纹，其次退回已存会话（同一服务端）的指纹。
	pin := a.pinForIdentity(id)
	if pin == "" {
		return nil, nil, errServerCertUnknown()
	}

	client, err := newMTLSClient(url, id.CertPEM, id.KeyPEM, pin, a.logger)
	if err != nil {
		return nil, nil, err
	}

	var out certLoginHTTPResponse
	if err := client.do(ctx, http.MethodPost, "/v1/auth/cert-login", nil, &out); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(out.Token) == "" {
		return nil, nil, errBadResponse(nil)
	}

	// 实例 ID 以身份记录的为准（安装时落盘），缺失时退回调用方给的（例如前端已知的实例 ID）。
	serverInstanceID := strings.TrimSpace(id.ServerInstanceID)
	if serverInstanceID == "" {
		serverInstanceID = strings.TrimSpace(instanceID)
	}

	return &Session{
		ServerURL:        client.base,
		ServerInstanceID: serverInstanceID,
		Token:            out.Token,
		CertSHA256:       pin,
		UserID:           out.User.ID,
		Username:         out.User.Username,
		ExpiresAt:        out.ExpiresAt,
	}, &out, nil
}

// resolveIdentity 为"要连某个服务端"挑选本地身份。
//
// 先按服务端严格匹配（实例 ID 优先，其次规范化地址，见 identityStore.GetForServer）；
// 作为兼容，本机**只有一份**身份时也接受它 —— 老版本客户端只能装一份证书，用户此时
// 传给我们的地址/实例 ID 可能与安装时不同（改过地址、实例 ID 是后来才补上的），
// 直接判定"未安装"会让升级后的老用户突然失去免密登录能力。
func (a *Agent) resolveIdentity(instanceID, serverURL string) (Identity, bool) {
	if strings.TrimSpace(instanceID) == "" && strings.TrimSpace(serverURL) == "" {
		return a.identity.Get()
	}
	if id, ok := a.identity.GetForServer(instanceID, serverURL); ok {
		return id, true
	}
	if a.identity.Len() == 1 {
		return a.identity.Get()
	}
	return Identity{}, false
}

// pinForIdentity 选择用于固定服务端证书的指纹：
// 身份文件记录的 server_cert_sha256 优先；否则退回**同一服务端**已存会话的指纹。
//
// 多服务端下这里必须核对"是不是同一个服务端"：否则会把 A 服务端的指纹拿去固定
// B 服务端的证书，握手阶段就被拒（表现为 TLS handshake error 反复刷屏）。
// 因此逐台会话比对，取属于该服务端的那一份。
func (a *Agent) pinForIdentity(id Identity) string {
	if p := strings.ToLower(strings.TrimSpace(id.ServerCertSHA256)); p != "" {
		return p
	}
	for _, sess := range a.store.Sessions() {
		if sameServer(id, sess.ServerInstanceID, sess.ServerURL) {
			return strings.ToLower(strings.TrimSpace(sess.CertSHA256))
		}
	}
	return ""
}

// sameServer 判断身份与给定服务端（实例 ID + 地址）是否指向同一个服务端实例。
//
// 两边都有实例 ID 时以实例 ID 为准（服务端重装后实例 ID 会变，此时旧证书/旧指纹不应再沿用）；
// 否则退回规范化地址比较。
func sameServer(id Identity, instanceID, serverURL string) bool {
	idInstance := strings.ToLower(strings.TrimSpace(id.ServerInstanceID))
	other := strings.ToLower(strings.TrimSpace(instanceID))
	if idInstance != "" && other != "" {
		return idInstance == other
	}
	url := normalizeServerURL(serverURL)
	return url != "" && normalizeServerURL(id.ServerURL) == url
}
