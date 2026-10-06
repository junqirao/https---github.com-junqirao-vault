package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
)

// identityFileName 是代理本地身份文件名；与状态文件同目录（见 identityPathFor）。
const identityFileName = "identity.json"

// identityFileVersion 是身份文件的结构版本。
//
// v1 是"整份文件就是一个 Identity"（只能保存一个服务端的证书）；v2 起为
// {"version":2,"active":...,"identities":[...]}，允许同时保存多个服务端的客户端身份
// （每个服务端一套证书/私钥，互不覆盖）。读取时两种结构都兼容。
const identityFileVersion = 2

// identityPathFor 返回与给定状态文件同目录的身份文件路径。
func identityPathFor(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), identityFileName)
}

// persistedIdentities 是身份文件的落盘结构（v2）。
type persistedIdentities struct {
	// Version 结构版本；v2 起为 identityFileVersion。
	Version int `json:"version"`
	// Active 是"未指定服务端"时使用的身份键（见 serverKeyOf）。
	Active string `json:"active,omitempty"`
	// Identities 各服务端的身份（含私钥材料，文件 0600 + 收紧 ACL）。
	Identities []Identity `json:"identities"`
}

// serverKeyOf 计算服务端的稳定标识，用于把身份归属到某个服务端实例。
//
// 优先用服务端实例 ID（地址变化不影响归属：同一实例换地址后证书依然可用）；
// 实例 ID 缺失时退回规范化地址 —— 与前端 ServerEntry.key 的算法保持一致。
func serverKeyOf(instanceID, serverURL string) string {
	if key := strings.ToLower(strings.TrimSpace(instanceID)); key != "" {
		return key
	}
	return normalizeServerURL(serverURL)
}

// normalizeServerURL 规范化服务端地址用于比较：去首尾空白、转小写、去掉末尾斜杠。
func normalizeServerURL(serverURL string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(serverURL)), "/")
}

// restrictFileToAdmins 收紧落盘文件的 ACL（实现见平台文件：Windows 下收紧为仅 SYSTEM 与
// Administrators 并去掉继承）。
//
// 这里是变量而不是直接调用，只为给测试留一个替换点：被收紧的文件只有提权进程读得回来，
// 非提权环境下的测试需要换成空实现才能验证存储逻辑本身。
var restrictFileToAdmins = restrictFileToAdminsImpl

// Identity 是本地客户端身份：客户端证书 + 私钥 + 服务端信任信息。
//
// ⚠️ KeyPEM 是客户端私钥（PKCS#8 PEM）：**只在本地落盘（0600）**，
// 绝不写日志、绝不进错误响应、绝不上报服务端。读取时会校验证书与私钥匹配。
type Identity struct {
	// ServerURL 服务端根地址（如 https://10.0.0.1:8443）。
	ServerURL string `json:"server_url"`
	// ServerInstanceID 服务端实例 ID（便于登录时直接写回会话）。
	ServerInstanceID string `json:"server_instance_id"`
	// ServerCertSHA256 服务端证书 DER 的 SHA-256（小写十六进制），用于登录时固定校验。
	ServerCertSHA256 string `json:"server_cert_sha256"`
	// UserID / Username 身份归属用户。
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// CertPEM 客户端证书（PEM）。
	CertPEM string `json:"cert_pem"`
	// KeyPEM 客户端私钥（PKCS#8 PEM）。绝不外泄。
	KeyPEM string `json:"key_pem"`
	// CAPEM 服务端 CA 证书（PEM），作为信任锚展示/备用。
	CAPEM string `json:"ca_pem"`
	// Serial / FingerprintSHA256 / SPKISHA256 证书元数据。
	Serial            string `json:"serial"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	SPKISHA256        string `json:"spki_sha256"`
	// NotBefore / NotAfter 证书有效期（毫秒时间戳）。
	NotBefore int64 `json:"not_before"`
	NotAfter  int64 `json:"not_after"`
	// InstalledAt 安装时间（毫秒时间戳）。
	InstalledAt int64 `json:"installed_at"`
}

// identityMeta 是对外暴露的身份元数据，**不含任何私钥材料**。
type identityMeta struct {
	Installed bool `json:"installed"`
	// ServerKey 身份归属的服务端标识（见 serverKeyOf）：实例 ID 优先，其次规范化地址。
	ServerKey         string `json:"server_key,omitempty"`
	ServerURL         string `json:"server_url,omitempty"`
	ServerInstanceID  string `json:"server_instance_id,omitempty"`
	ServerCertSHA256  string `json:"server_cert_sha256,omitempty"`
	UserID            string `json:"user_id,omitempty"`
	Username          string `json:"username,omitempty"`
	Serial            string `json:"serial,omitempty"`
	FingerprintSHA256 string `json:"fingerprint_sha256,omitempty"`
	SPKISHA256        string `json:"spki_sha256,omitempty"`
	NotBefore         int64  `json:"not_before,omitempty"`
	NotAfter          int64  `json:"not_after,omitempty"`
	InstalledAt       int64  `json:"installed_at,omitempty"`
}

// meta 生成对外元数据（去除私钥与 PEM 材料）。
func (id *Identity) meta() identityMeta {
	return identityMeta{
		Installed:         true,
		ServerKey:         serverKeyOf(id.ServerInstanceID, id.ServerURL),
		ServerURL:         id.ServerURL,
		ServerInstanceID:  id.ServerInstanceID,
		ServerCertSHA256:  id.ServerCertSHA256,
		UserID:            id.UserID,
		Username:          id.Username,
		Serial:            id.Serial,
		FingerprintSHA256: id.FingerprintSHA256,
		SPKISHA256:        id.SPKISHA256,
		NotBefore:         id.NotBefore,
		NotAfter:          id.NotAfter,
		InstalledAt:       id.InstalledAt,
	}
}

// identityStore 负责本地**多个服务端**身份的读写与校验。身份只在内存与本地文件间流转。
//
// 按 serverKeyOf 把身份归属到服务端实例：登录、续期、断线重连都先按目标服务端定位身份，
// 从根上避免"拿 A 服务端的证书去连 B 服务端"（服务端会以证书不属于本实例拒绝，
// 表现为反复握手/登录失败且难以定位）。
type identityStore struct {
	path   string
	logger *slog.Logger

	mu sync.Mutex
	// entries 各服务端的身份，键为 serverKeyOf（缺少服务端标识的身份不入册）。
	entries map[string]Identity
	// activeKey 是"未指定服务端"时使用的身份（最近安装的那份）；可能为空。
	activeKey string
}

// newIdentityStore 加载本地身份；文件不存在或校验失败时视为"未安装"（不阻断启动）。
func newIdentityStore(path string, logger *slog.Logger) *identityStore {
	if logger == nil {
		logger = slog.Default()
	}
	s := &identityStore{path: path, logger: logger, entries: map[string]Identity{}}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		s.load(data)
	case os.IsNotExist(err):
	default:
		logger.Warn("读取本地身份文件失败，已视为未安装", "path", path, "error", err)
	}
	return s
}

// load 解析身份文件：v2（多服务端列表）优先，其次兼容 v1（整份文件就是一个身份）。
func (s *identityStore) load(data []byte) {
	var file persistedIdentities
	if err := json.Unmarshal(data, &file); err == nil && (file.Version > 0 || len(file.Identities) > 0) {
		for i := range file.Identities {
			s.put(&file.Identities[i])
		}
		if _, ok := s.entries[file.Active]; ok {
			s.activeKey = file.Active
		}
	} else {
		// v1：只支持单个服务端，整份文件就是一个身份（升级后按 serverKeyOf 重新归属）。
		var single Identity
		if err := json.Unmarshal(data, &single); err != nil {
			s.logger.Warn("本地身份文件无法解析，已视为未安装", "path", s.path)
			return
		}
		s.put(&single)
	}
	if s.activeKey == "" {
		s.activeKey = s.firstKey()
	}
}

// put 入册一条身份；缺少服务端标识或校验失败时丢弃并留痕（绝不回显密钥材料）。
func (s *identityStore) put(id *Identity) {
	if id == nil {
		return
	}
	key := serverKeyOf(id.ServerInstanceID, id.ServerURL)
	if key == "" {
		s.logger.Warn("本地身份缺少服务端标识，已忽略", "path", s.path)
		return
	}
	if err := validateIdentity(id); err != nil {
		// 只记录错误类型，绝不回显可能包含密钥材料的原始内容。
		s.logger.Warn("本地身份校验失败，已忽略", "path", s.path, "reason", err.Error())
		return
	}
	s.entries[key] = *id
}

// Path 返回身份文件路径。
func (s *identityStore) Path() string { return s.path }

// Len 返回已保存的身份数量（即已安装客户端证书的服务端数量）。
func (s *identityStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ActiveKey 返回"活动"身份的键；无身份时为空。
func (s *identityStore) ActiveKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeKey
}

// Get 返回"活动"身份副本（调用方未指定服务端时使用）；未安装时 ok=false。
func (s *identityStore) Get() (Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked()
}

// GetForServer 返回指定服务端的身份副本；该服务端没有证书时 ok=false。
//
// 严格按服务端匹配（"只有一份就用它"的兜底只放在连接路径上，见 Agent.resolveIdentity）：
// 否则界面会把别的服务端的证书当成本服务端的显示出来。
func (s *identityStore) GetForServer(instanceID, serverURL string) (Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(instanceID, serverURL)
}

// List 返回全部身份副本，按服务端标识排序（展示顺序稳定）。
func (s *identityStore) List() []Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

// Meta 返回"活动"身份的对外元数据；未安装时 installed=false。
func (s *identityStore) Meta() identityMeta {
	if id, ok := s.Get(); ok {
		return id.meta()
	}
	return identityMeta{Installed: false}
}

// MetaFor 返回指定服务端身份的对外元数据；该服务端没有证书时 installed=false。
func (s *identityStore) MetaFor(instanceID, serverURL string) identityMeta {
	if id, ok := s.GetForServer(instanceID, serverURL); ok {
		return id.meta()
	}
	return identityMeta{Installed: false}
}

// Metas 返回全部身份的对外元数据（不含私钥材料），按服务端标识排序。
func (s *identityStore) Metas() []identityMeta {
	ids := s.List()
	out := make([]identityMeta, 0, len(ids))
	for i := range ids {
		out = append(out, ids[i].meta())
	}
	return out
}

// Install 校验并持久化身份（写前 MkdirAll，文件 0600），并把它置为"活动"身份。
//
// 同一服务端重复安装即覆盖（证书续签、换用户登录）；**不影响其他服务端的身份**。
func (s *identityStore) Install(id *Identity) error {
	if id == nil {
		return errors.New("agent: 身份不能为空")
	}
	if err := validateIdentity(id); err != nil {
		return err
	}
	key := serverKeyOf(id.ServerInstanceID, id.ServerURL)
	if key == "" {
		// 没有服务端标识就无法归属，落盘后也会在下次启动被丢弃：不如现在就拒绝。
		return apperr.InvalidParam("server_url")
	}
	if id.InstalledAt == 0 {
		id.InstalledAt = time.Now().UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	prev, hadPrev := s.entries[key]
	prevActive := s.activeKey
	s.entries[key] = *id
	s.activeKey = key
	if err := s.persistLocked(); err != nil {
		// 写盘失败则回滚内存，避免内存与磁盘不一致（下次启动会读到旧内容）。
		if hadPrev {
			s.entries[key] = prev
		} else {
			delete(s.entries, key)
		}
		s.activeKey = prevActive
		return err
	}
	return nil
}

// Remove 删除身份（幂等）：instanceID/serverURL 均为空时删除"活动"身份（旧版语义），
// 否则删除匹配到的那个服务端的身份；其他服务端的身份不受影响。
func (s *identityStore) Remove(instanceID, serverURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(instanceID) == "" && strings.TrimSpace(serverURL) == "" {
		return s.removeLocked(s.activeKey)
	}
	id, ok := s.findLocked(instanceID, serverURL)
	if !ok {
		return nil
	}
	return s.removeLocked(serverKeyOf(id.ServerInstanceID, id.ServerURL))
}

// RemoveAll 删除本机保存的全部身份（幂等）。
func (s *identityStore) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = map[string]Identity{}
	s.activeKey = ""
	return s.unlinkLocked()
}

// activeLocked 返回活动身份；活动键失配时退化为唯一那份身份（老版本只会有一份）。
func (s *identityStore) activeLocked() (Identity, bool) {
	if id, ok := s.entries[s.activeKey]; ok {
		return id, true
	}
	if len(s.entries) == 1 {
		for _, id := range s.entries {
			return id, true
		}
	}
	return Identity{}, false
}

// findLocked 按服务端定位身份（严格匹配）：服务端标识直接命中 → 实例 ID → 规范化地址。
//
// 不在这里做"只有一份身份就用它"的兜底：那是**连接路径**的兼容策略（见
// Agent.resolveIdentity）。查询/撤销必须严格，否则界面会把别的服务端的证书显示成本服务端的，
// 撤销 A 也会误判成"撤销掉了 A"。
func (s *identityStore) findLocked(instanceID, serverURL string) (Identity, bool) {
	instance := strings.ToLower(strings.TrimSpace(instanceID))
	url := normalizeServerURL(serverURL)
	if instance == "" && url == "" {
		return s.activeLocked()
	}
	if instance != "" {
		if id, ok := s.entries[instance]; ok {
			return id, true
		}
	}
	if url != "" {
		if id, ok := s.entries[url]; ok {
			return id, true
		}
	}
	// 存储键与查询条件对不上时逐条比对（例如实例 ID 是安装之后才补上的）。
	for _, id := range s.entries {
		if instance != "" && strings.EqualFold(strings.TrimSpace(id.ServerInstanceID), instance) {
			return id, true
		}
	}
	if url != "" {
		for _, id := range s.entries {
			if normalizeServerURL(id.ServerURL) == url {
				return id, true
			}
		}
	}
	return Identity{}, false
}

// removeLocked 删除一个键对应的身份并落盘；删空后直接删除文件。
func (s *identityStore) removeLocked(key string) error {
	delete(s.entries, key)
	if len(s.entries) == 0 {
		s.activeKey = ""
		return s.unlinkLocked()
	}
	if _, ok := s.entries[s.activeKey]; !ok {
		s.activeKey = s.firstKey()
	}
	return s.persistLocked()
}

// unlinkLocked 删除身份文件（幂等）。
func (s *identityStore) unlinkLocked() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent: 删除本地身份失败: %w", err)
	}
	return nil
}

// firstKey 返回按服务端标识排序后的第一个键；无身份时为空。
func (s *identityStore) firstKey() string {
	ids := s.sortedLocked()
	if len(ids) == 0 {
		return ""
	}
	return serverKeyOf(ids[0].ServerInstanceID, ids[0].ServerURL)
}

// sortedLocked 返回按服务端标识排序的身份副本。调用方需持有锁。
func (s *identityStore) sortedLocked() []Identity {
	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Identity, 0, len(keys))
	for _, key := range keys {
		out = append(out, s.entries[key])
	}
	return out
}

// persistLocked 写盘（v2 结构）。调用方需持有锁。
//
// 用"同目录临时文件 + 替换"而不是直接覆盖原文件：身份文件每次写完都会收紧为
// 仅 SYSTEM/Administrators，未提权的进程**无法再直接覆盖它**（Access is denied）。
// 多服务端下反复安装/撤销证书必然要重复写这个文件，因此先写临时文件（继承目录权限）、
// 替换目标、再收紧权限。
func (s *identityStore) persistLocked() error {
	file := persistedIdentities{Version: identityFileVersion, Active: s.activeKey, Identities: s.sortedLocked()}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, identityFileName+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// 临时文件同样含私钥材料：无论成功与否都不留在磁盘上。
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	// 私钥随文件落盘：Windows 下必须显式收紧 ACL，否则同一台机器的其他本地用户也能读到
	// （os.WriteFile 的模式位在 Windows 上不影响 ACL）。失败不阻断安装，但必须留痕。
	if err := restrictFileToAdmins(s.path); err != nil {
		s.logger.Warn("收紧身份文件权限失败（私钥文件可能对其他本地用户可读）", "error", err)
	}
	return nil
}

// validateIdentity 校验身份完整性：PEM 可解析、证书与私钥公钥一致。
func validateIdentity(id *Identity) error {
	if strings.TrimSpace(id.CertPEM) == "" || strings.TrimSpace(id.KeyPEM) == "" {
		return errors.New("identity: 证书或私钥为空")
	}
	cert, err := parseCertificatePEM(id.CertPEM)
	if err != nil {
		return err
	}
	key, err := parseECPrivateKeyPEM(id.KeyPEM)
	if err != nil {
		return err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("identity: 证书公钥不是 ECDSA")
	}
	if !pub.Equal(&key.PublicKey) {
		return errors.New("identity: 证书与私钥不匹配")
	}
	return nil
}

// parseCertificatePEM 解析 PEM 证书。
func parseCertificatePEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("identity: 证书不是合法的 PEM CERTIFICATE")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: 解析证书失败: %w", err)
	}
	return cert, nil
}

// parseECPrivateKeyPEM 解析 PKCS#8 PEM 私钥。
func parseECPrivateKeyPEM(keyPEM string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("identity: 私钥不是合法的 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: 解析私钥失败: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("identity: 私钥不是 ECDSA")
	}
	return key, nil
}

// generateClientKeyAndCSR 本地生成 ECDSA P-256 密钥对与 PKCS#10 CSR（PEM）。
//
// 私钥只在本地生成与保存，CSR 只包含公钥与主体信息。
func generateClientKeyAndCSR(commonName string) (keyPEM, csrPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("agent: 生成客户端私钥失败: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("agent: 序列化客户端私钥失败: %w", err)
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName, Organization: []string{"Vault"}},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return "", "", fmt.Errorf("agent: 生成 CSR 失败: %w", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	csrPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	return keyPEM, csrPEM, nil
}
