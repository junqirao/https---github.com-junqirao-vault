// Package updatepkg 定义客户端自更新「发布包」的格式与验签逻辑。
//
// 设计（见 docs/implementation.md 7.4 / 7.4.1 / 7.4.2）：
//   - 发布包 = manifest.json（清单） + manifest.json.sig（Ed25519 签名） + artifacts/（产物）；
//   - **统一由发布方私钥签名**，服务端只做 mirror（存储与分发），绝不持有私钥、绝不改写清单；
//   - 客户端内置发布方公钥，**先对原始字节验签，再解析**——解析后的任何字段都不可信，
//     因此顺序不可调换（先解析再验签会引入解析器层面的攻击面）；
//   - 校验链：签名 → SHA256（由调用方比对）→ 版本单调性（由调用方判定）→ 服务端兼容性（由调用方判定）。
//
// 本包同时被服务端（只读镜像，不解析）与客户端（验签）使用，不引入任何第三方依赖。
package updatepkg

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"vault/internal/apperr"
	"vault/internal/semver"
)

// SchemaVersion 是当前支持的清单结构版本。
//
// 客户端只在 schema 完全相等时才接受清单：结构变了就必须连客户端一起发版，
// 否则新字段的语义（尤其是安全相关字段）会被旧客户端静默忽略。
const SchemaVersion = 1

// 发布目录中的固定布局（发布工具产出、服务端 mirror、客户端消费都必须使用同一套名字）。
const (
	// ManifestFileName 清单文件名。
	ManifestFileName = "manifest.json"
	// SignatureFileName 清单签名文件名。
	SignatureFileName = "manifest.json.sig"
	// ArtifactsDirName 产物子目录名。
	ArtifactsDirName = "artifacts"
)

// 更新相关的稳定错误码（对外只暴露错误码，文案由前端 i18n 翻译）。
const (
	// CodeBadSignature 签名校验失败（疑似篡改）：必须硬失败，绝不允许降级为"忽略签名继续"。
	CodeBadSignature = "update.bad_signature"
	// CodeBadManifest 清单格式非法（schema / 必填字段 / 通道 / 版本号 / 摘要格式）。
	CodeBadManifest = "update.bad_manifest"
	// CodeNoManifest 服务端未配置或未找到更新清单（仅服务端 mirror 使用）。
	CodeNoManifest = "update.no_manifest"
)

// ArtifactKind 产物类型。
type ArtifactKind string

// 支持的产物类型。
const (
	// KindAgent Go 代理（Vault-Agent.exe）。
	KindAgent ArtifactKind = "agent"
	// KindClient Electron 客户端安装包。
	KindClient ArtifactKind = "client"
	// KindClientWeb Electron 渲染层资源包（Vite 产出的 dist/，index.html + assets/）。
	//
	// 与 KindClient 区分：KindClient 是"Electron 本体安装包"（替换整个客户端），
	// KindClientWeb 只热更渲染层静态资源，不替换 Electron 本体。它不绑定平台，
	// 因此校验时**不要求** os/arch。
	KindClientWeb ArtifactKind = "client_web"
)

// artifactKindOrder 是清单中产物类型的固定顺序，保证同一批输入产出逐字节一致的清单。
var artifactKindOrder = []ArtifactKind{KindAgent, KindClient, KindClientWeb}

// ArtifactKindOrder 返回产物类型的固定顺序（供发布工具按稳定顺序写清单）。
func ArtifactKindOrder() []ArtifactKind {
	out := make([]ArtifactKind, len(artifactKindOrder))
	copy(out, artifactKindOrder)
	return out
}

// ValidArtifactKind 判断是否为受支持的产物类型。
func ValidArtifactKind(k ArtifactKind) bool {
	for _, known := range artifactKindOrder {
		if k == known {
			return true
		}
	}
	return false
}

// Artifact 是单个产物的元数据。
type Artifact struct {
	// Kind 产物类型（agent|client|client_web）。
	Kind ArtifactKind `json:"kind"`
	// OS 目标操作系统（固定 windows）。
	OS string `json:"os"`
	// Arch 目标架构（如 amd64）。
	Arch string `json:"arch"`
	// Filename 产物文件名（不含目录）。
	Filename string `json:"filename"`
	// Size 字节数。
	Size int64 `json:"size"`
	// SHA256 内容的十六进制小写摘要（64 字符）。
	SHA256 string `json:"sha256"`
	// URL 相对路径，如 artifacts/Vault-Agent.exe。
	URL string `json:"url"`
}

// Manifest 是发布清单。
type Manifest struct {
	// Schema 结构版本，必须等于 SchemaVersion。
	Schema int `json:"schema"`
	// Channel 分发通道（stable|beta）。
	Channel string `json:"channel"`
	// Version 目标版本号（semver）。
	Version string `json:"version"`
	// ReleasedAt 发布时间（毫秒时间戳）。
	ReleasedAt int64 `json:"released_at"`
	// Notes 更新说明（可含换行）。
	Notes string `json:"notes,omitempty"`
	// MinSupportedVersion 允许直接升级的最低当前版本（留空表示不限制）。
	MinSupportedVersion string `json:"min_supported_version,omitempty"`
	// Artifacts 产物列表。
	Artifacts []Artifact `json:"artifacts"`
}

// FindArtifact 按 kind + os + arch 选取产物；找不到返回 nil。
//
// os / arch 比较不区分大小写；manifest 中同一产物只应出现一次。
func (m *Manifest) FindArtifact(kind ArtifactKind, goos, goarch string) *Artifact {
	if m == nil {
		return nil
	}
	for i := range m.Artifacts {
		a := &m.Artifacts[i]
		if a.Kind != kind {
			continue
		}
		if !strings.EqualFold(a.OS, goos) || !strings.EqualFold(a.Arch, goarch) {
			continue
		}
		return a
	}
	return nil
}

// FindArtifactKind 按 kind 选取第一个产物（忽略 os/arch）；找不到返回 nil。
//
// 用于平台无关的产物（如 client_web 渲染层资源包）：这类产物不绑定 os/arch，
// 用 FindArtifact 会因 os/arch 不匹配而取不到。
func (m *Manifest) FindArtifactKind(kind ArtifactKind) *Artifact {
	if m == nil {
		return nil
	}
	for i := range m.Artifacts {
		if m.Artifacts[i].Kind == kind {
			return &m.Artifacts[i]
		}
	}
	return nil
}

// CanonicalBytes 返回用于签名的规范化字节。
//
// 实现上**不做任何 JSON 重排**，只裁掉首尾空白：任何"规范化"重排（字段排序、去空格）
// 都会让签名与分发各自引入一份实现，两边一旦有差异就会出现"签名正确但验签失败"的幽灵问题。
// 因此约定：**发布工具必须对同一份字节签名与分发**（Vault-Server sign 直接签 manifest.json 的
// 文件内容），客户端也直接对收到的原始字节验签。
func CanonicalBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// Sign 用 Ed25519 私钥对 manifest 的规范化 JSON 字节签名，返回 base64 签名。
func Sign(priv ed25519.PrivateKey, manifestBytes []byte) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("updatepkg: 私钥长度非法")
	}
	sig := ed25519.Sign(priv, CanonicalBytes(manifestBytes))
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifyAndParse 是客户端唯一可信入口。
//
// 顺序（安全关键，不可调换）：
//  1. 用 pub 对 **raw 字节** 验签（必须在解析之前）；
//  2. 验签通过后才 Unmarshal；
//  3. 校验 schema、必填字段、channel 匹配、版本号可解析、artifact 的 sha256 形如 64 位十六进制。
//
// 验签失败返回 403 update.bad_signature；格式非法返回 422 update.bad_manifest。
// expectChannel 为空串表示不校验通道。
func VerifyAndParse(pub ed25519.PublicKey, manifestBytes []byte, signatureB64 string, expectChannel string) (*Manifest, error) {
	// ---- ① 先验签（对原始字节，解析前）----
	if len(pub) != ed25519.PublicKeySize {
		return nil, apperr.New(CodeBadSignature, 403).WithArg("reason", "bad_public_key")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureB64))
	if err != nil {
		return nil, apperr.New(CodeBadSignature, 403).WithArg("reason", "bad_signature_encoding").WithCause(err)
	}
	if !ed25519.Verify(pub, CanonicalBytes(manifestBytes), sig) {
		return nil, apperr.New(CodeBadSignature, 403).WithArg("reason", "signature_mismatch")
	}

	// ---- ② 验签通过后才解析 ----
	var m Manifest
	if err := json.Unmarshal(CanonicalBytes(manifestBytes), &m); err != nil {
		return nil, badManifest("json", err)
	}

	// ---- ③ 结构与语义校验 ----
	if err := m.validate(expectChannel); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate 校验清单的必填字段与取值合法性。
func (m *Manifest) validate(expectChannel string) error {
	if m.Schema != SchemaVersion {
		return badManifest("schema", fmt.Errorf("期望 %d，实际 %d", SchemaVersion, m.Schema))
	}
	if strings.TrimSpace(m.Channel) == "" {
		return badManifest("channel", fmt.Errorf("通道不能为空"))
	}
	if expectChannel != "" && !strings.EqualFold(strings.TrimSpace(m.Channel), strings.TrimSpace(expectChannel)) {
		return badManifest("channel", fmt.Errorf("通道不匹配"))
	}
	if strings.TrimSpace(m.Version) == "" {
		return badManifest("version", fmt.Errorf("版本号不能为空"))
	}
	if _, err := semver.Parse(m.Version); err != nil {
		return badManifest("version", err)
	}
	if len(m.Artifacts) == 0 {
		return badManifest("artifacts", fmt.Errorf("产物列表为空"))
	}
	for i := range m.Artifacts {
		if err := m.Artifacts[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// artifactNamePattern 限制产物文件名：只允许 ASCII 字母数字与 . _ -，
// 从而在客户端侧同时杜绝路径分隔符与 ".."（下载落地路径由该名字拼出）。
var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// sha256Pattern 匹配十六进制小写 64 位摘要。
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validate 校验单个产物的必填字段。
func (a *Artifact) validate() error {
	if !ValidArtifactKind(a.Kind) {
		return badManifest("artifact.kind", fmt.Errorf("未知产物类型 %q", string(a.Kind)))
	}
	// client_web（渲染层资源包）平台无关：不要求 os/arch；其余产物必须声明目标平台。
	if a.Kind != KindClientWeb {
		if strings.TrimSpace(a.OS) == "" || strings.TrimSpace(a.Arch) == "" {
			return badManifest("artifact.os_arch", fmt.Errorf("%s 缺少 os/arch", string(a.Kind)))
		}
	}
	if !ValidArtifactName(a.Filename) {
		return badManifest("artifact.filename", fmt.Errorf("非法文件名 %q", a.Filename))
	}
	if a.Size < 0 {
		return badManifest("artifact.size", fmt.Errorf("大小非法"))
	}
	if !strings.EqualFold(filepathBase(a.URL), a.Filename) {
		// URL 只允许指向同名文件（发布工具写 artifacts/<filename>），避免清单里出现
		// 与 Filename 不一致的路径（客户端按 Filename 拼接下载地址）。
		return badManifest("artifact.url", fmt.Errorf("url 与 filename 不一致"))
	}
	if strings.Contains(a.URL, "..") || strings.ContainsAny(a.URL, `\`) || strings.HasPrefix(a.URL, "/") {
		return badManifest("artifact.url", fmt.Errorf("url 非法"))
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		return badManifest("artifact.sha256", fmt.Errorf("摘要必须为 64 位十六进制小写"))
	}
	return nil
}

// filepathBase 返回路径的最后一段（只按 '/' 与 '\\' 切分，避免引入 path/filepath 的平台差异）。
func filepathBase(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ValidArtifactName 判断产物文件名是否合法（白名单校验）。
//
// 客户端与服务端都必须用它校验文件名：服务端用它防路径穿越，
// 客户端用它确保"清单里的名字"不会被用来写出 update 目录之外。
func ValidArtifactName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if !artifactNamePattern.MatchString(name) {
		return false
	}
	return true
}

// badManifest 构造清单格式错误（422）。
func badManifest(field string, cause error) error {
	e := apperr.New(CodeBadManifest, 422).WithArg("field", field)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return e
}

// PublicKeyFromBase64 解码 base64 标准编码的 Ed25519 公钥（32 字节）。
func PublicKeyFromBase64(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("updatepkg: 公钥 base64 解码失败: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("updatepkg: 公钥长度非法（期望 %d 字节，实际 %d 字节）", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// PublicKeyToBase64 编码 Ed25519 公钥为 base64 标准编码。
func PublicKeyToBase64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}
