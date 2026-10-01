package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vault/internal/domain"
	"vault/internal/platform"
)

// reconcileBudget 是单次对账的时间预算（见 docs/implementation.md 5.11：不超过 60s）。
const reconcileBudget = 60 * time.Second

// orphanTargetPrefix 是本服务端创建的 iSCSI 目标名前缀，用于识别孤儿目标。
const orphanTargetPrefix = "vault-"

// ReconcileIssue 是对账发现的一条差异。
type ReconcileIssue struct {
	// Kind 稳定分类，如 disk.file_missing / iscsi.orphan_target。
	Kind string `json:"kind"`
	// Ref 关联实体标识（磁盘 ID / 目标名 / 分配 ID 等）。
	Ref string `json:"ref"`
	// Detail 人类可读描述（不含敏感信息与底层原始报错）。
	Detail string `json:"detail"`
	// AutoFixed 本次是否已自动修复（危险操作默认只报告，为 false）。
	AutoFixed bool `json:"auto_fixed"`
}

// ReconcileReport 是一次对账的完整报告。
type ReconcileReport struct {
	// At 本次对账开始时间（Unix 毫秒）。
	At int64 `json:"at"`
	// DurationMs 本次对账耗时（毫秒）。
	DurationMs int64 `json:"duration_ms"`
	// Checked 本次检查的实体数量。
	Checked int `json:"checked"`
	// Issues 发现的差异列表（可能为空数组）。
	Issues []ReconcileIssue `json:"issues"`
	// NotChecked 因缺少平台能力或外部不可用而跳过的检查项说明。
	NotChecked []string `json:"not_checked"`
	// Truncated 为 true 表示本次因超出时间预算被截断。
	Truncated bool `json:"truncated"`
}

// LastReconcileReport 返回最近一次对账报告（副本）；从未对账时返回空报告。
func (a *App) LastReconcileReport() *ReconcileReport {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	if a.lastReconcile == nil {
		return &ReconcileReport{Issues: []ReconcileIssue{}, NotChecked: []string{}}
	}
	cp := *a.lastReconcile
	cp.Issues = append([]ReconcileIssue{}, a.lastReconcile.Issues...)
	cp.NotChecked = append([]string{}, a.lastReconcile.NotChecked...)
	return &cp
}

// runReconcile 执行一次完整对账，并把报告保存在内存中供查询接口读取。
//
// 可重入、幂等；危险操作默认只报告（见 5.11）。单次运行受 budget 限制，超时截断。
func (a *App) runReconcile(parent context.Context, budget time.Duration) error {
	log := a.Log.With("component", "reconcile")
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()

	rep := &ReconcileReport{At: start.UnixMilli(), Issues: []ReconcileIssue{}, NotChecked: []string{}}
	report := func(kind, ref, detail string, fixed bool) {
		rep.Issues = append(rep.Issues, ReconcileIssue{Kind: kind, Ref: ref, Detail: detail, AutoFixed: fixed})
	}

	// 1) 任务恢复：进程重启后 running 的任务退回 pending。
	if n, err := a.Store.ResetRunningJobs(ctx); err != nil {
		log.Error("重置运行中任务失败", "error", err)
		report("job.reset_failed", "", "重置运行中任务失败", false)
	} else if n > 0 {
		log.Info("已重置运行中任务", "count", n)
	}

	// 2) iSCSI 目标期望状态收敛（含启用状态回写）。
	if err := a.iscsi.ReconcileTargets(ctx); err != nil {
		log.Error("收敛 iSCSI 目标失败", "error", err)
		report("iscsi.reconcile_failed", "", "目标状态收敛失败", false)
	}

	// 3) 母盘临时共享崩溃恢复。
	if err := a.repo.ReconcileTempShares(ctx); err != nil {
		log.Error("收敛临时共享失败", "error", err)
		report("repo.temp_share_reconcile_failed", "", "临时共享收敛失败", false)
	}

	// 4) 存储底层卷：幂等重挂 + 刷新落盘就绪集合（fail-closed）。
	//
	// 挂载持久化不写 fstab、也不建 systemd unit，完全靠这一步：启动对账与本定时对账
	// 各跑一次，应对重启后挂载丢失、运维手工 umount 等。未成功挂载的存储会被剔除出
	// 落盘集合（storageSelection），避免写入落到宿主根文件系统上。
	if n, err := a.storage.RefreshStorageMounts(ctx); err != nil {
		log.Error("核对存储底层卷失败", "error", err)
		report("storage.volume_refresh_failed", "", "存储底层卷核对失败", false)
	} else if n > 0 {
		log.Warn("存在未就绪的存储底层卷，已从落盘集合剔除", "count", n)
		report("storage.volume_not_ready", "", "有存储底层卷未成功挂载，暂时不可用于落盘", false)
	}

	// 5) iSCSI 目标：孤儿识别（只报告）与 DB↔Windows 映射差异（只报告）。
	a.reconcileIscsi(ctx, rep, report, log)

	// 6) 磁盘：存在性、母盘指纹、差异盘版本、孤儿 VHDX。
	a.reconcileDisks(ctx, rep, report, log)

	// 7) 悬空分配（指向不存在的磁盘）。
	a.reconcileAllocations(ctx, rep, report)

	// 8) 已用量重算：以磁盘实际占用为唯一真源。
	// 增量账一旦出现口径不一致（历史上删除回退用逻辑大小、账上记物理大小），
	// 用量会逐次漂移甚至变成负数（用户配额 -7.98 GB）。这一步把漂移与历史脏数据
	// 一次性收敛；正常情况下是空操作。
	if drift, err := a.Store.RecomputeUsage(ctx); err != nil {
		log.Warn("重算已用量失败", "error", err)
		report("usage.recompute_failed", "", "重算已用量失败", false)
	} else if drift.Repos > 0 || drift.Users > 0 {
		log.Warn("已修正已用量（此前与磁盘实际占用不一致）",
			"repos", drift.Repos, "users", drift.Users)
		report("usage.recomputed", "",
			fmt.Sprintf("已修正已用量：存储库 %d 行、用户 %d 行", drift.Repos, drift.Users), true)
	}

	// 9) DiskIdentifier 唯一性抽样：平台层未提供读取能力，跳过并记录。
	rep.NotChecked = append(rep.NotChecked,
		"disk_identifier_uniqueness: 平台层未绑定读取 DiskIdentifier 的能力，已跳过抽样")

	rep.DurationMs = time.Since(start).Milliseconds()
	if ctx.Err() == context.DeadlineExceeded {
		rep.Truncated = true
		log.Warn("对账超出时间预算，已截断", "budget", budget.String())
	}

	a.reconcileMu.Lock()
	a.lastReconcile = rep
	a.reconcileMu.Unlock()

	log.Info("对账完成", "checked", rep.Checked, "issues", len(rep.Issues), "truncated", rep.Truncated)
	return nil
}

// reconcileIscsi 检查并清理孤儿 iSCSI 目标。
func (a *App) reconcileIscsi(ctx context.Context, rep *ReconcileReport,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger) {
	targets, err := a.Store.ListIscsiTargets(ctx, "")
	if err != nil {
		log.Error("查询 iSCSI 目标列表失败", "error", err)
		return
	}
	// byName 同时收录短名与完整 IQN：Windows 侧读回的 name 是完整 IQN，而 DB 存的是
	// 短名，二者都要能命中（真实事故：只比对短名导致新命名目标永远识别不出来）。
	prefix := a.iqnPrefix()
	byName := make(map[string]struct{}, len(targets)*2)
	for i := range targets {
		byName[targets[i].TargetName] = struct{}{}
		byName[targets[i].IQN(prefix)] = struct{}{}
		rep.Checked++
	}

	if a.Deps.Iscsi == nil || a.iscsi == nil {
		rep.NotChecked = append(rep.NotChecked, "iscsi_orphans: 本机未装配 iSCSI 目标管理器，已跳过")
		return
	}
	winTargets, err := a.Deps.Iscsi.ListTargets(ctx)
	if err != nil {
		// iSCSI 角色不可用时视为整体跳过，不计为差异。
		rep.NotChecked = append(rep.NotChecked, "iscsi_orphans: iSCSI 目标服务不可用，已跳过")
		return
	}
	// 孤儿目标：DB 无记录 → 自动删除（真实诉求："磁盘已删但 iSCSI 目标还挂着的，
	// 启动时自动清理"，避免资源泄漏）。删除只解映射+删目标，不删底层 VHDX 文件。
	//
	// ⚠️ 分批删除：每个 RemoveTarget 都要跑一次 PowerShell（本机实测 15-20 秒），而整个
	// 对账受 reconcileBudget（60s）约束，一次删太多必然 context deadline exceeded
	// （真实事故：大量孤儿时日志刷屏超时）。故每轮最多删 maxOrphanDelete 个，剩余留到
	// 下一轮对账（对账定期运行，慢慢清完即可）。
	const maxOrphanDelete = 2
	deleted := 0
	for i := range winTargets {
		w := &winTargets[i]
		rep.Checked++
		// 只处理本服务端创建的目标：短名形态 "vault-..." 或完整 IQN 形态
		// "<iqn_prefix>:vault-..."；第三方/手动创建的绝不碰。
		lower := strings.ToLower(w.Name)
		if !strings.HasPrefix(lower, orphanTargetPrefix) && !strings.Contains(lower, ":vault-") {
			continue
		}
		if _, ok := byName[w.Name]; ok {
			continue
		}
		if ctx.Err() != nil {
			// 对账预算已耗尽，剩余孤儿留下轮，避免刷超时日志。
			break
		}
		if deleted >= maxOrphanDelete {
			continue
		}
		// 走统一拆除实现：孤儿恰恰是"名字不符合我们推导规则"的目标，必须按平台读回的
		// 名字寻址（见 IscsiService.TeardownOrphanTarget）。
		if err := a.iscsi.TeardownOrphanTarget(ctx, w.Name); err != nil {
			report("iscsi.orphan_target", w.Name, "删除孤儿目标失败", false)
			log.Warn("删除孤儿 iSCSI 目标失败", "target", w.Name, "error", err)
			continue
		}
		deleted++
		report("iscsi.orphan_target", w.Name, "已删除孤儿目标", true)
		log.Warn("已删除孤儿 iSCSI 目标", "target", w.Name)
		a.audit(ctx, "system", "reconcile.orphan_target", "target:"+w.Name, "removed", domain.AuditResultOK)
	}
}

// reconcileDisks 检查磁盘存在性、母盘指纹、差异盘版本与孤儿 VHDX。
func (a *App) reconcileDisks(ctx context.Context, rep *ReconcileReport,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger) {
	disks, err := a.Store.ListAllDisks(ctx)
	if err != nil {
		log.Error("查询磁盘列表失败", "error", err)
		return
	}
	dbPaths := make(map[string]bool, len(disks))
	for i := range disks {
		dbPaths[pathKeyLocal(disks[i].VHDXPath)] = true
	}

	for i := range disks {
		if ctx.Err() != nil {
			return
		}
		d := &disks[i]
		rep.Checked++

		// 建盘中的记录：若其建盘任务已彻底失败，则把状态回滚为 error。
		// 不这么做的话磁盘会永远停在 creating —— 挂载/发布一直等 ready，
		// 用户每次点挂载都报 disk.not_ready 且永远不会变好。
		// （新失败由 worker 的 OnFinalFailure 即时回滚；这里兜住历史数据与进程崩溃场景。）
		if d.State == domain.DiskStateCreating {
			a.failCreatingDiskOnFailedJob(ctx, d, report, log)
		}

		// 存在性：建盘/删盘中的记录跳过，避免把"进行中"误判为缺失。
		if a.Disk != nil && d.State != domain.DiskStateCreating && d.State != domain.DiskStateDeleting {
			if !a.Disk.Exists(d.VHDXPath) {
				report("disk.file_missing", d.ID, "磁盘不存在: "+d.VHDXPath, false)
				log.Warn("磁盘缺失", "disk_id", d.ID, "path", d.VHDXPath)
				if d.State != domain.DiskStateError {
					d.State = domain.DiskStateError
					d.ObservedState = "file_missing"
					if err := a.Store.UpdateDisk(ctx, d); err != nil {
						log.Warn("标记磁盘错误状态失败", "disk_id", d.ID, "error", err)
					}
				}
				continue
			}
		}

		switch d.Kind {
		case domain.DiskKindParent:
			a.checkParentFingerprint(ctx, d, report, log)
		case domain.DiskKindDiff:
			a.checkDiffParentVersion(ctx, d, report, log)
		}
	}

	a.reconcileOrphanFiles(ctx, rep, report, log, dbPaths)
}

// failCreatingDiskOnFailedJob 检查"建盘中的磁盘"对应的建盘任务是否已彻底失败；是则回滚为 error。
//
// 判据刻意只取一种确定性信号：**最新一条 ref_id = 磁盘 ID 的任务处于 failed**。
//   - 不能用"没有任务"作为判据：上传建库流程中的磁盘同样处于 creating，
//     而它的任务 ref_id 是上传会话 ID，会被误判成"建盘从未开始"从而打断上传；
//   - 也不能用时间阈值：把大目录拷入本来就可能很久。
func (a *App) failCreatingDiskOnFailedJob(ctx context.Context, d *domain.Disk,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger) {
	// 只回滚中间态：ready/published/error 的磁盘不该被"建盘任务的历史结果"影响
	// （例如盘后来已建好、任务却在更早的一次尝试里失败过）。
	// 调用方也做了同样的判断，这里再兜一层：让本方法自身安全，可独立测试与复用。
	if d.State != domain.DiskStateCreating {
		return
	}
	lastJob, err := a.Store.GetLatestJobByRef(ctx, d.ID)
	if err != nil {
		log.Warn("查询建盘任务失败", "disk_id", d.ID, "error", err)
		return
	}
	if lastJob == nil || lastJob.State != domain.JobStateFailed {
		return
	}
	d.State = domain.DiskStateError
	d.ObservedState = "create_failed"
	if err := a.Store.UpdateDisk(ctx, d); err != nil {
		log.Warn("标记磁盘错误状态失败", "disk_id", d.ID, "error", err)
		return
	}
	report("disk.create_failed", d.ID, "建盘任务已失败，磁盘置为异常: "+lastJob.LastError, true)
	log.Warn("建盘任务已失败，磁盘置为异常",
		"disk_id", d.ID, "repo_id", d.RepoID, "job_id", lastJob.ID, "last_error", lastJob.LastError)
}

// checkParentFingerprint 校验母盘指纹；不一致时停用该库全部子盘 target。
func (a *App) checkParentFingerprint(ctx context.Context, d *domain.Disk,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger) {
	if a.Disk == nil || d.ContentFingerprint == "" {
		return
	}
	repo, err := a.Store.GetRepository(ctx, d.RepoID)
	if err != nil || !repo.IsShared() {
		return
	}
	if !a.Disk.Exists(d.VHDXPath) {
		return
	}
	actual, err := a.Disk.Fingerprint(d.VHDXPath)
	if err != nil {
		log.Warn("计算母盘指纹失败", "disk_id", d.ID, "error", err)
		return
	}
	if actual == d.ContentFingerprint {
		return
	}

	// 母盘被绕过改写：全部差异盘已不可信，停用该库全部子盘 target（见 5.4 / 5.11）。
	log.Error("母盘指纹不一致：母盘可能被绕过改写，将停用全部子盘目标",
		"repo_id", repo.ID, "disk_id", d.ID)
	diffs, err := a.Store.ListDiffDisksByParent(ctx, d.ID)
	if err != nil {
		log.Warn("查询差异盘失败", "disk_id", d.ID, "error", err)
		return
	}
	disabled := 0
	for i := range diffs {
		targets, err := a.Store.ListIscsiTargetsByDisk(ctx, diffs[i].ID)
		if err != nil {
			continue
		}
		for j := range targets {
			if !targets[j].DesiredEnabled && !targets[j].Enabled {
				continue
			}
			if err := a.iscsi.DisableTarget(ctx, targets[j].TargetName); err != nil {
				log.Warn("停用子盘目标失败", "target", targets[j].TargetName, "error", err)
				continue
			}
			disabled++
		}
	}
	a.audit(ctx, "system", "reconcile.parent_fingerprint_mismatch", "repo:"+repo.ID,
		"disk="+d.ID+" disabled_targets="+strconv.Itoa(disabled), domain.AuditResultError)
	report("parent.fingerprint_mismatch", d.ID,
		"母盘指纹与记录不一致，已停用子盘目标 "+strconv.Itoa(disabled)+" 个", true)
}

// checkDiffParentVersion 比对差异盘 parent_version 与母盘当前版本；不一致则标记不可挂载。
func (a *App) checkDiffParentVersion(ctx context.Context, d *domain.Disk,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger) {
	if d.ParentID == nil {
		return
	}
	repo, err := a.Store.GetRepository(ctx, d.RepoID)
	if err != nil || !repo.IsShared() {
		return
	}
	if d.ParentVersion == repo.ParentVersion {
		if d.ObservedState == "parent_version_mismatch" {
			d.ObservedState = ""
			if err := a.Store.UpdateDisk(ctx, d); err != nil {
				log.Warn("清除差异盘观测状态失败", "disk_id", d.ID, "error", err)
			}
		}
		return
	}
	// 标记为不可挂载（写 observed_state），不自动修改差异盘本身。
	d.ObservedState = "parent_version_mismatch"
	if err := a.Store.UpdateDisk(ctx, d); err != nil {
		log.Warn("标记差异盘版本不一致失败", "disk_id", d.ID, "error", err)
	}
	report("diff.parent_version_mismatch", d.ID,
		"差异盘基准版本与母盘当前版本不一致，标记为不可挂载", true)
}

// reconcileOrphanFiles 扫描各白名单根的 disks 目录下未被数据库登记的文件。
//
// 扫描本身交给 collectOrphanFiles（与管理端「孤儿磁盘」页面共用同一份实现）；
// 这里只决定**怎么处理**：差异盘 / 母盘类孤儿只报告（可能承载用户数据，见 5.11），
// 其余孤儿 VHDX 移入**同根**的 meta/orphan/（保证同卷移动）。
//
// 日志刻意降噪（真实反馈："打出来没用啊，干脆直接给个管理页面能手动删掉这些盘得了"）：
// 逐个文件打 WARN 会把日志刷满，而当时用户也无事可做；现在每轮只打**一条汇总 WARN**
// 并指名去管理端页面处理，单个文件的明细降到 Debug（需要时调日志级别即可拿到）。
func (a *App) reconcileOrphanFiles(ctx context.Context, rep *ReconcileReport,
	report func(kind, ref, detail string, fixed bool), log *slog.Logger, dbPaths map[string]bool) {
	// 本扫描的前提是"磁盘 = storages.path 目录下的 .vhdx 文件"，只对 Windows 成立；
	// Linux 上磁盘是 LVM thin LV（不在 storages.path 目录里），故整体跳过并记录。
	if a.platformKind() != platform.KindWindows {
		rep.NotChecked = append(rep.NotChecked, orphanScanSkipOtherPlatform)
		return
	}
	guards, err := a.storageGuards(ctx)
	if err != nil {
		// 读存储失败不应静默跳过（否则会表现为"没有任何根"）；记录后跳过本轮扫描。
		log.Error("读取存储失败，跳过孤儿文件扫描", "error", err)
		return
	}
	if guards.Empty() {
		return
	}
	files, checked, skipped := a.collectOrphanFiles(ctx, guards, dbPaths, log)
	rep.Checked += checked
	rep.NotChecked = append(rep.NotChecked, skipped...)

	raw := a.raw()
	counts := map[string]int{}
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		counts[f.Kind]++
		detail := "根 " + f.Root
		log.Debug("发现未登记的磁盘文件", "path", f.Path, "kind", f.Kind, "size_bytes", f.SizeBytes)

		switch f.Kind {
		case OrphanKindDiff, OrphanKindParent:
			// 可能承载用户数据：只报告，由管理员在「孤儿磁盘」页面确认后手动删除。
			kind := "orphan_diff_disk"
			if f.Kind == OrphanKindParent {
				kind = "orphan_parent_disk"
			}
			report(kind, f.Path, "存在未被登记的"+orphanKindLabel(f.Kind)+
				"文件（可能承载用户数据，仅报告，可在"+orphanAdminPageHint+"删除；"+detail+"）", false)
		default:
			orphanRoot := ""
			if g, ok := guards.GuardForPath(f.Path); ok {
				orphanRoot, _ = g.Resolve(raw.Storage.OrphanDir)
			}
			if orphanRoot == "" {
				report("orphan_vhdx", f.Path,
					"存在未被登记的 VHDX 文件（未配置孤儿目录，仅报告，可在"+orphanAdminPageHint+"删除；"+detail+"）", false)
				continue
			}
			if err := moveToOrphan(f.Path, orphanRoot); err != nil {
				log.Warn("移动孤儿 VHDX 失败", "path", f.Path, "root", f.Root, "error", err)
				report("orphan_vhdx", f.Path, "存在未被登记的 VHDX 文件（移动失败；"+detail+"）", false)
				continue
			}
			report("orphan_vhdx", f.Path, "未被登记的 VHDX 文件已移入同根孤儿目录（"+detail+"）", true)
			a.audit(ctx, "system", "reconcile.orphan_vhdx", "file:"+f.Path, "moved", domain.AuditResultOK)
		}
	}

	if len(files) > 0 {
		log.Warn("发现未登记的磁盘文件（仅报告，可在"+orphanAdminPageHint+"逐个删除）",
			"diff", counts[OrphanKindDiff], "parent", counts[OrphanKindParent], "other", counts[OrphanKindOther])
	}
}

// isUnderAny 判断 root 是否等于或位于 roots 中任一根之下。
func isUnderAny(root string, roots []string) bool {
	for _, r := range roots {
		if domain.UnderRoot(r, root) {
			return true
		}
	}
	return false
}

// reconcileAllocations 检查悬空分配（指向不存在的磁盘）。
func (a *App) reconcileAllocations(ctx context.Context, rep *ReconcileReport,
	report func(kind, ref, detail string, fixed bool)) {
	allocs, err := a.Store.ListAllAllocations(ctx)
	if err != nil {
		a.Log.Error("查询分配列表失败", "error", err)
		return
	}
	for i := range allocs {
		if ctx.Err() != nil {
			return
		}
		al := &allocs[i]
		rep.Checked++
		if _, err := a.Store.GetDisk(ctx, al.DiskID); err != nil {
			report("allocation.dangling", al.ID, "分配指向的磁盘不存在: "+al.DiskID, false)
		}
	}
}

// moveToOrphan 把孤儿文件移入孤儿目录（同卷 rename；跨卷时回退复制）。
func moveToOrphan(src, orphanRoot string) error {
	if err := os.MkdirAll(orphanRoot, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(orphanRoot, filepath.Base(src))
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(orphanRoot, filepath.Base(src)+"."+strconv.FormatInt(time.Now().UnixMilli(), 10))
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 跨卷：复制后删除（保持幂等）。
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// copyFile 复制常规文件。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// pathKeyLocal 生成大小写不敏感的路径键，用于与数据库路径比对。
func pathKeyLocal(p string) string {
	return strings.ToLower(filepath.Clean(p))
}
