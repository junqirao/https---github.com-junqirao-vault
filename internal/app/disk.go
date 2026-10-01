package app

import (
	"context"
	"path/filepath"
	"strings"

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
	reclaimFinalizeRepo = "repo"
	// reclaimFinalizeParentIdle 回收差异盘后把母盘置回 idle。
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

type reclaimPayload struct {
	RepoID       string   `json:"repo_id"`
	ParentDiskID string   `json:"parent_disk_id,omitempty"`
	DiskIDs      []string `json:"disk_ids,omitempty"`
	Finalize     string   `json:"finalize,omitempty"`
}

type compactPayload struct {
	DiskID string `json:"disk_id"`
}

type publishPayload struct {
	AllocationID string `json:"allocation_id"`
}

// DiskService 负责 VHDX 的同步查询与异步生命周期任务。
type DiskService struct {
	Deps
	Repos *RepoService
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
	w.Register(job.HandlerFunc{T: domain.JobCopyVHDX, F: s.runCopyVHDX})
	w.Register(job.HandlerFunc{T: domain.JobDeleteDisk, F: s.runDeleteDisk})
	w.Register(job.HandlerFunc{T: domain.JobReclaim, F: s.runReclaim})
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
	// 为什么必须回退（真实反馈："我一点空间都没用就占了 1G"）：分配时按母盘标称容量
	// 预留用量，差异盘建完后才按实测物理占用校正（见 runCreateDiff）。建盘彻底失败时
	// 校正永远不会发生，那份预留就成了**永久挂账** —— 每个失败的分配 1GB 起。
	//
	// releaseUsage 自身会跳过母盘（只有差异盘计费）与没有分配的情况，因此不会出现负账。
	if alloc, aErr := s.Store.GetAllocationByDisk(ctx, disk.ID); aErr == nil {
		s.releaseUsage(ctx, disk, alloc)
	} else if !isNotFound(aErr) {
		s.Log.Warn("查询分配失败，未能回退预留用量", "disk_id", disk.ID, "error", aErr)
	}
	disk.State = domain.DiskStateError
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	s.Log.Error("建盘任务彻底失败，磁盘已置为异常并回退预留用量",
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
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobDeleteDisk, diskID, "delete_disk:"+diskID, lock.DiskKey(diskID), payload)
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
// 幂等：disk 已是 ready 且文件存在时直接返回。sourceDir 为空表示建空盘。
// 供 DiskService 的建盘任务与 UploadService 的完成任务复用。
func (s *DiskService) buildVHDXFromSource(ctx context.Context, disk *domain.Disk, sourceDir string, rep job.Reporter) error {
	if disk.State == domain.DiskStateReady && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
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

	if sourceDir != "" {
		// 有源目录：由卷后端一次性完成"激活 → 格式化 → 拷入 → 卸载"，
		// 中间的挂载细节（Windows=盘符/robocopy，Linux=ntfs-3g/copyTree）不再泄漏到本层。
		if _, _, err := s.Vol.MountAndCopy(ctx, disk.VHDXPath, sourceDir, "NTFS", "VAULT"); err != nil {
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
		if _, err := s.Vol.EnsureFormatted(ctx, disk.VHDXPath, "NTFS", "VAULT"); err != nil {
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
	rep.Progress(100)
	s.audit(ctx, "", "disk.create.done", "disk:"+disk.ID, disk.VHDXPath, domain.AuditResultOK)
	return nil
}

// runCreateDiff 从母盘派生差异盘。
func (s *DiskService) runCreateDiff(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p diffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		return err
	}
	if disk.State == domain.DiskStateReady && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
		return nil
	}
	if s.Disk == nil {
		return errNotImplemented
	}
	parent, err := s.Store.GetDisk(ctx, p.ParentDiskID)
	if err != nil {
		return err
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		return err
	}
	rep.Progress(20)

	if !s.Disk.Exists(disk.VHDXPath) {
		if err := s.Disk.CreateDiff(ctx, disk.VHDXPath, parent.VHDXPath); err != nil {
			return err
		}
	}
	rep.Progress(60)

	// 记录差异盘基于的母盘版本；挂载前会与 repositories.parent_version 比对。
	disk.ParentVersion = repo.ParentVersion
	if fingerprint, err := s.Disk.Fingerprint(parent.VHDXPath); err == nil {
		parent.ContentFingerprint = fingerprint
		if err := s.Store.UpdateDisk(ctx, parent); err != nil {
			return err
		}
	}
	if size, err := s.Disk.PhysicalSize(disk.VHDXPath); err == nil {
		// 用实际物理占用校正分配时的逻辑预留。
		delta := size - disk.SizeBytes
		disk.PhysicalBytes = size
		if delta != 0 {
			if _, err := s.Store.AddRepoUsedBytes(ctx, disk.RepoID, delta); err == nil {
				if alloc, aErr := s.Store.GetAllocationByDisk(ctx, disk.ID); aErr == nil && alloc.UserID != "" {
					_, _ = s.Store.AddUserUsedBytes(ctx, alloc.UserID, delta)
				}
			}
		}
	}
	disk.State = domain.DiskStateReady
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	rep.Progress(100)
	s.audit(ctx, "", "disk.create_diff.done", "disk:"+disk.ID, "parent="+parent.ID, domain.AuditResultOK)
	return nil
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

// runReclaim 批量回收磁盘集合，并按 Finalize 收尾（置母盘 idle 或删除存储库）。
func (s *DiskService) runReclaim(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p reclaimPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
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
	if s.Iscsi != nil {
		for i := range targets {
			name := targets[i].TargetName
			if err := s.Iscsi.DetachLun(ctx, name, disk.VHDXPath); err != nil {
				s.Log.Warn("解除目标映射失败", "target", name, "error", err)
			}
			// 只停用（不带 BackingRef）：保持映射不动，随后由 RemoveTarget 一并拆除。
			if err := s.Iscsi.EnsureTarget(ctx, platform.TargetSpec{Name: name, Enabled: false}); err != nil {
				s.Log.Warn("停用目标失败", "target", name, "error", err)
			}
			if err := s.Iscsi.RemoveTarget(ctx, name); err != nil {
				s.Log.Warn("删除目标失败", "target", name, "error", err)
			}
		}
		if err := s.Iscsi.RemoveVirtualDisk(ctx, disk.VHDXPath); err != nil {
			s.Log.Warn("移除 iSCSI 虚拟盘登记失败", "path", disk.VHDXPath, "error", err)
		}
	}
	for i := range targets {
		if err := s.Store.DeleteIscsiTarget(ctx, targets[i].ID); err != nil {
			s.Log.Warn("删除目标记录失败", "target", targets[i].TargetName, "error", err)
		}
	}

	// ⚠️ 必须先完成上面的 LUN 下线与 backstore 回收，再删除底层磁盘：
	// Linux 上 LV 若仍被 LIO(iblock) 打开，lvremove 会被内核拒绝。
	if s.Disk != nil {
		if err := s.Disk.Delete(ctx, disk.VHDXPath); err != nil {
			return err
		}
	}

	if alloc != nil {
		if err := s.removeAllocation(ctx, alloc.ID); err != nil {
			return err
		}
	}
	if err := s.Store.DeleteDisk(ctx, disk.ID); err != nil && !isNotFound(err) {
		return err
	}
	s.releaseUsage(ctx, disk, alloc)

	// 差异盘清理完毕后，若已无兄弟子盘则母盘回到 idle。
	if disk.Kind == domain.DiskKindDiff && disk.ParentID != nil {
		if n, nErr := s.Store.CountDiffDisks(ctx, *disk.ParentID); nErr == nil && n == 0 {
			if repo, rErr := s.Store.GetRepository(ctx, disk.RepoID); rErr == nil && repo.Condition() == domain.ParentDerived {
				if err := s.Store.SetParentCondition(ctx, disk.RepoID, domain.ParentIdle, false); err != nil {
					s.Log.Warn("置母盘为 idle 失败", "repo_id", disk.RepoID, "error", err)
				}
			}
		}
	}
	return nil
}

// accountedUsage 返回某个差异盘**当前计入配额**的量。
//
// 口径：分配时按逻辑大小（SizeBytes）预留 → 建盘完成后按实测物理占用校正
// （见 runCreateDiff），因此账上记的是"实测物理占用"。删除回退必须用同一个口径。
//
// ⚠️ 这正是"用户配额变成负数"的根因所在：原实现在删除时按**逻辑大小**回退
// （`-disk.SizeBytes`），而账上按**物理大小**记，每删一个差异盘就多退
// （逻辑 − 物理）的差额。例如 2GB 的差异盘物理只有几十 MB 时，删 4 个就出现
// 截图里的 -7.98 GB。
//
// 物理占用尚未测出（0，如建盘失败或尚未测量）时退回逻辑大小，与预留金额一致。
func accountedUsage(disk *domain.Disk) int64 {
	if disk.PhysicalBytes > 0 {
		return disk.PhysicalBytes
	}
	return disk.SizeBytes
}

// releaseUsage 回退该差异盘此前计入的用量（金额必须与计入时一致，见 accountedUsage）。
func (s *DiskService) releaseUsage(ctx context.Context, disk *domain.Disk, alloc *domain.Allocation) {
	if disk.Kind != domain.DiskKindDiff || alloc == nil {
		return
	}
	delta := -accountedUsage(disk)
	if _, err := s.Store.AddRepoUsedBytes(ctx, disk.RepoID, delta); err != nil {
		s.Log.Warn("回退存储库用量失败", "repo_id", disk.RepoID, "error", err)
	}
	if alloc.UserID != "" && alloc.UserID != domain.TempAllocationUserID {
		if _, err := s.Store.AddUserUsedBytes(ctx, alloc.UserID, delta); err != nil {
			s.Log.Warn("回退用户用量失败", "user_id", alloc.UserID, "error", err)
		}
	}
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
