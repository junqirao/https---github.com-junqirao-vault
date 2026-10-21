package app

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/store"
)

// 回收任务的收尾动作。
const (
	// reclaimFinalizeRepo 回收后删除存储库记录本身。
	//
	// ⚠️ 这是**历史动作**：删除存储库已改为"先删记录、再回收资源"（见 RepoService.Delete），
	// 记录在 API 请求内就删掉了，因此不会再产生这种任务。保留它只为兼容升级前
	// 还留在 jobs 表里没执行完的任务。
	reclaimFinalizeRepo = "repo"
	// reclaimFinalizeArtifacts 记录已删，任务只按快照回收平台侧资源（iSCSI 目标 + VHDX 文件）。
	//
	// 为什么把"删记录"和"回收资源"拆成两件事（真实反馈："删除存储库要等很久"）：
	// 回收要逐块盘解除 LUN 映射 → 停用目标 → 删除目标 → 删 VHDX 文件，全是秒级的平台
	// 调用；把它挡在删除请求前面，界面上的库就会长时间停在「删除中」。
	// 拆开后：请求内只删记录（毫秒级，界面立刻不再显示该库），慢的留给后台任务，
	// 而且任务失败可以安全重试 —— 记录已经不存在，重试没有任何副作用。
	reclaimFinalizeArtifacts = "artifacts"
	// reclaimFinalizeParentIdle 回收差异盘后把母盘置回 idle。
	//
	// 当前代码已不再产生这种任务（清理差异盘改走 JobCleanupDiffs）；保留只为让升级前
	// 停在 jobs 表里的历史任务仍能正确收尾。
	reclaimFinalizeParentIdle = "parent_idle"
)

// ---- 任务 payload ----

type createDiskPayload struct {
	DiskID    string `json:"disk_id"`
	SourceDir string `json:"source_dir,omitempty"`
}

type diffPayload struct {
	DiskID       string `json:"disk_id"`
	RepoID       string `json:"repo_id"`
	ParentDiskID string `json:"parent_disk_id"`
}

type copyPayload struct {
	SrcDiskID   string `json:"src_disk_id"`
	NewRepoName string `json:"new_repo_name"`
	OwnerID     string `json:"owner_id"`
}

type deleteDiskPayload struct {
	DiskID       string `json:"disk_id"`
	AllocationID string `json:"allocation_id,omitempty"`
}

// diskArtifact 是"DB 记录已删、只剩平台侧资源待回收"的一块盘的快照。
//
// 记录先删，就必须在这里带上回收所需的全部信息：盘路径 + 它上面的 iSCSI 目标名。
// 任务执行时**不能**再回查 DB —— 那时候这些行（disks / iscsi_targets）已经不存在了。
type diskArtifact struct {
	DiskPath string   `json:"disk_path"`
	Targets  []string `json:"targets,omitempty"`
}

type reclaimPayload struct {
	RepoID       string   `json:"repo_id"`
	ParentDiskID string   `json:"parent_disk_id,omitempty"`
	DiskIDs      []string `json:"disk_ids,omitempty"`
	// Artifacts 仅在 Finalize=artifacts（记录已删）时使用，见 diskArtifact。
	Artifacts []diskArtifact `json:"artifacts,omitempty"`
	Finalize  string         `json:"finalize,omitempty"`
}

type compactPayload struct {
	DiskID string `json:"disk_id"`
}

type publishPayload struct {
	AllocationID string `json:"allocation_id"`
}

// prepareRepoPayload 是建库预创建任务（JobPrepareRepo）的入参。
//
// 只带"建几个池位"这种最小信息：池位对应的磁盘记录由任务自己建 ——
// 建库请求的事务里为 N 个池位各插一行，既拖慢请求，又会在一半失败时留下一堆半成品记录。
type prepareRepoPayload struct {
	// DiskID 母盘磁盘 ID。
	DiskID string `json:"disk_id"`
	// RepoID 存储库 ID。
	RepoID string `json:"repo_id"`
	// SourceDir 可选的源目录（语义同 JobCreateVHDXFromDir 的源）。
	SourceDir string `json:"source_dir,omitempty"`
	// Count 要预创建的池位数量（= 库的"共享数量"）。
	Count int `json:"count"`
}

// cleanupDiffsPayload 是维护态下"清理全部差异盘"任务的入参。
//
// 只带库与母盘 ID：任务每次尝试都重新列出**当前仍存在**的差异盘，天然幂等，
// 失败重试时不会重复处理已经删掉的盘。
type cleanupDiffsPayload struct {
	RepoID       string `json:"repo_id"`
	ParentDiskID string `json:"parent_disk_id"`
}

// resetDiffPayload 是"释放后回池"任务（JobResetDiff）的入参。
type resetDiffPayload struct {
	DiskID       string `json:"disk_id"`
	RepoID       string `json:"repo_id"`
	ParentDiskID string `json:"parent_disk_id"`
	// AllocationID 触发本次回池的分配：只用于日志与审计，
	// 分配记录本身是同步删除的（用户点完释放就该立刻看到槽位空出来）。
	AllocationID string `json:"allocation_id,omitempty"`
}

// DiskService 负责 VHDX 的同步查询与异步生命周期任务。
type DiskService struct {
	Deps
	Repos *RepoService
	// IscsiSvc 用于建库预创建与"回池"时下发 iSCSI 目标；在 App 装配时回填（两者互为依赖）。
	//
	// ⚠️ 不能叫 Iscsi：Deps 里已经嵌了一个同名的平台后端字段（platform.IscsiBackend），
	// 同名会把它遮蔽掉，本层所有 s.Iscsi.DetachLun 之类的调用都会编译不过。
	IscsiSvc *IscsiService
	// Leases 用于维护清理差异盘前撤销在线租约（通知客户端断开）；在 App 装配时回填。
	Leases *LeaseService
}

// RegisterHandlers 把磁盘相关任务注册到 worker。
//
// 必须在 worker Start 之前调用。
func (s *DiskService) RegisterHandlers(w *job.Worker) {
	if w == nil {
		return
	}
	w.Register(job.HandlerFunc{T: domain.JobCreateVHDX, F: s.runCreateVHDX})
	w.Register(job.HandlerFunc{T: domain.JobCreateVHDXFromDir, F: s.runCreateVHDX})
	w.Register(job.HandlerFunc{T: domain.JobCreateDiff, F: s.runCreateDiff})
	w.Register(job.HandlerFunc{T: domain.JobPrepareRepo, F: s.runPrepareRepo})
	w.Register(job.HandlerFunc{T: domain.JobResetDiff, F: s.runResetDiff})
	w.Register(job.HandlerFunc{T: domain.JobCopyVHDX, F: s.runCopyVHDX})
	w.Register(job.HandlerFunc{T: domain.JobDeleteDisk, F: s.runDeleteDisk})
	w.Register(job.HandlerFunc{T: domain.JobReclaim, F: s.runReclaim})
	w.Register(job.HandlerFunc{T: domain.JobCleanupDiffs, F: s.runCleanupDiffs})
	w.Register(job.HandlerFunc{T: domain.JobVerifyDiff, F: s.runVerifyDiff})
	w.Register(job.HandlerFunc{T: domain.JobCompact, F: s.runCompact})
}

// Get 按 ID 查询磁盘。
func (s *DiskService) Get(ctx context.Context, id string) (*domain.Disk, error) {
	return s.Store.GetDisk(ctx, id)
}

// ListByRepo 列出存储库下的全部磁盘。
func (s *DiskService) ListByRepo(ctx context.Context, repoID string) ([]domain.Disk, error) {
	return s.Store.ListDisksByRepo(ctx, repoID)
}

// MarkCreateFailed 把"建盘任务彻底失败"回写到磁盘状态（creating → error）。
//
// 为什么必须有：建盘（含差异盘派生）是异步任务，中间态是 creating；挂载/发布路径会
// 一直等 ready。任务重试用尽后若不改状态，磁盘就永远停在 creating——
// 用户每次点挂载都得到 disk.not_ready，而且永远不会变好（对账刻意跳过 creating）。
//
// 只处理仍是 creating 的磁盘：任务失败但盘已被其它路径推进到 ready/published 时不得覆盖。
// 任务 ref_id 不是磁盘 ID（如回收类任务）或磁盘已删除时静默返回。
func (s *DiskService) MarkCreateFailed(ctx context.Context, diskID string, cause error) error {
	if strings.TrimSpace(diskID) == "" {
		return nil
	}
	disk, err := s.Store.GetDisk(ctx, diskID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if disk.State != domain.DiskStateCreating {
		return nil
	}
	// 回退分配时预留的用量（**幂等**：只有从 creating 首次转入 error 才会走到这里，
	// 对账会重复调用本函数，重复回退会把用量退成负数 —— 历史工单：-7.98 GB）。
	//
	// 为什么要回退（真实反馈："我一点空间都没用就占了 1G"）：配额口径是
	// **已用 = 已分配磁盘总量**（见 store.accountedUsageSQL），盘一旦建出来就按标称容量
	// 计费，直到它被删除；而建盘彻底失败时这份盘根本不存在，不能被它一直占着配额。
	//
	// 先落库状态再重算：计费口径对 state=error 的盘计 0，正是这条预留的出口。
	userID := s.allocatedUserID(ctx, disk.ID)
	disk.State = domain.DiskStateError
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	s.resyncUsage(ctx, disk.RepoID, userID)
	s.Log.Error("建盘任务彻底失败，磁盘已置为异常并按实际占用重算用量",
		"disk_id", disk.ID, "repo_id", disk.RepoID, "path", disk.VHDXPath, "error", cause)
	return nil
}

// StartCopy 复制母盘为新的独立存储库（异步）。
//
// 前置条件（见 5.9.2）：源盘未挂载、未被 iSCSI 发布、无活跃租约、非差异盘。
func (s *DiskService) StartCopy(ctx context.Context, diskID, newRepoName, operatorID string) (*domain.Job, error) {
	name := newRepoName
	if name == "" {
		return nil, apperr.InvalidParam("new_repo_name")
	}
	disk, err := s.Store.GetDisk(ctx, diskID)
	if err != nil {
		return nil, err
	}
	if disk.Kind != domain.DiskKindParent {
		// 差异盘按路径引用母盘，复制后链路失效，禁止复制。
		return nil, apperr.InvalidParam("disk_id").WithArg("reason", "only_parent_can_be_copied")
	}
	if disk.Mounted {
		return nil, apperr.DiskBusy()
	}
	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, diskID)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t.Enabled || t.DesiredEnabled {
			return nil, apperr.DiskBusy().WithArg("reason", "published")
		}
	}
	active, err := s.Store.CountActiveLeasesByRepo(ctx, disk.RepoID)
	if err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, apperr.RepoHasActiveLease(active)
	}
	if _, err := s.Store.GetRepositoryByName(ctx, name); err == nil {
		return nil, apperr.RepoNameTaken().WithArg("name", name)
	} else if !isNotFound(err) {
		return nil, err
	}

	owner := operatorID
	if owner == "" {
		if repo, rErr := s.Store.GetRepository(ctx, disk.RepoID); rErr == nil {
			owner = repo.OwnerID
		}
	}
	payload := copyPayload{SrcDiskID: diskID, NewRepoName: name, OwnerID: owner}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobCopyVHDX, diskID, "copy_vhdx:"+diskID+":"+name, lock.DiskKey(diskID), payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "disk.copy.start", "disk:"+diskID, "new_repo="+name, domain.AuditResultOK)
	return j, nil
}

// StartDelete 删除磁盘（异步）。
func (s *DiskService) StartDelete(ctx context.Context, diskID, operatorID string) (*domain.Job, error) {
	disk, err := s.Store.GetDisk(ctx, diskID)
	if err != nil {
		return nil, err
	}
	if disk.Mounted {
		return nil, apperr.DiskBusy()
	}
	payload := deleteDiskPayload{DiskID: diskID}
	// 删母盘用 RepoKey：派生差异盘会独占打开母盘，回收存储库的任务也用 RepoKey 删它，
	// 三者必须串行，否则撞 platform.sharing_violation（同 runCreateDiff 的说明）。
	// 差异盘之间互不影响，继续按盘加锁，保持并发删除。
	lockKey := lock.DiskKey(diskID)
	if disk.Kind == domain.DiskKindParent {
		lockKey = lock.RepoKey(disk.RepoID)
	}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobDeleteDisk, diskID, "delete_disk:"+diskID, lockKey, payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "disk.delete.start", "disk:"+diskID, disk.VHDXPath, domain.AuditResultOK)
	return j, nil
}

// StartCompact 回收磁盘空间（Retrim / Compact，异步）。
//
// 只对未挂载的盘执行；同一卷串行（IO 密集，见 5.9.4）。
func (s *DiskService) StartCompact(ctx context.Context, diskID string) (*domain.Job, error) {
	disk, err := s.Store.GetDisk(ctx, diskID)
	if err != nil {
		return nil, err
	}
	if disk.Mounted {
		return nil, apperr.DiskBusy()
	}
	if s.Disk != nil && !s.Disk.Exists(disk.VHDXPath) {
		return nil, apperr.DiskNotFound().WithArg("id", diskID)
	}
	payload := compactPayload{DiskID: diskID}
	// 空间回收可重复执行，幂等键留空（唯一索引对空值不生效），避免第二次请求被去重。
	// 卷级锁按"磁盘所在卷"取键，使同一卷上的多个白名单根共享同一把锁。
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobCompact, diskID, "", lock.VolumeKey(domain.VolumeID(disk.VHDXPath)), payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "", "disk.compact.start", "disk:"+diskID, "", domain.AuditResultOK)
	return j, nil
}

// SamplePhysicalUsage 采样磁盘物理占用并写回 disks.physical_bytes。
//
// 由定时任务调用（每 5 分钟，见 5.7 遥测）。
func (s *DiskService) SamplePhysicalUsage(ctx context.Context) error {
	if s.Disk == nil {
		return nil
	}
	kinds := []domain.DiskKind{domain.DiskKindParent, domain.DiskKindDiff, domain.DiskKindStandalone}
	for _, kind := range kinds {
		disks, err := s.Store.ListDisksByKind(ctx, kind)
		if err != nil {
			return err
		}
		for i := range disks {
			d := &disks[i]
			if !s.Disk.Exists(d.VHDXPath) {
				continue
			}
			size, err := s.Disk.PhysicalSize(d.VHDXPath)
			if err != nil || size == d.PhysicalBytes {
				continue
			}
			d.PhysicalBytes = size
			if err := s.Store.UpdateDisk(ctx, d); err != nil {
				s.Log.Warn("写回磁盘物理占用失败", "disk_id", d.ID, "error", err)
			}
		}
	}
	// 配额口径（已用 = 已分配磁盘总量）与 physical_bytes 无关，采样只影响展示；
	// 这里顺手做一次全量对账，纯属兜底收敛历史脏数据（正常情况下是空操作）。
	if drift, err := s.Store.RecomputeUsage(ctx); err != nil {
		s.Log.Warn("按实际占用重算已用量失败", "error", err)
	} else if drift.Repos > 0 || drift.Users > 0 {
		s.Log.Info("已按实际磁盘占用校正已用量", "repos", drift.Repos, "users", drift.Users)
	}
	return nil
}

// ---- 任务处理器 ----

// runCreateVHDX 创建 VHDX（可选从服务端目录拷入内容）并置为 ready。
//
// 幂等：disk.state 已为 ready 时直接返回。
func (s *DiskService) runCreateVHDX(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p createDiskPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		return err
	}
	return s.buildVHDXFromSource(ctx, disk, p.SourceDir, rep)
}

// buildVHDXFromSource 是建盘核心流水线：创建动态盘 → 挂载 → 格式化 → 拷入目录 →
// 校验 → 分离 → 回写状态（见 docs/implementation.md 5.2 / 5.10 步骤 ⑤）。
//
// 幂等：disk 已是 ready/published 且文件存在时直接返回。sourceDir 为空表示建空盘。
// 供 DiskService 的建盘任务与 UploadService 的完成任务复用。
func (s *DiskService) buildVHDXFromSource(ctx context.Context, disk *domain.Disk, sourceDir string, rep job.Reporter) error {
	// published 是 ready 的超集（盘建好且目标已下发，见 awaitBackingReady 的说明）：
	// 独享盘在这里被预创建目标后就是 published，若只认 ready，任务重试会把整棵源目录
	// 再拷一遍（拷入目标盘已经含有内容）。
	if isDiskBuilt(disk.State) && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
		// 盘已建好，但预创建的目标可能在上一次尝试里没下发成功（下面的预创建只告警不失败）：
		// 幂等补一次，任务重试到这里仍有第二次机会。
		s.prePublishTarget(ctx, disk)
		return nil
	}
	if s.Disk == nil || s.Vol == nil {
		return errNotImplemented
	}
	rep.Progress(5)

	if !s.Disk.Exists(disk.VHDXPath) {
		if err := s.Disk.Create(ctx, disk.VHDXPath, disk.SizeBytes); err != nil {
			return err
		}
	}
	rep.Progress(15)

	// 文件系统按盘的标记走（建盘时按客户端操作系统定下，见 domain.ClientOS）：
	// Windows 客户端 → NTFS，Linux 客户端（预留）→ ext4。
	fs := disk.FileSystemOrDefault().String()

	if sourceDir != "" {
		// 有源目录：由卷后端一次性完成"激活 → 分区 → 格式化 → 拷入 → 卸载"，
		// 中间的挂载细节（Windows=盘符/robocopy，Linux=分区+ntfs3/ntfs-3g/copyTree）不再泄漏到本层。
		//
		// 拷入这一段是整条流水线里唯一的长活（源目录有多大就要写多大的盘：30G 的库就是
		// 30G 的读 + 30G 的写），而它此前只上报 15 → 85 两个端点，中间几十分钟进度条纹丝不动，
		// 用户看到的就是"卡住了"。这里把卷后端采样到的字节进度映射到同一区间，让它走得动。
		onCopy := copyProgressReporter(rep, 15, 85)
		if _, _, err := s.Vol.MountAndCopy(ctx, disk.VHDXPath, sourceDir, fs, "VAULT", onCopy); err != nil {
			return err
		}
	} else {
		// 空盘：仍需格式化出文件系统，客户端挂载时才不用再格式化。
		if err := s.Disk.Activate(ctx, disk.VHDXPath, false); err != nil {
			return err
		}
		detached := false
		defer func() {
			if detached {
				return
			}
			if dErr := s.Disk.Deactivate(context.WithoutCancel(ctx), disk.VHDXPath); dErr != nil {
				s.Log.Warn("卸载虚拟磁盘失败", "disk_id", disk.ID, "error", dErr)
			}
		}()
		if _, err := s.Vol.EnsureFormatted(ctx, disk.VHDXPath, fs, "VAULT"); err != nil {
			return err
		}
		if err := s.Disk.Deactivate(ctx, disk.VHDXPath); err != nil {
			return err
		}
		detached = true
	}
	rep.Progress(85)

	if size, err := s.Disk.PhysicalSize(disk.VHDXPath); err == nil {
		disk.PhysicalBytes = size
	}
	disk.Mounted = false
	disk.State = domain.DiskStateReady
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	// 贴"出厂标签"（见 diskmeta.go）：盘建好之后才写，且失败只告警 ——
	// 迁移时能否认出这块盘靠它，但建盘本身不该被它拖失败。
	s.writeDiskMetaAfterBuild(ctx, disk, sourceDir)
	rep.Progress(100)
	s.audit(ctx, "", "disk.create.done", "disk:"+disk.ID, disk.VHDXPath, domain.AuditResultOK)
	s.prePublishTarget(ctx, disk)
	return nil
}

// copyProgressReporter 把"拷入虚拟盘"的字节进度映射到任务进度的 [from, to] 区间。
//
// 只在整数百分比真的前进时才上报：拷入可能持续几十分钟、采样间隔只有两秒，每次都写一遍
// 任务进度就是成千上万条无意义的 UPDATE（值还一模一样）。
func copyProgressReporter(rep job.Reporter, from, to int) platform.CopyProgressFunc {
	span := to - from
	last := 0
	return func(copied, total int64) {
		if span <= 0 || total <= 0 {
			return
		}
		pct := to
		if copied < total {
			pct = from + int(int64(span)*copied/total)
		}
		if pct <= last {
			return
		}
		last = pct
		rep.Progress(pct)
	}
}

// isDiskBuilt 判断磁盘是否已经建好（published 蕴含"已建好"，是 ready 的超集）。
func isDiskBuilt(state domain.DiskState) bool {
	return state == domain.DiskStateReady || state == domain.DiskStatePublished
}

// prePublishTarget 为"会直接交给用户"的盘提前建好并启用 iSCSI 目标（服务端"预先创建"）。
//
// 覆盖两条原先只能等用户挂载时现场下发的路径：
//   - 独享库的 standalone 盘：建库时就有，用户拿到的就是它；
//   - 非池化共享库的差异盘：分配时才派生（JobCreateDiff），派生完成即预建。
//
// 共享库的母盘（parent）刻意跳过：共享库交给用户的是差异盘，母盘只作为派生来源，
// 提前发布它等于在平台上多开一条能读到基线数据的通路，没有必要。
//
// 失败只告警不返回错误：盘本身是好的，把建盘任务判失败会让一块完好的盘卡在重试链上；
// 最坏结果是回到老行为 —— 用户挂载时服务端现场下发（多等十几秒）。
func (s *DiskService) prePublishTarget(ctx context.Context, disk *domain.Disk) {
	if disk == nil || disk.Kind == domain.DiskKindParent {
		return
	}
	if s.IscsiSvc == nil {
		return
	}
	repo, err := s.Store.GetRepository(ctx, disk.RepoID)
	if err != nil {
		if !isNotFound(err) {
			s.Log.Warn("预创建 iSCSI 目标失败：读取存储库出错",
				"disk_id", disk.ID, "repo_id", disk.RepoID, "error", err)
		}
		return
	}
	if err := s.IscsiSvc.EnsureDiskTarget(ctx, repo, disk); err != nil {
		s.Log.Warn("预创建 iSCSI 目标失败，用户挂载时将现场下发",
			"disk_id", disk.ID, "repo_id", repo.ID, "error", err)
		return
	}
	s.Log.Info("已预先创建并启用 iSCSI 目标", "disk_id", disk.ID, "repo_id", repo.ID)
}

// runCreateDiff 从母盘派生差异盘。
//
// 与"回收存储库"共用 lock.RepoKey（见 Allocate 里入队处的说明）：派生差异盘要
// **独占打开母盘 VHDX**，必须与同库的其它派生、以及删母盘的回收任务串行。
func (s *DiskService) runCreateDiff(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p diffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		if isNotFound(err) {
			// 存储库删除会把库/盘/分配记录一次性清掉，这个建盘任务随后就没有意义了。
			// 不能当失败：worker 会立刻重试，最后还会把"已经不存在的盘"标成异常，
			// 在日志里留下一串误导性的错误。
			s.Log.Info("差异盘记录已不存在（存储库可能刚被删除），跳过建盘", "disk_id", p.DiskID)
			return nil
		}
		return err
	}
	if isDiskBuilt(disk.State) && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
		// 盘已派生好（published 是 ready 的超集）：不重复派生，但补一次预创建 ——
		// 预创建只告警不失败，上一次尝试可能就是"盘建成了、目标没下发成功"，
		// 任务重试只能从这里进入，直接 return 会让预创建永远没有第二次机会。
		s.prePublishTarget(ctx, disk)
		return nil
	}
	if s.Disk == nil {
		return errNotImplemented
	}
	parent, err := s.Store.GetDisk(ctx, p.ParentDiskID)
	if err != nil {
		if isNotFound(err) {
			s.Log.Info("母盘记录已不存在（存储库可能刚被删除），跳过建盘",
				"disk_id", p.DiskID, "parent_disk_id", p.ParentDiskID)
			return nil
		}
		return err
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			s.Log.Info("存储库记录已不存在，跳过建盘", "disk_id", p.DiskID, "repo_id", p.RepoID)
			return nil
		}
		return err
	}
	if err := s.ensureDiffDisk(ctx, disk, parent, repo, rep); err != nil {
		return err
	}
	// 派生完成立刻把 iSCSI 目标建好启用（服务端"预先创建"）：用户拿着这次分配点挂载
	// 时，Publish 命中指纹即整段跳过，不必再等一次约 35 秒的现场下发。
	s.prePublishTarget(ctx, disk)
	return nil
}

// ensureDiffDisk 保证一块差异盘物理存在且状态为 ready（幂等）。
//
// 两个调用方共用：
//   - JobCreateDiff：分配时按需派生（非池化库，或池化库的池位被事后调大的那部分）；
//   - JobPrepareRepo：建库时把"共享数量"个池位一次性派生好（见 5.14）。
//
// 文件已存在时只补状态与版本，不重复派生：任务重试、以及"释放回池"重建后的复检
// 都会走到这里，重复派生会把用户数据抹掉。
func (s *DiskService) ensureDiffDisk(ctx context.Context, disk, parent *domain.Disk,
	repo *domain.Repository, rep job.Reporter) error {
	if disk.State == domain.DiskStateReady && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
		return nil
	}
	if s.Disk == nil {
		return errNotImplemented
	}
	rep.Progress(20)

	if !s.Disk.Exists(disk.VHDXPath) {
		// 母盘必须先在平台上解除映射：母盘 VHDX 仍被 iSCSI 目标打开时，创建差异盘会以
		// 共享冲突失败（维护态退出的目标清理是异步、尽力而为的，可能还没跑完）。
		if s.IscsiSvc != nil {
			s.IscsiSvc.TeardownDiskTargets(ctx, parent.ID)
		}
		if err := s.createDiffWithRetry(ctx, disk.VHDXPath, parent.VHDXPath); err != nil {
			return err
		}
	}
	rep.Progress(60)

	// 建盘期间记录可能被删除（删除存储库会把记录一次性清掉，而它不等这把锁）。
	// 这时候刚建出来的文件就是孤儿，必须在这里删掉：如果回收任务抢在本次建盘**之前**
	// 跑完了（两者共用 lock.RepoKey，先后不确定），它的快照里已经"处理过"这个路径，
	// 不会再来删第二次，留下就是永久孤儿文件。
	if _, err := s.Store.GetDisk(ctx, disk.ID); err != nil {
		if isNotFound(err) {
			s.Log.Warn("建盘期间磁盘记录已被删除，回滚刚创建的差异盘",
				"disk_id", disk.ID, "path", disk.VHDXPath)
			return s.removeDiskFile(ctx, disk.VHDXPath)
		}
		return err
	}

	// 记录差异盘基于的母盘版本；挂载前会与 repositories.parent_version 比对。
	disk.ParentVersion = repo.ParentVersion
	if fingerprint, err := s.Disk.Fingerprint(parent.VHDXPath); err == nil {
		parent.ContentFingerprint = fingerprint
		if err := s.Store.UpdateDisk(ctx, parent); err != nil {
			return err
		}
	}
	if size, err := s.Disk.PhysicalSize(disk.VHDXPath); err == nil {
		disk.PhysicalBytes = size
	}
	disk.State = domain.DiskStateReady
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	// 盘建好了：账目本该就是标称容量（分配时已计费），这里重算一次只为收敛历史脏数据。
	userID := s.allocatedUserID(ctx, disk.ID)
	s.resyncUsage(ctx, disk.RepoID, userID)
	rep.Progress(100)
	s.audit(ctx, "", "disk.create_diff.done", "disk:"+disk.ID, "parent="+parent.ID, domain.AuditResultOK)
	return nil
}

// codeSharingViolation 是"文件被占用"的平台错误码（Windows 上由 winvhd 抛出，
// 对应 ERROR_SHARING_VIOLATION）。
//
// 这里刻意重复一份字符串、而不是 import winvhd：那个包带 `//go:build windows`，
// 而 app 是两端共用的，import 它会让 Linux 构建直接失败
// （同类说明见 internal/platform/linuxlvm/errors.go）。
const codeSharingViolation = "platform.sharing_violation"

// createDiffRetryDelays 是"母盘/子盘被占用"时的原地退避间隔。
//
// 为什么必须有它：worker 的重试**没有退避**，3 次尝试在 10ms 内就跑完了
// （真实工单：attempt=3 而 cost_ms=4）；而共享冲突描述的恰恰是"对方**此刻**还开着
// 这个文件"这种瞬时状态 —— 没有退避等于没有重试，一次瞬时占用就把磁盘永久判死。
var createDiffRetryDelays = []time.Duration{
	300 * time.Millisecond,
	time.Second,
	2 * time.Second,
}

// createDiffWithRetry 创建差异盘，只在"文件被占用"时退避重试。
//
// 只对共享冲突重试：它是瞬时状态（iSCSI 服务刚解除映射、回收任务刚删完、
// 杀毒/索引服务正在扫描母盘），等一会儿必然能成；其它错误（母盘损坏、空间不足、
// 路径非法）重试没有意义，直接失败反而更快暴露问题。
func (s *DiskService) createDiffWithRetry(ctx context.Context, childPath, parentPath string) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = s.Disk.CreateDiff(ctx, childPath, parentPath); err == nil {
			return nil
		}
		if apperr.CodeOf(err) != codeSharingViolation || attempt >= len(createDiffRetryDelays) {
			// 子盘与母盘都记下来：光看错误文本分不清卡在"子盘被占用"还是"母盘被占用"，
			// 而这两者的处置完全不同。
			s.Log.Error("创建差异盘失败", "child", childPath, "parent", parentPath,
				"attempt", attempt+1, "error", err)
			return err
		}
		delay := createDiffRetryDelays[attempt]
		s.Log.Warn("创建差异盘遇到文件占用，退避后重试",
			"child", childPath, "parent", parentPath, "attempt", attempt+1,
			"retry_in_ms", delay.Milliseconds(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// runCopyVHDX 全量复制母盘为新库，并重置磁盘标识（见 5.9.3）。
func (s *DiskService) runCopyVHDX(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p copyPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if s.Disk == nil {
		return errNotImplemented
	}
	src, err := s.Store.GetDisk(ctx, p.SrcDiskID)
	if err != nil {
		return err
	}
	raw := s.raw()
	// 目标母盘选根：优先与源盘同根（同卷），空间不足时改选可用空间最大的根。
	guard, err := s.Repos.pickGuardForPath(ctx, src.VHDXPath, src.SizeBytes)
	if err != nil {
		return err
	}

	var dstRepo *domain.Repository
	var dstDisk *domain.Disk
	err = s.Store.Tx(ctx, func(tx *store.Store) error {
		r, err := tx.GetRepositoryByName(ctx, p.NewRepoName)
		if err != nil && !isNotFound(err) {
			return err
		}
		if r == nil {
			cond := domain.ParentIdle
			maxDiff := raw.Storage.DefaultMaxDiffDisks
			if maxDiff <= 0 {
				maxDiff = 50
			}
			r = &domain.Repository{
				Name:            p.NewRepoName,
				Mode:            domain.RepoModeShared,
				OwnerID:         p.OwnerID,
				ParentCondition: &cond,
				MaxDiffDisks:    maxDiff,
				State:           domain.RepoStateActive,
			}
			if err := tx.CreateRepository(ctx, r); err != nil {
				return err
			}
		}
		var d *domain.Disk
		if r.ParentDiskID != nil {
			if d, err = tx.GetDisk(ctx, *r.ParentDiskID); err != nil {
				return err
			}
		}
		if d == nil {
			abs, err := s.Disk.DiskRef(guard.Root, filepath.Join(raw.Storage.DisksDir, "parents", r.ID, "base.vhdx"))
			if err != nil {
				return apperr.InvalidParam("path")
			}
			d = &domain.Disk{
				RepoID:    r.ID,
				Kind:      domain.DiskKindParent,
				VHDXPath:  abs,
				SizeBytes: src.SizeBytes,
				VHDType:   domain.VHDTypeDynamic,
				State:     domain.DiskStateCreating,
				// 拷贝出来的母盘沿用源盘的格式标记（内容整盘复制，布局不变）。
				FileSystem: src.FileSystemOrDefault(),
			}
			if err := tx.CreateDisk(ctx, d); err != nil {
				return err
			}
			pid := d.ID
			r.ParentDiskID = &pid
			if err := tx.UpdateRepository(ctx, r); err != nil {
				return err
			}
		}
		dstRepo, dstDisk = r, d
		return nil
	})
	if err != nil {
		return err
	}
	if dstDisk.State == domain.DiskStateReady && s.Disk.Exists(dstDisk.VHDXPath) {
		return nil
	}
	rep.Progress(15)

	// Clone 内部已包含"复制 + 重置磁盘标识"，调用点不再重复重置（见 platform.DiskBackend.Clone）。
	if err := s.Disk.Clone(ctx, src.VHDXPath, dstDisk.VHDXPath); err != nil {
		return err
	}
	rep.Progress(90)

	if size, err := s.Disk.PhysicalSize(dstDisk.VHDXPath); err == nil {
		dstDisk.PhysicalBytes = size
	}
	dstDisk.State = domain.DiskStateReady
	if err := s.Store.UpdateDisk(ctx, dstDisk); err != nil {
		return err
	}
	rep.Progress(100)
	s.audit(ctx, p.OwnerID, "disk.copy.done", "disk:"+src.ID, "new_repo="+dstRepo.ID, domain.AuditResultOK)
	return nil
}

// runDeleteDisk 删除单个磁盘及其目标、分配记录。
func (s *DiskService) runDeleteDisk(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p deleteDiskPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	rep.Progress(10)
	if err := s.deleteDiskArtifacts(ctx, p.DiskID, p.AllocationID); err != nil {
		return err
	}
	rep.Progress(100)
	return nil
}

// runReclaim 批量回收磁盘集合，并按 Finalize 收尾。
//
// 两条路径：
//   - Finalize=artifacts（当前删除存储库走的路径）：DB 记录在 API 请求内已删除，
//     这里只按 payload 快照回收平台侧资源，不回查 DB；
//   - 其它（历史路径 / 清理差异盘）：按 DiskIDs 逐块"回收资源 + 记录"，最后收尾。
func (s *DiskService) runReclaim(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p reclaimPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}

	if p.Finalize == reclaimFinalizeArtifacts {
		total := len(p.Artifacts)
		for i := range p.Artifacts {
			if err := s.deleteDiskPhysical(ctx, p.Artifacts[i].DiskPath, p.Artifacts[i].Targets); err != nil {
				return err
			}
			if total > 0 {
				rep.Progress((i + 1) * 80 / total)
			}
		}
		rep.Progress(100)
		s.Log.Info("已完成存储库资源回收（记录此前已删除）",
			"repo_id", p.RepoID, "disk_count", total)
		return nil
	}

	total := len(p.DiskIDs)
	for i, id := range p.DiskIDs {
		if err := s.deleteDiskArtifacts(ctx, id, ""); err != nil {
			return err
		}
		if total > 0 {
			rep.Progress((i + 1) * 80 / total)
		}
	}

	switch p.Finalize {
	case reclaimFinalizeRepo:
		if err := s.purgeRepository(ctx, p.RepoID); err != nil {
			return err
		}
	case reclaimFinalizeParentIdle:
		if p.RepoID != "" {
			if err := s.Store.SetParentCondition(ctx, p.RepoID, domain.ParentIdle, false); err != nil {
				return err
			}
		}
	}
	rep.Progress(100)
	return nil
}

// runCleanupDiffs 是 JobCleanupDiffs 的实现：维护态下清理该库全部差异盘。
//
// 顺序必须是"先通知客户端断开，再拆 iSCSI 目标，最后删盘文件"：
//   - 客户端还在使用差异盘（或平台侧 LUN 映射/会话没拆）时，VHDX 文件被占用，删除必然失败；
//   - 撤销租约会给客户端推 revoke 指令，客户端据此主动卸载并展示原因（见 LeaseService.RevokeWithReason）。
//
// 每次尝试都重新列出当前仍存在的差异盘，因此任务重试幂等。
func (s *DiskService) runCleanupDiffs(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p cleanupDiffsPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if strings.TrimSpace(p.ParentDiskID) == "" {
		return nil
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			// 库已经删掉：没有可清理的对象。
			return nil
		}
		return err
	}
	disks, err := s.Store.ListDiffDisksByParent(ctx, p.ParentDiskID)
	if err != nil {
		return err
	}
	rep.Progress(3)

	// ① 撤销该库所有在线租约：服务端把 kick 指令推给客户端，客户端主动卸载并展示原因。
	// 这一步是"能删文件"的前提，必须尽力完成；失败的租约仍会被停用目标 -> 拆除目标兜底。
	if s.Leases != nil {
		leases, lErr := s.Store.ListActiveLeasesByRepo(ctx, repo.ID)
		if lErr != nil {
			return lErr
		}
		for i := range leases {
			if err := s.Leases.RevokeWithReason(ctx, leases[i].ID, "system", RevokeReasonRepoMaintenance); err != nil {
				s.Log.Warn("维护清理：撤销租约失败（继续拆除目标）",
					"repo_id", repo.ID, "lease_id", leases[i].ID, "error", err)
			}
		}
	}
	rep.Progress(10)

	if len(disks) == 0 {
		rep.Progress(100)
		return nil
	}
	for i := range disks {
		if err := s.deleteDiskArtifacts(ctx, disks[i].ID, ""); err != nil {
			return err
		}
		rep.Progress(10 + 90*(i+1)/len(disks))
	}
	s.audit(ctx, "system", "repo.parent.cleanup_diffs.done", "repo:"+repo.ID,
		"disks="+strconv.Itoa(len(disks)), domain.AuditResultOK)
	s.Log.Info("维护清理差异盘完成", "repo_id", repo.ID, "disk_count", len(disks))
	return nil
}

// purgeRepository 删除存储库记录及其残留的分配、成员与目标。
func (s *DiskService) purgeRepository(ctx context.Context, repoID string) error {
	if repoID == "" {
		return nil
	}
	allocations, err := s.Store.ListAllocationsByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	// 先记下**本库**的分配 ID：下面的目标清理只允许命中这些分配。
	//
	// ⚠️ 全量目标列表里还有**其他库**的目标。无差别删除会把别的库的目标 DB 记录也删掉，
	// 而它们的 Windows 目标还在——正在进行的挂载随后 SetIscsiTargetEnabled 0 行受影响，
	// 直接 404 iscsi.target_not_found；后续挂载重建 DB 记录还会换新 CHAP 密钥
	// （真实事故：删一个库导致另一个库的挂载失败）。
	ownAllocations := make(map[string]bool, len(allocations))
	for i := range allocations {
		ownAllocations[allocations[i].ID] = true
	}
	for i := range allocations {
		if err := s.removeAllocation(ctx, allocations[i].ID); err != nil {
			return err
		}
	}
	targets, err := s.Store.ListIscsiTargets(ctx, "")
	if err != nil {
		return err
	}
	for i := range targets {
		t := &targets[i]
		if t.AllocationID == nil || !ownAllocations[*t.AllocationID] {
			continue
		}
		if err := s.Store.DeleteIscsiTarget(ctx, t.ID); err != nil {
			s.Log.Warn("清理目标记录失败", "target", t.TargetName, "error", err)
		}
	}
	if err := s.Store.ReplaceRepoMembers(ctx, repoID, nil); err != nil {
		s.Log.Warn("清理存储库成员失败", "repo_id", repoID, "error", err)
	}
	if err := s.Store.DeleteRepository(ctx, repoID); err != nil && !isNotFound(err) {
		return err
	}
	s.audit(ctx, "", "repo.purged", "repo:"+repoID, "", domain.AuditResultOK)
	return nil
}

// runCompact 回收 VHDX 物理空间。
func (s *DiskService) runCompact(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p compactPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if s.Disk == nil {
		return errNotImplemented
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		return err
	}
	if disk.Mounted {
		return apperr.DiskBusy()
	}
	before, _ := s.Disk.PhysicalSize(disk.VHDXPath)
	rep.Progress(30)
	if err := s.Disk.Optimize(ctx, disk.VHDXPath); err != nil {
		return err
	}
	after, err := s.Disk.PhysicalSize(disk.VHDXPath)
	if err != nil {
		after = before
	}
	disk.PhysicalBytes = after
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	rep.Progress(100)
	s.Log.Info("已完成空间回收", "disk_id", disk.ID, "before_bytes", before, "after_bytes", after)
	return nil
}

// ---- 内部辅助 ----

// deleteDiskPhysical 只回收磁盘的**平台侧资源**：目标映射与目标、虚拟盘登记、VHDX 文件。
//
// 完全不碰 DB —— 调用方（记录已删的存储库回收任务，见 reclaimFinalizeArtifacts）已经
// 把 disks / iscsi_targets 记录删掉了，那时候回查 DB 只会读到"不存在"。
// 幂等：目标不存在、文件不存在都视为成功，因此任务重试是安全的。
func (s *DiskService) deleteDiskPhysical(ctx context.Context, path string, targetNames []string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	s.detachTargets(ctx, path, targetNames)
	return s.removeDiskFile(ctx, path)
}

// detachTargets 在平台上拆除该盘的目标：解除映射 → 停用 → 删除目标 → 移除虚拟盘登记。
//
// 全部尽力而为（只记日志、不返回错误）：目标可能已经被人工删掉，这里失败不该让整个回收
// 任务卡死（会连带重试整批盘）。真正的失败点 —— 文件删不掉 —— 由 removeDiskFile 判定。
func (s *DiskService) detachTargets(ctx context.Context, path string, targetNames []string) {
	if s.Iscsi == nil {
		return
	}
	if s.IscsiSvc == nil {
		// 装配异常（App.New 一定会回填）：宁可不拆，也不在这里再写一套平台调用 ——
		// 多一套实现正是"短名寻址、目标删不掉"的成因。
		s.Log.Error("未装配 iSCSI 服务，跳过平台侧目标拆除", "path", path)
		return
	}
	for _, name := range targetNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		// 目标拆除走唯一实现：寻址名由服务按 iqn_prefix 现推（短名问不到平台）。
		s.IscsiSvc.TeardownTargetByShortName(ctx, name, TeardownOptions{DetachDiskPath: path})
	}
	if err := s.Iscsi.RemoveVirtualDisk(ctx, path); err != nil {
		s.Log.Warn("移除 iSCSI 虚拟盘登记失败", "path", path, "error", err)
	}
}

// removeDiskFile 删除底层虚拟盘文件（后端实现幂等：文件不存在视为成功）。
func (s *DiskService) removeDiskFile(ctx context.Context, path string) error {
	if s.Disk == nil {
		return nil
	}
	// 顺手删掉同名的元数据 sidecar：它描述的是**这个文件**，文件没了标签就没意义，
	// 而孤儿扫描只认 .vhdx，留着它没有任何机制能再清理（见 diskmeta.go）。
	removeDiskMetaFile(s.Log, path)
	return s.Disk.Delete(ctx, path)
}

// targetNamesOf 取出目标名列表（跳过空名，避免往平台侧传非法目标名）。
func targetNamesOf(targets []domain.IscsiTarget) []string {
	names := make([]string, 0, len(targets))
	for i := range targets {
		if targets[i].TargetName != "" {
			names = append(names, targets[i].TargetName)
		}
	}
	return names
}

// deleteDiskArtifacts 删除磁盘的全部外部痕迹：目标映射与目标、虚拟盘登记、文件、
// 分配记录与磁盘记录，并回退配额用量。对已不存在的磁盘是幂等的。
func (s *DiskService) deleteDiskArtifacts(ctx context.Context, diskID, allocationID string) error {
	disk, err := s.Store.GetDisk(ctx, diskID)
	if err != nil {
		if !isNotFound(err) {
			return err
		}
		return s.removeAllocation(ctx, allocationID)
	}
	if disk.Mounted {
		return apperr.DiskBusy()
	}

	var alloc *domain.Allocation
	if allocationID != "" {
		if alloc, err = s.Store.GetAllocation(ctx, allocationID); err != nil && !isNotFound(err) {
			return err
		}
	}
	if alloc == nil {
		if a, aErr := s.Store.GetAllocationByDisk(ctx, disk.ID); aErr == nil {
			alloc = a
		} else if !isNotFound(aErr) {
			return aErr
		}
	}

	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, disk.ID)
	if err != nil {
		return err
	}
	// 平台侧：解映射 → 停用 → 删除目标 → 移除虚拟盘登记。
	s.detachTargets(ctx, disk.VHDXPath, targetNamesOf(targets))
	for i := range targets {
		if err := s.Store.DeleteIscsiTarget(ctx, targets[i].ID); err != nil {
			s.Log.Warn("删除目标记录失败", "target", targets[i].TargetName, "error", err)
		}
	}

	// ⚠️ 必须先完成上面的 LUN 下线与 backstore 回收，再删除底层磁盘：
	// Linux 上 LV 若仍被 LIO(iblock) 打开，lvremove 会被内核拒绝。
	if err := s.removeDiskFile(ctx, disk.VHDXPath); err != nil {
		return err
	}

	// 用户 ID 要在删除分配行之前取到。
	userID := ""
	if alloc != nil {
		userID = alloc.UserID
		if err := s.removeAllocation(ctx, alloc.ID); err != nil {
			return err
		}
	}
	if err := s.Store.DeleteDisk(ctx, disk.ID); err != nil && !isNotFound(err) {
		return err
	}
	// 盘记录已经不在表里了，此时按 disks 表重算就等于"把这块盘的占用退干净"。
	s.resyncUsage(ctx, disk.RepoID, userID)
	// 差异盘数量不再改变母盘状态：正常态（idle）本来就允许存在差异盘，
	// 维护态要等用户显式退出维护才回到 idle（见 TransitionParent）。
	return nil
}

// resyncUsage 把"该盘所在存储库 / 所属用户"的已用量**按 disks 表的分配情况重算**。
//
// ⚠️ 不要再改回"删除时按记忆的金额做减法"。真实工单：删掉一个差异盘后存储库已用
// 变成 **-100M** —— 账上记的金额与删除那一刻算出来的金额对不上（当时的口径是物理占用，
// 差异盘会长大、母盘容量也可能被改），差额就沉淀成了负数。
//
// 以 disks 表（唯一真源）重算是幂等的：多算一次、少算一次都能自愈。
//
// 计费口径见 store.accountedUsageSQL（已用 = 已分配磁盘总量：有未释放的分配行就按
// 标称容量计费，建盘终态失败的盘计 0）。
func (s *DiskService) resyncUsage(ctx context.Context, repoID, userID string) {
	if strings.TrimSpace(repoID) != "" {
		if _, err := s.Store.RecomputeRepoUsage(ctx, repoID); err != nil {
			s.Log.Warn("按实际占用重算存储库用量失败", "repo_id", repoID, "error", err)
		}
	}
	if strings.TrimSpace(userID) != "" && userID != domain.TempAllocationUserID {
		if _, err := s.Store.RecomputeUserUsage(ctx, userID); err != nil {
			s.Log.Warn("按实际占用重算用户用量失败", "user_id", userID, "error", err)
		}
	}
}

// allocatedUserID 返回该盘分配记录上的用户 ID；没有分配或查询失败时返回空串。
//
// 分配行一旦被删除就查不到了，所以需要用户 ID 的调用方要在删除前拿。
func (s *DiskService) allocatedUserID(ctx context.Context, diskID string) string {
	alloc, err := s.Store.GetAllocationByDisk(ctx, diskID)
	if err != nil {
		if !isNotFound(err) {
			s.Log.Warn("查询分配失败，无法按用户重算用量", "disk_id", diskID, "error", err)
		}
		return ""
	}
	if alloc == nil {
		return ""
	}
	return alloc.UserID
}

// removeAllocation 删除分配记录；不存在视为成功。
func (s *DiskService) removeAllocation(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := s.Store.DeleteAllocation(ctx, id); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}
