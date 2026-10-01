package api

import (
	"encoding/pem"
	"net/http"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// loginRequest 是口令登录请求。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse 是登录成功响应。
type loginResponse struct {
	Token     string  `json:"token"`
	ExpiresAt int64   `json:"expires_at"`
	User      userDTO `json:"user"`
}

// handleLogin 口令登录（管理页）。
func (r *Router) handleLogin(w http.ResponseWriter, req *http.Request) {
	var in loginRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	ip := clientIP(req)
	sess, err := r.deps.App.Auth().Login(req.Context(), in.Username, in.Password, ip)
	if err != nil {
		r.deps.App.AuditWithIP(req.Context(), "", "auth.login", "user:"+in.Username, "", ip, domain.AuditResultDenied)
		r.writeError(w, req, err)
		return
	}
	user, err := r.deps.App.Users().Get(req.Context(), sess.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.AuditWithIP(req.Context(), sess.UserID, "auth.login", "user:"+sess.Username, "", ip, domain.AuditResultOK)
	r.writeJSON(w, http.StatusOK, loginResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UnixMilli(),
		User:      toUserDTO(user),
	})
}

// handleLogout 注销当前会话。
func (r *Router) handleLogout(w http.ResponseWriter, req *http.Request) {
	token := rawTokenFrom(req.Context())
	if token == "" {
		r.writeError(w, req, apperr.AuthRequired())
		return
	}
	if sessions := r.deps.App.Sessions; sessions != nil {
		sessions.Delete(token)
	}
	if p, err := r.principal(req); err == nil {
		r.deps.App.Audit(req.Context(), p.UserID, "auth.logout", "session", "", domain.AuditResultOK)
	}
	r.writeNoContent(w)
}

// enrollRequest 是客户端证书注册请求。
type enrollRequest struct {
	Token string `json:"token"`
	// CSR 是 PKCS#10 请求（PEM）；私钥必须留在客户端。
	CSR string `json:"csr"`
}

// enrollResponse 是证书注册结果。
//
// 只返回证书与信任锚，**绝不返回任何私钥材料**。
type enrollResponse struct {
	CertPEM           string `json:"cert_pem"`
	CAPEM             string `json:"ca_pem"`
	Serial            string `json:"serial"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	SPKISHA256        string `json:"spki_sha256"`
	NotBefore         int64  `json:"not_before"`
	NotAfter          int64  `json:"not_after"`
}

// handleEnroll 用一次性令牌换取客户端证书。
func (r *Router) handleEnroll(w http.ResponseWriter, req *http.Request) {
	var in enrollRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	issued, err := r.deps.App.Auth().Enroll(req.Context(), app.EnrollRequest{Token: in.Token, CSR: in.CSR})
	if err != nil {
		r.deps.App.AuditWithIP(req.Context(), "", "auth.enroll", "certificate", "", clientIP(req), domain.AuditResultDenied)
		r.writeError(w, req, err)
		return
	}
	ca := r.deps.App.CA
	caPEM := ""
	if ca != nil {
		caPEM = string(ca.CAPEM())
	}
	r.deps.App.AuditWithIP(req.Context(), "", "auth.enroll", "certificate:"+issued.Serial, "", clientIP(req), domain.AuditResultOK)
	r.writeJSON(w, http.StatusCreated, enrollResponse{
		CertPEM:           string(issued.CertPEM),
		CAPEM:             caPEM,
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore.UnixMilli(),
		NotAfter:          issued.NotAfter.UnixMilli(),
	})
}

// clientCertificateRequest 是"用当前会话为自己换取客户端证书"的请求。
type clientCertificateRequest struct {
	// CSR 是 PKCS#10 请求（PEM）；私钥必须留在客户端。
	CSR string `json:"csr"`
}

// handleClientCertificate 以当前会话用户身份签发客户端证书（需登录，非超管亦可）。
//
// 请求体携带客户端本地生成的 CSR，服务端只对公钥签发，**绝不返回私钥**。
func (r *Router) handleClientCertificate(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in clientCertificateRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	issued, err := r.deps.App.Auth().IssueClientCertificateFromCSR(req.Context(), p.UserID, in.CSR)
	if err != nil {
		r.deps.App.AuditWithIP(req.Context(), p.UserID, "auth.client-certificate", "certificate", "", clientIP(req), domain.AuditResultDenied)
		r.writeError(w, req, err)
		return
	}
	r.deps.App.AuditWithIP(req.Context(), p.UserID, "auth.client-certificate", "certificate:"+issued.Serial, "", clientIP(req), domain.AuditResultOK)
	// 响应复用 enrollResponse 形状：只含证书与信任锚，不含私钥。
	r.writeJSON(w, http.StatusOK, enrollResponse{
		CertPEM:           string(issued.CertPEM),
		CAPEM:             string(issued.CAPEM),
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore.UnixMilli(),
		NotAfter:          issued.NotAfter.UnixMilli(),
	})
}

// handleCertLogin 用客户端 TLS 证书换取会话（免密登录，匿名接口）。
//
// 与 handleLogin **共用** AuthService 的会话签发路径，响应结构完全一致。
func (r *Router) handleCertLogin(w http.ResponseWriter, req *http.Request) {
	ip := clientIP(req)
	if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
		r.deps.App.AuditWithIP(req.Context(), "", "auth.cert-login", "certificate", "", ip, domain.AuditResultDenied)
		r.writeError(w, req, apperr.New("auth.cert_required", http.StatusUnauthorized))
		return
	}
	// 把 DER 还原为 PEM 后走统一的证书认证链（签名、有效期、实例绑定、登记状态、绑定 IP）。
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: req.TLS.PeerCertificates[0].Raw})
	principal, err := r.deps.App.Auth().AuthenticateCert(req.Context(), certPEM, ip)
	if err != nil {
		r.deps.App.AuditWithIP(req.Context(), "", "auth.cert-login", "certificate", "", ip, domain.AuditResultDenied)
		r.writeError(w, req, err)
		return
	}
	sess, err := r.deps.App.Auth().CreateSession(req.Context(), principal.UserID, ip)
	if err != nil {
		r.deps.App.AuditWithIP(req.Context(), principal.UserID, "auth.cert-login", "certificate", "", ip, domain.AuditResultDenied)
		r.writeError(w, req, err)
		return
	}
	user, err := r.deps.App.Users().Get(req.Context(), sess.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.AuditWithIP(req.Context(), sess.UserID, "auth.cert-login", "certificate:"+principal.CertFingerprint, "", ip, domain.AuditResultOK)
	r.writeJSON(w, http.StatusOK, loginResponse{
		Token:     sess.Token,
		ExpiresAt: sess.ExpiresAt.UnixMilli(),
		User:      toUserDTO(user),
	})
}
