package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"vault/internal/apperr"
)

const (
	// defaultCommonName 是 CA 证书的默认 CN。
	defaultCommonName = "Vault Internal CA"
	// defaultCAValidity 是 CA 默认有效期（10 年）。
	defaultCAValidity = 10 * 365 * 24 * time.Hour
	// defaultClientCertValidity 是客户端证书默认有效期（1 年），到期前 30 天应提示续期。
	defaultClientCertValidity = 365 * 24 * time.Hour
	// clockSkew 是校验证书有效期时允许的时钟偏移。
	clockSkew = time.Minute
	// certBackdate 是签发时把 NotBefore 往前推的量，容忍签发方与使用方的时钟差异。
	certBackdate = time.Minute

	// CA 与证书的落盘文件名。
	caCertFileName     = "ca.crt"
	caKeyFileName      = "ca.key"
	serverCertFileName = "server.crt"
	serverKeyFileName  = "server.key"
)

// Config 配置。
type Config struct {
	// Dir CA 与证书的存放目录。为空则不落盘（仅内存，用于测试）。
	Dir string
	// CommonName CA 的 CN，默认 "Vault Internal CA"。
	CommonName string
	// InstanceID 本服务端实例 ID，会写入签发的客户端证书扩展。
	InstanceID string
	// Validity CA 有效期，默认 10 年。
	Validity time.Duration
	// ClientCertValidity 客户端证书默认有效期，默认 1 年。
	ClientCertValidity time.Duration
	// Logger 日志句柄；为空时使用 slog 默认句柄。日志中只出现指纹与序列号，绝不含私钥内容。
	Logger *slog.Logger
}

// CA 是一套自建 CA 的运行期句柄，持有 CA 证书与私钥，用于签发服务端/客户端证书。
//
// CA 私钥是系统信任根：落盘时以 PKCS#8（PEM 类型 "PRIVATE KEY"）写入 ca.key，
// 权限按 0600 设置（Windows 上需部署时配合 ACL，见 writeFileAtomic 说明）。
// CA 构造后其字段只读，可安全并发使用；EnsureServerCert 内部用 mu 串行化签发与落盘。
type CA struct {
	dir            string
	commonName     string
	instanceID     string
	validity       time.Duration
	clientValidity time.Duration
	logger         *slog.Logger

	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
	keyPEM  []byte

	// mu 保护 memServerCert（Dir 为空时的内存缓存），并把落盘场景下的文件读写串行化，
	// 避免并发调用 EnsureServerCert 时互相覆盖 server.crt / server.key。
	mu            sync.Mutex
	memServerCert *serverCert
}

// serverCert 是服务端证书 PEM、有效期与其 SAN 覆盖的主机集合。
type serverCert struct {
	certPEM   []byte
	keyPEM    []byte
	hosts     hostSet
	notBefore time.Time
	notAfter  time.Time
}

// LoadOrCreateCA 加载或创建 CA。
//
// 目录下文件：ca.crt（PEM 证书）、ca.key（PEM PKCS#8 私钥，权限 0600）。
// 已存在则加载；不存在则生成并落盘；只存在其中一个时视为 CA 损坏并返回错误 ——
// 不静默重建，因为静默重建会更换信任根，导致所有客户端失联（安全敏感事件，见 R23）。
func LoadOrCreateCA(cfg Config) (*CA, error) {
	commonName := cfg.CommonName
	if commonName == "" {
		commonName = defaultCommonName
	}
	validity := cfg.Validity
	if validity <= 0 {
		validity = defaultCAValidity
	}
	clientValidity := cfg.ClientCertValidity
	if clientValidity <= 0 {
		clientValidity = defaultClientCertValidity
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ca := &CA{
		dir:            cfg.Dir,
		commonName:     commonName,
		instanceID:     cfg.InstanceID,
		validity:       validity,
		clientValidity: clientValidity,
		logger:         logger,
	}

	if cfg.Dir == "" {
		// 仅内存 CA：不落盘，每次启动都是新的信任根，只适用于测试。
		if err := ca.generate(validity); err != nil {
			return nil, err
		}
		logger.Info("已生成内存 CA（未落盘）", "ca_fingerprint", ca.CAFingerprint())
		return ca, nil
	}

	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("cert: 创建 CA 目录失败: %w", err)
	}
	certPath := filepath.Join(cfg.Dir, caCertFileName)
	keyPath := filepath.Join(cfg.Dir, caKeyFileName)

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	switch {
	case certExists && keyExists:
		if err := ca.load(certPath, keyPath); err != nil {
			return nil, err
		}
		logger.Info("已加载 CA", "ca_fingerprint", ca.CAFingerprint())
	case certExists != keyExists:
		return nil, fmt.Errorf("cert: CA 文件不完整（%s 与 %s 必须成对存在），请人工确认后处理",
			caCertFileName, caKeyFileName)
	default:
		if err := ca.generate(validity); err != nil {
			return nil, err
		}
		// 先写私钥再写证书：任一失败都会留下"不完整"状态，下次启动将显式报错而非静默重建。
		if err := writeFileAtomic(keyPath, ca.keyPEM, 0o600); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(certPath, ca.certPEM, 0o644); err != nil {
			return nil, err
		}
		logger.Info("已生成并落盘 CA", "ca_fingerprint", ca.CAFingerprint())
	}
	return ca, nil
}

// generate 生成一套全新的 CA（ECDSA P-256）。
func (c *CA) generate(validity time.Duration) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("cert: 生成 CA 私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: c.commonName, Organization: []string{"Vault"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("cert: 自签 CA 证书失败: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("cert: 解析自签 CA 证书失败: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("cert: 序列化 CA 私钥失败: %w", err)
	}

	c.cert = cert
	c.key = key
	c.certPEM = encodeCertPEM(der)
	c.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return nil
}

// load 从磁盘加载 CA 证书与 PKCS#8 私钥，并校验二者配套、证书确为 CA。
func (c *CA) load(certPath, keyPath string) error {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("cert: 读取 CA 证书失败: %w", err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return err
	}
	if !cert.IsCA {
		return fmt.Errorf("cert: %s 不是 CA 证书（缺 basicConstraints CA:TRUE）", caCertFileName)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("cert: 读取 CA 私钥失败: %w", err)
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return err
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return fmt.Errorf("cert: CA 证书与私钥不匹配（%s / %s）", caCertFileName, caKeyFileName)
	}

	c.cert = cert
	c.key = key
	c.certPEM = certPEM
	c.keyPEM = keyPEM
	return nil
}

// parsePrivateKeyPEM 解析 PEM 中的 PKCS#8 私钥（本包写出的格式）。
func parsePrivateKeyPEM(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("cert: 私钥 PEM 解析失败（期望 PKCS#8 \"PRIVATE KEY\" 块）")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert: 解析 PKCS#8 私钥失败: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("cert: 私钥类型 %T 不支持签名", key)
	}
	return signer, nil
}

// CAPEM 返回 CA 证书 PEM（用于下发给客户端建立信任，或 TLS 链）。
func (c *CA) CAPEM() []byte {
	out := make([]byte, len(c.certPEM))
	copy(out, c.certPEM)
	return out
}

// CAFingerprint 返回 CA 证书 DER 的 SHA-256（十六进制小写，无分隔符），用于客户端指纹核对（TOFU）。
func (c *CA) CAFingerprint() string {
	return fingerprintSHA256(c.cert)
}

// EnsureServerCert 确保存在可用的服务端证书（HTTPS 用）。
//
// hosts 中的每一项都会写入 SAN：能解析为 IP 的写入 IPAddresses，否则写入 DNSNames。
// 已有证书若仍在使用期内且 SAN 覆盖全部 hosts 则直接复用，否则重新签发
// （hosts 变化即换证书，对应风险 R20：换 IP/换域名后 SAN 不匹配导致 TLS 校验失败）。
//
// Dir 非空时读写 <Dir>/server.crt 与 <Dir>/server.key（私钥 0600）；Dir 为空时在内存中缓存。
// 服务端证书有效期取 CA 有效期（默认 10 年）且不超过 CA 到期时间。
func (c *CA) EnsureServerCert(hosts []string) (certPEM, keyPEM []byte, err error) {
	dnsNames, ips, wanted := splitHosts(hosts)
	if len(wanted) == 0 {
		return nil, nil, apperr.InvalidParam("hosts")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dir == "" {
		if c.memServerCert != nil && c.memServerCert.valid() && c.memServerCert.covers(wanted) {
			return c.memServerCert.certPEM, c.memServerCert.keyPEM, nil
		}
		issued, err := c.issueServerCert(dnsNames, ips, wanted)
		if err != nil {
			return nil, nil, err
		}
		c.memServerCert = issued
		c.logger.Info("已签发服务端证书",
			"cert_fingerprint", fingerprintOfPEM(issued.certPEM),
			"cert_serial", serialOfPEM(issued.certPEM))
		return issued.certPEM, issued.keyPEM, nil
	}

	certPath := filepath.Join(c.dir, serverCertFileName)
	keyPath := filepath.Join(c.dir, serverKeyFileName)
	if fileExists(certPath) && fileExists(keyPath) {
		if issued, err := loadServerCert(certPath, keyPath); err == nil && issued.valid() && issued.covers(wanted) {
			return issued.certPEM, issued.keyPEM, nil
		}
		// 证书损坏 / 已过期 / SAN 不覆盖 → 落到下面重新签发。
	}

	issued, err := c.issueServerCert(dnsNames, ips, wanted)
	if err != nil {
		return nil, nil, err
	}
	if err := writeFileAtomic(keyPath, issued.keyPEM, 0o600); err != nil {
		return nil, nil, err
	}
	if err := writeFileAtomic(certPath, issued.certPEM, 0o644); err != nil {
		return nil, nil, err
	}
	c.logger.Info("已签发服务端证书",
		"cert_fingerprint", fingerprintOfPEM(issued.certPEM),
		"cert_serial", serialOfPEM(issued.certPEM))
	return issued.certPEM, issued.keyPEM, nil
}

// issueServerCert 生成服务端密钥对并签发服务端证书（调用方需持有 c.mu）。
func (c *CA) issueServerCert(dnsNames []string, ips []net.IP, wanted hostSet) (*serverCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("cert: 生成服务端私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	notAfter := now.Add(c.validity)
	if maxNotAfter := c.cert.NotAfter.Add(-certBackdate); notAfter.After(maxNotAfter) {
		notAfter = maxNotAfter
	}

	commonName := ""
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	} else if len(ips) > 0 {
		commonName = ips[0].String()
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Vault"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("cert: 签发服务端证书失败: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("cert: 序列化服务端私钥失败: %w", err)
	}

	return &serverCert{
		certPEM:   encodeCertPEM(der),
		keyPEM:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		hosts:     wanted,
		notBefore: tmpl.NotBefore,
		notAfter:  tmpl.NotAfter,
	}, nil
}

// loadServerCert 从磁盘读取服务端证书与私钥。
func loadServerCert(certPath, keyPath string) (*serverCert, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("cert: 读取服务端证书失败: %w", err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("cert: 读取服务端私钥失败: %w", err)
	}
	if _, err := parsePrivateKeyPEM(keyPEM); err != nil {
		return nil, err
	}

	_, _, hosts := splitHosts(certSANHosts(cert))
	return &serverCert{
		certPEM:   certPEM,
		keyPEM:    keyPEM,
		hosts:     hosts,
		notBefore: cert.NotBefore,
		notAfter:  cert.NotAfter,
	}, nil
}

// valid 判断服务端证书当前是否仍在有效期内（允许一个 clockSkew 的偏移）。
func (s *serverCert) valid() bool {
	now := time.Now()
	return now.Before(s.notAfter.Add(clockSkew)) && now.After(s.notBefore.Add(-clockSkew))
}

// covers 判断该证书的 SAN 是否覆盖请求的主机集合。
func (s *serverCert) covers(wanted hostSet) bool {
	for host := range wanted {
		if _, ok := s.hosts[host]; !ok {
			return false
		}
	}
	return true
}

// certSANHosts 返回证书 SAN 中的全部主机名（DNS 名 + IP 字符串）。
func certSANHosts(cert *x509.Certificate) []string {
	hosts := make([]string, 0, len(cert.DNSNames)+len(cert.IPAddresses))
	hosts = append(hosts, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	return hosts
}

// fingerprintOfPEM 计算 PEM 证书的指纹，仅用于日志。
func fingerprintOfPEM(certPEM []byte) string {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return ""
	}
	return fingerprintSHA256(cert)
}

// serialOfPEM 提取 PEM 证书的序列号（十六进制字符串），仅用于日志。
func serialOfPEM(certPEM []byte) string {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return ""
	}
	return serialString(cert)
}
