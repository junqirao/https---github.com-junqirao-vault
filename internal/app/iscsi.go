package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// 取 16：Windows（12–16 字符）与 Linux LIO（内核硬限制 12–16 字节纯 ASCII）两侧的
// 合法区间上限。secret.RandomSecret 现在直接产出 n 个纯字母数字字符（不再 base64），
// 因此这里的 n 就是最终字符数（16），不存在"编码膨胀超长"的问题。
const defaultChapSecretChars = 16

// IscsiService 负责 iSCSI 目标的发布、授权、鉴权与收敛。
//
// 授权模型：**一个分配对应一个独立目标**（见 docs/implementation.md 5.3），
// 因为 Windows 的 CHAP 是"每目标一组账号"，无法在同一目标内区分多用户。
type IscsiService struct {
	Deps
	Disks *DiskService

	// pushedMu / pushedSpecs 缓存"本次进程内已成功下发过的目标期望状态指纹"（key=目标名）。
	//
	// 为什么需要它：Windows 上一次全量下发（建/改目标 + 授权 + CHAP + 映射 + 启用）实测约 **15 秒**，
	// 而**每次挂载**、每次对账都会走到 pushTarget（真实工单："每次挂载都要等十几秒"）。
	// 而平台层的 GetTarget 只能读回 Name/Enabled/Initiators/Devices 四类字段，**读不回 CHAP 与只读标志**，
	// 所以只靠"读现状比对"无法证明"CHAP 仍是上次下发的那一组"。
	//
	// 跳过下发需要**两个条件同时成立**（见 targetUnchanged）：
	//  ① 指纹一致 → 期望状态与上次成功下发时逐字段相同（含密钥；密钥只进哈希，绝不进日志/DB）；
	//  ② 现状比对 → Windows 侧没被外部改动（启用状态 / 授权列表 / 映射盘）。
	// 进程重启后缓存为空 → 首次仍会全量下发一次（安全侧）。
	pushedMu    sync.Mutex
	pushedSpecs map[string]string

	// actualIQNMu / actualIQNs 记录**平台侧实际的目标名**（key 为短名，小写）。
	//
	// 为什么必须读回来：即便我们按 IQN 下发，平台也可能按自己的规则改写目标名
	// （Windows 目标服务器就有这种形态）。下发给客户端的 IQN 只要与平台实际存储的不一致，
	// 表现就是"端口通、登录失败"。读回后再下发，等于以平台为准。
	// 纯内存即可：每次挂载都会先走 Publish（见 LeaseService.RequestMount）。
	actualIQNMu sync.Mutex
	actualIQNs  map[string]string
}

// targetIQN 返回该目标**应当下发给客户端**的 IQN：优先用平台读回的实际名字，
// 读回记录缺失时回退到按前缀推导（列表展示等未下发过的场景）。
func (s *IscsiService) targetIQN(t *domain.IscsiTarget) string {
	if t == nil {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(t.TargetName))
	s.actualIQNMu.Lock()
	actual := s.actualIQNs[key]
	s.actualIQNMu.Unlock()
	if strings.TrimSpace(actual) != "" {
		return actual
	}
	return t.IQN(s.iqnPrefix())
}

// rememberActualIQN 记录平台侧实际的目标名。
func (s *IscsiService) rememberActualIQN(shortName, actual string) {
	shortName = strings.ToLower(strings.TrimSpace(shortName))
	actual = strings.TrimSpace(actual)
	if shortName == "" || actual == "" {
		return
	}
	s.actualIQNMu.Lock()
	if s.actualIQNs == nil {
		s.actualIQNs = make(map[string]string)
	}
	s.actualIQNs[shortName] = actual
	s.actualIQNMu.Unlock()
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

// migrateLegacyTargetName 清理"旧命名"目标（一次性迁移），避免同一个 VHDX 被两个目标映射。
//
// 旧目标用的是短名，而客户端拿到的 IQN 从来就没对上过 —— 也就是说它们从未被成功连接，
// 因此删除是安全的。删除失败不阻断（新目标仍会创建，映射冲突时会有明确报错）。
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
		s.forgetPushedSpec(name)
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
			s.migrateLegacyTargetName(ctx, t.TargetName, iqn)
			if err := s.pushTarget(ctx, t, t.DesiredEnabled); err != nil {
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
			spec, sErr := s.buildTargetSpec(ctx, t, enabled)
			if sErr != nil {
				s.Log.Error("构造目标期望状态失败", "target", t.TargetName, "error", sErr)
				continue
			}
			if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
				s.Log.Error("收敛目标启用状态失败", "target", t.TargetName, "error", err)
				continue
			}
			if err := s.Store.SetIscsiTargetEnabled(ctx, t.ID, enabled); err != nil {
				s.Log.Warn("写回目标启用状态失败", "target", t.TargetName, "error", err)
			}
			s.Log.Info("已收敛 iSCSI 目标启用状态", "target", t.TargetName, "enabled", enabled)
		}
	}
	return nil
}

// Publish 把一个分配对应的 VHDX 通过 iSCSI 暴露出去。
//
// 幂等：目标记录与 Windows 侧配置都会"先查后建/先查后改"。
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
		allocID := alloc.ID
		target = &domain.IscsiTarget{
			TargetName:     targetNameFor(repo.ID, alloc.ID),
			DiskID:         &diskID,
			Purpose:        domain.PurposeUser,
			AllocationID:   &allocID,
			AuthMode:       domain.AuthModeCHAP,
			ChapUser:       "vault-" + shortID(alloc.ID),
			ChapSecretEnc:  encSecret,
			Enabled:        false,
			DesiredEnabled: true,
		}
		if err := s.Store.CreateIscsiTarget(ctx, target); err != nil {
			return nil, err
		}
	} else if target.DiskID == nil || *target.DiskID != disk.ID {
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
	// 一次性迁移：清掉"旧命名"（短名）的目标，否则同一个 VHDX 会同时被两个目标映射。
	s.migrateLegacyTargetName(ctx, target.TargetName, iqn)

	// 阶段推进：整条链路最慢的一步（Windows 上一次全量下发实测约 15 秒，见 pushedMu 说明）。
	s.emitMountPhase(allocationID, MountPhaseConfiguringTarget)
	if err := s.pushTarget(ctx, target, true); err != nil {
		return nil, err
	}
	// 读回平台侧**实际**的目标名，作为下发给客户端的 IQN。
	//
	// 以平台为准而不是以推导值为准：只要两者不一致，客户端就是"端口通、登录失败"。
	// 读不到时回退到推导值并告警（不阻塞挂载，让排障有据）。
	actual := iqn
	if found := s.discoverActualTargetName(ctx, iqn, target.TargetName); found != "" {
		if !strings.EqualFold(found, iqn) {
			s.Log.Warn("平台侧实际目标名与下发的 IQN 不一致，以实际值为准",
				"target", target.TargetName, "expected", iqn, "actual", found)
		}
		actual = found
	} else {
		s.Log.Warn("下发后读不回 iSCSI 目标（平台可能改写了目标名），按推导 IQN 下发",
			"target", target.TargetName, "iqn", iqn)
	}
	s.rememberActualIQN(target.TargetName, actual)

	// 诊断：把平台侧读回的实际状态记下来（名字 / 启用 / initiator 授权 / 映射）。
	//
	// connect 阶段失败报 "target name is not found or is marked as hidden from login" 时，
	// 只有这几项能定案：到底是被 Windows 改了名、没启用、还是 initiator 白名单拒绝
	// （真实事故：目标明明 created+enabled+mapped，客户端仍连不上）。挂载失败时请贴这行。
	if s.Iscsi != nil {
		// 按寻址名（iqn）读回——TargetIqn 是连接名，`-TargetName` 查不到它。
		if info, err := s.Iscsi.GetTarget(ctx, iqn); err == nil && info != nil {
			s.Log.Info("iSCSI 目标实际状态",
				"target", actual,
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
		// 按**寻址名**（下发的完整 IQN = Windows TargetName）操作。
		// 不能用 targetIQN：那是客户端连接用的 TargetIqn（Windows 改写后的名字）。
		iqn := target.IQN(s.iqnPrefix())
		if diskPath != "" {
			if err := s.Iscsi.DetachLun(ctx, iqn, diskPath); err != nil {
				s.Log.Warn("解除映射失败", "target", target.TargetName, "error", err)
			}
		}
		if err := s.Iscsi.EnsureTarget(ctx, platform.TargetSpec{Name: iqn, Enabled: false}); err != nil {
			s.Log.Warn("停用目标失败", "target", target.TargetName, "error", err)
		}
		if err := s.Iscsi.RemoveTarget(ctx, iqn); err != nil {
			s.Log.Warn("删除目标失败", "target", target.TargetName, "error", err)
		}
		// 目标已删除：丢弃指纹，避免下次"以为已下发过"而跳过（见 pushedSpecs 的说明）。
		s.forgetPushedSpec(iqn)
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
		spec, sErr := s.buildTargetSpec(ctx, target, target.DesiredEnabled)
		if sErr != nil {
			return nil, sErr
		}
		if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
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
			spec, sErr := s.buildTargetSpec(ctx, target, false)
			if sErr != nil {
				return sErr
			}
			if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
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

// pushTarget 把 DB 中的目标配置全量下发给平台后端（enabled 由调用方给出）。
func (s *IscsiService) pushTarget(ctx context.Context, target *domain.IscsiTarget, enabled bool) error {
	if s.Iscsi == nil {
		return errNotImplemented
	}
	spec, err := s.buildTargetSpec(ctx, target, enabled)
	if err != nil {
		return err
	}
	// 无变化则跳过下发：本次全量下发在 Windows 上实测约 15 秒，而挂载路径每次都会走到这里，
	// 绝大多数时候目标状态根本没变（真实工单：每次挂载都要等十几秒）。
	if s.targetUnchanged(ctx, spec) {
		s.Log.Info("iSCSI 目标状态未变化，跳过下发", "target", spec.Name)
		return nil
	}
	if err := s.Iscsi.EnsureTarget(ctx, spec); err != nil {
		return err
	}
	s.rememberPushedSpec(spec)
	return nil
}

// targetUnchanged 判断本次全量下发是否可以安全跳过（判据见 IscsiService.pushedSpecs 的说明）。
func (s *IscsiService) targetUnchanged(ctx context.Context, spec platform.TargetSpec) bool {
	if s.lastPushedSpec(spec.Name) != targetSpecFingerprint(spec) {
		return false
	}
	info, err := s.Iscsi.GetTarget(ctx, spec.Name)
	if err != nil || info == nil {
		// 读不到现状（目标不存在或查询失败）：老实下发。
		// 绝不因为"我记得下发过"就跳过一个可能已经不存在的目标。
		return false
	}
	return targetInfoMatches(spec, info)
}

// lastPushedSpec 读取某目标上次成功下发的指纹；从未下发过时返回空串。
func (s *IscsiService) lastPushedSpec(name string) string {
	s.pushedMu.Lock()
	defer s.pushedMu.Unlock()
	return s.pushedSpecs[name]
}

// rememberPushedSpec 记录某目标成功下发的指纹（key 统一小写，与目标名大小写不敏感一致）。
func (s *IscsiService) rememberPushedSpec(spec platform.TargetSpec) {
	s.pushedMu.Lock()
	defer s.pushedMu.Unlock()
	if s.pushedSpecs == nil {
		s.pushedSpecs = make(map[string]string)
	}
	s.pushedSpecs[strings.ToLower(strings.TrimSpace(spec.Name))] = targetSpecFingerprint(spec)
}

// forgetPushedSpec 丢弃某目标的指纹（目标被删除后必须调用，避免下次"以为已下发"）。
func (s *IscsiService) forgetPushedSpec(name string) {
	s.pushedMu.Lock()
	defer s.pushedMu.Unlock()
	delete(s.pushedSpecs, strings.ToLower(strings.TrimSpace(name)))
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
// 它们由"指纹一致"来兜底（见 pushedSpecs）。
func targetInfoMatches(spec platform.TargetSpec, info *platform.TargetInfo) bool {
	if info == nil {
		return false
	}
	if spec.Enabled != info.Enabled {
		return false
	}
	if !sameStringsFold(spec.Initiators, info.Initiators) {
		return false
	}
	if spec.BackingRef == "" {
		// 期望"不发布任何盘"：实际还有映射就是不一致。
		return len(info.Devices) == 0
	}
	return containsFold(info.Devices, spec.BackingRef)
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
	return s.Iscsi.EnsureTarget(ctx, spec)
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

// targetNameFor 生成面向用户的分配目标名：vault-<repoID前8位>-<allocationID前8位>。
//
// ⚠️ 分隔符**只能**用 '-'，不能用 '_'：目标名会被 Windows 当作 IQN 后缀校验，
// 而 IQN 语法（RFC 3720）只允许字母、数字、'.'、'-'、':'，下划线非法——
// 用 '_' 命名时 New-IscsiServerTarget 会直接以"无法创建 iSCSI 目标。"失败
// （日志 reason=set_target_failed step=create_target）。
func targetNameFor(repoID, allocationID string) string {
	return "vault-" + shortID(repoID) + "-" + shortID(allocationID)
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
