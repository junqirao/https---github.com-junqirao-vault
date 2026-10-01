package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// RandomSecret 生成 n 字节随机数据的 URL 安全 Base64（无 padding）编码。
// 用于 iSCSI CHAP 密钥、enrollment token 等。n <= 0 返回错误。
//
// ⚠️ 为什么必须用 RawURLEncoding（而非 StdEncoding）：CHAP 密钥要经 PowerShell 命令行
// 传给 Set-IscsiServerTarget / Connect-IscsiTarget。标准 base64 会产出 `+` `/` `=`，
// 这些字符在命令行/脚本参数传递里可能被破坏，导致目标侧存了错密钥、客户端拿着对密钥，
// 登录时被拒（真实事故：挂载 connect 阶段失败，换成纯字母数字密钥即成功）。RawURLEncoding
// 用 `-` `_` 替代 `+` `/` 且无 padding，12 字节仍是 16 字符（落在 Windows 12–16 字符、
// Linux LIO 12–16 字节纯 ASCII 的合法区间内），且不含任何命令行敏感字符。
func RandomSecret(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("secret: 随机密钥长度必须为正数")
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secret: 生成随机密钥失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
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
