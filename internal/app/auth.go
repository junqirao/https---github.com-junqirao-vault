package app

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/cert"
	"vault/internal/domain"
	"vault/internal/secret"
)

// ---------- 会话 ----------

// Session 是管理页会话。
type Session struct {
	Token     string
	UserID    string
	Username  string
	Role      domain.Role
	IP        string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionStore 是内存会话表。
//
// 单实例部署下无需持久化：服务端重启使会话失效是安全上可接受的行为，
// 且避免把会话密钥落库带来的额外风险。
type SessionStore struct {
	mu   sync.RWMutex
	ttl  time.Duration
	data map[string]*Session
}

// NewSessionStore 构造会话表。
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionStore{ttl: ttl, data: make(map[string]*Session)}
}

// Create 为用户创建会话。
func (s *SessionStore) Create(user *domain.User, ip string) (*Session, error) {
	token, err := secret.RandomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{
		Token:     token,
		UserID:    user.ID,
		Username:  user.Username,
		Role:      user.Role,
		IP:        ip,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
	s.mu.Lock()
	s.data[token] = sess
	s.mu.Unlock()
	return sess, nil
}

// Get 查询会话；过期或不存在返回 false。
func (s *SessionStore) Get(token string) (*Session, bool) {
	s.mu.RLock()
	sess, ok := s.data[token]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(sess.ExpiresAt) {
		s.Delete(token)
		return nil, false
	}
	return sess, true
}

// Delete 删除会话。
func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.data, token)
	s.mu.Unlock()
}

// Cleanup 清理过期会话，返回清理数量。
func (s *SessionStore) Cleanup() int {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.data {
		if now.After(v.ExpiresAt) {
			delete(s.data, k)
			n++
		}
	}
	return n
}

// ---------- 防爆破 ----------

type failKey struct {
	username string
	ip       string
}

type failRecord struct {
	count     int
	firstSeen time.Time
	lockedTil time.Time
}

type loginTracker struct {
	mu      sync.Mutex
	records map[failKey]*failRecord

	maxFailures int
	window      time.Duration
	lockDur     time.Duration
}

func newLoginTracker(maxFailures int, window, lockDur time.Duration) *loginTracker {
	return &loginTracker{
		records:     make(map[failKey]*failRecord),
		maxFailures: maxFailures,
		window:      window,
		lockDur:     lockDur,
	}
}

// Locked 判断是否处于锁定期。
func (t *loginTracker) Locked(username, ip string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.records[failKey{username: username, ip: ip}]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if now.Before(rec.lockedTil) {
		return true, time.Until(rec.lockedTil)
	}
	return false, 0
}

// Fail 记录一次失败；达到阈值则锁定。
func (t *loginTracker) Fail(username, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := failKey{username: username, ip: ip}
	now := time.Now()
	rec, ok := t.records[k]
	if !ok || now.Sub(rec.firstSeen) > t.window {
		rec = &failRecord{firstSeen: now}
		t.records[k] = rec
	}
	rec.count++
	if rec.count >= t.maxFailures {
		rec.lockedTil = now.Add(t.lockDur)
		rec.count = 0
		rec.firstSeen = now
	}
}

// Success 清空该键的失败计数。
func (t *loginTracker) Success(username, ip string) {
	t.mu.Lock()
	delete(t.records, failKey{username: username, ip: ip})
	t.mu.Unlock()
}

// Cleanup 清理已过期且未锁定的记录。
func (t *loginTracker) Cleanup() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	n := 0
	for k, rec := range t.records {
		if now.After(rec.lockedTil) && now.Sub(rec.firstSeen) > t.window {
			delete(t.records, k)
			n++
		}
	}
	return n
}

// ---------- AuthService ----------

// AuthService 负责口令登录、证书认证与证书注册。
type AuthService struct {
	Deps
	Users *UserService

	trackerOnce sync.Once
	tracker     *loginTracker

	// superAdminDisabled 由配置开关 security.super_admin_enabled 决定，
	// 为 true 时超级管理员角色一律拒绝登录（账号数据保留，仅禁用登录）。
	superAdminDisabled bool
}

func (s *AuthService) failures() *loginTracker {
	s.trackerOnce.Do(func() {
		d := s.securityDefaults()
		s.tracker = newLoginTracker(d.MaxLoginFail, d.LoginWindow, d.LoginLock)
	})
	return s.tracker
}

// Login 口令登录（管理页）。用户名不存在与密码错误返回同一错误，避免枚举用户。
func (s *AuthService) Login(ctx context.Context, username, password, ip string) (*Session, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, apperr.InvalidParam("username/password")
	}

	if locked, remain := s.failures().Locked(username, ip); locked {
		return nil, apperr.AuthLocked().WithArg("retry_after_seconds", int(remain.Seconds()))
	}

	user, err := s.Users.GetByUsername(ctx, username)
	if err != nil || !user.Enabled || user.PasswordHash == "" {
		s.failures().Fail(username, ip)
		return nil, apperr.AuthRequired()
	}

	ok, verifyErr := secret.VerifyPassword(user.PasswordHash, password)
	if verifyErr != nil {
		s.Log.Error("口令哈希校验异常", "username", username, "error", verifyErr)
		s.failures().Fail(username, ip)
		return nil, apperr.AuthRequired()
	}
	if !ok {
		s.failures().Fail(username, ip)
		return nil, apperr.AuthRequired()
	}

	// 内置超级管理员被配置停用时，即使口令正确也拒绝登录。
	if user.Role == domain.RoleSuperAdmin && s.SuperAdminDisabled() {
		return nil, apperr.AuthForbidden().WithArg("reason", "super_admin_disabled")
	}

	s.failures().Success(username, ip)
	sess, err := s.issueSession(user, ip)
	if err != nil {
		return nil, err
	}
	s.Log.Info("用户登录成功", "username", username, "ip", ip, "role", user.Role)
	return sess, nil
}

// issueSession 是**唯一的会话签发路径**：口令登录与证书登录都经由此处，
// 保证两种认证方式产出的会话结构、TTL、来源 IP 记录完全一致。
func (s *AuthService) issueSession(user *domain.User, ip string) (*Session, error) {
	if s.Sessions == nil {
		return nil, errNotImplemented
	}
	return s.Sessions.Create(user, ip)
}

// CreateSession 为指定用户签发会话（供证书登录等已认证路径复用）。
func (s *AuthService) CreateSession(ctx context.Context, userID, ip string) (*Session, error) {
	user, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.issueSession(user, ip)
}

// AuthenticateCert 用客户端证书认证。
//
// 校验链（缺一不可）：
//  1. 证书由本服务端 CA 签发、在有效期内、用途含 ClientAuth；
//  2. 证书扩展中的服务端实例 ID 等于本机（防止拿别的服务端的证书）；
//  3. 证书已在数据库登记且状态为 active（吊销后立即失效）；
//  4. 绑定 IP 策略（若配置）匹配来源地址；
//  5. 证书绑定的用户存在且启用。
func (s *AuthService) AuthenticateCert(ctx context.Context, certPEM []byte, remoteIP string) (*Principal, error) {
	if s.CA == nil {
		return nil, apperr.New("auth.ca_unavailable", 503)
	}
	instanceID := s.raw().Server.InstanceID

	parsed, err := cert.VerifyClientCert(s.CA.CAPEM(), certPEM, instanceID)
	if err != nil {
		return nil, err
	}

	rec, err := s.Store.GetCertificateByFingerprint(ctx, parsed.FingerprintSHA256)
	if err != nil {
		// 未登记的证书：可能是被吊销后清理，或来自其他服务端的证书。
		return nil, apperr.AuthForbidden().WithArg("reason", "cert_not_registered")
	}
	if rec.Status != domain.CertificateStatusActive {
		return nil, apperr.AuthForbidden().WithArg("reason", "cert_revoked")
	}
	if !ipAllowed(rec.BoundIP, remoteIP) {
		return nil, apperr.AuthForbidden().WithArg("reason", "ip_not_allowed")
	}

	user, err := s.Users.Get(ctx, rec.UserID)
	if err != nil {
		return nil, apperr.AuthForbidden().WithArg("reason", "user_not_found")
	}
	if !user.Enabled {
		return nil, apperr.AuthForbidden().WithArg("reason", "user_disabled")
	}
	if user.Role == domain.RoleSuperAdmin && s.SuperAdminDisabled() {
		return nil, apperr.AuthForbidden().WithArg("reason", "super_admin_disabled")
	}

	return &Principal{
		UserID:          user.ID,
		Username:        user.Username,
		Role:            user.Role,
		AuthMethod:      "cert",
		CertFingerprint: rec.Fingerprint,
	}, nil
}

// IssueEnrollmentToken 为用户签发一次性注册令牌。
func (s *AuthService) IssueEnrollmentToken(ctx context.Context, userID string) (token string, expiresAt time.Time, err error) {
	if s.Tokens == nil {
		return "", time.Time{}, errNotImplemented
	}
	if _, err := s.Users.Get(ctx, userID); err != nil {
		return "", time.Time{}, err
	}
	return s.Tokens.Issue(userID, s.raw().Server.InstanceID)
}

// EnrollRequest 是客户端申请证书的请求。
type EnrollRequest struct {
	// Token 一次性注册令牌。
	Token string
	// CSR PKCS#10 请求（PEM）。**私钥必须留在客户端**，服务端只签发公钥证书。
	CSR string
	// CommonName 可选，服务端会以配置的用户名覆盖，防止客户端伪造身份字段。
	CommonName string
}

// Enroll 用一次性令牌换取客户端证书。
func (s *AuthService) Enroll(ctx context.Context, req EnrollRequest) (*cert.IssuedCert, error) {
	if s.Tokens == nil || s.CA == nil {
		return nil, errNotImplemented
	}
	if strings.TrimSpace(req.CSR) == "" {
		return nil, apperr.InvalidParam("csr")
	}

	userID, _, ok := s.Tokens.Consume(strings.TrimSpace(req.Token))
	if !ok {
		return nil, apperr.New("auth.enrollment_token_invalid", 401)
	}

	user, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.Enabled {
		return nil, apperr.AuthForbidden().WithArg("reason", "user_disabled")
	}

	issued, err := s.CA.IssueClientCertFromCSR(cert.ClientCertRequest{
		UserID:     user.ID,
		Username:   user.Username,
		CommonName: user.Username,
		InstanceID: s.raw().Server.InstanceID,
	}, []byte(req.CSR))
	if err != nil {
		return nil, err
	}

	if err := s.Store.CreateCertificate(ctx, &domain.Certificate{
		UserID:      user.ID,
		Serial:      issued.Serial,
		Fingerprint: issued.FingerprintSHA256,
		SPKISHA256:  issued.SPKISHA256,
		Status:      domain.CertificateStatusActive,
		NotBefore:   issued.NotBefore.UnixMilli(),
		NotAfter:    issued.NotAfter.UnixMilli(),
	}); err != nil {
		return nil, err
	}

	s.Log.Info("客户端证书注册成功",
		"username", user.Username,
		"cert_serial", issued.Serial,
		"cert_fingerprint", issued.FingerprintSHA256,
		"spki", issued.SPKISHA256)
	return issued, nil
}

// IssuedClientCertificate 是"服务端生成密钥对并签发客户端证书"的一次性结果。
//
// ⚠️ KeyPEM 只在本次返回值中携带一次：**绝不落库、绝不写日志、绝不写审计详情**。
type IssuedClientCertificate struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte

	Serial            string
	FingerprintSHA256 string
	SPKISHA256        string
	NotBefore         time.Time
	NotAfter          time.Time
}

// IssueClientCertificate 为指定用户生成密钥对并签发客户端证书，登记元数据后返回。
//
// 与 Enroll 的 CSR 流程不同：此处由**服务端生成密钥对**，因此返回值携带私钥，
// 调用方必须只在本次响应中一次性交付，不得持久化。boundIP / boundMAC 为可选绑定信息
// （boundMAC 仅作展示与排障线索，不参与鉴权）。
func (s *AuthService) IssueClientCertificate(ctx context.Context, userID, boundIP, boundMAC string) (*IssuedClientCertificate, error) {
	if s.CA == nil {
		return nil, errNotImplemented
	}
	user, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.Enabled {
		return nil, apperr.AuthForbidden().WithArg("reason", "user_disabled")
	}

	issued, err := s.CA.IssueClientCert(cert.ClientCertRequest{
		UserID:     user.ID,
		Username:   user.Username,
		CommonName: user.Username,
		InstanceID: s.raw().Server.InstanceID,
	})
	if err != nil {
		return nil, err
	}

	// 一个用户最多保留**一张**有效证书：先把既有的 active 证书全部吊销，再登记新证书，
	// 避免同一用户存在多张可用证书导致身份不唯一（列表里也会出现多张"有效"证书）。
	// 顺序刻意是"先吊销、后登记"：若登记失败，用户处于"无有效证书"状态，重新签发即可，
	// 不会出现"两张都有效"的中间态。
	if err := s.revokeActiveCertificates(ctx, user.ID); err != nil {
		return nil, err
	}

	if err := s.Store.CreateCertificate(ctx, &domain.Certificate{
		UserID:      user.ID,
		Serial:      issued.Serial,
		Fingerprint: issued.FingerprintSHA256,
		SPKISHA256:  issued.SPKISHA256,
		Status:      domain.CertificateStatusActive,
		BoundIP:     boundIP,
		BoundMAC:    boundMAC,
		NotBefore:   issued.NotBefore.UnixMilli(),
		NotAfter:    issued.NotAfter.UnixMilli(),
	}); err != nil {
		return nil, err
	}

	// 只记录 serial / fingerprint，绝不记录私钥。
	s.Log.Info("已为用户签发客户端证书（服务端生成密钥对）",
		"username", user.Username,
		"cert_serial", issued.Serial,
		"cert_fingerprint", issued.FingerprintSHA256)
	return &IssuedClientCertificate{
		CertPEM:           issued.CertPEM,
		KeyPEM:            issued.KeyPEM,
		CAPEM:             s.CA.CAPEM(),
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore,
		NotAfter:          issued.NotAfter,
	}, nil
}

// IssueClientCertificateFromCSR 用客户端提交的 CSR 为**指定用户**签发客户端证书。
//
// 与 Enroll 的差别仅在授权来源：Enroll 消耗一次性令牌，本方法由 HTTP 层从已登录会话的
// principal 取出 userID 后调用，因此无需令牌。私钥始终留在客户端，返回值不含 KeyPEM。
//
// 与 IssueClientCertificate 一样遵循"一个用户最多一张有效证书"：先吊销既有 active 证书，
// 再登记新证书（顺序刻意如此，避免出现两张都有效的中间态）。
func (s *AuthService) IssueClientCertificateFromCSR(ctx context.Context, userID, csrPEM string) (*IssuedClientCertificate, error) {
	if s.CA == nil {
		return nil, errNotImplemented
	}
	if strings.TrimSpace(csrPEM) == "" {
		return nil, apperr.InvalidParam("csr")
	}
	user, err := s.Users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !user.Enabled {
		return nil, apperr.AuthForbidden().WithArg("reason", "user_disabled")
	}

	issued, err := s.CA.IssueClientCertFromCSR(cert.ClientCertRequest{
		UserID:     user.ID,
		Username:   user.Username,
		CommonName: user.Username,
		InstanceID: s.raw().Server.InstanceID,
	}, []byte(csrPEM))
	if err != nil {
		return nil, err
	}

	if err := s.revokeActiveCertificates(ctx, user.ID); err != nil {
		return nil, err
	}

	if err := s.Store.CreateCertificate(ctx, &domain.Certificate{
		UserID:      user.ID,
		Serial:      issued.Serial,
		Fingerprint: issued.FingerprintSHA256,
		SPKISHA256:  issued.SPKISHA256,
		Status:      domain.CertificateStatusActive,
		NotBefore:   issued.NotBefore.UnixMilli(),
		NotAfter:    issued.NotAfter.UnixMilli(),
	}); err != nil {
		return nil, err
	}

	// 只记录 serial / fingerprint，绝不记录私钥（本来也不在服务端）。
	s.Log.Info("已为用户签发客户端证书（客户端提交 CSR，私钥不外传）",
		"username", user.Username,
		"cert_serial", issued.Serial,
		"cert_fingerprint", issued.FingerprintSHA256)
	return &IssuedClientCertificate{
		CertPEM:           issued.CertPEM,
		KeyPEM:            nil, // 私钥在客户端，服务端不持有。
		CAPEM:             s.CA.CAPEM(),
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore,
		NotAfter:          issued.NotAfter,
	}, nil
}

// revokeActiveCertificates 吊销某用户当前全部有效证书（用于"一个用户最多一张有效证书"）。
func (s *AuthService) revokeActiveCertificates(ctx context.Context, userID string) error {
	certs, err := s.Store.ListCertificatesByUser(ctx, userID)
	if err != nil {
		return err
	}
	for i := range certs {
		if certs[i].Status != domain.CertificateStatusActive {
			continue
		}
		if err := s.Store.RevokeCertificate(ctx, certs[i].ID); err != nil {
			return err
		}
		s.Log.Info("签发新证书前已吊销该用户的旧证书",
			"user_id", userID, "cert_serial", certs[i].Serial)
	}
	return nil
}

// SyncSuperAdminSwitch 应用"内置超级管理员开关"。
//
// 语义变更（本轮调整）：
//   - 超级管理员账号**存放在数据库中**，由初始化向导创建，口令以 argon2id 哈希存储；
//   - 配置文件中的 `security.super_admin_enabled` 只作为**可用性开关**：
//     关闭后该账号立刻无法登录（不删数据），可用于收紧权限或锁定恢复。
//
// 注意：本方法**不会创建也不会修改密码**，避免"配置文件里躺着口令哈希"这一反模式。
func (s *AuthService) SyncSuperAdminSwitch(ctx context.Context) error {
	enabled := s.raw().Security.SuperAdminEnabled
	on := enabled == nil || *enabled

	if on {
		if s.superAdminDisabled {
			s.Log.Info("内置超级管理员已按配置恢复启用")
		}
		s.superAdminDisabled = false
		return nil
	}

	if !s.superAdminDisabled {
		s.Log.Warn("内置超级管理员已按配置停用：该角色账号将无法登录")
	}
	s.superAdminDisabled = true
	return nil
}

// SuperAdminDisabled 返回内置超级管理员当前是否被配置停用。
func (s *AuthService) SuperAdminDisabled() bool { return s.superAdminDisabled }

// Cleanup 清理过期的会话、失败记录与注册令牌。
func (s *AuthService) Cleanup() {
	if s.Sessions != nil {
		s.Sessions.Cleanup()
	}
	if s.Tokens != nil {
		s.Tokens.Cleanup()
	}
	if t := s.failures(); t != nil {
		t.Cleanup()
	}
}

// ipAllowed 判断来源 IP 是否在允许列表内。
//
// boundIP 支持逗号分隔的 IP 或 CIDR；为空表示不限制。
// 注意：IP 只是**附加收紧策略**，不作为身份来源（见 docs/implementation.md 9.1）。
func ipAllowed(boundIP, remoteIP string) bool {
	boundIP = strings.TrimSpace(boundIP)
	if boundIP == "" {
		return true
	}
	remote := net.ParseIP(strings.TrimSpace(remoteIP))
	if remote == nil {
		return false
	}

	for _, part := range strings.Split(boundIP, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			if _, network, err := net.ParseCIDR(part); err == nil && network.Contains(remote) {
				return true
			}
			continue
		}
		if ip := net.ParseIP(part); ip != nil && ip.Equal(remote) {
			return true
		}
	}
	return false
}
