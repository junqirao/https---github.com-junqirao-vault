package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// secretAlphabet 是 CHAP 密钥允许的字符集：**纯字母数字**（大小写字母 + 数字）。
//
// ⚠️ 为什么必须纯字母数字：CHAP 密钥要经 PowerShell 命令行传给
// Set-IscsiServerTarget / Connect-IscsiTarget。任何非字母数字字符（标准 base64 的 +/、
// URL 安全 base64 的 -/_）都可能在命令行/脚本参数解析里被破坏，导致服务端存了错密钥、
// 客户端拿着对密钥，登录被拒（真实事故：挂载 connect 阶段失败，换成纯字母数字密钥
// "vault1234567890" 即成功）。纯字母数字在任何 shell/编码下都逐字节不变，最稳妥。
const secretAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// RandomSecret 生成 n 个**字符**的纯字母数字随机密钥（用于 iSCSI CHAP 密钥等）。
// n <= 0 返回错误。
func RandomSecret(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("secret: 随机密钥长度必须为正数")
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secret: 生成随机密钥失败: %w", err)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = secretAlphabet[int(b)%len(secretAlphabet)]
	}
	return string(out), nil
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
