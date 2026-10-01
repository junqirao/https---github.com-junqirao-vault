package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// RandomSecret 生成 n 字节随机数据并 Base64 编码（用于 iSCSI CHAP 密钥、enrollment token 等）。
// n <= 0 返回错误。
func RandomSecret(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("secret: 随机密钥长度必须为正数")
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secret: 生成随机密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// RandomToken 生成 URL 安全的随机令牌（默认 32 字节）。
// 使用 base64url 无 padding 编码，可直接拼进 URL / 表单，不需要再转义。
func RandomToken(n int) (string, error) {
	if n <= 0 {
		n = 32
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secret: 生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
