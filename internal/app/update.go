package app

import (
	"os"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
	"vault/internal/updatepkg"
)

// UpdateService 提供"更新分发"能力：把发布目录中的清单、签名与产物**原样**镜像给客户端。
//
// 安全红线（见 docs/implementation.md 7.4.2）——以下约束是刻意设计，不要"优化"掉：
//   - 服务端**不持有**更新签名私钥，也**绝不改写** manifest 的任何字节（哪怕"补一个字段"）：
//     客户端验签覆盖整份字节，任何改写都会让客户端验签失败；
//     因此服务端被攻陷也无法伪造更新，最坏只是拒绝服务；
//   - 服务端**不**自行计算或替换 sha256：摘要只来自已被发布方签名的清单；
//   - 服务端不做版本比较、不做产物挑选：这些判断只应由持有公钥的客户端完成。
type UpdateService struct {
	Deps
}

// UpdateArtifactsDir 返回更新包目录的绝对路径；未配置时返回空串。
//
// 解析规则（重要）：绝对路径原样使用；**相对路径按配置文件所在目录解析**，
// 与 pki/ 目录采用同一套"配置目录"语义（见 cmd/vault-server 中
// filepath.Dir(absConfig)+"/pki"）。原因：服务端常被计划任务/脚本以任意工作目录启动，
// 若相对路径基于进程工作目录解析，"updates" 会被解析到错误位置，客户端表现为
// update.no_manifest（reason=manifest_missing），排查成本高且难以复现。
func (d Deps) UpdateArtifactsDir() string {
	dir := strings.TrimSpace(d.raw().Update.ArtifactsDir)
	if dir == "" {
		return ""
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if base := d.configDir(); base != "" {
		return filepath.Join(base, dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// configDir 返回配置文件所在目录的绝对路径；不可用时返回空串。
//
// 相对路径以配置目录为基准，因此这里对配置路径取绝对化（配置路径本身可能是相对的，
// 此时以其启动时的工作目录为基准，与 config.Load 的行为一致）。
func (d Deps) configDir() string {
	l := d.cfg()
	if l == nil || strings.TrimSpace(l.Path) == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Dir(l.Path))
	if err != nil {
		return ""
	}
	return abs
}

// UpdateChannel 返回服务端配置的分发通道（默认 stable）。
func (d Deps) UpdateChannel() string {
	ch := strings.TrimSpace(d.raw().Update.Channel)
	if ch == "" {
		return "stable"
	}
	return ch
}

// UpdateManifestBytes 返回发布目录中清单文件的**原始字节**。
//
// 只读文件、不做任何解析或改写。channel 非空且与服务端配置的通道不一致时返回 404：
// 服务端一次只镜像一个通道（发布目录里只有一份清单），声明的通道与配置不符时
// 应明确告诉客户端"本服务端不提供该通道"，而不是把另一通道的清单发过去。
func (a *App) UpdateManifestBytes(channel string) ([]byte, error) {
	dir := a.UpdateArtifactsDir()
	if dir == "" {
		return nil, errNoManifest("artifacts_dir_not_configured")
	}
	if want := strings.TrimSpace(channel); want != "" && !strings.EqualFold(want, a.UpdateChannel()) {
		return nil, errNoManifest("channel_unavailable")
	}
	data, err := os.ReadFile(filepath.Join(dir, updatepkg.ManifestFileName))
	if err != nil {
		// 不区分"不存在"与"权限不足"等底层原因，避免向匿名调用者泄露服务端文件系统信息。
		if a.Log != nil {
			a.Log.Debug("读取更新清单失败", "dir", dir, "error", err)
		}
		return nil, errNoManifest("manifest_missing")
	}
	return data, nil
}

// UpdateSignatureBytes 返回发布目录中清单签名文件的原始字节。
func (a *App) UpdateSignatureBytes() ([]byte, error) {
	dir := a.UpdateArtifactsDir()
	if dir == "" {
		return nil, errNoManifest("artifacts_dir_not_configured")
	}
	data, err := os.ReadFile(filepath.Join(dir, updatepkg.SignatureFileName))
	if err != nil {
		if a.Log != nil {
			a.Log.Debug("读取更新签名失败", "dir", dir, "error", err)
		}
		return nil, errNoManifest("signature_missing")
	}
	return data, nil
}

// UpdateArtifactPath 校验并返回产物的绝对路径。
//
// 文件名白名单校验（updatepkg.ValidArtifactName）：只允许 [A-Za-z0-9._-] 且拒绝 "." / ".."，
// 从而杜绝 "../" 之类的路径穿越。校验不通过一律 404（不返回 400，避免暴露服务端校验细节）。
func (a *App) UpdateArtifactPath(filename string) (string, error) {
	dir := a.UpdateArtifactsDir()
	if dir == "" {
		return "", errNoManifest("artifacts_dir_not_configured")
	}
	name := strings.TrimSpace(filename)
	if !updatepkg.ValidArtifactName(name) {
		return "", errNoManifest("artifact_invalid_name")
	}
	path := filepath.Join(dir, updatepkg.ArtifactsDirName, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", errNoManifest("artifact_missing")
	}
	return path, nil
}

// errNoManifest 构造"更新清单/产物不可用"错误（404）。
func errNoManifest(reason string) *apperr.Error {
	return apperr.New(updatepkg.CodeNoManifest, 404).WithArg("reason", reason)
}
