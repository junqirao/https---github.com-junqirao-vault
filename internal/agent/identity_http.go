package agent

import (
	"context"
	"net/http"
	"strings"
	"time"

	"vault/internal/apperr"
)

// handleGetIdentity 返回本地身份元数据（**不含私钥**）。
func (a *Agent) handleGetIdentity(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, a.identity.Meta())
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
func (a *Agent) handleDeleteIdentity(w http.ResponseWriter, _ *http.Request) {
	if err := a.identity.Remove(); err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// identityLoginRequest 是证书登录请求；server_url 可选。
type identityLoginRequest struct {
	ServerURL string `json:"server_url"`
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

	session, out, err := a.certLogin(ctx, in.ServerURL)
	if err != nil {
		a.writeError(w, err)
		return
	}

	// 与 POST /agent/session 一致地写回本地状态，供心跳/事件订阅等后续能力复用。
	a.setSession(session)
	a.logger.Info("已用本地客户端证书免密登录", "server_url", session.ServerURL, "username", session.Username)
	// 原样透出服务端响应。
	a.writeJSON(w, http.StatusOK, out)
}

// certLogin 用本地客户端证书向服务端换取新会话（mTLS），返回可直接写回本地状态的 Session。
//
// 供 handleIdentityLogin（用户点击/自动免密登录）与后台自动续期循环共用，避免复制粘贴。
// 失败返回服务端结构化错误（如 auth.cert_invalid / auth.forbidden / agent.server_unreachable）。
func (a *Agent) certLogin(ctx context.Context, serverURL string) (*Session, *certLoginHTTPResponse, error) {
	id, ok := a.identity.Get()
	if !ok {
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

	return &Session{
		ServerURL:        client.base,
		ServerInstanceID: id.ServerInstanceID,
		Token:            out.Token,
		CertSHA256:       pin,
		UserID:           out.User.ID,
		Username:         out.User.Username,
		ExpiresAt:        out.ExpiresAt,
	}, &out, nil
}

// pinForIdentity 选择用于固定服务端证书的指纹：
// 身份文件记录的 server_cert_sha256 优先；否则退回与身份同一服务端的已存会话指纹。
func (a *Agent) pinForIdentity(id Identity) string {
	if p := strings.ToLower(strings.TrimSpace(id.ServerCertSHA256)); p != "" {
		return p
	}
	if sess, ok := a.store.Session(); ok && sess.ServerURL == id.ServerURL {
		return strings.ToLower(strings.TrimSpace(sess.CertSHA256))
	}
	return ""
}
