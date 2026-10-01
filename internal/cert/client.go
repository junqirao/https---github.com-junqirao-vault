package cert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/http"
	"time"

	"vault/internal/apperr"
)

// ClientCertRequest 签发客户端证书的请求。
type ClientCertRequest struct {
	// UserID 证书绑定的用户 ID，必须非空，会写入自定义扩展。
	UserID string
	// Username 用户名，CommonName 为空时用它作为 CN。
	Username string
	// CommonName 证书 CN，为空时用 Username。
	CommonName string
	// InstanceID 写入自定义扩展；为空时用 CA 配置中的 InstanceID。
	InstanceID string
	// NotAfter 为空时用配置的 ClientCertValidity。
	NotAfter time.Time
}

// IssuedCert 签发结果。
type IssuedCert struct {
	CertPEM []byte
	// KeyPEM 私钥 PEM（PKCS#8）。
	//
	// ⚠️ RenewClientCert 复用调用方原有的公钥，不产生新私钥，且本包拿不到原私钥，
	// 因此续期时该字段为 nil —— 调用方需自行保留原私钥，本方法只更新证书。
	KeyPEM []byte
	// Serial 证书序列号（十六进制小写）。
	Serial string
	// FingerprintSHA256 证书 DER 的 SHA-256，十六进制小写无分隔。
	FingerprintSHA256 string
	// SPKISHA256 公钥 SubjectPublicKeyInfo 的 SHA-256，十六进制小写无分隔。
	// ★ 身份绑定与续期比对必须用该值。
	SPKISHA256 string
	// Subject 证书主体（pkix.Name 的字符串形式）。
	Subject string
	// NotBefore 生效时间。
	NotBefore time.Time
	// NotAfter 失效时间。
	NotAfter time.Time
}

// ParsedCert 是解析结果。
type ParsedCert struct {
	Serial            string
	FingerprintSHA256 string
	SPKISHA256        string
	Subject           string
	CommonName        string
	NotBefore         time.Time
	NotAfter          time.Time
	// InstanceID 从自定义扩展中解析出的服务端实例 ID；不存在时为空。
	InstanceID string
	// UserID 从自定义扩展中解析出的用户 ID；不存在时为空。
	UserID string
}

// IssueClientCert 为客户端签发全新证书（新密钥对，ECDSA P-256）。
func (c *CA) IssueClientCert(req ClientCertRequest) (*IssuedCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("cert: 生成客户端私钥失败: %w", err)
	}
	tmpl, err := c.clientCertTemplate(req)
	if err != nil {
		return nil, err
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("cert: 签发客户端证书失败: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("cert: 序列化客户端私钥失败: %w", err)
	}

	issued, err := c.newIssuedCert(der, encodePrivateKeyPEM(keyDER))
	if err != nil {
		return nil, err
	}
	c.logger.Info("已签发客户端证书",
		"cert_serial", issued.Serial,
		"cert_fingerprint", issued.FingerprintSHA256,
		"spki_fingerprint", issued.SPKISHA256)
	return issued, nil
}

// RenewClientCert 用**已有证书中的公钥**签发新证书，实现"保留密钥对、SPKI 不变"。
//
// existingCertPEM 必须是本 CA 签发的客户端证书，否则返回错误（用 CheckSignatureFrom 校验签名，
// 不做有效期校验，因此已过期证书也能正常续期）。
//
// 由于续期不产生新私钥，返回值的 KeyPEM 为空 —— 调用方需自行保留原私钥，本方法只更新证书。
func (c *CA) RenewClientCert(req ClientCertRequest, existingCertPEM []byte) (*IssuedCert, error) {
	existing, err := parseCertPEM(existingCertPEM)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(existing.RawIssuer, c.cert.RawSubject) {
		return nil, errors.New("cert: 待续期证书的签发者不是本 CA")
	}
	if err := existing.CheckSignatureFrom(c.cert); err != nil {
		return nil, fmt.Errorf("cert: 待续期证书并非本 CA 签发: %w", err)
	}
	oldSPKI, err := spkiSHA256(existing.PublicKey)
	if err != nil {
		return nil, err
	}

	tmpl, err := c.clientCertTemplate(req)
	if err != nil {
		return nil, err
	}
	// 复用旧证书的公钥 → 新证书的 SPKI 与旧证书完全一致。
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, existing.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("cert: 续期客户端证书失败: %w", err)
	}

	issued, err := c.newIssuedCert(der, nil)
	if err != nil {
		return nil, err
	}
	c.logger.Info("已续期客户端证书（保留原密钥对，SPKI 不变）",
		"cert_serial", issued.Serial,
		"cert_fingerprint", issued.FingerprintSHA256,
		"previous_spki_fingerprint", oldSPKI,
		"spki_fingerprint", issued.SPKISHA256)
	return issued, nil
}

// clientCertTemplate 构造客户端证书模板（含自定义扩展，不含密钥）。
func (c *CA) clientCertTemplate(req ClientCertRequest) (*x509.Certificate, error) {
	if req.UserID == "" {
		return nil, apperr.InvalidParam("user_id")
	}
	commonName := req.CommonName
	if commonName == "" {
		commonName = req.Username
	}
	if commonName == "" {
		return nil, apperr.InvalidParam("common_name")
	}

	now := time.Now()
	notAfter := req.NotAfter
	if notAfter.IsZero() {
		notAfter = now.Add(c.clientValidity)
	} else if !notAfter.After(now) {
		return nil, apperr.InvalidParam("not_after")
	}
	if notAfter.After(c.cert.NotAfter) {
		// 不允许签发出比 CA 更长寿的证书，否则 CA 过期后证书仍然"有效"会让人误判。
		return nil, apperr.InvalidParam("not_after")
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	instanceID := req.InstanceID
	if instanceID == "" {
		instanceID = c.instanceID
	}

	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Vault"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		// 自定义扩展：服务端实例 ID + 用户 ID，均非 Critical（见 oid.go 说明）。
		ExtraExtensions: buildExtensions(instanceID, req.UserID),
	}, nil
}

// newIssuedCert 由签发出的 DER 组装 IssuedCert。
func (c *CA) newIssuedCert(der, keyPEM []byte) (*IssuedCert, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("cert: 解析签发的证书失败: %w", err)
	}
	spki, err := spkiSHA256(cert.PublicKey)
	if err != nil {
		return nil, err
	}
	return &IssuedCert{
		CertPEM:           encodeCertPEM(der),
		KeyPEM:            keyPEM,
		Serial:            serialString(cert),
		FingerprintSHA256: fingerprintSHA256(cert),
		SPKISHA256:        spki,
		Subject:           cert.Subject.String(),
		NotBefore:         cert.NotBefore,
		NotAfter:          cert.NotAfter,
	}, nil
}

// Parse 解析 PEM 证书（不做信任校验）。
func Parse(certPEM []byte) (*ParsedCert, error) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	return buildParsedCert(cert)
}

// VerifyClientCert 校验客户端证书：
//
//	① 由 caPEM 签发；
//	② 在有效期内（允许 1 分钟时钟偏移）；
//	③ 证书用途包含 ClientAuth；
//	④ 若 requireInstanceID 非空，则证书扩展中的 instance_id 必须等于它，
//	   否则返回 apperr.CertNotForThisServer()（扩展仅用于校验，见 9.1）。
//
// 校验顺序保证：先验签、再读扩展 —— 不信任未经验签的扩展内容。
func VerifyClientCert(caPEM, certPEM []byte, requireInstanceID string) (*ParsedCert, error) {
	caCert, err := parseCertPEM(caPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	if !hasExtKeyUsage(leaf, x509.ExtKeyUsageClientAuth) {
		return nil, apperr.New("auth.cert_invalid", http.StatusUnauthorized).
			WithArg("reason", "missing_client_auth_eku")
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	opts := x509.VerifyOptions{
		Roots:       roots,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		CurrentTime: time.Now(),
	}
	if _, err := leaf.Verify(opts); err != nil {
		if err := verifyWithClockSkew(leaf, opts, err); err != nil {
			return nil, mapVerifyError(err)
		}
	}

	parsed, err := buildParsedCert(leaf)
	if err != nil {
		return nil, err
	}
	if requireInstanceID != "" && parsed.InstanceID != requireInstanceID {
		return nil, apperr.CertNotForThisServer()
	}
	return parsed, nil
}

// buildParsedCert 由已解析的证书构造 ParsedCert（含自定义扩展解析）。
func buildParsedCert(cert *x509.Certificate) (*ParsedCert, error) {
	instanceID, err := extensionString(cert.Extensions, oidServerInstanceID)
	if err != nil {
		return nil, err
	}
	userID, err := extensionString(cert.Extensions, oidUserID)
	if err != nil {
		return nil, err
	}
	spki, err := spkiSHA256(cert.PublicKey)
	if err != nil {
		return nil, err
	}
	return &ParsedCert{
		Serial:            serialString(cert),
		FingerprintSHA256: fingerprintSHA256(cert),
		SPKISHA256:        spki,
		Subject:           cert.Subject.String(),
		CommonName:        cert.Subject.CommonName,
		NotBefore:         cert.NotBefore,
		NotAfter:          cert.NotAfter,
		InstanceID:        instanceID,
		UserID:            userID,
	}, nil
}

// verifyWithClockSkew 在证书仅因时间原因校验失败时，用 ±1 分钟的时间再试一次，
// 容忍服务端与客户端之间的时钟偏移；其他失败原因不做重试。
func verifyWithClockSkew(cert *x509.Certificate, opts x509.VerifyOptions, verifyErr error) error {
	var invalidErr x509.CertificateInvalidError
	if !errors.As(verifyErr, &invalidErr) || invalidErr.Reason != x509.Expired {
		return verifyErr
	}
	base := opts.CurrentTime
	for _, delta := range []time.Duration{-clockSkew, clockSkew} {
		opts.CurrentTime = base.Add(delta)
		if _, err := cert.Verify(opts); err == nil {
			return nil
		}
	}
	return verifyErr
}

// mapVerifyError 把标准库校验错误映射为业务错误码。
func mapVerifyError(err error) error {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return apperr.New("auth.cert_unknown_authority", http.StatusUnauthorized).WithCause(err)
	}
	return apperr.New("auth.cert_invalid", http.StatusUnauthorized).WithCause(err)
}

// hasExtKeyUsage 判断证书用途中是否包含指定项目。
func hasExtKeyUsage(cert *x509.Certificate, usage x509.ExtKeyUsage) bool {
	for _, u := range cert.ExtKeyUsage {
		if u == usage || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}
