package cert

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// IssueClientCertFromCSR 用客户端提供的 CSR 签发证书。
//
// 这是 enrollment 的**正确姿势**：私钥始终留在客户端，服务端只对公钥签发证书，
// 避免私钥经网络传输（见 docs/implementation.md 9.1「签发」环节）。
//
// 安全约定：证书 Subject / 有效期 / 服务端实例扩展等全部以**服务端传入的 req 为准**，
// 不采用 CSR 中的 Subject —— 防止客户端伪造身份字段。
//
// 私钥在客户端侧，服务端不持有，因此返回值的 KeyPEM 为空。
func (c *CA) IssueClientCertFromCSR(req ClientCertRequest, csrPEM []byte) (*IssuedCert, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, errors.New("cert: CSR 不是合法的 PEM")
	}
	if block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("cert: CSR 的 PEM 类型应为 CERTIFICATE REQUEST，实际为 %q", block.Type)
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cert: 解析 CSR 失败: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("cert: CSR 自签名校验失败: %w", err)
	}

	tmpl, err := c.clientCertTemplate(req)
	if err != nil {
		return nil, err
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("cert: 签发客户端证书失败: %w", err)
	}

	return c.newIssuedCert(der, nil)
}
