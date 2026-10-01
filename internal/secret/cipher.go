// Package secret 提供密钥保险箱与口令哈希能力。
//
// 依据 docs/implementation.md 9.4「密钥保护」：
//   - CHAP 密钥等敏感字段以 AES-256-GCM 加密后落库，日志/审计/API 响应一律过滤；
//   - 管理页口令使用 argon2id（PHC 字符串格式）存储，校验走常数时间比较。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// masterKeySize 是 AES-256 要求的主密钥长度（字节）。
const masterKeySize = 32

// Cipher 提供 AES-256-GCM 加解密，用于 CHAP 密钥等敏感字段落库。
//
// 密文布局为 nonce || ciphertext||tag，nonce 长度由 GCM 决定（标准库为 12 字节）。
// Cipher 构造后不可变，可安全地被多个 goroutine 并发使用。
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher 用 32 字节主密钥构造。长度不符返回错误。
func NewCipher(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != masterKeySize {
		return nil, fmt.Errorf("secret: 主密钥长度必须为 %d 字节，实际为 %d 字节", masterKeySize, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("secret: 构造 AES 密码失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: 构造 GCM 模式失败: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 返回 nonce||ciphertext（含 GCM tag）。空明文返回空切片与 nil。
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return []byte{}, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secret: 生成 nonce 失败: %w", err)
	}
	// Seal 的第一个参数为 dst：复用 nonce 切片前缀，避免额外分配与拷贝。
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 解出明文。数据为空返回空切片。
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}
	nonceSize := c.aead.NonceSize()
	if len(data) < nonceSize {
		return nil, errors.New("secret: 密文长度非法，短于 nonce")
	}
	plaintext, err := c.aead.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		// 不回显密钥/密文内容，仅给出语义化错误（GCM 校验失败通常意味着密钥不匹配或数据被篡改）。
		return nil, fmt.Errorf("secret: 解密失败（密钥不匹配或数据被篡改）: %w", err)
	}
	return plaintext, nil
}

// EncryptString 加密字符串，等价于 Encrypt([]byte(s))。
func (c *Cipher) EncryptString(s string) ([]byte, error) {
	return c.Encrypt([]byte(s))
}

// DecryptString 解密并转为字符串。数据为空返回空字符串与 nil。
func (c *Cipher) DecryptString(data []byte) (string, error) {
	plaintext, err := c.Decrypt(data)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// LoadOrCreateMasterKey 读取或生成主密钥。
//
// current 非空时直接按 base64（标准编码）解析并返回，generated=false；
// 为空时生成 32 字节随机密钥，返回 base64 字符串供调用方写回配置文件，generated=true。
//
// 调用方注意：主密钥是数据保险箱的根密钥，写回配置文件后必须限制文件权限。
func LoadOrCreateMasterKey(current string) (key []byte, encoded string, generated bool, err error) {
	if trimmed := strings.TrimSpace(current); trimmed != "" {
		raw, decErr := base64.StdEncoding.DecodeString(trimmed)
		if decErr != nil {
			return nil, "", false, fmt.Errorf("secret: 主密钥不是合法的 base64: %w", decErr)
		}
		if len(raw) != masterKeySize {
			return nil, "", false, fmt.Errorf("secret: 主密钥长度必须为 %d 字节，实际为 %d 字节", masterKeySize, len(raw))
		}
		return raw, trimmed, false, nil
	}

	raw := make([]byte, masterKeySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", false, fmt.Errorf("secret: 生成主密钥失败: %w", err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw), true, nil
}
