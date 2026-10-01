package agent

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"vault/internal/updatepkg"
)

// updatePublicKeyBase64 是发布方 Ed25519 公钥（32 字节 base64 标准编码）。
//
// 由构建期注入：
//
//	go build -ldflags "-X vault/internal/agent.updatePublicKeyBase64=<base64>" ./cmd/vault-agent
//
// ⚠️ 只有公钥可注入/入库；私钥仅存在于发布方离线环境（见 cmd/vault-server 的 sign 子命令）。
var updatePublicKeyBase64 = ""

// devUpdatePublicKeyBase64 是内置的开发用公钥。
//
// 它对应的**私钥不存在于本仓库**（生成后即丢弃），因此这个回退值只是"fail-closed"的占位：
// 未注入真实公钥时，任何更新都无法通过验签（绝不会退化成"接受未签名/任意签名"）。
// 联调或正式构建都必须用 -X 注入真实公钥。
const devUpdatePublicKeyBase64 = "nAntw6o2kAYL6hfCLpLHkUceQ6AYEte5IKWtfr525o4="

// updatePublicKey 是当前生效的发布方公钥（构建期注入优先，否则回退开发公钥）。
var updatePublicKey = loadUpdatePublicKey()

// loadUpdatePublicKey 解析注入的公钥；缺失或非法时回退到内置开发公钥。
//
// 此处刻意**不打印日志**：包初始化阶段日志器尚未装配，警告统一由 Agent 构造时输出
// （见 logUpdateKeyStatus），避免提示被写进无人查看的默认输出。
func loadUpdatePublicKey() ed25519.PublicKey {
	if pub, err := updatepkg.PublicKeyFromBase64(updatePublicKeyBase64); err == nil {
		return pub
	}
	dev, err := updatepkg.PublicKeyFromBase64(devUpdatePublicKeyBase64)
	if err != nil {
		// 常量本身非法属于编码错误：返回定长全零公钥（fail-closed，任何签名都无法通过）。
		return make(ed25519.PublicKey, ed25519.PublicKeySize)
	}
	return dev
}

// usingDevUpdatePublicKey 判断当前生效公钥是否就是内置开发公钥。
func usingDevUpdatePublicKey() bool {
	return string(updatePublicKey) == string(mustDevKey())
}

// mustDevKey 返回内置开发公钥（解析失败时返回 nil，不会与任何注入公钥相等）。
func mustDevKey() ed25519.PublicKey {
	pub, err := updatepkg.PublicKeyFromBase64(devUpdatePublicKeyBase64)
	if err != nil {
		return nil
	}
	return pub
}

// UpdatePublicKeyFingerprint 返回当前信任的发布方公钥指纹（SHA-256 前 16 位十六进制）。
//
// 供界面展示"当前信任的发布方"：指纹变化意味着发布方公钥换了（应当人工确认）。
func UpdatePublicKeyFingerprint() string {
	sum := sha256.Sum256(updatePublicKey)
	return hex.EncodeToString(sum[:8])
}

// logUpdateKeyStatus 记录公钥来源；使用开发公钥时必须 WARN 提示生产构建要注入真实公钥。
func (a *Agent) logUpdateKeyStatus() {
	logger := a.logger
	if logger == nil {
		logger = slog.Default()
	}
	injected := strings.TrimSpace(updatePublicKeyBase64) != ""
	if usingDevUpdatePublicKey() || !injected {
		logger.Warn("当前使用开发公钥进行更新验签：任何更新都无法通过验签，生产构建必须注入真实公钥",
			"inject_with", "-X vault/internal/agent.updatePublicKeyBase64=<base64>",
			"fingerprint", UpdatePublicKeyFingerprint())
		return
	}
	logger.Info("更新验签公钥已注入", "fingerprint", UpdatePublicKeyFingerprint())
}
