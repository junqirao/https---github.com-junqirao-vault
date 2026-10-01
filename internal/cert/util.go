package cert

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// writeFileAtomic 原子写文件：先写同目录临时文件，fsync 后 os.Rename 覆盖目标。
//
// 目的：避免进程在中途崩溃时留下半截 CA 文件（一半的证书/私钥会让 CA 不可用且难以排查）。
// Windows 上 os.Rename 会覆盖已存在的目标文件；os.CreateTemp 默认权限已是 0600。
//
// ⚠️ 关于权限：Windows 不支持 POSIX 权限位，os.Chmod/perm 在 Windows 上**实际效果有限**
// （只影响只读位，不会阻止其他账户读取）。这里仍按 0600 传参，Unix 上生效；
// Windows 部署时需由部署文档配合 ACL（仅授予服务账户读取权限）来保护 CA 私钥 —— 本包不自行实现 ACL。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cert: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("cert: 写入临时文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("cert: 同步临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cert: 关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("cert: 设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cert: 替换目标文件失败: %w", err)
	}
	tmp = ""
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// randomSerial 生成 128 位随机证书序列号（不使用递增序号，避免被枚举）。
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("cert: 生成证书序列号失败: %w", err)
	}
	if serial.Sign() == 0 {
		serial = big.NewInt(1)
	}
	return serial, nil
}

// parseCertPEM 解析 PEM 中的第一段 CERTIFICATE。
func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	rest := certPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("cert: PEM 中未找到 CERTIFICATE 块")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("cert: 解析 X.509 证书失败: %w", err)
		}
		return cert, nil
	}
}

// encodeCertPEM 把 DER 证书编码为 PEM。
func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// encodePrivateKeyPEM 把 PKCS#8 私钥 DER 编码为 PEM（类型 "PRIVATE KEY"）。
func encodePrivateKeyPEM(keyDER []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// fingerprintSHA256 返回证书 DER 的 SHA-256（十六进制小写、无分隔符）。
func fingerprintSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// spkiSHA256 返回公钥 SubjectPublicKeyInfo DER 的 SHA-256（十六进制小写、无分隔符）。
//
// ★ 身份绑定与续期比对必须用该值：续期保留密钥对 → SPKI 不变 → 无需重新审批。
func spkiSHA256(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("cert: 序列化公钥（SPKI）失败: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// serialString 返回证书序列号的十六进制小写表示（与 IssuedCert.Serial 保持一致）。
func serialString(cert *x509.Certificate) string {
	return strings.ToLower(cert.SerialNumber.Text(16))
}

// publicKeysEqual 比较两把公钥是否相同（用于校验证书与私钥是否配套）。
func publicKeysEqual(a, b any) bool {
	derA, errA := x509.MarshalPKIXPublicKey(a)
	derB, errB := x509.MarshalPKIXPublicKey(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(derA, derB)
}

// hostSet 是规范化后的主机名集合，用于判断证书 SAN 是否已覆盖请求的 hosts。
type hostSet map[string]struct{}

// splitHosts 把 hosts 拆分为 SAN 所需的 DNS 名与 IP，并做去重与规范化。
// 能解析为 IP 的写入 IPAddresses，否则写入 DNSNames（见 9.1 约束 4：域名 + 固定 IP 都要有）。
func splitHosts(hosts []string) (dnsNames []string, ips []net.IP, normalized hostSet) {
	normalized = make(hostSet, len(hosts))
	for _, raw := range hosts {
		host := strings.TrimSpace(raw)
		if host == "" {
			continue
		}
		// IP 用规范形式（net.IP.String()）作为去重键，避免 IPv6 的等价写法被当成不同主机。
		if ip := net.ParseIP(host); ip != nil {
			key := ip.String()
			if _, ok := normalized[key]; ok {
				continue
			}
			normalized[key] = struct{}{}
			ips = append(ips, ip)
			continue
		}
		key := strings.ToLower(host)
		if _, ok := normalized[key]; ok {
			continue
		}
		normalized[key] = struct{}{}
		dnsNames = append(dnsNames, host)
	}
	return dnsNames, ips, normalized
}
