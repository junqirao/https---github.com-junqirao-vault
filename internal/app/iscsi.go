package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/secret"
)

// defaultChapSecretChars 是自动生成的 CHAP 密钥的**字符数**（纯字母数字）。
//
// ⚠️ 取 15 而非 16：16 是 Windows CHAP 密钥的**上限边界**，实测会触发"目标侧与发起
// 程序侧对密钥的截断/校验不一致"，表现为 connect 阶段报
// "target name is not found or is marked as hidden from login"（真实事故：手动用
// 15 字符密钥连接即成功，16 字符即失败）。15 落在 12–16 的安全区间内，
// 15×log2(62)≈89 bit 熵仍足够。
const defaultChapSecretChars = 15

// IscsiService 负责 iSCSI 目标的发布、授权、鉴权与收敛。
//
// 授权模型：**一个分配对应一个独立目标**（见 docs/implementation.md 5.3），
// 因为 Windows 的 CHAP 是"每目标一组账号"，无法在同一目标内区分多用户。
//
// # 分配与分发：下发一次，之后只读库
//
// Windows 上一次全量下发（建/改目标 + 授权 + CHAP + 映射 + 启用）实测约 **35 秒**
// （两次 Set-IscsiServerTarget 各约 15 秒 + 一次映射约 5 秒），而"连一块已经连过的盘"
// 在业务上是**分发**动作，不该付这份成本（真实工单："再连一次要等 1 分钟"）。
//
// 因此把两件事分开（见 docs/implementation.md 5.3）：
//
//   - **分配阶段**（建库预创建 EnsurePoolTarget / 首次挂载 / 配置变更）：全量下发一次，
//     下发成功后把**期望状态指纹**写进 `iscsi_targets.applied_fingerprint`；
//   - **分发阶段**（之后每次挂载）：只拿 DB 里的指纹与新构造的期望状态比一比，一致就整段跳过 ——
//     连"读回平台现状"都不做（读回同样要跑 PowerShell，目标多时更慢）。
//
// 指纹包含 CHAP 密钥与只读标志（密钥只进哈希，绝不落库/进日志）：平台侧读不回这两项，
// 所以"读现状比对"本来就证明不了"CHAP 仍是上次那组"，反而是它把跳过判死的原因之一。
//
// 平台侧被外部改动的兜底放在**对账器**里（ReconcileTargets）：目标不存在 → 重建；
// 启用状态不符 → 按 DB 收敛；读回与记录不一致 → 告警（见该函数的说明）。
type IscsiService struct {
	Deps
	Disks *DiskService
}

// targetIQN 返回该目标**应当下发给客户端**的 IQN：优先用下发/对账时读回并持久化的实际名字，
// 从未读回过时回退到按前缀推导（列表展示等未下发过的场景）。
//
// 为什么必须读回一次：即便我们按 IQN 下发，平台也可能按自己的规则改写目标名
// （Windows 目标服务器就有这种形态）。下发给客户端的 IQN 只要与平台实际存储的不一致，
// 表现就是"端口通、登录失败"。读回后落库，等于以平台为准且只付一次读回成本。
func (s *IscsiService) targetIQN(t *domain.IscsiTarget) string {
	if t == nil {
		return ""
	}
	if actual := strings.TrimSpace(t.ActualIQN); actual != "" {
		return actual
	}
	return t.IQN(s.iqnPrefix())
}

// rememberActualIQN 记录平台侧实际的目标名（落库，跨重启有效；未变化时不写）。
func (s *IscsiService) rememberActualIQN(ctx context.Context, t *domain.IscsiTarget, actual string) {
	actual = strings.TrimSpace(actual)
	if t == nil || t.ID == "" || actual == "" || actual == strings.TrimSpace(t.ActualIQN) {
		return
	}
	if err := s.Store.SetIscsiTargetActualIQN(ctx, t.ID, actual); err != nil {
		s.Log.Warn("写回 iSCSI 实际 IQN 失败", "target", t.TargetName, "actual_iqn", actual, "error", err)
		return
	}
	t.ActualIQN = actual
}

// legacyWinIQNPrefix 是 Windows 目标服务器给"非 IQN 形态"目标名加上的默认命名权。
//
// 来源：iscsi_set_target.ps1 的兼容性判断（实测到 iqn.1991-05.com.microsoft:<短名>）。
const legacyWinIQNPrefix = "iqn.1991-05.com.microsoft:"

// legacyTargetNameMatches 判断平台上的目标名是否属于"旧命名"形态（短名，或被 Windows
// 加上了默认命名权的形式）。
func legacyTargetNameMatches(name, shortName string) bool {
	name = strings.TrimSpace(name)
	shortName = strings.TrimSpace(shortName)
	if name == "" || shortName == "" {
		return false
	}
	if strings.EqualFold(name, shortName) {
		return true
	}
	return strings.EqualFold(name, legacyWinIQNPrefix+shortName)
}

// discoverActualTargetName 定位目标**客户端登录时必须用的实际 IQN**，找不到时返回空串。
//
// Windows 会改写目标名：我们按 TargetName（如 iqn.2026-01.com.vault:vault-xxx）下发，
// 但平台真正暴露给 initiator 的是 TargetIqn（形如
// iqn.1991-05.com.microsoft:<host>-<name>-target）。客户端拿 TargetName 去连必然
// "target name is not found or is marked as hidden from login"（真实事故）。
// 故这里优先读回 TargetIqn，其次回退到 Name（Linux LIO 二者一致）。
//
// 两条查找路径：
//
//	① 按我们下发的 IQN 精确查（正常情况）；
//	② 查不到时按"短名后缀"在全部目标里找。
func (s *IscsiService) discoverActualTargetName(ctx context.Context, iqn, shortName string) string {
	if info, err := s.Iscsi.GetTarget(ctx, iqn); err == nil && info != nil {
		if actual := strings.TrimSpace(info.IQN); actual != "" {
			return actual
		}
		if name := strings.TrimSpace(info.Name); name != "" {
			return name
		}
	}
	targets, err := s.Iscsi.ListTargets(ctx)
	if err != nil {
		return ""
	}
	suffix := strings.ToLower(":" + strings.TrimSpace(shortName))
	short := strings.ToLower(strings.TrimSpace(shortName))
	for i := range targets {
		name := strings.TrimSpace(targets[i].Name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if lower == short || strings.HasSuffix(lower, suffix) {
			if actual := strings.TrimSpace(targets[i].IQN); actual != "" {
				return actual
			}
			return name
		}
	}
	return ""
}

// isRewrittenIQN 判断平台读回的**实际**目标名是否只是"我们下发的 IQN 被套进了平台自己的命名"。
//
// Windows 目标服务器就是这么干的（实测形态）：下发的 TargetName
// `iqn.2026-01.com.vault:vault-xxx` 会被改写成 TargetIqn
// `iqn.1991-05.com.microsoft:<host>-iqn.2026-01.com.vault:vault-xxx-target` ——
// **我们下发的那段 IQN 被完整保留在内**。LIO 只认短名时则表现为
// `iqn.1991-05.com.microsoft:<短名>`（同样保留了目标标识）。
//
// 这类"改写"是每次挂载都会发生的**常态**，不是故障：正确做法就是按读回值下发
// （见 rememberActualIQN / targetIQN），所以不该当作异常刷 WARN。
// 只有读回值与下发的目标**毫无关联**（外部改动 / 串目标）时才值得告警。
func isRewrittenIQN(actual, iqn string) bool {
	a := strings.ToLower(strings.TrimSpace(actual))
	e := strings.ToLower(strings.TrimSpace(iqn))
	if a == "" || e == "" {
		return false
	}
	if a == e || strings.Contains(a, e) {
		return true
	}
	// 退一步：平台用自己的默认命名权 + 短名（短名取自 IQN 冒号之后的部分）。
	short := e
	if i := strings.LastIndex(e, ":"); i >= 0 {
		short = e[i+1:]
	}
	if short == "" {
		return false
	}
	return strings.HasPrefix(a, legacyWinIQNPrefix) && strings.Contains(a, short)
}

// migrateLegacyTargetName 清理"旧命名"目标（一次性迁移），避免同一个 VHDX 被两个目标映射。
//
// 旧目标用的是短名，而客户端拿到的 IQN 从来就没对上过 —— 也就是说它们从未被成功连接，
// 因此删除是安全的。删除失败不阻断（新目标仍会创建，映射冲突时会有明确报错）。
//
// 只在"真的要下发"时才调用（见 pushTarget）：它要枚举平台上的全部目标，也是一次 PowerShell。
func (s *IscsiService) migrateLegacyTargetName(ctx context.Context, shortName, iqn string) {
	if s.Iscsi == nil || strings.EqualFold(strings.TrimSpace(shortName), strings.TrimSpace(iqn)) {
		return
	}
	targets, err := s.Iscsi.ListTargets(ctx)
	if err != nil {
		s.Log.Warn("列出 iSCSI 目标失败，跳过旧命名迁移", "target", shortName, "error", err)
		return
	}
	for i := range targets {
		name := strings.TrimSpace(targets[i].Name)
		if name == "" || strings.EqualFold(name, iqn) || !legacyTargetNameMatches(name, shortName) {
			continue
		}
		s.Log.Warn("发现旧命名的 iSCSI 目标，删除后按完整 IQN 重建", "legacy", name, "iqn", iqn)
		if rmErr := s.Iscsi.RemoveTarget(ctx, name); rmErr != nil {
			s.Log.Warn("删除旧命名 iSCSI 目标失败", "legacy", name, "error", rmErr)
			continue
		}
		s.clearAppliedByName(ctx, name)
	}
}

// SetAuthInput 是设置 iSCSI 鉴权的入参。
type SetAuthInput struct {
	AuthMode domain.AuthMode
	ChapUser string
	// ChapSecret 为空且 mode=chap 时自动生成随机密钥（长度见 defaultChapSecretBytes）。
	ChapSecret string
}

// TargetSummary 是 iSCSI 目标的对外视图。
//
// 安全约束：**绝不包含 chap_secret_enc 密文或明文密钥**（见 docs/implementation.md 5.3）。
type TargetSummary struct {
	ID             string `json:"id"`
	TargetName     string `json:"target_name"`
	IQN            string `json:"iqn"`
	RepoID         string `json:"repo_id,omitempty"`
	DiskID         string `json:"disk_id,omitempty"`
	Purpose        string `json:"purpose"`
	AllocationID   string `json:"allocation_id,omitempty"`
	AuthMode       string `json:"auth_mode"`
	ChapUser       string `json:"chap_user,omitempty"`
	HasChapSecret  bool   `json:"has_chap_secret"`
	Enabled        bool   `json:"enabled"`
	DesiredEnabled bool   `json:"desired_enabled"`
	ReadOnly       bool   `json:"read_only"`
	// MappedDevice 数据库中登记的映射虚拟盘路径。
	MappedDevice string `json:"mapped_device,omitempty"`
	// Initiators 授权白名单（本地权威记录）。
	Initiators []domain.InitiatorID `json:"initiators"`
	// WindowsMappedDevices 来自 Windows 侧的实际映射（iSCSI 角色不可用时为空）。
	WindowsMappedDevices []string `json:"windows_mapped_devices,omitempty"`
	// WindowsEnabled 来自 Windows 侧的实际启用状态（角色不可用时为 false）。
	WindowsEnabled bool  `json:"windows_enabled"`
	CreatedAt      int64 `json:"created_at"`
	UpdatedAt      int64 `json:"updated_at"`
}

// ListSummaries 列出 iSCSI 目标视图；repoID 非空时仅返回该库下的目标。
func (s *IscsiService) ListSummaries(ctx context.Context, repoID string) ([]TargetSummary, error) {
	targets, err := s.Store.ListIscsiTargets(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]TargetSummary, 0, len(targets))
	for i := range targets {
		sum, err := s.buildSummary(ctx, &targets[i], false)
		if err != nil {
			return nil, err
		}
		if repoID != "" && sum.RepoID != repoID {
			continue
		}
		out = append(out, *sum)
	}
	return out, nil
}

// Summary 返回单个目标的视图（含 Windows 侧实际状态，尽力而为）。
func (s *IscsiService) Summary(ctx context.Context, targetID string) (*TargetSummary, error) {
	t, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return nil, err
	}
	return s.buildSummary(ctx, t, true)
}

// RepoIDOfTarget 返回目标所属存储库 ID；无法归属时返回空串。
func (s *IscsiService) RepoIDOfTarget(ctx context.Context, targetID string) (string, error) {
	t, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return "", err
	}
	return s.repoIDOf(ctx, t), nil
}

// buildSummary 组装目标视图。
func (s *IscsiService) buildSummary(ctx context.Context, t *domain.IscsiTarget, withWindows bool) (*TargetSummary, error) {
	ids, err := s.Store.ListInitiatorIDs(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	sum := &TargetSummary{
		ID: t.ID, TargetName: t.TargetName, IQN: t.IQN(s.iqnPrefix()),
		RepoID:  s.repoIDOf(ctx, t),
		Purpose: string(t.Purpose), AuthMode: string(t.AuthMode), ChapUser: t.ChapUser,
		HasChapSecret: len(t.ChapSecretEnc) > 0,
		Enabled:       t.Enabled, DesiredEnabled: t.DesiredEnabled, ReadOnly: t.ReadOnly,
		Initiators: ids, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if t.DiskID != nil {
		sum.DiskID = *t.DiskID
		if d, dErr := s.Store.GetDisk(ctx, *t.DiskID); dErr == nil {
			sum.MappedDevice = d.VHDXPath
		}
	}
	if t.AllocationID != nil {
		sum.AllocationID = *t.AllocationID
	}
	if withWindows && s.Iscsi != nil {
		// 按**寻址名**（下发的完整 IQN = Windows TargetName）查询，不是客户端连接用的
		// TargetIqn（后者见 targetIQN / discoverActualTargetName）。
		if info, wErr := s.Iscsi.GetTarget(ctx, t.IQN(s.iqnPrefix())); wErr == nil {
			sum.WindowsEnabled = info.Enabled
			sum.WindowsMappedDevices = info.Devices
		}
	}
	return sum, nil
}

// repoIDOf 解析目标所属存储库：优先按磁盘，其次按分配。
func (s *IscsiService) repoIDOf(ctx context.Context, t *domain.IscsiTarget) string {
	if t.DiskID != nil {
		if d, err := s.Store.GetDisk(ctx, *t.DiskID); err == nil {
			return d.RepoID
		}
	}
	if t.AllocationID != nil {
		if a, err := s.Store.GetAllocation(ctx, *t.AllocationID); err == nil {
			return a.RepoID
		}
	}
	return ""
}

// ListSessions 枚举目标（name 为空表示全部）的在线会话。
//
// 平台不支持会话枚举（如 Windows）时返回 platform.ErrUnsupported，调用方据此走降级路径。
func (s *IscsiService) ListSessions(ctx context.Context, name string) ([]platform.Session, error) {
	if s.Iscsi == nil {
		return nil, platform.ErrUnsupported
	}
	return s.Iscsi.ListSessions(ctx, name)
}

// ForceLogout 强制登出指定会话。
//
// ⚠️ 平台动作可能只是降级实现（停用目标 + 拆除 ACL），见 platform.IscsiBackend.ForceLogout。
func (s *IscsiService) ForceLogout(ctx context.Context, name, sessionID string) error {
	if s.Iscsi == nil {
		return platform.ErrUnsupported
	}
	return s.Iscsi.ForceLogout(ctx, name, sessionID)
}

// RegisterHandlers 把 iSCSI 相关任务注册到 worker。
func (s *IscsiService) RegisterHandlers(w *job.Worker) {
	if w == nil {
		return
	}
	w.Register(job.HandlerFunc{T: domain.JobPublish, F: s.runPublish})
}

// Available 探测 iSCSI 目标服务器能力。
func (s *IscsiService) Available(ctx context.Context) error {
	if s.Iscsi == nil {
		return apperr.New(apperr.CodeUnavailable, 503).WithArg("component", "iscsitarget")
	}
	return s.Iscsi.Available(ctx)
}

// ReconcileTargets 收敛 iSCSI 目标的期望状态与实际状态。
//
// 规则：DB 是期望值的来源；实际 Enabled 与期望不一致时按 DB 重新下发
// （见 docs/implementation.md 5.11 对账器）。
func (s *IscsiService) ReconcileTargets(ctx context.Context) error {
	if s.Iscsi == nil {
		return nil
	}
	targets, err := s.Store.ListIscsiTargets(ctx, "")
	if err != nil {
		return err
	}
	for i := range targets {
		t := &targets[i]
		// 查询/重建用**寻址名**（下发的完整 IQN = Windows TargetName）。
		// 不能用 targetIQN：那是客户端连接用的 TargetIqn（Windows 改写后的名字）。
		iqn := t.IQN(s.iqnPrefix())
		info, getErr := s.Iscsi.GetTarget(ctx, iqn)
		if getErr != nil {
			// ⚠️ 只有"确实不存在"（404）才重建。其它错误——脚本超时、权限不足、
			// 目标名非法（iscsi.invalid_target_name）等——一律原样保留并跳过本轮：
			// 把任何错误都当成"不存在"去 upsert，会把真因伪装成"重建失败"，
			// 还会每轮白跑一次昂贵的 PowerShell 调用（真实服务器上单次约 10 秒）。
			if !isNotFound(getErr) {
				s.Log.Error("查询 iSCSI 目标失败，跳过本轮收敛",
					"target", t.TargetName, "error", getErr)
				continue
			}
			// Windows 侧不存在：用 SetTarget（upsert）重建目标并按 DB 期望值启用。
			// 重建前先清掉旧命名的目标：它们与新 IQN 目标会争抢同一个 VHDX 的映射。
			//
			// 必须**强制**下发：记账里的指纹可能还留着（目标是被外部删掉的），
			// 走会跳过的 pushTarget 会把"重建"变成空操作。
			s.migrateLegacyTargetName(ctx, t.TargetName, iqn)
			if err := s.forcePushTarget(ctx, t, t.DesiredEnabled); err != nil {
				s.Log.Error("重建 iSCSI 目标失败", "target", t.TargetName, "error", err)
				continue
			}
			if err := s.Store.SetIscsiTargetEnabled(ctx, t.ID, t.DesiredEnabled); err != nil {
				s.Log.Warn("写回目标启用状态失败", "target", t.TargetName, "error", err)
			}
			continue
		}
		if info.Enabled != t.DesiredEnabled {
			enabled := t.DesiredEnabled
			// 强制下发：平台现状已经确知与期望不符，跳过判据在这里不适用。
			if err := s.forcePushTarget(ctx, t, enabled); err != nil {
				s.Log.Error("收敛目标启用状态失败", "target", t.TargetName, "error", err)
				continue
			}
			if err := s.Store.SetIscsiTargetEnabled(ctx, t.ID, enabled); err != nil {
				s.Log.Warn("写回目标启用状态失败", "target", t.TargetName, "error", err)
			}
			s.Log.Info("已收敛 iSCSI 目标启用状态", "target", t.TargetName, "enabled", enabled)
			continue
		}
		// 启用状态一致，但读回与"已下发的期望状态"对不上：平台被外部改动过
		// （授权列表被清、映射被换、目标被改名）。这里**只告警、不自动重下发**：
		// 下发一次约 35 秒，对账要遍历全部目标，逐个重下发会撑爆对账预算（见 5.11）；
		// 而且比对本身依赖平台读回的字段形态，误判的代价（每轮对账把"已下发"全部重置）
		// 比漏报大得多。真正的自愈入口是上面的 404 重建与启用状态收敛。
		if t.AppliedAt > 0 {
			if expected, bErr := s.buildTargetSpec(ctx, t, t.DesiredEnabled); bErr == nil &&
				!targetInfoMatches(expected, info) {
				s.Log.Warn("iSCSI 目标平台现状与已下发的期望状态不一致（授权/映射可能被外部改动），"+
					"如需重新下发请对该目标执行一次鉴权变更或重新挂载",
					"target", t.TargetName, "enabled", info.Enabled, "initiators", len(info.Initiators),
					"devices", len(info.Devices))
			}
		}
	}
	return nil
}

// Publish 把一个分配对应的 VHDX 通过 iSCSI 暴露出去。
//
// 幂等：目标记录与 Windows 侧配置都会"先查后建/先查后改"。
// createUserTarget 新建一个面向该磁盘的 user 目标（默认 CHAP）。
//
// allocationID 允许为空：建库预创建时还没有任何分配（池位目标就是这样建的），
// 之后分配/挂载时再由 resolveUserTarget 把关联补上。
func (s *IscsiService) createUserTarget(ctx context.Context, repo *domain.Repository,
	disk *domain.Disk, allocationID *string) (*domain.IscsiTarget, error) {
	if s.Cipher == nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "cipher_unavailable")
	}
	// 默认使用 CHAP：Windows 侧无会话枚举能力，租约 + CHAP 是唯一的访问控制手段。
	plainSecret, err := secret.RandomSecret(defaultChapSecretChars)
	if err != nil {
		return nil, err
	}
	encSecret, err := s.Cipher.EncryptString(plainSecret)
	if err != nil {
		return nil, err
	}
	diskID := disk.ID
	target := &domain.IscsiTarget{
		TargetName: targetNameForDisk(repo.ID, disk.ID),
		DiskID:     &diskID,
		Purpose:    domain.PurposeUser,
		// CHAP 用户名按**磁盘**派生（与目标名同源）：池化库在建库预创建时就把凭据定好了，
		// 分配与挂载都不再碰它，客户端拿到的用户名与平台侧始终对得上。
		AllocationID:   allocationID,
		AuthMode:       domain.AuthModeCHAP,
		ChapUser:       "vault-" + shortID(disk.ID),
		ChapSecretEnc:  encSecret,
		Enabled:        false,
		DesiredEnabled: true,
	}
	if err := s.Store.CreateIscsiTarget(ctx, target); err != nil {
		return nil, err
	}
	return target, nil
}

// findUserTargetForDisk 查一块盘上的 user 目标，**不改任何绑定**。
//
// 盘上没有 user 目标时返回 (nil, nil)：调用方按"现建"的老路径处理。
func (s *IscsiService) findUserTargetForDisk(ctx context.Context, diskID string) (*domain.IscsiTarget, error) {
	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, diskID)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	for i := range targets {
		if targets[i].Purpose == domain.PurposeUser {
			return &targets[i], nil
		}
	}
	return nil, nil
}

// resolveUserTarget 取这块盘上的 user 目标；没有就建一个，必要时把它绑到分配上。
//
// 这是"目标从哪来"的唯一入口：池化预建、分配即预建、挂载时现建，三条来源都走这里，
// 于是"这块盘到底该不该有目标、已经有了就认领"这件事只有一个判断点。
//
// bindAllocation 语义：
//   - nil：不改变目标与分配的绑定（只保证"目标存在"）；
//   - 非 nil：按该值绑定；传空串表示"建库阶段，目标还没有归属"。
//
// 并发安全：目标名由"库 + 盘"派生（targetNameForDisk），schema 上 target_name 是 UNIQUE。
// 挂载路径（Publish）与预创建路径（EnsureDiskTarget）完全可能同时算到同一个目标名 ——
// 盘在 JobCreateDiff 结尾刚变 ready，客户端就被 awaitBackingReady 唤醒。输的一方 INSERT
// 失败，这里再查一次把赢家的记录认下来继续；否则两个目标映射同一块盘，
// 客户端连上哪个都是薛定谔的盘。
func (s *IscsiService) resolveUserTarget(ctx context.Context, repo *domain.Repository,
	disk *domain.Disk, bindAllocation *string) (*domain.IscsiTarget, error) {
	target, err := s.findUserTargetForDisk(ctx, disk.ID)
	if err != nil {
		return nil, err
	}
	if target == nil {
		// 新建时只在有真实绑定时才写关联：空串等价于"没有分配"，保持 NULL 语义与老路径一致。
		createBind := bindAllocation
		if createBind != nil && *createBind == "" {
			createBind = nil
		}
		created, err := s.createUserTarget(ctx, repo, disk, createBind)
		if err != nil {
			// 并发：另一条路径同时建出了同一个目标名的记录。让赢家赢，把它认下来。
			again, findErr := s.findUserTargetForDisk(ctx, disk.ID)
			if findErr != nil || again == nil {
				return nil, err
			}
			target = again
		} else {
			target = created
		}
	}
	if bindAllocation != nil && (target.AllocationID == nil || *target.AllocationID != *bindAllocation) {
		if err := s.Store.SetIscsiTargetAllocation(ctx, target.ID, *bindAllocation); err != nil {
			return nil, err
		}
		allocID := *bindAllocation
		target.AllocationID = &allocID
	}
	return target, nil
}

// EnsurePoolTarget 保证池位盘有一个已启用的 user 目标（建库预创建阶段三反复调用）。
//
// 与 Publish 的区别是**不依赖分配**：建库时还没有任何分配，目标先建好、启用，
// 客户端什么都不用做；之后的分配与挂载只是把它认领过去（resolveUserTarget）。
//
// 幂等：先查后建、先查后改，任务重试不会多出第二个映射同一块盘的目标。
func (s *IscsiService) EnsurePoolTarget(ctx context.Context, repo *domain.Repository, disk *domain.Disk) error {
	// 建库阶段目标确实还没有归属，显式按空值绑定，沿用老语义（见 resolveUserTarget）。
	empty := ""
	return s.ensureUserTarget(ctx, repo, disk, &empty)
}

// EnsureDiskTarget 提前为一块盘建好并启用 user 目标：资源先就位，再分配给用户。
//
// 覆盖原先两条"只能等用户挂载时现场下发"的路径：
//   - 非池化共享库的差异盘：分配时才派生（JobCreateDiff），在派生完成后顺手把目标建好；
//   - 独享库的盘：建库时就有（分配只是把它指给用户），在盘建好后就把目标建好。
//
// 收益：用户点挂载只剩"连接"——Publish 命中目标指纹后整段跳过（省掉一次约 35 秒的
// 全量下发），挂载不再卡在服务端现场建目标上。
//
// 与 EnsurePoolTarget 的唯一区别是**不碰 AllocationID**：本方法可能在分配之后才被调用
// （任务重试、补建），把已经认领的绑定抹掉会让后续按 allocation 查目标（Unpublish、
// 会话管理、摘要）查不到东西。
func (s *IscsiService) EnsureDiskTarget(ctx context.Context, repo *domain.Repository, disk *domain.Disk) error {
	return s.ensureUserTarget(ctx, repo, disk, nil)
}

// ensureUserTarget 是 EnsurePoolTarget / EnsureDiskTarget 的公共实现：
// 保证目标存在且已启用（必要时绑定），并把期望状态下发到平台。
func (s *IscsiService) ensureUserTarget(ctx context.Context, repo *domain.Repository,
	disk *domain.Disk, bindAllocation *string) error {
	if s.Iscsi == nil {
		// 平台没装配 iSCSI 后端（Linux/测试环境）：盘照建，发布留给挂载路径。
		return nil
	}
	target, err := s.resolveUserTarget(ctx, repo, disk, bindAllocation)
	if err != nil {
		return err
	}
	if target == nil {
		return nil
	}

	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	iqn := target.IQN(s.iqnPrefix())
	// 这里是整条预创建链路最慢的一步：Windows 上一次全量下发实测约 35 秒（见 IscsiService 说明）。
	// 付一次就够 —— 期望状态与"已下发"记账都进了 DB，之后的每次挂载只是分发。
	if _, err := s.pushTarget(ctx, target, true); err != nil {
		return err
	}
	// 读回平台侧**实际**的目标名（可能被改写）并落库：下发过一次之后，
	// 挂载路径直接用这个值，不必每次再读回。
	if found := s.discoverActualTargetName(ctx, iqn, target.TargetName); found != "" {
		s.rememberActualIQN(ctx, target, found)
	}
	if err := s.Store.SetIscsiTargetEnabled(ctx, target.ID, true); err != nil {
		return err
	}
	target.Enabled = true
	target.DesiredEnabled = true

	if disk.State != domain.DiskStatePublished {
		disk.State = domain.DiskStatePublished
		if err := s.Store.UpdateDisk(ctx, disk); err != nil {
			return err
		}
	}
	// 预创建路径：池位盘（建库阶段三）与"分配即预建"的盘（非池化差异盘 / 独享库盘）共用。
	s.audit(ctx, "", "iscsi.publish", "target:"+target.TargetName, "disk="+disk.ID, domain.AuditResultOK)
	return nil
}

func (s *IscsiService) Publish(ctx context.Context, allocationID string) (*domain.IscsiTarget, error) {
	alloc, err := s.Store.GetAllocation(ctx, allocationID)
	if err != nil {
		return nil, err
	}
	disk, err := s.Store.GetDisk(ctx, alloc.DiskID)
	if err != nil {
		return nil, err
	}
	repo, err := s.Store.GetRepository(ctx, alloc.RepoID)
	if err != nil {
		return nil, err
	}

	target, err := s.Store.GetIscsiTargetByAllocation(ctx, allocationID)
	if err != nil {
		if !isNotFound(err) {
			return nil, err
		}
		target = nil
	}

	if target == nil {
		// 池化库（以及"分配即预建/建库即预建"的库）：目标早就建好了（那时还没有分配，
		// 所以只按 disk 关联）。先认领它，而不是再建一个 —— 两个目标映射同一个 VHDX 会在
		// 平台侧互相抢映射，客户端连上哪个都是薛定谔的盘。
		//
		// 目标确实不存在时（既没池化也没预建）由同一条路径建出来并直接绑到这次分配上。
		target, err = s.resolveUserTarget(ctx, repo, disk, &alloc.ID)
		if err != nil {
			return nil, err
		}
	}
	if target == nil {
		// 理论不可达（resolveUserTarget 只会返回"找到或建好"的目标）；留一手不裸解引用。
		return nil, apperr.IscsiTargetNotFound()
	}
	if target.DiskID == nil || *target.DiskID != disk.ID {
		target.DiskID = &disk.ID
		if err := s.Store.UpdateIscsiTarget(ctx, target); err != nil {
			return nil, err
		}
	}

	// 差异盘是分配时**异步**派生的（JobCreateDiff），而客户端往往紧接着就点挂载。
	// 不等它建完就去 Import-IscsiVirtualDisk，只会得到"VHDX 文件不存在"，
	// 最终以 platform.ps_failed 500 抛给用户——而实际上一百多毫秒后盘就好了。
	// 阶段推进（下发界面）：差异盘是分配时异步派生的，客户端常常紧接着就点挂载，
	// 这里可能要等几秒到十几秒。
	s.emitMountPhase(allocationID, MountPhasePreparingDisk)
	if err := s.awaitBackingReady(ctx, disk); err != nil {
		return nil, err
	}

	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	iqn := target.IQN(s.iqnPrefix())

	// 阶段推进：分配阶段（首次挂载 / 配置变更）要全量下发一次，Windows 实测约 35 秒
	// （两次 Set-IscsiServerTarget 各约 15 秒 + 一次映射约 5 秒）；分发阶段整段跳过。
	// 两者的分工见 IscsiService 的说明。
	s.emitMountPhase(allocationID, MountPhaseConfiguringTarget)
	pushed, err := s.pushTarget(ctx, target, true)
	if err != nil {
		return nil, err
	}

	// 实际 IQN 与诊断只在"刚下发过 / 还没读回过"时做：两者都要再起一次 PowerShell，
	// 而分发路径（已下发且已读回）用落库的值就够了。
	if pushed || strings.TrimSpace(target.ActualIQN) == "" {
		// 读回平台侧**实际**的目标名，作为下发给客户端的 IQN。
		//
		// 以平台为准而不是以推导值为准：只要两者不一致，客户端就是"端口通、登录失败"。
		// 读不到时回退到推导值并告警（不阻塞挂载，让排障有据）。
		if found := s.discoverActualTargetName(ctx, iqn, target.TargetName); found != "" {
			switch {
			case strings.EqualFold(found, iqn):
				// 平台原样保留，无需任何日志。
			case isRewrittenIQN(found, iqn):
				// 平台把我们的 IQN 套进了自己的命名里（Windows 的常态形态，见 isRewrittenIQN）。
				// 这是**下发后**的正常形态，故只记 Debug：它对"能不能连上"没有任何解释力，
				// 却在 WARN 级别刷屏（真实工单：每次挂载都看到这条）。
				s.Log.Debug("平台改写了目标名，以实际值为准",
					"target", target.TargetName, "expected", iqn, "actual", found)
			default:
				// 真异常：读回的名字与下发的目标毫无关联（外部改动 / 串到别的目标）。
				s.Log.Warn("平台侧实际目标名与下发的 IQN 无关，以实际值为准",
					"target", target.TargetName, "expected", iqn, "actual", found)
			}
			s.rememberActualIQN(ctx, target, found)
		} else {
			s.Log.Warn("下发后读不回 iSCSI 目标（平台可能改写了目标名），按推导 IQN 下发",
				"target", target.TargetName, "iqn", iqn)
		}

		// 诊断：把平台侧读回的实际状态记下来（名字 / 启用 / initiator 授权 / 映射）。
		//
		// connect 阶段失败报 "target name is not found or is marked as hidden from login" 时，
		// 只有这几项能定案：到底是被 Windows 改了名、没启用、还是 initiator 白名单拒绝
		// （真实事故：目标明明 created+enabled+mapped，客户端仍连不上）。挂载失败时请贴这行。
		// 按寻址名（iqn）读回——TargetIqn 是连接名，`-TargetName` 查不到它。
		if info, err := s.Iscsi.GetTarget(ctx, iqn); err == nil && info != nil {
			s.Log.Info("iSCSI 目标实际状态",
				"target", s.targetIQN(target),
				"enabled", info.Enabled,
				"initiators", strings.Join(info.Initiators, ","),
				"mapped_devices", len(info.Devices))
		} else {
			s.Log.Warn("读回 iSCSI 目标状态失败", "target", iqn, "error", err)
		}
	}

	if err := s.Store.SetIscsiTargetEnabled(ctx, target.ID, true); err != nil {
		return nil, err
	}
	target.Enabled = true
	target.DesiredEnabled = true

	if disk.State != domain.DiskStatePublished {
		disk.State = domain.DiskStatePublished
		if err := s.Store.UpdateDisk(ctx, disk); err != nil {
			return nil, err
		}
	}
	s.audit(ctx, "", "iscsi.publish", "target:"+target.TargetName, "alloc="+allocationID, domain.AuditResultOK)
	return target, nil
}

// Unpublish 取消发布：解除映射、停用并删除目标（DB 记录一并删除）。
func (s *IscsiService) Unpublish(ctx context.Context, targetID string) error {
	target, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return err
	}
	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	var diskPath string
	if target.DiskID != nil {
		if disk, dErr := s.Store.GetDisk(ctx, *target.DiskID); dErr == nil {
			diskPath = disk.VHDXPath
		}
	}
	if s.Iscsi != nil {
		// 按**寻址名**拆，走唯一实现（见 teardownTargetLocked）。不能用短名
		// （target.TargetName）：平台上不存在这个名字，三步会静默变成 not_found；
		// 也不能用 targetIQN（客户端连接名，Windows 改写后的形态）。
		// 本方法已持有 lock.TargetKey，故用不加锁版本。
		_ = s.teardownTargetLocked(ctx, target.TargetName, target.IQN(s.iqnPrefix()),
			TeardownOptions{DetachDiskPath: diskPath})
	}
	if err := s.restoreDiskState(ctx, target); err != nil {
		return err
	}
	if err := s.Store.DeleteIscsiTarget(ctx, targetID); err != nil {
		return err
	}
	s.audit(ctx, "", "iscsi.unpublish", "target:"+target.TargetName, "", domain.AuditResultOK)
	return nil
}

// TeardownOptions 描述一次目标拆除的附加动作。
type TeardownOptions struct {
	// DetachDiskPath 非空时，先解除该路径在本目标上的 LUN 映射。
	// 映射是"目标 + 路径"成对登记的，解除必须带上目标；留空表示只拆目标本身。
	DetachDiskPath string
	// skipDisable 跳过"先停用再删除"里的停用，只有对账清孤儿用（见 TeardownOrphanTarget）。
	skipDisable bool
}

// TeardownTarget 按 DB 目标拆除平台侧目标（并清掉下发指纹）。调用方无需持锁。
//
// 这是**唯一**的目标拆除实现：删盘/回收（DiskService）、撤销发布（Unpublish）、
// 撤销母盘临时共享（RepoService）、对账清理孤儿（App.reconcileIscsi）都调它。
// 此前四条路径各写一套，寻址名一半用**短名**、一半用**完整 IQN**；短名在平台上查不到，
// 于是"解映射 → 停用 → 删除"全部静默变成 not_found（脚本把"目标不存在"当幂等成功），
// 结果是磁盘删了、目标还挂着的"孤儿目标"；"停用"那步还会按短名凭空建出一个新目标。
// （真实诉求："磁盘已删但 iSCSI 目标还挂着的自动清理"。）
func (s *IscsiService) TeardownTarget(ctx context.Context, t *domain.IscsiTarget, opt TeardownOptions) {
	if t == nil {
		return
	}
	unlock := s.Locks.Acquire(lock.TargetKey(t.TargetName))
	defer unlock()
	_ = s.teardownTargetLocked(ctx, t.TargetName, t.IQN(s.iqnPrefix()), opt)
}

// TeardownTargetByShortName 按**目标短名**拆除：寻址名由 iqn_prefix 现推。
//
// 存在的必要：存储库回收任务在事务里先删掉 iscsi_targets 记录，后台任务手里只剩短名
// 快照（见 diskArtifact）。短名不是寻址名，绝不能直接拿去问平台。
func (s *IscsiService) TeardownTargetByShortName(ctx context.Context, shortName string, opt TeardownOptions) {
	shortName = strings.TrimSpace(shortName)
	if shortName == "" {
		return
	}
	unlock := s.Locks.Acquire(lock.TargetKey(shortName))
	defer unlock()
	iqn := (&domain.IscsiTarget{TargetName: shortName}).IQN(s.iqnPrefix())
	_ = s.teardownTargetLocked(ctx, shortName, iqn, opt)
}

// TeardownOrphanTarget 拆除**平台上存在、DB 已无记录**的孤儿目标。
//
// name 必须是**平台读回的目标名**（ListTargets 的 Name）：孤儿恰恰是"名字不符合我们的
// 推导规则"的那批（旧短名形态、平台改写形态），按前缀推导寻址名只会又找不到它。
func (s *IscsiService) TeardownOrphanTarget(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	unlock := s.Locks.Acquire(lock.TargetKey(name))
	defer unlock()
	// 跳过停用：删除本身就会带走会话，而对账预算只有 60s、一次 PowerShell 要 15-20 秒
	// （真实事故：大批孤儿时日志刷屏 context deadline exceeded），多一步必然删不动。
	return s.teardownTargetLocked(ctx, name, name, TeardownOptions{skipDisable: true})
}

// teardownTargetLocked 是目标拆除的唯一实现：解映射（可选）→ 停用 → 删除 → 清指纹。
// 调用方必须已持有 lock.TargetKey(shortName)。iqn 是可问到平台的**寻址名**，
// shortName 只用于日志与指纹清理。
//
//   - 先停用再删除：停用让在线 initiator 立刻掉线；删除失败时目标至少停在停用态，
//     不会继续对外提供数据。
//   - 全程尽力而为：目标可能已被人工删掉，失败只记日志，由调用方决定是否中断
//     （删盘路径的真正失败点是文件删不掉，见 removeDiskFile）。
func (s *IscsiService) teardownTargetLocked(ctx context.Context, shortName, iqn string, opt TeardownOptions) error {
	if s.Iscsi == nil || strings.TrimSpace(iqn) == "" {
		return nil
	}
	if opt.DetachDiskPath != "" {
		if err := s.Iscsi.DetachLun(ctx, iqn, opt.DetachDiskPath); err != nil {
			s.Log.Warn("解除目标映射失败", "target", shortName, "iqn", iqn, "path", opt.DetachDiskPath, "error", err)
		}
	}
	if !opt.skipDisable {
		if err := s.Iscsi.EnsureTarget(ctx, platform.TargetSpec{Name: iqn, Enabled: false}); err != nil {
			s.Log.Warn("停用目标失败", "target", shortName, "iqn", iqn, "error", err)
		}
	}
	err := s.Iscsi.RemoveTarget(ctx, iqn)
	if err != nil {
		s.Log.Warn("删除目标失败", "target", shortName, "iqn", iqn, "error", err)
	}
	// 目标已删除：丢弃下发记账，避免下次"以为已下发过"而跳过（见 IscsiService 的说明）。
	// 按短名查（DB 记录用它做主键定位），平台上的寻址名（iqn）与它可能是两个形态。
	s.clearAppliedByName(ctx, shortName)
	return err
}

// SetAuthorization 以"先读后合并再整体回写"的方式更新 initiator 白名单。
//
// ⚠️ Windows 的 `Set-IscsiServerTarget -InitiatorIds` 是**全量替换**语义，
// 且模块没有增量 cmdlet，因此这里必须先读现有值、合并后一次性回写（见 5.3）。
func (s *IscsiService) SetAuthorization(ctx context.Context, targetID string, ids []domain.InitiatorID) error {
	target, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !id.Valid() {
			return apperr.InvalidParam("initiator_id")
		}
	}

	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	current, err := s.Store.ListInitiatorIDs(ctx, targetID)
	if err != nil {
		return err
	}
	merged := mergeInitiators(current, ids)
	if err := s.Store.ReplaceInitiatorIDs(ctx, targetID, merged); err != nil {
		return err
	}
	return s.pushInitiatorIDs(ctx, target, merged)
}

// AddInitiator 追加一条 initiator 白名单（读-合并-整体回写）。
func (s *IscsiService) AddInitiator(ctx context.Context, targetID string, id domain.InitiatorID) error {
	if !id.Valid() {
		return apperr.InvalidParam("initiator_id")
	}
	target, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return err
	}
	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	current, err := s.Store.ListInitiatorIDs(ctx, targetID)
	if err != nil {
		return err
	}
	merged := mergeInitiators(current, []domain.InitiatorID{id})
	if err := s.Store.ReplaceInitiatorIDs(ctx, targetID, merged); err != nil {
		return err
	}
	return s.pushInitiatorIDs(ctx, target, merged)
}

// RemoveInitiator 移除一条 initiator 白名单（读-过滤-整体回写）。
func (s *IscsiService) RemoveInitiator(ctx context.Context, targetID string, id domain.InitiatorID) error {
	target, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return err
	}
	unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
	defer unlock()

	current, err := s.Store.ListInitiatorIDs(ctx, targetID)
	if err != nil {
		return err
	}
	kept := make([]domain.InitiatorID, 0, len(current))
	for _, cur := range current {
		if cur.Type == id.Type && cur.Value == id.Value {
			continue
		}
		kept = append(kept, cur)
	}
	if err := s.Store.ReplaceInitiatorIDs(ctx, targetID, kept); err != nil {
		return err
	}
	return s.pushInitiatorIDs(ctx, target, kept)
}

// SetAuth 配置目标的鉴权模式。authMode=chap 且未提供密钥时自动生成 16 字节密钥。
//
// 密钥明文只用于下发给 Windows 侧（日志中一律脱敏为 ***），落库一律加密。
func (s *IscsiService) SetAuth(ctx context.Context, targetID string, in SetAuthInput) (*domain.IscsiTarget, error) {
	if !in.AuthMode.Valid() {
		return nil, apperr.InvalidParam("auth_mode")
	}
	target, err := s.Store.GetIscsiTarget(ctx, targetID)
	if err != nil {
		return nil, err
	}

	var plainSecret string
	switch in.AuthMode {
	case domain.AuthModeCHAP:
		if s.Cipher == nil {
			return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "cipher_unavailable")
		}
		user := strings.TrimSpace(in.ChapUser)
		if user == "" {
			user = target.ChapUser
		}
		if user == "" {
			user = "vault-" + shortID(target.ID)
		}
		plainSecret = in.ChapSecret
		if plainSecret == "" {
			if plainSecret, err = secret.RandomSecret(defaultChapSecretChars); err != nil {
				return nil, err
			}
		}
		enc, err := s.Cipher.EncryptString(plainSecret)
		if err != nil {
			return nil, err
		}
		target.AuthMode = in.AuthMode
		target.ChapUser = user
		target.ChapSecretEnc = enc
	default:
		// 切回 none/ip：清空 CHAP 凭据。
		// ⚠️ Windows 侧关闭 CHAP 的参数行为需实测确认（脚本仅在提供 ChapUser 时启用 CHAP）。
		target.AuthMode = in.AuthMode
		target.ChapUser = ""
		target.ChapSecretEnc = nil
	}

	if err := s.Store.UpdateIscsiTarget(ctx, target); err != nil {
		return nil, err
	}
	s.Log.Info("已更新 iSCSI 鉴权配置",
		"target", target.TargetName, "auth_mode", target.AuthMode, "chap_user", target.ChapUser, "chap_secret", "***")

	if s.Iscsi != nil {
		unlock := s.Locks.Acquire(lock.TargetKey(target.TargetName))
		defer unlock()
		// CHAP 凭据已写入 target（含密文），buildTargetSpec 会解密后放进期望状态。
		// 必须强制下发：凭据已经变了，跳过判据在这里只会把新密钥留在 DB 里。
		if err := s.forcePushTarget(ctx, target, target.DesiredEnabled); err != nil {
			return nil, err
		}
	}
	s.audit(ctx, "", "iscsi.set_auth", "target:"+target.TargetName, string(target.AuthMode), domain.AuditResultOK)
	return target, nil
}

// DisableTarget 停用目标（踢下线的第一步）。
//
// ⚠️ 语义降级：Windows 的 IscsiTarget 模块与 Linux 的 LIO 都**没有**精确到会话的
// 服务端强制登出，"踢下线"= 停用目标 + 通知客户端自行断开，且必须**保持停用**（见 5.8）。
func (s *IscsiService) DisableTarget(ctx context.Context, targetName string) error {
	if strings.TrimSpace(targetName) == "" {
		return apperr.InvalidParam("target_name")
	}
	unlock := s.Locks.Acquire(lock.TargetKey(targetName))
	defer unlock()

	target, err := s.Store.GetIscsiTargetByName(ctx, targetName)
	if err != nil && !isNotFound(err) {
		return err
	}

	if s.Iscsi != nil {
		if target != nil {
			// 全量下发（Enabled=false）：保持 ACL/CHAP/映射不变，只把目标停用。
			// 必须强制下发：这里的目的就是**改变**平台状态，跳过判据不适用。
			if err := s.forcePushTarget(ctx, target, false); err != nil {
				return err
			}
		} else if err := s.Iscsi.EnsureTarget(ctx, platform.TargetSpec{Name: targetName, Enabled: false}); err != nil {
			// DB 无记录：退化为"仅停用"的最小期望状态，保证目标确实被关掉。
			return err
		}
	}
	if target == nil {
		return nil
	}
	if err := s.Store.SetIscsiTargetEnabled(ctx, target.ID, false); err != nil {
		return err
	}
	s.audit(ctx, "", "iscsi.disable_target", "target:"+targetName, "", domain.AuditResultOK)
	return nil
}

// buildTargetSpec 构造一个目标的**完整期望状态**。
//
// 之所以全量而不增量：Windows 的 `-InitiatorIds` 本身就是整体覆盖，
// Linux LIO 的 configfs 写入同样"以目录内容为准"，两边都天然适合全量下发
// （见 platform.TargetSpec 说明）。所有下发路径（pushTarget / pushInitiatorIDs /
// SetAuth / DisableTarget / ReconcileTargets）都经由本函数，避免各处口径漂移。
//
// CHAP 明文只在本方法内解密，绝不写入日志。
func (s *IscsiService) buildTargetSpec(ctx context.Context, target *domain.IscsiTarget, enabled bool) (platform.TargetSpec, error) {
	// Name 用**完整 IQN**（= Windows 的 TargetName，寻址用），不能是短名。
	//
	// 注意：Name 只是**寻址名**，不是客户端登录要用的名字。Windows 目标服务器会**改写**
	// 目标名——TargetName 保持我们下发的值，但真正暴露给 initiator 的是 TargetIqn
	// （形如 iqn.1991-05.com.microsoft:<host>-<name>-target）。客户端登录名以读回的
	// TargetIqn 为准（见 discoverActualTargetName / targetIQN），这里只管寻址名。
	spec := platform.TargetSpec{
		Name:     target.IQN(s.iqnPrefix()),
		Enabled:  enabled,
		ReadOnly: target.ReadOnly,
	}
	if target.DiskID != nil {
		disk, err := s.Store.GetDisk(ctx, *target.DiskID)
		if err != nil {
			return spec, err
		}
		spec.BackingRef = disk.VHDXPath
	}
	ids, err := s.Store.ListInitiatorIDs(ctx, target.ID)
	if err != nil {
		return spec, err
	}
	spec.Initiators = initiatorValues(ids)

	if target.AuthMode == domain.AuthModeCHAP && target.ChapUser != "" && len(target.ChapSecretEnc) > 0 {
		if s.Cipher == nil {
			return spec, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "cipher_unavailable")
		}
		plain, err := s.Cipher.DecryptString(target.ChapSecretEnc)
		if err != nil {
			return spec, err
		}
		spec.ChapUser = target.ChapUser
		spec.ChapSecret = plain
	}
	return spec, nil
}

// 等待被发布磁盘就绪的参数。
//
// 取值理由：差异盘创建实测在百毫秒级（同一卷内建差异盘），因此正常情况下轮询一两次即返回；
// 上限刻意压得比客户端请求超时（30s）短，避免"用户已经超时、服务端还在干等"。
const (
	awaitBackingTimeout      = 15 * time.Second
	awaitBackingPollInterval = 200 * time.Millisecond
)

// awaitBackingReady 有限等待磁盘创建完成（状态 ready 且底层盘确实存在）。
//
// 为什么需要：分配（repo.Allocate）把差异盘创建交给异步任务，紧接着的挂载请求会抢跑。
// 就绪即返回；超时返回**可重试**的 disk.not_ready（409），而不是把一个"文件不存在"的
// 脚本错误包装成 platform.ps_failed 500 —— 后者会让用户以为出了硬故障。
//
// 平台未装配磁盘后端时（单元测试等）直接放行，保持既有行为。
func (s *IscsiService) awaitBackingReady(ctx context.Context, disk *domain.Disk) error {
	// 未装配存储（单元测试）时无从查询状态，保持既有行为直接放行。
	if s.Store == nil || disk == nil || disk.ID == "" {
		return nil
	}
	deadline := time.Now().Add(awaitBackingTimeout)
	for {
		if d, err := s.Store.GetDisk(ctx, disk.ID); err == nil {
			disk = d
		}

		switch disk.State {
		case domain.DiskStateReady, domain.DiskStatePublished:
			// ⚠️ published 也必须算"底层盘就绪"。
			//
			// 卸载（LeaseService.Release）后磁盘**保持 published**——这是刻意的：
			// 目标继续发布，客户端可以再次挂载。而 published 蕴含"盘已建好且已发布成功"，
			// 是 ready 的超集。只认 ready 会让"卸载后再挂载"每次白等 15 秒，
			// 然后报 disk.not_ready（用户看到的就是这个，且 100% 复现）。
			if s.Disk != nil && !s.Disk.Exists(disk.VHDXPath) {
				// 状态说就绪但文件不在：交给后续步骤报明确错误，这里不空等。
				return apperr.DiskNotFound().WithArg("id", disk.ID)
			}
			return nil
		case domain.DiskStateError:
			// 建盘已彻底失败（状态由 MarkCreateFailed 回滚写入）：立刻给出确定结论，
			// 既不白等 15 秒，也不用"仍在创建中，请稍后重试"误导用户。
			return apperr.DiskCreateFailed().WithArg("disk_id", disk.ID)
		}

		if !time.Now().Before(deadline) {
			return apperr.DiskNotReady().
				WithArg("disk_id", disk.ID).
				WithArg("state", string(disk.State))
		}
		select {
		case <-ctx.Done():
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(ctx.Err())
		case <-time.After(awaitBackingPollInterval):
		}
	}
}

// pushTarget 把 DB 中的目标配置下发给平台后端（enabled 由调用方给出）。
//
// 返回值 pushed=false 表示"当前这份期望状态早就下发过、本次整段跳过"（分发阶段的快路径）。
//
// 跳过判据**只看持久化记账**（applied_fingerprint），不再每次读回平台现状：
//
//   - 读回本身也要起一次 PowerShell（目标多时枚举更慢），是"再连一次要等很久"的另一半成本；
//   - 平台读不回 CHAP 与只读标志，靠现状比对证明不了"密钥仍是上次那组"，反而会把跳过判死。
//
// 平台侧被外部删除/停用由对账器收敛（见 ReconcileTargets），代价是发现得晚（对账周期），
// 换来的是挂载路径不再付这份钱。
func (s *IscsiService) pushTarget(ctx context.Context, target *domain.IscsiTarget, enabled bool) (bool, error) {
	if s.Iscsi == nil {
		return false, errNotImplemented
	}
	spec, err := s.buildTargetSpec(ctx, target, enabled)
	if err != nil {
		return false, err
	}
	if s.targetApplied(target, spec) {
		s.Log.Info("iSCSI 目标已按当前期望状态下发过，跳过下发", "target", spec.Name)
		return false, nil
	}
	// 一次性迁移：清掉"旧命名"（短名）的目标，否则同一个 VHDX 会同时被两个目标映射。
	// 只在真要下发时做 —— 它要枚举平台上的全部目标，同样是一次 PowerShell。
	s.migrateLegacyTargetName(ctx, target.TargetName, spec.Name)
	if err := s.applyTargetSpec(ctx, target, spec); err != nil {
		return false, err
	}
	return true, nil
}

// forcePushTarget 无条件全量下发（对账、鉴权变更、停用等"已经确知平台现状与期望不符"的场景）。
func (s *IscsiService) forcePushTarget(ctx context.Context, target *domain.IscsiTarget, enabled bool) error {
	if s.Iscsi == nil {
		return errNotImplemented
	}
	spec, err := s.buildTargetSpec(ctx, target, enabled)
	if err != nil {
		return err
	}
	return s.applyTargetSpec(ctx, target, spec)
}

// applyTargetSpec 下发一个构造好的期望状态，并把"已下发"记账写回 DB。
func (s *IscsiService) applyTargetSpec(ctx context.Context, target *domain.IscsiTarget, spec platform.TargetSpec) error {
	if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
		return err
	}
	s.markApplied(ctx, target, spec)
	return nil
}

// targetApplied 判断"当前这份期望状态是否已经成功下发到平台"。
//
// 指纹逐字段包含 Name/Enabled/ReadOnly/BackingRef/CHAP（含密钥哈希）/授权列表，
// 因此任何一项变了（换盘、轮换密钥、改白名单、停用/启用）都不再命中 → 重新下发。
func (s *IscsiService) targetApplied(target *domain.IscsiTarget, spec platform.TargetSpec) bool {
	if target == nil || target.ID == "" || target.AppliedAt <= 0 {
		return false
	}
	if strings.TrimSpace(target.AppliedFingerprint) == "" {
		return false
	}
	return target.AppliedFingerprint == targetSpecFingerprint(spec)
}

// markApplied 记录"这份期望状态已成功下发"。
//
// 写库失败只告警：最坏结果是下次多下发一次（15 秒），不该让一次成功的下发变成失败。
func (s *IscsiService) markApplied(ctx context.Context, target *domain.IscsiTarget, spec platform.TargetSpec) {
	fp := targetSpecFingerprint(spec)
	if err := s.Store.MarkIscsiTargetApplied(ctx, target.ID, fp); err != nil {
		s.Log.Warn("写回 iSCSI 下发记账失败", "target", target.TargetName, "error", err)
		return
	}
	target.AppliedFingerprint = fp
	target.AppliedAt = time.Now().UnixMilli()
}

// clearApplied 抹掉下发记账（平台侧目标已被拆除或被判定为漂移），下次必须重新下发。
func (s *IscsiService) clearApplied(ctx context.Context, target *domain.IscsiTarget) {
	if target == nil || target.ID == "" {
		return
	}
	if err := s.Store.ClearIscsiTargetApplied(ctx, target.ID); err != nil {
		if !isNotFound(err) {
			s.Log.Warn("清除 iSCSI 下发记账失败", "target", target.TargetName, "error", err)
		}
		return
	}
	target.AppliedFingerprint = ""
	target.AppliedAt = 0
	target.ActualIQN = ""
}

// clearAppliedByName 按目标短名抹掉下发记账（DB 里没有这条记录时静默返回）。
func (s *IscsiService) clearAppliedByName(ctx context.Context, shortName string) {
	shortName = strings.TrimSpace(shortName)
	if shortName == "" {
		return
	}
	target, err := s.Store.GetIscsiTargetByName(ctx, shortName)
	if err != nil {
		if !isNotFound(err) {
			s.Log.Warn("读取目标失败，未清除其下发记账", "target", shortName, "error", err)
		}
		return
	}
	s.clearApplied(ctx, target)
}

// targetSpecFingerprint 计算目标期望状态的指纹（含 CHAP 密钥，但密钥只作为哈希输入）。
//
// initiators 排序后参与哈希：授权列表是集合语义，顺序变化不该被当成"状态变了"。
func targetSpecFingerprint(spec platform.TargetSpec) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			_, _ = h.Write([]byte(part))
			_, _ = h.Write([]byte{0}) // 分隔符，避免相邻字段拼出歧义
		}
	}
	write(
		spec.Name,
		strconv.FormatBool(spec.Enabled),
		strconv.FormatBool(spec.ReadOnly),
		spec.BackingRef,
		spec.ChapUser,
		spec.ChapSecret,
		strconv.FormatBool(spec.EnableReverseChap),
		spec.ReverseChapUser,
		spec.ReverseChapSecret,
	)
	initiators := append([]string(nil), spec.Initiators...)
	sort.Strings(initiators)
	write(initiators...)
	return hex.EncodeToString(h.Sum(nil))
}

// targetInfoMatches 用**可读回的实际状态**校验期望状态是否已经生效。
//
// 只比可读的子集（Enabled / Initiators / 映射盘）：CHAP 与只读标志读不回来，
// 它们由"期望状态指纹的持久化记账"来兜底（见 targetApplied / AppliedFingerprint）。
func targetInfoMatches(spec platform.TargetSpec, info *platform.TargetInfo) bool {
	if info == nil {
		return false
	}
	if spec.Enabled != info.Enabled {
		return false
	}
	if !initiatorsMatch(spec.Initiators, info.Initiators) {
		return false
	}
	if spec.BackingRef == "" {
		// 期望"不发布任何盘"：实际还有映射就是不一致。
		return len(info.Devices) == 0
	}
	return containsFold(info.Devices, spec.BackingRef)
}

// initiatorsMatch 比较期望的白名单与平台读回的白名单是否等价。
//
// 必须先把通配项 `IQN:*` 归一掉：平台脚本在"白名单为空"时会下发通配（Windows 上
// 空列表 == 拒绝所有，必须显式放开，见 iscsi_set_target.ps1），因此读回的是 `["IQN:*"]`，
// 而我们期望的是空列表 `[]`。若直接用 sameStringsFold 比较，二者永远不等 →
// targetInfoMatches 永远 false → pushTarget 的"已下发就跳过"彻底失效 →
// **每次挂载都重付一次约 15 秒的全量下发**（真实工单：挂载慢十几秒）。
// `IQN:*` 与"空"语义相同（都是"不限制发起端"），故视为等价。
func initiatorsMatch(expected, actual []string) bool {
	return sameStringsFold(stripWildcardInitiators(expected), stripWildcardInitiators(actual))
}

// stripWildcardInitiators 去掉表示"不限制"的通配项，其余原样保留。
func stripWildcardInitiators(values []string) []string {
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if idx := strings.Index(value, ":"); idx >= 0 {
			idType := strings.ToUpper(strings.TrimSpace(value[:idx]))
			if idType == "IQN" && strings.TrimSpace(value[idx+1:]) == "*" {
				continue
			}
		}
		out = append(out, value)
	}
	return out
}

// sameStringsFold 比较两个字符串列表是否等价（集合语义：忽略顺序、大小写与空项）。
func sameStringsFold(a, b []string) bool {
	left := normalizeSet(a)
	right := normalizeSet(b)
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

func normalizeSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		out[trimmed] = true
	}
	return out
}

// containsFold 判断列表中是否包含目标值（大小写与分隔符不敏感，用于 Windows 路径比较）。
func containsFold(values []string, want string) bool {
	normalized := normalizeWindowsPath(want)
	for _, value := range values {
		if normalizeWindowsPath(value) == normalized {
			return true
		}
	}
	return false
}

func normalizeWindowsPath(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "/", `\`))
}

// pushInitiatorIDs 全量下发 initiator 白名单（其余字段沿用 DB 中的当前期望状态）。
func (s *IscsiService) pushInitiatorIDs(ctx context.Context, target *domain.IscsiTarget, ids []domain.InitiatorID) error {
	if s.Iscsi == nil {
		return errNotImplemented
	}
	spec, err := s.buildTargetSpec(ctx, target, target.DesiredEnabled)
	if err != nil {
		return err
	}
	spec.Initiators = initiatorValues(ids)
	if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
		return err
	}
	// 白名单是期望状态的一部分：必须记账，否则下次挂载会因为指纹不一致而白跑一次全量下发。
	s.markApplied(ctx, target, spec)
	return nil
}

// restoreDiskState 目标移除后把磁盘状态从 published 恢复为 ready。
func (s *IscsiService) restoreDiskState(ctx context.Context, target *domain.IscsiTarget) error {
	if target.DiskID == nil {
		return nil
	}
	disk, err := s.Store.GetDisk(ctx, *target.DiskID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if disk.State != domain.DiskStatePublished {
		return nil
	}
	disk.State = domain.DiskStateReady
	return s.Store.UpdateDisk(ctx, disk)
}

// runPublish 是 JobPublish 的处理器。
func (s *IscsiService) runPublish(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p publishPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if p.AllocationID == "" {
		return apperr.InvalidParam("allocation_id")
	}
	rep.Progress(20)
	if _, err := s.Publish(ctx, p.AllocationID); err != nil {
		return err
	}
	rep.Progress(100)
	return nil
}

// ---- 命名与转换 ----

// shortID 取 ID 的前 8 个字符，用于目标命名（全局唯一性由完整 UUID 保证，此处仅求可读）。
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// targetNameForDisk 生成面向用户的分配目标名：vault-<repoID前8位>-<diskID前8位>。
//
// 按**磁盘**而不是按分配命名：池化库在建库时就为每块池位盘建好了目标（那时还没有分配），
// 分配只是把目标绑上去；名字跟着盘走，释放回池后原地重建同一块盘也不需要换名字
// （换名字意味着 Windows 侧要多一次"删旧目标 + 建新目标"的全量下发）。
//
// ⚠️ 分隔符**只能**用 '-'，不能用 '_'：目标名会被 Windows 当作 IQN 后缀校验，
// 而 IQN 语法（RFC 3720）只允许字母、数字、'.'、'-'、':'，下划线非法——
// 用 '_' 命名时 New-IscsiServerTarget 会直接以"无法创建 iSCSI 目标。"失败
// （日志 reason=set_target_failed step=create_target）。
func targetNameForDisk(repoID, diskID string) string {
	return "vault-" + shortID(repoID) + "-" + shortID(diskID)
}

// tempTargetName 生成母盘临时共享的固定目标名：vault-<repoID前8位>-temp。
//
// 分隔符约束同 targetNameFor（不得出现 '_'）。
func tempTargetName(repoID string) string {
	return "vault-" + shortID(repoID) + "-temp"
}

// initiatorValues 转为后端需要的字符串列表。
//
// 格式为 `IdType:Value`（如 `IQN:iqn.xxx`）——这是 Windows 的
// `-InitiatorIds` 参数要求的原样格式；Linux(LIO) 后端会自行解析出其中的 IQN 值。
// 返回非 nil 空切片表示"显式清空"。
func initiatorValues(ids []domain.InitiatorID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// mergeInitiators 合并两组 initiator，去重并保持稳定顺序。
func mergeInitiators(base, more []domain.InitiatorID) []domain.InitiatorID {
	out := make([]domain.InitiatorID, 0, len(base)+len(more))
	seen := make(map[string]bool, len(base)+len(more))
	appendOne := func(id domain.InitiatorID) {
		key := id.String()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, id)
	}
	for _, id := range base {
		appendOne(id)
	}
	for _, id := range more {
		appendOne(id)
	}
	return out
}
