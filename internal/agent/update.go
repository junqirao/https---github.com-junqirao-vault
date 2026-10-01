package agent

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vault/internal/apperr"
	"vault/internal/semver"
	"vault/internal/updatepkg"
	"vault/internal/version"

	"golang.org/x/sys/windows"
)

// 自更新相关常量。
const (
	// updateDirName 更新工作目录名（相对于代理数据目录，默认 %ProgramData%\Vault\update）。
	updateDirName = "update"
	// stagedDirName 暂存子目录名。
	stagedDirName = "staged"
	// backupDirName 备份子目录名。
	backupDirName = "backup"
	// pendingFileName 待确认的更新记录文件名。
	pendingFileName = "pending.json"
	// applyUpdateSubcommand 是 agent 的"替换器"子命令名（临时副本进程以它启动）。
	applyUpdateSubcommand = "apply-update"
	// partSuffix 下载中的临时后缀（下载完成后改名去掉它）。
	partSuffix = ".part"
	// downloadAttempts 下载最大尝试次数（含首次）。
	downloadAttempts = 3
	// maxUpdateArtifactBytes 单个产物的下载上限（防止镜像端无限流式响应打满磁盘）。
	maxUpdateArtifactBytes = 2 << 30
	// rollbackAttemptLimit 连续失败多少次后回滚。
	rollbackAttemptLimit = 3
	// updateTargetOS 自更新只支持 Windows。
	updateTargetOS = "windows"
	// authenticodeTimeout Authenticode 校验的超时。
	authenticodeTimeout = 30 * time.Second
	// updaterExitDelay 保证替换器副本已获得进程句柄后才退出（避免 pid 复用误判）。
	updaterExitDelay = 500 * time.Millisecond

	// ---- 渲染层资源热更（client_web）----
	// webappDirName 渲染层资源根目录名（相对于代理数据目录，即 %ProgramData%\Vault\webapp）。
	// 这是与 Electron 主进程的**冻结契约**：主进程据此目录加载 index.html，不要改名。
	webappDirName = "webapp"
	// webCurrentFileName 激活指针文件名（<webapp>/current.json）。
	webCurrentFileName = "current.json"
	// webIndexFileName 渲染层入口文件；解压后必须存在它才算有效产物。
	webIndexFileName = "index.html"
	// webTempPrefix 解压临时目录后缀前缀（<version>.tmp-<random>）；失败/取消时整体清理。
	webTempPrefix = ".tmp-"
	// webVersionsKept 激活后保留的历史版本目录数（更旧的删除，避免无限堆积）。
	webVersionsKept = 2
	// webExtractSlackBytes 解压总大小上限的固定余量（1 MiB）。
	webExtractSlackBytes = 1 << 20
	// webExtractSizeRatioNum/Den 解压总大小上限系数：产物声明 size 的 1.5 倍。
	//
	// 依据：zip 解压后总体积会大于压缩包，但正常 Vite 产物（index.html + assets/*.js/css）
	// 的膨胀比例有限，1.5x + 1MiB 足以容纳任何真实产物；同时为损坏/恶意 zip（zip bomb）
	// 设定硬上界，避免把客户端磁盘写满。
	webExtractSizeRatioNum = 3
	webExtractSizeRatioDen = 2
)

// UpdateInfo 是自更新状态（GET /agent/state 的 update 字段）。
type UpdateInfo struct {
	Channel          string `json:"channel"`
	CurrentVersion   string `json:"current_version"`
	AvailableVersion string `json:"available_version,omitempty"`
	Downloading      bool   `json:"downloading"`
	// ReceivedBytes / TotalBytes 是自更新产物下载进度（向前兼容的新增字段）。
	ReceivedBytes int64 `json:"received_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
	// Reason 是最近一次"无可用更新"的稳定原因码（no_manifest / server_unreachable / not_newer / ...）。
	Reason string `json:"reason,omitempty"`
	// ServerReason 是更新源（服务端）返回的具体原因（如 manifest_missing / artifacts_dir_not_configured），
	// 与 Reason 一起用于排障"为什么没有更新"（见 docs/agent-api.md 自更新章节）。
	ServerReason string `json:"server_reason,omitempty"`
}

// BreakServer 描述"升级后将无法连接的服务端"（见 docs/implementation.md 7.4.3）。
//
// 出现该字段时前端必须让用户确认：升级会中断与这些服务端的协作，直到其管理员升级服务端。
type BreakServer struct {
	// ServerName 服务端展示名。
	ServerName string `json:"server_name"`
	// Max 该服务端支持的客户端版本上界。
	Max string `json:"max"`
}

// UpdateCheckResult 是 POST /agent/update/check 的响应。
//
// 字段与前端 frontend/packages/ui/src/api/agentTypes.ts 的 UpdateInfo 对齐
// （available / version / notes / size_bytes / source）；其余为排障与展示用的扩展字段
// （前端未声明不会报错，但也不应依赖）。
type UpdateCheckResult struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Notes     string `json:"notes"`
	SizeBytes int64  `json:"size_bytes"`
	// Source 当前更新源（服务端展示名），供设置页展示"当前更新源"。
	Source string `json:"source"`
	// Reason 不可用时的稳定原因码（server_unreachable / not_newer / no_artifact / no_manifest / ...）。
	Reason string `json:"reason,omitempty"`
	// ServerCode / ServerReason / ServerArgs 原样保留更新源（服务端）的结构化错误（仅在软失败时出现）。
	//
	// 服务端对"没有更新"只返回错误码 update.no_manifest + args.reason
	// （artifacts_dir_not_configured / manifest_missing / channel_unavailable / signature_missing / artifact_*）；
	// 只有把它透出，前端与排障才能看到真正原因，而不是统一的 no_manifest。
	ServerCode   string         `json:"server_code,omitempty"`
	ServerReason string         `json:"server_reason,omitempty"`
	ServerArgs   map[string]any `json:"server_args,omitempty"`
	// Channel 清单声明的分发通道。
	Channel string `json:"channel,omitempty"`
	// SHA256 目标产物的十六进制小写摘要（来自已验签的清单）。
	SHA256 string `json:"sha256,omitempty"`
	// ReleasedAt 发布时间的毫秒时间戳。
	ReleasedAt int64 `json:"released_at,omitempty"`
	// PublicKeyFingerprint 当前信任的发布方公钥指纹（SHA-256 前 16 位十六进制）。
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	// BreaksServers 升级后将不再兼容的服务端列表（非空时需用户确认）。
	BreaksServers []BreakServer `json:"breaks_servers,omitempty"`
}

// pendingUpdate 是写入 update\pending.json 的"待确认更新"记录。
//
// 它只在"新的 exe 已被替换器副本换上去、本进程即将退出"与"下一次启动对账"之间存活：
// 是"这次更新到底成没成"的唯一依据。
type pendingUpdate struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	StagedPath  string `json:"staged_path"`
	TargetPath  string `json:"target_path"`
	BackupPath  string `json:"backup_path"`
	StartedAt   int64  `json:"started_at"`
	Attempts    int    `json:"attempts"`
}

// updateCandidate 是一次**已验签通过**的更新候选（只在内存中传递，不对外序列化）。
type updateCandidate struct {
	manifest *updatepkg.Manifest
	artifact *updatepkg.Artifact
	client   *serverClient
	// serverName 更新源展示名。
	serverName string
	// breaks 升级后将不兼容的服务端。
	breaks []BreakServer
}

// result 把候选转换为对外响应。
func (c *updateCandidate) result() *UpdateCheckResult {
	return &UpdateCheckResult{
		Available:            true,
		Version:              c.manifest.Version,
		Notes:                c.manifest.Notes,
		SizeBytes:            c.artifact.Size,
		Source:               c.serverName,
		Channel:              c.manifest.Channel,
		SHA256:               c.artifact.SHA256,
		ReleasedAt:           c.manifest.ReleasedAt,
		PublicKeyFingerprint: UpdatePublicKeyFingerprint(),
		BreaksServers:        c.breaks,
	}
}

// updateInfo 返回当前自更新状态快照。
func (a *Agent) updateInfo() UpdateInfo {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	return UpdateInfo{
		Channel:          a.cfg.Get().UpdateChannel,
		CurrentVersion:   a.version,
		AvailableVersion: a.availableVersion,
		Downloading:      a.downloading,
		ReceivedBytes:    a.updateReceived,
		TotalBytes:       a.updateTotal,
		Reason:           a.updateReason,
		ServerReason:     a.updateServerReason,
	}
}

// publishUpdateEvent 广播 update 事件（供托盘/设置页展示进度）。
func (a *Agent) publishUpdateEvent() {
	info := a.updateInfo()
	a.hub.Publish(Event{Type: "update", Data: map[string]any{
		"available_version": info.AvailableVersion,
		"downloading":       info.Downloading,
		"received_bytes":    info.ReceivedBytes,
		"total_bytes":       info.TotalBytes,
	}})
}

// updateDir 返回自更新工作目录（与本地状态文件同级的 update 子目录）。
func (a *Agent) updateDir() string {
	return filepath.Join(filepath.Dir(a.store.Path()), updateDirName)
}

// stagedDir 返回产物暂存目录。
func (a *Agent) stagedDir() string { return filepath.Join(a.updateDir(), stagedDirName) }

// pendingPath 返回待确认更新记录路径。
func (a *Agent) pendingPath() string { return filepath.Join(a.updateDir(), pendingFileName) }

// checkUpdate 检查是否有可用更新。
//
// 顺序（安全关键，见 docs/implementation.md 7.4.2）：
//  1. 取更新源 → 2. 拉取清单与签名 → 3. **先验签** → 4. 版本单调性 → 5. 选产物
//  6. 服务端兼容性预检 → 7. 组装结果。
func (a *Agent) checkUpdate(ctx context.Context) (*UpdateCheckResult, error) {
	cand, res, err := a.resolveUpdate(ctx)
	if err != nil {
		return nil, err
	}
	if cand == nil {
		return res, nil
	}

	// 记录"有新版本"状态并广播，供托盘/设置页展示。
	a.updateMu.Lock()
	a.availableVersion = cand.manifest.Version
	a.updateReceived = 0
	a.updateTotal = cand.artifact.Size
	a.updateReason = ""
	a.updateServerReason = ""
	a.updateMu.Unlock()
	a.publishUpdateEvent()

	out := cand.result()
	a.logger.Info("发现可用更新",
		"version", out.Version, "current", a.version, "source", out.Source,
		"size_bytes", out.SizeBytes, "channel", out.Channel,
		"breaks_servers", len(out.BreaksServers))
	return out, nil
}

// resolveUpdate 完成"取源 → 拉取 → 验签 → 单调性 → 选产物 → 兼容性预检"（自更新用 agent 产物）。
func (a *Agent) resolveUpdate(ctx context.Context) (*updateCandidate, *UpdateCheckResult, error) {
	return a.resolveUpdateKind(ctx, updatepkg.KindAgent)
}

// resolveUpdateKind 是自更新与渲染层热更共用的解析流程，按 kind 选取产物。
//
// 返回值语义：
//   - (cand, nil, nil)：存在可用更新；
//   - (nil, res, nil)：**软失败**（不可达 / 无清单 / 非更新 / 无匹配产物），res.Reason 说明原因，
//     这不是错误，前端展示为"无可用更新"，避免把"暂时查不到"变成刺眼的报错；
//   - (nil, nil, err)：**硬失败**（验签失败 / 清单非法），必须原样上报，绝不降级。
func (a *Agent) resolveUpdateKind(ctx context.Context, kind updatepkg.ArtifactKind) (*updateCandidate, *UpdateCheckResult, error) {
	// ① 更新源：当前会话对应的服务端。
	//
	// TODO(多服务端回退)：设计目标是 primary → 其他 COMPATIBLE 服务端按配置顺序回退（§7.4.1）。
	// 代理当前只持有**单个会话**（前端每次只推送一个服务端），拿不到候选列表，
	// 因此这里实现为"用当前服务端；不可达则明确返回 server_unreachable"。
	// 落地方式：前端推送会话时附带候选服务端列表（或新增 GET /agent/servers），
	// 这里改为按序尝试并在结果中返回真正生效的更新源。
	session, ok := a.store.Session()
	if !ok || strings.TrimSpace(session.ServerURL) == "" {
		a.noteUpdateError("update_source", errNoSession())
		return nil, a.unavailable("no_session"), nil
	}
	client, err := newServerClient(session.ServerURL, session.Token, session.CertSHA256, a.logger)
	if err != nil {
		a.noteUpdateError("update_source", err)
		return nil, a.unavailable("server_unreachable"), nil
	}

	channel := strings.TrimSpace(a.cfg.Get().UpdateChannel)
	if channel == "" {
		channel = "stable"
	}

	// ② 拉取清单与签名（两个请求，都是原始字节：验签必须针对字节而不是解析结果）。
	manifestBytes, err := client.GetRawBytes(ctx, "/v1/system/update/manifest?channel="+url.QueryEscape(channel))
	if err != nil {
		a.noteUpdateError("update_manifest", err)
		if apperr.CodeOf(err) == updatepkg.CodeNoManifest {
			// 保留服务端错误码与 args（reason=manifest_missing / channel_unavailable / ...），
			// 否则前端只能看到一个笼统的 no_manifest，无从判断是"没放清单"还是"通道不符"。
			code, reason, args := serverErrorFields(err)
			a.logger.Warn("更新源未提供更新清单（服务端无可用更新包）",
				"server", session.ServerURL, "code", code, "reason", reason)
			return nil, a.unavailableDetail("no_manifest", code, args), nil
		}
		a.logger.Warn("拉取更新清单失败", "server", session.ServerURL, "error", err)
		return nil, a.unavailable("server_unreachable"), nil
	}
	sigBytes, err := client.GetRawBytes(ctx, "/v1/system/update/signature")
	if err != nil {
		a.noteUpdateError("update_signature", err)
		if apperr.CodeOf(err) == updatepkg.CodeNoManifest {
			code, reason, args := serverErrorFields(err)
			a.logger.Warn("更新源未提供清单签名（服务端无可用更新包）",
				"server", session.ServerURL, "code", code, "reason", reason)
			return nil, a.unavailableDetail("no_manifest", code, args), nil
		}
		a.logger.Warn("拉取更新签名失败", "server", session.ServerURL, "error", err)
		return nil, a.unavailable("server_unreachable"), nil
	}

	// ③ ★ 先验签（对原始字节），验签通过后才解析。
	manifest, err := updatepkg.VerifyAndParse(updatePublicKey, manifestBytes, string(sigBytes), channel)
	if err != nil {
		if apperr.CodeOf(err) == updatepkg.CodeBadSignature {
			// 硬失败：绝不允许降级为"忽略签名继续"。
			//
			// 审计：代理侧没有审计存储（审计记录归属服务端，而此时按设计连服务端都不可信），
			// 因此只能以 ERROR 级别结构化日志留痕（action=self_update.verify），
			// 供日志采集/运维告警使用。日志里只出现公钥指纹，绝不出现任何私钥材料。
			a.logger.Error("更新清单验签失败：疑似发布物被篡改，已拒绝该更新",
				"action", "self_update.verify",
				"server", session.ServerURL,
				"channel", channel,
				"public_key_fingerprint", UpdatePublicKeyFingerprint(),
				"error", err)
		} else {
			a.logger.Warn("更新清单格式非法", "server", session.ServerURL, "error", err)
		}
		return nil, nil, err
	}

	// ④ 版本单调性：agent 必须严格新于当前**代理版本**。
	//
	// 这是防**降级攻击**的关键一步：被控服务端可以下发一份"签名合法但版本更旧"的清单
	// （例如含已修复漏洞的旧版），若不校验单调性，客户端就会自己把自己降级。
	//
	// client_web 例外：渲染层资源的单调性应针对"当前已激活的渲染层版本"判定，
	// 不能拿代理 exe 的版本来卡它（两者并非同一序列）；该判定在 runWebUpdate 中完成。
	if kind != updatepkg.KindClientWeb {
		newer, err := isNewer(manifest.Version, a.version)
		if err != nil {
			return nil, a.unavailable("bad_version"), nil
		}
		if !newer {
			a.logger.Info("更新清单版本不高于当前版本，忽略（防降级）",
				"manifest_version", manifest.Version, "current", a.version)
			res := a.unavailable("not_newer")
			res.Version = manifest.Version
			return nil, res, nil
		}
	}

	// ⑤ 选产物：agent 只接受当前平台（windows/<GOARCH>）的产物；
	//    client_web（渲染层资源包）平台无关，按 kind 直接取。
	var artifact *updatepkg.Artifact
	if kind == updatepkg.KindClientWeb {
		artifact = manifest.FindArtifactKind(updatepkg.KindClientWeb)
	} else {
		artifact = manifest.FindArtifact(kind, updateTargetOS, runtime.GOARCH)
	}
	if artifact == nil {
		a.logger.Warn("更新清单中没有匹配的产物",
			"kind", string(kind), "version", manifest.Version, "platform", updateTargetOS+"/"+runtime.GOARCH)
		res := a.unavailable("no_artifact")
		res.Version = manifest.Version
		return nil, res, nil
	}
	if artifact.Size > maxUpdateArtifactBytes {
		a.logger.Warn("更新产物超出大小上限，拒绝", "size", artifact.Size, "limit", maxUpdateArtifactBytes)
		res := a.unavailable("artifact_too_large")
		res.Version = manifest.Version
		return nil, res, nil
	}

	// ⑥ 客户端兼容性预检（§7.4.3）：升级后若超出该服务端的 client_compat.max，
	//    不阻断，但把冲突信息交给前端提示用户确认。
	breaks := a.precheckServerCompat(ctx, client, manifest.Version)

	return &updateCandidate{
		manifest:   manifest,
		artifact:   artifact,
		client:     client,
		serverName: a.serverDisplayName(),
		breaks:     breaks,
	}, nil, nil
}

// precheckServerCompat 预检"升级后是否会导致当前服务端不兼容"。
//
// TODO(多服务端)：理想实现是对全部已配置服务端逐个预检（§7.4.3）。
// 代理当前只有单会话，因此只能预检当前服务端。预检失败按"无冲突"处理（不阻断升级）：
// 拉不到契约时宁可让升级继续，也不要因为一个探测失败而永远无法升级。
func (a *Agent) precheckServerCompat(ctx context.Context, client *serverClient, targetVersion string) []BreakServer {
	info, err := client.SystemInfo(ctx)
	if err != nil {
		a.logger.Warn("升级兼容性预检失败（无法获取服务端 client_compat），已跳过", "error", err)
		return nil
	}
	if !info.ClientCompat.Enabled || strings.TrimSpace(info.ClientCompat.Max) == "" {
		return nil
	}
	maxV, err := semver.Parse(info.ClientCompat.Max)
	if err != nil {
		return nil
	}
	newV, err := semver.Parse(targetVersion)
	if err != nil {
		return nil
	}
	if newV.LTE(maxV) {
		return nil
	}
	return []BreakServer{{ServerName: a.serverDisplayName(), Max: info.ClientCompat.Max}}
}

// noteUpdateError 把更新源故障记入"服务端最近错误"，便于界面与排障看到原因。
//
// 只补 last_error，**不动 connected 标志**：更新源取不到清单并不代表服务端 API 不可用
// （服务端未配置 artifacts_dir 时也会返回 404），不能因此把"已连接"改成"未连接"。
func (a *Agent) noteUpdateError(stage string, err error) {
	if err == nil {
		return
	}
	server := a.store.Server()
	server.LastError = describeError(stage, err)
	a.store.SetServer(server)
}

// unavailable 构造"无可用更新"的软失败结果，并记录原因供 GET /agent/state 展示。
func (a *Agent) unavailable(reason string) *UpdateCheckResult {
	info := a.updateInfo()
	a.updateMu.Lock()
	a.updateReason = reason
	a.updateServerReason = ""
	a.updateReceived = 0
	a.updateTotal = 0
	a.updateMu.Unlock()
	return &UpdateCheckResult{
		Available:            false,
		Version:              info.CurrentVersion,
		SizeBytes:            0,
		Source:               a.serverDisplayName(),
		Reason:               reason,
		Channel:              info.Channel,
		PublicKeyFingerprint: UpdatePublicKeyFingerprint(),
	}
}

// unavailableDetail 同 unavailable，但额外**原样保留更新源（服务端）的结构化错误**：
// 错误码（如 update.no_manifest）与 args（如 {"reason":"manifest_missing"}）。
// 这是"为什么没有更新"可诊断的关键——服务端的具体原因不应被压成一个笼统的软失败码。
func (a *Agent) unavailableDetail(reason, serverCode string, serverArgs map[string]any) *UpdateCheckResult {
	res := a.unavailable(reason)
	res.ServerCode = serverCode
	res.ServerArgs = serverArgs
	if v, ok := serverArgs["reason"].(string); ok {
		res.ServerReason = v
		a.updateMu.Lock()
		a.updateServerReason = v
		a.updateMu.Unlock()
	}
	return res
}

// serverErrorFields 提取服务端结构化错误的错误码与 args；非结构化错误返回零值。
func serverErrorFields(err error) (code, reason string, args map[string]any) {
	e, ok := apperr.As(err)
	if !ok {
		return "", "", nil
	}
	code, args = e.Code, e.Args
	if v, ok := args["reason"].(string); ok {
		reason = v
	}
	return code, reason, args
}

// applyUpdate 启动自更新流程。
//
// 严格顺序（见 docs/implementation.md 7.4）：
//  1. 已在更新中 → 409 update.in_progress；
//  2. 有活跃挂载 → 409 agent.busy_mounts（**不自动卸载**）；
//  3. 重新 check() 并**再次验签**（不信任之前的结果，避免 TOCTOU）；
//  4. 下载到 update\staged\<filename>.part → 校验 sha256 → 改名去掉 .part；
//  5. Authenticode 校验（尽力而为，失败仅 WARN）；
//  6. 写 update\pending.json；
//  7. 复制自身为临时副本，并以 apply-update 子命令启动该副本；
//  8. 返回 started=true，随后由调用方（handler）触发优雅退出。
func (a *Agent) applyUpdate(ctx context.Context) error {
	// ① 并发保护：一次只允许一个更新流程。
	if !a.beginUpdate() {
		return apperr.New(CodeUpdateInProgress, http.StatusConflict)
	}
	started := false
	defer func() {
		if !started {
			a.endUpdate()
		}
	}()

	// ② 有活跃挂载则拒绝更新：更新必然重启进程，iSCSI 会话会断开。
	//    这里**不自动卸载**——用户的数据可能在用，必须由用户先显式卸载。
	if n := a.ActiveMountCount(); n > 0 {
		return errBusyMounts(n)
	}

	// ③ 重新拉取并再次验签：不信任此前 check() 的结果（TOCTOU）。
	cand, res, err := a.resolveUpdate(ctx)
	if err != nil {
		return err
	}
	if cand == nil {
		reason := "not_available"
		if res != nil {
			reason = res.Reason
		}
		return apperr.New(CodeUpdateNotAvailable, http.StatusConflict).WithArg("reason", reason)
	}

	// ④ 下载产物（边下边算 sha256，支持断点续传与重试）。
	if err := os.MkdirAll(a.stagedDir(), 0o755); err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "prepare_staging").WithCause(err)
	}
	partPath := filepath.Join(a.stagedDir(), cand.artifact.Filename+partSuffix)
	// 下载进度按"≥1MiB 或 ≥500ms"节流推送 update 事件（与母盘下载链路同一策略）。
	prog := newUpdateProgress(a, cand.manifest.Version, cand.artifact.Size)
	if err := a.downloadArtifact(ctx, cand.client, cand.artifact, partPath, prog); err != nil {
		return err
	}
	stagedPath := strings.TrimSuffix(partPath, partSuffix)
	// 原子落地：先把完整文件写成 .part，校验通过后再改名（同目录 rename 是原子操作）。
	if err := os.Remove(stagedPath); err != nil && !os.IsNotExist(err) {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "clean_staged").WithCause(err)
	}
	if err := os.Rename(partPath, stagedPath); err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "commit_staged").WithCause(err)
	}

	// ⑤ Authenticode 校验（尽力而为，不阻断）。
	a.verifyAuthenticode(ctx, stagedPath)

	// ⑥ 写 pending.json：这是"更新是否成功"的唯一依据，必须在启动替换器之前落盘。
	targetPath, err := os.Executable()
	if err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "resolve_exe").WithCause(err)
	}
	targetPath, err = filepath.Abs(targetPath)
	if err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "resolve_exe").WithCause(err)
	}
	backupPath := filepath.Join(a.updateDir(), backupDirName, filepath.Base(targetPath))
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "prepare_backup").WithCause(err)
	}
	pending := pendingUpdate{
		FromVersion: a.version,
		ToVersion:   cand.manifest.Version,
		StagedPath:  stagedPath,
		TargetPath:  targetPath,
		BackupPath:  backupPath,
		StartedAt:   time.Now().UnixMilli(),
		Attempts:    0,
	}
	if err := writePendingUpdate(a.pendingPath(), &pending); err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "write_pending").WithCause(err)
	}

	// ⑦ 启动替换器副本：运行中的 exe 无法覆盖自身，替换与重启必须交给**另一个文件**。
	if err := a.launchApplyUpdate(pending); err != nil {
		// 替换器没能启动 ⇒ 尚未发生任何替换：清掉 pending，
		// 避免下次启动把"根本没开始"误判成"更新失败待回滚"。
		if rmErr := os.Remove(a.pendingPath()); rmErr != nil && !os.IsNotExist(rmErr) {
			a.logger.Warn("清理更新对账文件失败", "error", rmErr)
		}
		return err
	}

	started = true
	a.updateStarted.Store(true)
	a.updateMu.Lock()
	a.availableVersion = cand.manifest.Version
	a.downloading = false
	a.updateReceived = cand.artifact.Size
	a.updateTotal = cand.artifact.Size
	a.updateMu.Unlock()
	a.publishUpdateEvent()
	a.logger.Info("自更新已就绪，等待替换器副本完成替换与重启",
		"from", a.version, "to", cand.manifest.Version, "staged", stagedPath, "target", targetPath)
	return nil
}

// beginUpdate 尝试进入"更新中"状态；已在更新中返回 false。
func (a *Agent) beginUpdate() bool {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	if a.downloading {
		return false
	}
	a.downloading = true
	return true
}

// endUpdate 退出"更新中"状态（失败路径）。
func (a *Agent) endUpdate() {
	a.updateMu.Lock()
	a.downloading = false
	a.updateMu.Unlock()
}

// UpdateStarted 返回本次退出是否由自更新触发。
//
// 为 true 时**不得卸载挂载**：更新只是短暂重启进程，挂载配置与状态都已落盘，
// 重启后由自动挂载恢复（见 docs/implementation.md 7.4「不影响挂载」的说明：
// 更新期间 iSCSI 会话必然短暂断开，这是无法避免的取舍）。
func (a *Agent) UpdateStarted() bool { return a.updateStarted.Load() }

// artifactProgress 抽象更新产物的下载进度上报（自更新与渲染层热更共用同一套下载链路）。
type artifactProgress interface {
	// reset 重置进度基线（总大小 / 已收字节）并立即上报一次。
	reset(total, received int64)
	// add 累加已收字节。
	add(n int64)
}

// downloadArtifact 下载产物到 partPath，边下边算 sha256，失败按指数退避重试。
func (a *Agent) downloadArtifact(ctx context.Context, client *serverClient, art *updatepkg.Artifact, partPath string, prog artifactProgress) error {
	path := "/v1/system/update/artifacts/" + url.PathEscape(art.Filename)
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(1<<(attempt-2)) * time.Second // 1s → 2s
			a.logger.Warn("下载更新失败，准备重试",
				"attempt", attempt, "max", downloadAttempts, "delay", delay.String(), "error", lastErr)
			select {
			case <-ctx.Done():
				return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
					WithArg("stage", "download").WithCause(ctx.Err())
			case <-time.After(delay):
			}
		}
		if err := a.downloadOnce(ctx, client, path, art, partPath, prog); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// downloadOnce 执行一次（可能带断点续传的）下载并校验摘要。
func (a *Agent) downloadOnce(ctx context.Context, client *serverClient, path string, art *updatepkg.Artifact, partPath string, prog artifactProgress) error {
	// 续传：把已落地的前缀重新读入哈希状态（每个字节只哈希一次）。
	offset, digest, err := resumeState(partPath)
	if err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "resume").WithCause(err)
	}

	resp, err := client.OpenRange(ctx, path, offset)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			// 服务端无视 Range 返回了完整内容：丢弃前缀，从头开始（哈希状态一并重置）。
			a.logger.Info("服务端不支持断点续传，改为从头下载", "filename", art.Filename)
			offset, digest = 0, sha256.New()
		}
	case http.StatusPartialContent:
		// 正常续传。
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return parseServerError(resp.StatusCode, data)
	}

	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "open_staged").WithCause(err)
	}
	defer func() { _ = f.Close() }()

	if offset == 0 {
		if err := f.Truncate(0); err != nil {
			return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
				WithArg("stage", "truncate_staged").WithCause(err)
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "seek_staged").WithCause(err)
	}

	// 进度基线：把续传前缀计入已收字节（没有 prog 时退化为不推送）。
	if prog != nil {
		prog.reset(art.Size, offset)
	}

	// 边下边写边算：MultiWriter 同时喂给文件与哈希，避免"下完再读一遍"。
	// 若清单声明了大小，则最多只读 size+1 字节，杜绝镜像端无限流式响应写满磁盘。
	var src io.Reader = resp.Body
	if art.Size > 0 {
		src = io.LimitReader(resp.Body, art.Size+1)
	}
	n, err := io.Copy(progressWriter{w: io.MultiWriter(f, digest), prog: prog}, src)
	if err != nil {
		// 中断的响应体保留 .part，下次续传。
		return apperr.New(CodeUpdateFailed, http.StatusInternalServerError).
			WithArg("stage", "download_body").WithCause(err)
	}

	total := offset + n
	if art.Size > 0 && total != art.Size {
		_ = os.Remove(partPath) // 长度不对：丢弃，下次从头来
		return apperr.New(CodeDigestMismatch, http.StatusUnprocessableEntity).
			WithArg("expected_size", art.Size).WithArg("actual_size", total)
	}
	got := hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(got, art.SHA256) {
		_ = os.Remove(partPath) // 摘要不符：丢弃，绝不接受
		return apperr.New(CodeDigestMismatch, http.StatusUnprocessableEntity).
			WithArg("expected", art.SHA256).WithArg("actual", got)
	}
	if prog != nil {
		prog.reset(art.Size, total) // 收尾：把进度推到 100%
	}
	a.logger.Info("更新产物下载并校验完成", "filename", art.Filename, "size", total, "sha256", got)
	return nil
}

// progressWriter 把写入字节数同步上报给进度器（prog 为 nil 时退化为纯透传）。
type progressWriter struct {
	w    io.Writer
	prog artifactProgress
}

func (p progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 && p.prog != nil {
		p.prog.add(int64(n))
	}
	return n, err
}

// resumeState 返回 part 文件已有的字节数与对应的哈希状态；文件不存在时返回 0 与空哈希。
func resumeState(partPath string) (int64, hash.Hash, error) {
	digest := sha256.New()
	f, err := os.Open(partPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, digest, nil
		}
		return 0, nil, err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(digest, f)
	if err != nil {
		return 0, nil, err
	}
	return n, digest, nil
}

// verifyAuthenticode 尽力而为地校验产物的 Authenticode 签名。
//
// 失败只 WARN 不阻断：当前项目尚未配置代码签名证书，任何"强制校验"都会把所有更新
// 挡在门外。**生产环境应改为强制校验**（需要代码签名证书 + 把下面的 WARN 变成硬失败）。
// 真正不可绕过的完整性保证仍来自 Ed25519 清单签名与 sha256 比对。
func (a *Agent) verifyAuthenticode(ctx context.Context, path string) {
	// 路径来自本进程控制的 staged 目录（文件名已被清单校验为 [A-Za-z0-9._-]+），
	// 且这里做了单引号转义，因此拼接进 PowerShell 是安全的（winps 的注入约束针对外部输入）。
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	script := fmt.Sprintf(
		`$s = Get-AuthenticodeSignature -LiteralPath %s; [pscustomobject]@{ ok = $true; status = [string]$s.Status; signer = [string]$s.SignerCertificate.Subject } | ConvertTo-Json -Compress`,
		quoted)

	runCtx, cancel := context.WithTimeout(ctx, authenticodeTimeout)
	defer cancel()

	var out struct {
		Status string `json:"status"`
		Signer string `json:"signer"`
	}
	if err := a.ps.RunJSON(runCtx, script, &out); err != nil {
		a.logger.Warn("更新产物 Authenticode 校验未能执行（不阻断，生产环境应改为强制校验）",
			"error", err)
		return
	}
	if !strings.EqualFold(out.Status, "Valid") {
		a.logger.Warn("更新产物未通过 Authenticode 校验（不阻断，生产环境应改为强制校验）",
			"status", out.Status)
		return
	}
	a.logger.Info("更新产物 Authenticode 校验通过", "signer", out.Signer)
}

// launchApplyUpdate 复制自身为临时副本，并以 apply-update 子命令启动该副本，
// 由它在本次进程退出后完成"替换 + 重启"。
//
// **为什么必须用「旧版自身」的副本，而不是用已下载的「新版」可执行文件来执行替换**：
// 旧版是已知可正常工作的；若新版二进制有缺陷（例如启动即崩溃、或缺少替换逻辑），
// 用它当替换器会导致替换失败，而此时已经没有可用的 agent 兜底。
// 副本从**另一个文件路径**运行，因此可以覆盖正式路径上的 Vault-Agent.exe
// （Windows 不允许覆盖正在运行的 exe）。
func (a *Agent) launchApplyUpdate(pending pendingUpdate) error {
	args := []string{
		"--pid", strconv.Itoa(os.Getpid()),
		"--staged", pending.StagedPath,
		"--target", pending.TargetPath,
		"--backup", pending.BackupPath,
		"--restart",
	}
	if _, err := startApplyUpdateCopy(args, a.logger); err != nil {
		return apperr.New(CodeUpdaterMissing, http.StatusServiceUnavailable).
			WithArg("stage", "launch_updater").WithCause(err)
	}
	// 稍等片刻再让调用方触发退出，确保副本已进入等待流程（避免 pid 复用误判）。
	time.Sleep(updaterExitDelay)
	return nil
}

// isNewer 判断候选版本是否严格新于当前版本（版本单调性校验，禁止字符串比较）。
// webTargetApplies 判断某个渲染层资源版本能否被客户端**真正加载**。
//
// 客户端主进程的规则是"热更层版本必须高于内置（=应用）版本"，因此这里用应用版本
// 作为基准；decided=false 表示无法判定（应用版本缺失或不可解析，例如开发构建），
// 此时调用方应保留原有行为，不要误拦。
func (a *Agent) webTargetApplies(target string) (applies bool, decided bool) {
	app := strings.TrimSpace(a.version)
	if app == "" {
		return true, false
	}
	if _, err := semver.Parse(app); err != nil {
		// 应用版本不可解析（如开发构建）：无从比较，保留原有行为，不误拦。
		return true, false
	}
	newer, err := isNewer(target, app)
	if err != nil {
		// 目标版本不可解析：无法证明它比应用新；客户端主进程同样会拒绝不可解析的版本
		// （版本比较得到"相等"→ 不采用），因此这里按"不可加载"保守处理。
		return false, true
	}
	return newer, true
}

func isNewer(candidate, current string) (bool, error) {
	cand, err := semver.Parse(candidate)
	if err != nil {
		return false, err
	}
	cur, err := semver.Parse(current)
	if err != nil {
		return false, err
	}
	return semver.Compare(cand, cur) > 0, nil
}

// defaultVersion 返回有效的代理版本号（未注入时用构建版本）。
func defaultVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return version.Version
	}
	return strings.TrimSpace(v)
}

// ---- 启动对账与回滚 ----

// ReconcileResult 是自更新启动对账的结果。
type ReconcileResult struct {
	// RolledBack 为 true 表示已启动回滚：调用方应尽快退出，
	// 由替换器副本用备份还原并重启（正在运行的 exe 无法被自身覆盖）。
	RolledBack bool
}

// ReconcileUpdate 在代理启动早期对账上一次自更新的结果（见 docs/implementation.md 7.4）。
//
// 规则：
//   - pending.json 不存在 → 无事可做；
//   - 存在且当前版本 == to_version → 更新成功：删除 pending 与备份，写 INFO 日志；
//   - 存在且当前版本 != to_version → attempts++；
//     attempts >= 3 → 用备份回滚（失败时保留 pending），写 ERROR 日志与审计留痕；
//     否则保留 pending 供下次重试。
//
// ⚠️ 回滚同样必须交给替换器副本：**正在运行的 exe 无法覆盖自身**。
// 因此这里的做法是"以备份作为 staged、target 不变"复用同一套替换流程，再让本进程退出。
func ReconcileUpdate(updateDir, currentVersion string, logger *slog.Logger) (ReconcileResult, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(updateDir) == "" {
		return ReconcileResult{}, nil
	}
	pendingPath := filepath.Join(updateDir, pendingFileName)

	data, err := os.ReadFile(pendingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ReconcileResult{}, nil
		}
		return ReconcileResult{}, fmt.Errorf("agent: 读取更新对账文件失败: %w", err)
	}
	var pending pendingUpdate
	if err := json.Unmarshal(data, &pending); err != nil {
		logger.Error("更新对账文件无法解析，已删除以免反复失败", "path", pendingPath, "error", err)
		_ = os.Remove(pendingPath)
		return ReconcileResult{}, nil
	}

	// 更新成功：当前版本已等于目标版本。
	if sameVersion(currentVersion, pending.ToVersion) {
		logger.Info("自更新成功，清理待确认记录与备份",
			"from", pending.FromVersion, "to", pending.ToVersion)
		_ = os.Remove(pendingPath)
		_ = os.Remove(pending.BackupPath)
		return ReconcileResult{}, nil
	}

	pending.Attempts++
	logger.Warn("检测到未完成的自更新（版本未变）",
		"expected", pending.ToVersion, "actual", currentVersion, "attempts", pending.Attempts)

	if pending.Attempts < rollbackAttemptLimit {
		// 保留 pending 供下次重试（可能是替换器副本尚未完成替换就退出了）。
		if err := writePendingUpdate(pendingPath, &pending); err != nil {
			return ReconcileResult{}, err
		}
		return ReconcileResult{}, nil
	}

	// 连续失败达阈值：回滚。
	if _, err := os.Stat(pending.BackupPath); err != nil {
		logger.Error("自更新连续失败，但备份缺失，无法回滚（请人工处理）",
			"action", "self_update.rollback", "backup", pending.BackupPath, "error", err)
		return ReconcileResult{}, nil
	}
	// 先删 pending 再启动替换器：避免回滚后重启的新进程再次进入回滚分支。
	if err := os.Remove(pendingPath); err != nil {
		logger.Error("删除更新对账文件失败，已中止回滚", "path", pendingPath, "error", err)
		return ReconcileResult{}, err
	}

	args := []string{
		"--pid", strconv.Itoa(os.Getpid()),
		"--staged", pending.BackupPath,
		"--target", pending.TargetPath,
		"--restart",
	}
	// 回滚同样必须交给"另一个文件"执行（正在运行的 exe 无法覆盖自身）：
	// 这里复用同一套"复制自身为临时副本 + apply-update"的替换流程。
	if _, err := startApplyUpdateCopy(args, logger); err != nil {
		logger.Error("启动回滚失败（请人工恢复）", "action", "self_update.rollback", "error", err)
		return ReconcileResult{}, err
	}

	// 审计：代理侧无审计存储（审计记录归属服务端），以 ERROR 级别结构化日志留痕。
	logger.Error("自更新连续失败已回滚",
		"action", "self_update.rollback",
		"from", pending.FromVersion, "to", pending.ToVersion, "attempts", pending.Attempts)
	return ReconcileResult{RolledBack: true}, nil
}

// writePendingUpdate 原子写入待确认更新记录（先写临时文件再改名）。
func writePendingUpdate(path string, pending *pendingUpdate) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// sameVersion 判断两个版本号是否相同（解析失败时退化为字符串比较）。
func sameVersion(a, b string) bool {
	va, errA := semver.Parse(a)
	vb, errB := semver.Parse(b)
	if errA != nil || errB != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	return semver.Compare(va, vb) == 0
}

// ---- 自更新下载进度 ----

// updateProgress 汇总自更新产物下载进度：统一经 progressThrottle 节流推送 update 事件
// （与母盘下载、上传链路共用同一套节流参数，避免多处参数漂移）。
type updateProgress struct {
	a        *Agent
	version  string
	received atomic.Int64
	total    atomic.Int64

	mu       sync.Mutex
	throttle progressThrottle
}

// newUpdateProgress 构造自更新进度上报器。
func newUpdateProgress(a *Agent, version string, total int64) *updateProgress {
	p := &updateProgress{a: a, version: version, throttle: newProgressThrottle(0)}
	p.total.Store(total)
	return p
}

// reset 重置进度基线（总大小 / 已收字节）并立即推送一次。
func (p *updateProgress) reset(total, received int64) {
	if total > 0 {
		p.total.Store(total)
	}
	p.received.Store(received)
	p.mu.Lock()
	p.throttle.allow(true, received, time.Now())
	p.mu.Unlock()
	p.report(received, p.total.Load())
}

// add 累加已收字节（节流推送）。
func (p *updateProgress) add(n int64) {
	if n <= 0 {
		return
	}
	got := p.received.Add(n)
	p.mu.Lock()
	ok := p.throttle.allow(false, got, time.Now())
	p.mu.Unlock()
	if !ok {
		return
	}
	p.report(got, p.total.Load())
}

// report 更新自更新进度状态并推送 update 事件。
func (p *updateProgress) report(received, total int64) {
	p.a.updateMu.Lock()
	p.a.availableVersion = p.version
	p.a.downloading = true
	p.a.updateReceived = received
	if total > 0 {
		p.a.updateTotal = total
	}
	p.a.updateMu.Unlock()
	p.a.publishUpdateEvent()
}

// ---- 渲染层资源热更（client_web）----
//
// 目标：**不替换 Electron 本体**，只热更渲染层静态资源（Vite 产出的 dist/），实现快速迭代。
//
// 落盘布局（与 Electron 主进程的**冻结契约**，见 docs/agent-api.md「客户端资源热更」）：
//
//	<代理数据目录>/webapp/                根目录（%ProgramData%\Vault\webapp）
//	<webapp>/<version>/                   版本目录（index.html + assets/**）
//	<webapp>/current.json                 激活指针 {"version","dir","activated_at"}
//
// 校验沿用自更新的四重校验思路：清单签名（check 阶段已验）→ 产物 sha256（边下边算）→
// 版本单调性（只允许升级，不允许降级）→ 解压内容安全（zip slip / 大小上限）。

// 渲染层热更状态取值（WebUpdateState.state）。
const (
	// WebStateIdle 空闲（尚未热更，且没有已激活版本）。
	WebStateIdle = "idle"
	// WebStateDownloading 正在下载资源包。
	WebStateDownloading = "downloading"
	// WebStateVerifying 正在校验（大小 / sha256）。
	WebStateVerifying = "verifying"
	// WebStateExtracting 正在解压并准备激活。
	WebStateExtracting = "extracting"
	// WebStateActivated 已激活（current.json 已指向新版本）。
	WebStateActivated = "activated"
	// WebStateFailed 失败（error 为稳定错误码）。
	WebStateFailed = "failed"
)

// WebUpdateState 是渲染层资源热更的状态（对外契约类型，页面刷新后由 GET /agent/update/web 恢复展示）。
type WebUpdateState struct {
	// State idle|downloading|verifying|extracting|activated|failed。
	State string `json:"state"`
	// AvailableVersion 本次尝试热更到的目标版本（解析清单后填充）。
	AvailableVersion string `json:"available_version,omitempty"`
	// ActiveVersion 当前已激活的版本（current.json 指向的版本）。
	ActiveVersion string `json:"active_version,omitempty"`
	// ReceivedBytes / TotalBytes 下载进度。
	ReceivedBytes int64 `json:"received_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
	// Error 失败时的稳定错误码（如 agent.web_update_failed / update.not_available / not_newer）。
	Error string `json:"error,omitempty"`
	// UpdatedAt 最后一次状态变更的毫秒时间戳。
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// webCurrentPointer 是 <webapp>/current.json 的内容（激活指针）。
type webCurrentPointer struct {
	// Version 已激活版本。
	Version string `json:"version"`
	// Dir 版本目录名（当前等于 Version；保留字段以便将来支持同版本多目录）。
	Dir string `json:"dir"`
	// ActivatedAt 激活时间（毫秒时间戳）。
	ActivatedAt int64 `json:"activated_at"`
}

// webUpdateManager 管理渲染层资源热更的内存态与下载进度。
//
// 磁盘上的"已激活版本"以 current.json 为准（进程重启后依然可用），内存态只负责
// 展示与并发保护；因此本结构不需要任何持久化。
type webUpdateManager struct {
	a *Agent

	mu      sync.Mutex
	running bool
	seeded  bool
	state   WebUpdateState

	// 下载进度（经 progressThrottle 节流报送；受 mu 保护）。
	received atomic.Int64
	total    atomic.Int64
	throttle progressThrottle
}

func newWebUpdateManager(a *Agent) *webUpdateManager {
	return &webUpdateManager{
		a:        a,
		state:    WebUpdateState{State: WebStateIdle},
		throttle: newProgressThrottle(0),
	}
}

// snapshot 返回状态快照；首次调用时从 current.json 恢复 active_version（页面刷新后仍能展示）。
func (m *webUpdateManager) snapshot() WebUpdateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.seeded {
		m.seeded = true
		if cur, ok := readWebCurrent(m.a.webCurrentPath()); ok {
			if applies, decided := m.a.webTargetApplies(cur.Version); decided && !applies {
				// 指针里的版本不高于本机应用版本：客户端主进程不会加载它（会用内置资源），
				// 因此这里**不能**报告"已激活"——否则设置页会显示一个根本不生效的版本号
				// （历史遗留指针常见于此，例如应用已升级到 0.1.37 而指针仍停在 0.1.28）。
				m.a.logger.Warn("已激活的渲染层资源不高于本机应用版本，客户端将使用内置资源",
					"active", cur.Version, "app", m.a.version)
			} else {
				m.state.ActiveVersion = cur.Version
				if m.state.State == "" || m.state.State == WebStateIdle {
					m.state.State = WebStateActivated
				}
			}
		}
	}
	if m.state.State == "" {
		m.state.State = WebStateIdle
	}
	return m.state
}

// setState 在锁内变更状态并推送 web_update 事件。
func (m *webUpdateManager) setState(mutate func(*WebUpdateState)) {
	m.mu.Lock()
	mutate(&m.state)
	m.state.UpdatedAt = time.Now().UnixMilli()
	m.mu.Unlock()
	m.a.publishWebState()
}

// reset 实现 artifactProgress：重置进度基线并推送一次。
func (m *webUpdateManager) reset(total, received int64) {
	if total > 0 {
		m.total.Store(total)
	}
	m.received.Store(received)
	m.mu.Lock()
	m.throttle.allow(true, received, time.Now())
	m.mu.Unlock()
	m.push(received, m.total.Load())
}

// add 实现 artifactProgress：累加已收字节并按节流推送。
func (m *webUpdateManager) add(n int64) {
	if n <= 0 {
		return
	}
	got := m.received.Add(n)
	m.mu.Lock()
	ok := m.throttle.allow(false, got, time.Now())
	m.mu.Unlock()
	if !ok {
		return
	}
	m.push(got, m.total.Load())
}

// push 把进度写入状态并推送（注意：不能在持有 m.mu 时调用）。
func (m *webUpdateManager) push(received, total int64) {
	m.setState(func(s *WebUpdateState) {
		s.ReceivedBytes = received
		if total > 0 {
			s.TotalBytes = total
		}
	})
}

// finishRun 结束"运行中"标记。
func (m *webUpdateManager) finishRun() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// publishWebState 广播 web_update 事件（data 即 WebUpdateState）。
func (a *Agent) publishWebState() {
	if a.web == nil {
		return
	}
	a.hub.Publish(Event{Type: "web_update", Data: a.web.snapshot()})
}

// webUpdateState 返回渲染层热更状态（供 GET /agent/update/web）。
func (a *Agent) webUpdateState() WebUpdateState {
	if a.web == nil {
		return WebUpdateState{State: WebStateIdle}
	}
	return a.web.snapshot()
}

// startWebUpdate 启动一次后台热更流程并立即返回当前状态（幂等：已在运行直接返回）。
func (a *Agent) startWebUpdate() WebUpdateState {
	m := a.web
	if m == nil {
		// 未启用热更（如构造参数里关掉了）：返回空闲态，绝不 panic。
		return WebUpdateState{State: WebStateIdle}
	}
	active := m.snapshot().ActiveVersion
	m.mu.Lock()
	if m.running {
		s := m.state
		m.mu.Unlock()
		return s
	}
	m.running = true
	m.state = WebUpdateState{
		State:         WebStateDownloading,
		ActiveVersion: active,
		UpdatedAt:     time.Now().UnixMilli(),
	}
	s := m.state
	m.mu.Unlock()
	a.publishWebState()
	safeGo(a.logger, "web_update", func() { a.runWebUpdate(a.bgContext()) })
	return s
}

// runWebUpdate 后台执行"解析清单 → 下载 → 校验 → 解压 → 激活"。
func (a *Agent) runWebUpdate(ctx context.Context) {
	m := a.web
	defer m.finishRun()

	cand, res, err := a.resolveUpdateKind(ctx, updatepkg.KindClientWeb)
	if err != nil {
		a.logger.Warn("客户端资源热更检查失败", "error", err)
		m.setState(func(s *WebUpdateState) {
			s.State = WebStateFailed
			s.Error = apperr.CodeOf(err)
		})
		return
	}
	if cand == nil {
		reason := "not_available"
		if res != nil && res.Reason != "" {
			reason = res.Reason
		}
		a.logger.Info("没有可热更的渲染层资源", "reason", reason)
		m.setState(func(s *WebUpdateState) {
			s.State = WebStateFailed
			s.Error = reason
		})
		return
	}

	target := cand.manifest.Version
	m.setState(func(s *WebUpdateState) {
		s.State = WebStateDownloading
		s.AvailableVersion = target
		s.ReceivedBytes = 0
		s.TotalBytes = cand.artifact.Size
		s.Error = ""
	})

	cur, hasCur := readWebCurrent(a.webCurrentPath())

	// 幂等：目标版本就是当前已激活版本 → 直接返回已激活，不重复下载。
	if hasCur && sameVersion(cur.Version, target) {
		a.logger.Info("渲染层资源已是目标版本，无需热更", "version", target)
		m.setState(func(s *WebUpdateState) {
			s.State = WebStateActivated
			s.ActiveVersion = cur.Version
			s.ReceivedBytes = cand.artifact.Size
			s.TotalBytes = cand.artifact.Size
		})
		return
	}

	// 版本单调性：仅允许更新到比当前 active 更"新"的版本（不允许降级）；无 active 时允许任意版本。
	if hasCur && strings.TrimSpace(cur.Version) != "" {
		newer, cmpErr := isNewer(target, cur.Version)
		if cmpErr != nil || !newer {
			a.logger.Warn("渲染层资源版本不高于当前已激活版本，拒绝（防降级）",
				"target", target, "active", cur.Version)
			m.setState(func(s *WebUpdateState) {
				s.State = WebStateFailed
				s.Error = "not_newer"
			})
			return
		}
	}

	// 第二道门槛：目标版本必须**高于本机应用（客户端）版本**，否则激活了也不会被加载。
	//
	// 客户端 Electron 主进程只在"热更层版本 > 内置层（=应用）版本"时才加载热更层
	// （见 frontend/apps/client/electron/webapp.ts）。上面只跟"指针里的旧版本"比较，
	// 若放行一个不高于应用版本的资源，就会造成分裂状态：设置页显示"已更新到 vX，
	// 重启后生效"，而界面、版本号、样式纹丝不动（真实工单）。
	// 这里提前拦下并如实回报 not_newer，与渲染层的加载口径保持一致。
	if applies, decided := a.webTargetApplies(target); decided && !applies {
		a.logger.Info("渲染层资源不高于本机应用版本，客户端不会加载它，跳过热更",
			"target", target, "app", a.version)
		m.setState(func(s *WebUpdateState) {
			s.State = WebStateFailed
			s.Error = "not_newer"
		})
		return
	}

	if err := a.applyWebUpdate(ctx, cand); err != nil {
		a.logger.Warn("渲染层资源热更失败", "version", target, "error", err)
		m.setState(func(s *WebUpdateState) {
			s.State = WebStateFailed
			s.Error = apperr.CodeOf(err)
		})
		return
	}
	m.setState(func(s *WebUpdateState) {
		s.State = WebStateActivated
		s.ActiveVersion = target
		s.ReceivedBytes = cand.artifact.Size
		s.TotalBytes = cand.artifact.Size
		s.Error = ""
	})
	a.logger.Info("渲染层资源热更完成", "version", target)
}

// applyWebUpdate 执行"下载 → 校验 → 解压 → 原子激活"；失败时不留下半激活状态。
func (a *Agent) applyWebUpdate(ctx context.Context, cand *updateCandidate) error {
	version := cand.manifest.Version
	webappDir := a.webappDir()
	if err := os.MkdirAll(webappDir, 0o755); err != nil {
		return errWebUpdateFailed("prepare_dir", err)
	}
	// 先清理历史遗留的临时目录/下载残留（尽力而为，不影响本次流程）。
	cleanupWebTemp(webappDir, a.logger)

	// 下载（边下边算 sha256；进度经 web 状态节流推送）。
	zipPath := filepath.Join(webappDir, cand.artifact.Filename+partSuffix)
	if err := a.downloadArtifact(ctx, cand.client, cand.artifact, zipPath, a.web); err != nil {
		return err
	}
	defer func() { _ = os.Remove(zipPath) }()

	a.web.setState(func(s *WebUpdateState) { s.State = WebStateVerifying })
	// sha256/大小已在 downloadArtifact 内校验；这里再做一次显式确认（幂等、便于独立排障）。
	if err := verifyArtifactFile(zipPath, cand.artifact); err != nil {
		return err
	}

	a.web.setState(func(s *WebUpdateState) { s.State = WebStateExtracting })

	// 解压到临时目录，确认 index.html 存在后再原子改名激活。
	suffix, err := randomHex(4)
	if err != nil {
		return errWebUpdateFailed("extract", err)
	}
	tmpDir := filepath.Join(webappDir, version+webTempPrefix+suffix)
	if err := os.RemoveAll(tmpDir); err != nil {
		return errWebUpdateFailed("extract", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return errWebUpdateFailed("extract", err)
	}
	activated := false
	defer func() {
		if !activated {
			// 失败/取消：清理临时目录，绝不留下半激活状态。
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := extractWebZip(zipPath, tmpDir, webExtractMaxBytes(cand.artifact.Size)); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(tmpDir, webIndexFileName)); err != nil {
		return errWebUpdateFailed("index_missing", err)
	}

	finalDir := a.webVersionDir(version)
	if err := os.RemoveAll(finalDir); err != nil {
		return errWebUpdateFailed("replace_version", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return errWebUpdateFailed("activate", err)
	}
	activated = true

	// 原子替换激活指针：用临时文件 + MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH)，
	// **不能**用 os.Rename（Windows 上它不覆盖已存在目标）。
	if err := writeWebCurrent(a.webCurrentPath(), webCurrentPointer{
		Version:     version,
		Dir:         version,
		ActivatedAt: time.Now().UnixMilli(),
	}); err != nil {
		// 新版本目录已就位但指针未更新：current.json 仍指向旧版本，旧版本保持可用，
		// 绝不留下"指向不存在目录"的半激活状态。
		return errWebUpdateFailed("activate_pointer", err)
	}
	pruneWebVersions(webappDir, version, a.logger)
	return nil
}

// webappDir 返回渲染层资源根目录：<代理数据目录>/webapp（%ProgramData%\Vault\webapp）。
func (a *Agent) webappDir() string { return filepath.Join(DataDir(), webappDirName) }

// webCurrentPath 返回激活指针路径：<webapp>/current.json。
func (a *Agent) webCurrentPath() string {
	return filepath.Join(a.webappDir(), webCurrentFileName)
}

// webVersionDir 返回指定版本的资源目录：<webapp>/<version>。
func (a *Agent) webVersionDir(version string) string {
	return filepath.Join(a.webappDir(), version)
}

// verifyArtifactFile 复核产物文件的大小与 sha256（与清单声明一致）。
func verifyArtifactFile(path string, art *updatepkg.Artifact) error {
	f, err := os.Open(path)
	if err != nil {
		return errWebUpdateFailed("verify_open", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return errWebUpdateFailed("verify_read", err)
	}
	if art.Size > 0 && n != art.Size {
		return apperr.New(CodeDigestMismatch, http.StatusUnprocessableEntity).
			WithArg("expected_size", art.Size).WithArg("actual_size", n)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, art.SHA256) {
		return apperr.New(CodeDigestMismatch, http.StatusUnprocessableEntity).
			WithArg("expected", art.SHA256).WithArg("actual", got)
	}
	return nil
}

// webExtractMaxBytes 返回解压后允许的总字节上限：产物声明 size 的 1.5 倍 + 1MiB。
//
// 依据见 webExtractSizeRatioNum 的说明；size 未知（<=0）时只给固定余量。
func webExtractMaxBytes(artifactSize int64) int64 {
	if artifactSize <= 0 {
		return webExtractSlackBytes
	}
	return artifactSize*webExtractSizeRatioNum/webExtractSizeRatioDen + webExtractSlackBytes
}

// extractWebZip 把 zip 解压到 destDir，拒绝路径穿越（zip slip）并限制解压总大小。
func extractWebZip(zipPath, destDir string, maxBytes int64) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return errWebUpdateFailed("extract_open", err)
	}
	defer func() { _ = zr.Close() }()

	remaining := maxBytes
	for _, f := range zr.File {
		rel := strings.ReplaceAll(f.Name, `\`, "/")
		if err := validateWebZipEntry(rel); err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		// 二次确认：拼接后必须仍在 destDir 内（防 zip slip 的兜底）。
		if !isWithinDir(destDir, target) {
			return errWebUpdateFailed("zip_slip", nil)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return errWebUpdateFailed("extract", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return errWebUpdateFailed("extract", err)
		}
		if err := writeZipEntry(f, target, &remaining); err != nil {
			return err
		}
	}
	return nil
}

// validateWebZipEntry 校验 zip 条目名：只接受**相对路径**，拒绝绝对路径 / ".." / 驱动器号。
func validateWebZipEntry(rel string) error {
	if rel == "" {
		return errWebUpdateFailed("zip_entry", errors.New("空条目名"))
	}
	if strings.HasPrefix(rel, "/") {
		return errWebUpdateFailed("zip_entry", errors.New("绝对路径条目"))
	}
	// 驱动器号（C:/...）或 NTFS ADS / UNC 中的冒号：一律拒绝。
	if strings.Contains(rel, ":") {
		return errWebUpdateFailed("zip_entry", errors.New("条目含冒号"))
	}
	clean := strings.TrimSuffix(rel, "/")
	if clean == "" {
		return errWebUpdateFailed("zip_entry", errors.New("空条目名"))
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return errWebUpdateFailed("zip_entry", errors.New("条目含非法路径段"))
		}
	}
	return nil
}

// isWithinDir 判断 target 是否位于 dir 之内（含 dir 自身）。
func isWithinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// writeZipEntry 写出单个 zip 条目，并从共享预算中扣减已写字节（防 zip bomb）。
//
// 用 LimitReader 而非信任 zip 头声明的 UncompressedSize：即便头部谎报也无法越过上限。
func writeZipEntry(f *zip.File, target string, remaining *int64) error {
	rc, err := f.Open()
	if err != nil {
		return errWebUpdateFailed("extract_open_entry", err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return errWebUpdateFailed("extract_write", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(rc, *remaining+1))
	closeErr := out.Close()
	if copyErr != nil {
		return errWebUpdateFailed("extract_write", copyErr)
	}
	if closeErr != nil {
		return errWebUpdateFailed("extract_write", closeErr)
	}
	*remaining -= n
	if *remaining < 0 {
		return errWebUpdateFailed("extract_too_large", nil)
	}
	return nil
}

// writeWebCurrent 原子写入激活指针（临时文件 + MoveFileEx 替换）。
func writeWebCurrent(path string, ptr webCurrentPointer) error {
	data, err := json.MarshalIndent(ptr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readWebCurrent 读取激活指针；不存在或不可解析时 ok=false。
func readWebCurrent(path string) (webCurrentPointer, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return webCurrentPointer{}, false
	}
	var ptr webCurrentPointer
	if err := json.Unmarshal(data, &ptr); err != nil {
		return webCurrentPointer{}, false
	}
	if strings.TrimSpace(ptr.Version) == "" {
		return webCurrentPointer{}, false
	}
	return ptr, true
}

// cleanupWebTemp 清理 <webapp> 下遗留的临时解压目录与下载残留（尽力而为）。
func cleanupWebTemp(webappDir string, logger *slog.Logger) {
	entries, err := os.ReadDir(webappDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir() && strings.Contains(name, webTempPrefix):
			if err := os.RemoveAll(filepath.Join(webappDir, name)); err != nil {
				logger.Warn("清理热更临时目录失败", "dir", name, "error", err)
			}
		case !e.IsDir() && strings.HasSuffix(name, partSuffix):
			_ = os.Remove(filepath.Join(webappDir, name))
		}
	}
}

// pruneWebVersions 只保留最近 webVersionsKept 个版本目录，更旧的删除（避免无限堆积）。
func pruneWebVersions(webappDir, keepVersion string, logger *slog.Logger) {
	entries, err := os.ReadDir(webappDir)
	if err != nil {
		return
	}
	type versionedDir struct {
		name string
		v    semver.Version
	}
	dirs := make([]versionedDir, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.Contains(name, webTempPrefix) {
			_ = os.RemoveAll(filepath.Join(webappDir, name))
			continue
		}
		v, parseErr := semver.Parse(name)
		if parseErr != nil {
			continue // 非版本号目录（如用户手工放的其它目录）：不动它
		}
		dirs = append(dirs, versionedDir{name: name, v: v})
	}
	// 版本号降序：最新在前。
	sort.Slice(dirs, func(i, j int) bool { return semver.Compare(dirs[i].v, dirs[j].v) > 0 })
	if len(dirs) <= webVersionsKept {
		return
	}
	for _, d := range dirs[webVersionsKept:] {
		if sameVersion(d.name, keepVersion) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(webappDir, d.name)); err == nil {
			logger.Info("已清理更旧的渲染层资源版本", "version", d.name)
		}
	}
}
