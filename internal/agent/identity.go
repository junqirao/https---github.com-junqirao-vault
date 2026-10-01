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
	"strings"
	"sync"
	"time"
)

// identityFileName 是代理本地身份文件名；与状态文件同目录（见 identityPathFor）。
const identityFileName = "identity.json"

// identityPathFor 返回与给定状态文件同目录的身份文件路径。
func identityPathFor(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), identityFileName)
}

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
	Installed         bool   `json:"installed"`
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

// identityStore 负责本地身份的读写与校验。身份只在内存与本地文件间流转。
type identityStore struct {
	path   string
	logger *slog.Logger

	mu      sync.Mutex
	current *Identity
}

// newIdentityStore 加载本地身份；文件不存在或校验失败时视为"未安装"（不阻断启动）。
func newIdentityStore(path string, logger *slog.Logger) *identityStore {
	if logger == nil {
		logger = slog.Default()
	}
	s := &identityStore{path: path, logger: logger}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			logger.Warn("本地身份文件无法解析，已视为未安装", "path", path)
			return s
		}
		if err := validateIdentity(&id); err != nil {
			// 只记录错误类型，绝不回显可能包含密钥材料的原始内容。
			logger.Warn("本地身份文件校验失败，已视为未安装", "path", path, "reason", err.Error())
			return s
		}
		s.current = &id
	case os.IsNotExist(err):
	default:
		logger.Warn("读取本地身份文件失败，已视为未安装", "path", path, "error", err)
	}
	return s
}

// Path 返回身份文件路径。
func (s *identityStore) Path() string { return s.path }

// Get 返回身份副本；未安装时 ok=false。
func (s *identityStore) Get() (Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return Identity{}, false
	}
	return *s.current, true
}

// Meta 返回对外元数据；未安装时 installed=false。
func (s *identityStore) Meta() identityMeta {
	if id, ok := s.Get(); ok {
		return id.meta()
	}
	return identityMeta{Installed: false}
}

// Install 校验并持久化身份（写前 MkdirAll，文件 0600）。
func (s *identityStore) Install(id *Identity) error {
	if id == nil {
		return errors.New("agent: 身份不能为空")
	}
	if err := validateIdentity(id); err != nil {
		return err
	}
	if id.InstalledAt == 0 {
		id.InstalledAt = time.Now().UnixMilli()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(id); err != nil {
		return err
	}
	cp := *id
	s.current = &cp
	return nil
}

// Remove 删除本地身份文件（幂等）。
func (s *identityStore) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = nil
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("agent: 删除本地身份失败: %w", err)
	}
	return nil
}

// persistLocked 写盘。调用方需持有锁。
func (s *identityStore) persistLocked(id *Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
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
