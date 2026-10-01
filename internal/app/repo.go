package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/store"
)

// RepoService 负责存储库、成员、分配与母盘用途状态机的编排。
//
// 事务边界约定：配额校验与"读-校验-写"必须在 Store.Tx + LockRepository 内完成，
// 否则会出现配额超卖（见 docs/implementation.md 5.13）。
// maxPreparedDisks 是建库时允许预创建的池位（= 共享数量）上限。
//
// 每个池位都要真派生一次 VHDX 并下发一次 iSCSI 目标（Windows 上单个目标约 15-20 秒），
// 不设上限的话一次误填就能把服务端占住几十分钟。需要更多并发用户就建多个库。
const maxPreparedDisks = 32

type RepoService struct {
	Deps
	// IscsiSvc 用来拆除母盘临时共享的 iSCSI 目标：目标拆除只有一套实现
	// （IscsiService.TeardownTarget），这里不再自己拼平台的三个调用。
	IscsiSvc *IscsiService
}

// parentIDOf 取库的母盘 ID；没有母盘（独享库或数据异常）时返回空串。
func parentIDOf(repo *domain.Repository) string {
	if repo == nil || repo.ParentDiskID == nil {
		return ""
	}
	return *repo.ParentDiskID
}

// ParentAction 是母盘用途迁移动作（对外由 API 的 parent/actions 路由暴露）。
type ParentAction string

const (
	// ParentActionDerive 标记母盘已被派生（idle → derived）。
	ParentActionDerive ParentAction = "derive"
	// ParentActionTempShare 母盘临时只读共享（idle → temp_shared）。
	ParentActionTempShare ParentAction = "temp_share"
	// ParentActionUnshare 撤销临时共享（temp_shared → idle）。
	ParentActionUnshare ParentAction = "unshare"
	// ParentActionMaintenance 进入维护态（idle/derived → maintenance）。
	ParentActionMaintenance ParentAction = "maintenance"
	// ParentActionFinishMaint 完成维护（maintenance → idle，母盘版本号 +1）。
	ParentActionFinishMaint ParentAction = "finish_maintenance"
	// ParentActionCleanup 清理全部差异盘（derived → idle，异步执行）。
	ParentActionCleanup ParentAction = "cleanup_diffs"
)

// CreateRepoInput 是创建存储库的入参。
type CreateRepoInput struct {
	Name string
	Mode domain.RepoMode
	// OwnerID 存储库归属用户。
	OwnerID string
	// SourceDir 可选的**服务端本地目录**；为空表示创建空盘。
	SourceDir string
	// SizeBytes 用户填写的容量；0 表示按源目录自动计算。
	SizeBytes int64
	// MaxDiffDisks 差异盘上限；0 表示使用配置默认值。
	MaxDiffDisks int
	// QuotaBytes 应用层配额；0 表示不限。
	QuotaBytes int64
	// Group 分组标签，写入 meta.group。
	Group string
	// StorageID 可选的目标存储 ID：
	//   - 为空 → 在所有**启用**的存储中按"所在卷可用空间最大"自动选（现有 Pick 语义）；
	//   - 非空 → 使用该存储的根；存储不存在或已停用会返回明确错误。
	StorageID string
}

// UpdateRepoInput 是更新存储库的入参；指针字段为 nil 表示不修改。
type UpdateRepoInput struct {
	Name         *string
	QuotaBytes   *int64
	MaxDiffDisks *int
	Group        *string
}

// AllocateInput 是创建分配的入参。
type AllocateInput struct {
	RepoID string
	// UserID 被分配的用户。
	UserID string
	// OperatorID 发起人（用于权限校验与审计）。
	OperatorID string
}

// Create 创建存储库。
//
// 共享模式会同时创建母盘磁盘记录并置 parent_condition=idle；
// 独享模式创建一个 standalone 磁盘。物理建盘交给 Job（幂等，见 5.12）。
func (s *RepoService) Create(ctx context.Context, in CreateRepoInput) (*domain.Repository, *domain.Disk, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, nil, apperr.InvalidParam("name")
	}
	if !in.Mode.Valid() {
		return nil, nil, apperr.InvalidParam("mode")
	}
	ownerID := strings.TrimSpace(in.OwnerID)
	if ownerID == "" {
		return nil, nil, apperr.InvalidParam("owner_id")
	}

	// share 是"共享数量"：共享模式下建库时就按这个数量把差异盘池建好
	// （每个池位 = 一块差异盘 + 一个 iSCSI 目标），之后每次分配占用一格。
	//
	// 它同时就是差异盘上限（见 domain.Repository.MaxDiffDisks 注释）：预创建多少，
	// 就最多能分给多少个用户 —— 避免"预创建 5 个但上限 3 个"这类自相矛盾的配置。
	share := in.MaxDiffDisks
	if share < 0 {
		return nil, nil, apperr.InvalidParam("max_diff_disks")
	}
	if share > maxPreparedDisks {
		// 每个池位都要真派生一次 VHDX 并下发一次 iSCSI 目标（Windows 上单个目标约 15-20 秒），
		// 不设上限的话一次误填就能把服务端占住几十分钟。
		return nil, nil, apperr.InvalidParam("max_diff_disks")
	}
	if in.Mode != domain.RepoModeShared {
		// 独享模式没有差异盘，"共享数量"无意义；静默归零而不是报错（老客户端仍会带这个字段）。
		share = 0
	}
	// preparing：这次建库要不要顺手把差异盘池建好（共享模式 + 填了共享数量）。
	// 后面的事务与任务派发都要用它，因此在事务外先算好。
	preparing := in.Mode == domain.RepoModeShared && share > 0

	raw := s.raw()
	srcDir := strings.TrimSpace(in.SourceDir)
	if srcDir != "" {
		// 白名单校验：source_dir 必须落在 storage.source_roots（留空则回退生效存储根）之下，
		// 否则返回 source_dir.not_allowed。避免服务端任意本地目录被当作建库源读取。
		validated, err := s.validateSourceDir(ctx, srcDir)
		if err != nil {
			return nil, nil, err
		}
		srcDir = validated
	}

	// 源目录实际占用：VHDX 容量按 5.2 规则计算。
	var dirBytes int64
	if srcDir != "" {
		n, err := dirSize(srcDir)
		if err != nil {
			return nil, nil, err
		}
		dirBytes = n
	}
	size := s.sizing().CalculateVHDXSize(dirBytes, in.SizeBytes)

	// 选存储与根：
	//   - 指定 storage_id → 必须存在且启用，直接用它的根；
	//   - 未指定 → 在所有启用存储里选"所在卷可用空间最大"的根（Pick 语义）。
	guard, chosen, err := s.pickCreateStorage(ctx, strings.TrimSpace(in.StorageID), size)
	if err != nil {
		return nil, nil, err
	}
	if err := s.ensureVolumeFreeAt(ctx, guard.Root, size); err != nil {
		return nil, nil, err
	}
	s.Log.Info("已选择存储放置新磁盘",
		"storage_id", storageIDOf(chosen), "storage_name", storageNameOf(chosen),
		"root", guard.Root, "size_bytes", size, "explicit", in.StorageID != "")

	var repo *domain.Repository
	var disk *domain.Disk

	err = s.Store.Tx(ctx, func(tx *store.Store) error {
		owner, err := tx.GetUserByID(ctx, ownerID)
		if err != nil {
			return err
		}
		// 配额校验只有一个口径：**容量占用（用户）配额**。
		//
		// 概念上不要把"容量"和"配额"拆成两件事：
		//   - 用户设定的 `size_bytes` 是这个库的**容量**（VHDX 标称容量）；
		//   - 这个容量就是它占用的配额，从**用户的配额**里扣除（下面这一条校验即此）。
		// 库自身不再有"配额"概念（老数据里残留的 repo.quota_bytes 仍按库级上限兼容处理，
		// 但界面已不再收集，且不再用"配额必须 ≥ 容量"这类跨概念规则拒绝建库 —— 那是
		// 把两个概念拆开后的产物，会让用户填了容量却被配额拒绝，且提示极难懂）。
		if owner.QuotaBytes > 0 && owner.UsedBytes+size > owner.QuotaBytes {
			return apperr.RepoQuotaExceeded(owner.QuotaBytes, owner.UsedBytes, size)
		}

		// maxDiff 既是"共享数量"也是差异盘上限（见 domain.Repository.MaxDiffDisks）。
		// 显式填了数字就用它（这些池位会被预创建）；没填才回退配置默认值 ——
		// 默认值只当上限用、不做预创建，免得默认配置下每建一个库就派生几十块盘、建几十个 iSCSI 目标。
		maxDiff := share
		if maxDiff <= 0 {
			maxDiff = raw.Storage.DefaultMaxDiffDisks
		}
		if maxDiff <= 0 {
			maxDiff = 50
		}

		// 预创建（池化）：填了共享数量就必须先把池建好，再让库进入可用状态。
		// 否则用户要么分配到"还没建的池位"，要么挂载时又退回等建盘的老路径 ——
		// 那正是共享数量想消除的东西。
		state := domain.RepoStateActive
		meta := domain.RepoMeta{Group: strings.TrimSpace(in.Group)}
		if preparing {
			state = domain.RepoStateCreating
			meta.Pool = true
			meta.Prepare = &domain.RepoPrepare{Phase: domain.RepoPhaseParent, Total: 1}
		}

		r := &domain.Repository{
			Name:         name,
			Mode:         in.Mode,
			OwnerID:      ownerID,
			MaxDiffDisks: maxDiff,
			QuotaBytes:   in.QuotaBytes,
			State:        state,
			Meta:         meta,
		}
		if in.Mode == domain.RepoModeShared {
			cond := domain.ParentIdle
			r.ParentCondition = &cond
		}
		if err := tx.CreateRepository(ctx, r); err != nil {
			return err
		}

		kind := domain.DiskKindStandalone
		rel := filepath.Join(raw.Storage.DisksDir, "standalone", r.ID, "data.vhdx")
		if in.Mode == domain.RepoModeShared {
			kind = domain.DiskKindParent
			rel = filepath.Join(raw.Storage.DisksDir, "parents", r.ID, "base.vhdx")
		}
		abs, err := s.Disk.DiskRef(guard.Root, rel)
		if err != nil {
			// 引用非法只可能是配置问题，返回通用参数错误，不泄漏根目录细节。
			return apperr.InvalidParam("path")
		}

		d := &domain.Disk{
			RepoID:    r.ID,
			Kind:      kind,
			VHDXPath:  abs,
			SizeBytes: size,
			VHDType:   domain.VHDTypeDynamic,
			State:     domain.DiskStateCreating,
		}
		if err := tx.CreateDisk(ctx, d); err != nil {
			return err
		}
		if in.Mode == domain.RepoModeShared {
			pid := d.ID
			r.ParentDiskID = &pid
			if err := tx.UpdateRepository(ctx, r); err != nil {
				return err
			}
		}
		repo, disk = r, d
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// 派生列：库容量就是当场算出的那块盘的标称容量（见 domain.Repository.CapacityBytes）。
	// 这里刚建好记录、还没人回读，补上它，创建响应里的库才和后续 Get/List 口径一致。
	repo.CapacityBytes = size

	if preparing {
		// 预创建：母盘 → 共享数量个差异盘 → 同数量的 iSCSI 目标（见 DiskService.runPrepareRepo）。
		// 锁用库级 Key：整库（母盘 + 池）在这期间必须独占，否则会有人分配到一块还在派生的盘。
		payload := prepareRepoPayload{DiskID: disk.ID, RepoID: repo.ID, SourceDir: srcDir, Count: share}
		if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobPrepareRepo, disk.ID,
			"prepare_repo:"+repo.ID, lock.RepoKey(repo.ID), payload); err != nil {
			return repo, disk, err
		}
	} else {
		jobType := domain.JobCreateVHDX
		if srcDir != "" {
			jobType = domain.JobCreateVHDXFromDir
		}
		payload := createDiskPayload{DiskID: disk.ID, SourceDir: srcDir}
		if _, _, err := s.Jobs.EnqueueWith(ctx, jobType, disk.ID, "create_vhdx:"+disk.ID, lock.DiskKey(disk.ID), payload); err != nil {
			return repo, disk, err
		}
	}

	s.audit(ctx, in.OwnerID, "repo.create", "repo:"+repo.ID, string(in.Mode)+" name="+name, domain.AuditResultOK)
	return repo, disk, nil
}

// Get 按 ID 查询存储库。
func (s *RepoService) Get(ctx context.Context, id string) (*domain.Repository, error) {
	return s.Store.GetRepository(ctx, id)
}

// List 列出存储库；ownerID 为空表示不过滤。
func (s *RepoService) List(ctx context.Context, ownerID string, limit, offset int) ([]domain.Repository, error) {
	return s.Store.ListRepositories(ctx, ownerID, limit, offset)
}

// Update 更新存储库的可变字段（改名 / 配额 / 差异盘上限 / 分组）。
func (s *RepoService) Update(ctx context.Context, id string, in UpdateRepoInput) (*domain.Repository, error) {
	var out *domain.Repository
	err := s.Store.Tx(ctx, func(tx *store.Store) error {
		r, err := tx.LockRepository(ctx, id)
		if err != nil {
			return err
		}
		if in.Name != nil {
			name := strings.TrimSpace(*in.Name)
			if name == "" {
				return apperr.InvalidParam("name")
			}
			r.Name = name
		}
		if in.QuotaBytes != nil {
			if *in.QuotaBytes < 0 {
				return apperr.InvalidParam("quota_bytes")
			}
			r.QuotaBytes = *in.QuotaBytes
		}
		if in.MaxDiffDisks != nil {
			if *in.MaxDiffDisks < 0 {
				return apperr.InvalidParam("max_diff_disks")
			}
			r.MaxDiffDisks = *in.MaxDiffDisks
		}
		if in.Group != nil {
			r.Meta.Group = strings.TrimSpace(*in.Group)
		}
		if err := tx.UpdateRepository(ctx, r); err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 回读一次拿派生列：库容量不落库（见 domain.Repository.CapacityBytes），而上面
	// LockRepository 的行锁查询不带派生列。缺了它，"编辑存储库"的响应里容量为 0，
	// 前端拿这个库画进度条就没有分母。
	if updated, err := s.Store.GetRepository(ctx, id); err == nil {
		out = updated
	}
	s.audit(ctx, "", "repo.update", "repo:"+id, "", domain.AuditResultOK)
	return out, nil
}

// Delete 删除存储库：**先把记录删干净，再把慢的资源回收丢给后台任务**。
//
// 为什么必须拆开（真实反馈："删除存储库要等很久"）：
//
//	资源回收要逐块盘解除 LUN 映射 → 停用目标 → 删除目标 → 删除 VHDX 文件，全是秒级的
//	平台调用。原先的做法是"记录留到最后一并删除"，于是回收期间库里一直挂着
//	state=deleting 停在列表上，用户看到的就是长时间的「删除中」。
//
// 现在请求内只做三件快事：
//  1. 校验无活跃租约（有人在用就拒绝，见 409 repo.has_active_lease）；
//  2. 在一个事务里清掉库 / 分配 / 磁盘 / 目标 / 成员记录，并按 disks 表重算用户用量；
//  3. 把"盘路径 + 目标名"快照提交给回收任务（Finalize=artifacts）。
//
// 记录一删，界面立刻不再显示该库；后台任务只回收平台侧资源，且失败可以安全重试
// （记录已经不存在，重试没有任何副作用）。
//
// 已知取舍：回收完成前"库里已无记录、但 VHDX 仍在盘上"。这段时间用户的配额**已经**退回
// （按 disks 表重算），比"记录留着但空间迟迟不释放"更符合用户直觉。
func (s *RepoService) Delete(ctx context.Context, id string) error {
	repo, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	active, err := s.Store.CountActiveLeasesByRepo(ctx, id)
	if err != nil {
		return err
	}
	if active > 0 {
		return apperr.RepoHasActiveLease(active)
	}

	var payload reclaimPayload
	var userIDs []string
	err = s.Store.Tx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockRepository(ctx, id); err != nil {
			return err
		}
		disks, err := tx.ListDisksByRepo(ctx, id)
		if err != nil {
			return err
		}
		// 仍挂在客户端上的盘不能删：文件删不掉，事后还会留下一个"看不见的占用"。
		// 在删记录之前判定，用户拿到明确的 disk.busy（409），而不是等半天换来一个失败任务。
		for i := range disks {
			if disks[i].Mounted {
				return apperr.DiskBusy().WithArg("disk_id", disks[i].ID)
			}
		}

		// 目标只从**本库的磁盘**上取：全量目标列表里还有别的库的目标，无差别删除会误伤
		// （真实事故：删一个库导致另一个库的挂载失败，见 purgeRepository 的注释）。
		// 同时把"盘路径 + 目标名"记进快照 —— 记录删掉后就再也查不到了。
		for i := range disks {
			targets, tErr := tx.ListIscsiTargetsByDisk(ctx, disks[i].ID)
			if tErr != nil {
				return tErr
			}
			payload.Artifacts = append(payload.Artifacts, diskArtifact{
				DiskPath: disks[i].VHDXPath,
				Targets:  targetNamesOf(targets),
			})
			for j := range targets {
				if err := tx.DeleteIscsiTarget(ctx, targets[j].ID); err != nil && !isNotFound(err) {
					return err
				}
			}
		}

		allocations, err := tx.ListAllocationsByRepo(ctx, id)
		if err != nil {
			return err
		}
		// 用户 ID 要在这里收全：分配行马上就要删掉，删完就找不到"该给谁退用量"了。
		// 临时共享的占位分配（domain.TempAllocationUserID）不计入任何用户配额。
		seen := make(map[string]bool, len(allocations))
		for i := range allocations {
			u := allocations[i].UserID
			if u == "" || u == domain.TempAllocationUserID || seen[u] {
				continue
			}
			seen[u] = true
			userIDs = append(userIDs, u)
		}

		// ⚠️ 删除顺序受外键约束（SQLite 开了 foreign_keys=ON）：
		// allocations 引用 disks 与 repositories，disks 引用 repositories，
		// 所以只能是 分配 → 磁盘 → 成员 → 库；反过来会被外键拒绝。
		for i := range allocations {
			if err := tx.DeleteAllocation(ctx, allocations[i].ID); err != nil && !isNotFound(err) {
				return err
			}
		}
		for i := range disks {
			if err := tx.DeleteDisk(ctx, disks[i].ID); err != nil && !isNotFound(err) {
				return err
			}
		}
		if err := tx.ReplaceRepoMembers(ctx, id, nil); err != nil {
			return err
		}
		if err := tx.DeleteRepository(ctx, id); err != nil && !isNotFound(err) {
			return err
		}

		// 用户用量必须在这里就重算：库没了，之后不会再有"删某块盘 → 顺手重算用量"的
		// 时机，漏掉这一步用户的已用量就永久挂账。在事务内做还能读到本事务刚删掉的行，
		// 账目与记录的删除是同一个原子动作。重算失败不回滚删除（记录该删还是要删），只记日志。
		// 存储库自身的用量随记录一起消失，无需重算。
		for _, userID := range userIDs {
			if _, err := tx.RecomputeUserUsage(ctx, userID); err != nil {
				s.Log.Warn("重算用户用量失败", "user_id", userID, "error", err)
			}
		}

		// 回收任务与记录删除放在**同一个事务**：保证"记录删掉了 ⇒ 回收任务一定存在"。
		// 若拆成"先提交事务、再入队任务"，中间进程被杀就会留下永远没人回收的 VHDX 与
		// iSCSI 目标（DB 里连一条痕迹都没有）。这里刻意不走 Jobs.EnqueueWith ——
		// 那条路径绑定的是根连接池，不在本事务内。
		payload.RepoID = id
		payload.Finalize = reclaimFinalizeArtifacts
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, _, err := tx.CreateJob(ctx, &domain.Job{
			Type:    domain.JobReclaim,
			RefID:   id,
			IdemKey: "reclaim_repo:" + id,
			LockKey: lock.RepoKey(id),
			Payload: string(raw),
			State:   domain.JobStatePending,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.audit(ctx, "", "repo.delete", "repo:"+id, repo.Name, domain.AuditResultOK)
	s.Log.Info("已删除存储库记录，资源回收转入后台",
		"repo_id", id, "name", repo.Name, "disk_count", len(payload.Artifacts))
	return nil
}

// SetMembers 覆盖式设置可管理列表（仅 owner 或超级管理员可操作）。
func (s *RepoService) SetMembers(ctx context.Context, repoID, operatorID string, members []domain.RepoMember) error {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return err
	}
	op, err := s.Store.GetUserByID(ctx, operatorID)
	if err != nil {
		return err
	}
	if repo.OwnerID != operatorID && !op.IsSuperAdmin() {
		return apperr.AuthForbidden().WithArg("reason", "owner_only")
	}

	seen := make(map[string]bool, len(members))
	normalized := make([]domain.RepoMember, 0, len(members))
	for _, m := range members {
		if m.UserID == "" {
			return apperr.InvalidParam("members")
		}
		if !m.Perm.Valid() {
			return apperr.InvalidParam("perm")
		}
		if seen[m.UserID] {
			return apperr.InvalidParam("members")
		}
		seen[m.UserID] = true
		if _, err := s.Store.GetUserByID(ctx, m.UserID); err != nil {
			return err
		}
		normalized = append(normalized, domain.RepoMember{RepoID: repoID, UserID: m.UserID, Perm: m.Perm})
	}

	if err := s.Store.Tx(ctx, func(tx *store.Store) error {
		return tx.ReplaceRepoMembers(ctx, repoID, normalized)
	}); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "repo.members.set", "repo:"+repoID, "", domain.AuditResultOK)
	return nil
}

// ListMembers 列出存储库成员。
func (s *RepoService) ListMembers(ctx context.Context, repoID string) ([]domain.RepoMember, error) {
	if _, err := s.Store.GetRepository(ctx, repoID); err != nil {
		return nil, err
	}
	return s.Store.ListRepoMembers(ctx, repoID)
}

// CanAccess 判断用户对存储库的访问权与权限等级。
//
// super_admin 始终允许且权限为 manage；owner 视为 manage。
func (s *RepoService) CanAccess(ctx context.Context, repoID, userID string, role domain.Role) (bool, domain.Permission, error) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return false, "", err
	}
	if role == domain.RoleSuperAdmin {
		return true, domain.PermManage, nil
	}
	if repo.OwnerID == userID {
		return true, domain.PermManage, nil
	}
	m, err := s.Store.GetRepoMember(ctx, repoID, userID)
	if err != nil {
		return false, "", err
	}
	if m == nil {
		return false, "", nil
	}
	return true, m.Perm, nil
}

// Allocate 创建分配。共享模式创建差异盘（异步），独享模式复用已有单盘。
//
// 本方法只落库 + 提交任务，耗时操作全部交给 Job。
func (s *RepoService) Allocate(ctx context.Context, in AllocateInput) (*domain.Allocation, error) {
	if strings.TrimSpace(in.RepoID) == "" {
		return nil, apperr.InvalidParam("repo_id")
	}
	userID := strings.TrimSpace(in.UserID)
	if userID == "" {
		return nil, apperr.InvalidParam("user_id")
	}
	operatorID := in.OperatorID
	if operatorID == "" {
		operatorID = userID
	}

	var alloc *domain.Allocation
	var diffDiskID string
	var parentDiskID string
	// poolDiskID 非空表示这次分配用的是池中**已建好并已发布**的差异盘（池化库的常态路径）：
	// 不需要派发建盘任务，只需在事务外把该盘上预建的 iSCSI 目标绑到这次分配上。
	var poolDiskID string
	// staleAllocID/staleDiskID 记录"母盘版本已更新、必须作废重建"的旧分配与旧差异盘。
	var staleAllocID string
	var staleDiskID string

	unlock := s.Locks.Acquire(lock.RepoKey(in.RepoID))
	defer unlock()

	// 预取当前生效的存储守卫：既避免在写事务内读 storages 小表（SQLite 写事务期间的额外读
	// 可能触发 BUSY），也供"差异盘预取失败"时退化为第一个根使用。
	guards, err := s.storageGuards(ctx)
	if err != nil {
		return nil, err
	}

	// 差异盘选根：优先与母盘同根（同卷），父盘所在根空间不足时改选可用空间最大的根。
	// 在事务外预取：Pick 会触发一次卷空间查询（PowerShell），避免在写事务内做慢 IO。
	var diffGuard domain.PathGuard
	hasDiffGuard := false
	if repo0, rErr := s.Store.GetRepository(ctx, in.RepoID); rErr == nil && repo0.IsShared() && repo0.ParentDiskID != nil {
		if parent0, pErr := s.Store.GetDisk(ctx, *repo0.ParentDiskID); pErr == nil {
			g, pickErr := s.pickGuardForPath(ctx, parent0.VHDXPath, parent0.SizeBytes)
			if pickErr != nil {
				return nil, pickErr
			}
			diffGuard, hasDiffGuard = g, true
		}
	}

	err = s.Store.Tx(ctx, func(tx *store.Store) error {
		repo, err := tx.LockRepository(ctx, in.RepoID)
		if err != nil {
			return err
		}
		if repo.State == domain.RepoStateCreating {
			// 建库还没完成（母盘 / 池位盘 / iSCSI 目标还在建）：此刻分配只会拿到一块
			// 还没派生好的池位盘，用户点挂载又得等 —— 等建完再分配，体验才是"分完即挂"。
			return apperr.RepoCreating()
		}
		if repo.State != domain.RepoStateActive {
			return apperr.New("repo.state_invalid", 409).WithArg("state", string(repo.State))
		}

		op, err := tx.GetUserByID(ctx, operatorID)
		if err != nil {
			return err
		}
		target, err := tx.GetUserByID(ctx, userID)
		if err != nil {
			return err
		}
		if !target.Enabled {
			return apperr.New("user.disabled", 409).WithArg("user_id", userID)
		}
		if repo.OwnerID != operatorID && !op.IsSuperAdmin() {
			if m, mErr := tx.GetRepoMember(ctx, repo.ID, operatorID); mErr != nil {
				return mErr
			} else if m == nil || m.Perm != domain.PermManage {
				return apperr.AuthForbidden().WithArg("reason", "manage_required")
			}
		}

		// 幂等：同一用户在同一库已有**可复用**的分配时直接复用。
		//
		// 复用判据（按产品约定"母盘不共存多版本"）：
		//   ① 分配未释放、且不在删除中 —— releasing 表示差异盘正在被物理删除，
		//      复用会把挂载挂到一块正在消失的盘上（Publish 会卡在 awaitBackingReady）；
		//   ② 差异盘**仍基于当前母盘版本**（disk.parent_version == repo.parent_version）。
		//
		// 版本对不上（母盘更新过）→ 旧差异盘连同它的 iSCSI 目标一起作废（事务后入队删除），
		// 继续走下面的"重建差异盘"分支：用户不需要先手动释放再分配。
		//
		// 另注：released 目前是"永远不会被写入"的终止态（释放分配时直接删行），
		// 保留该判断是为将来引入软删除（meta/trash，见 implementation.md 5.10）。
		existing, err := tx.ListAllocationsByRepo(ctx, repo.ID)
		if err != nil {
			return err
		}
		for i := range existing {
			if existing[i].UserID != userID {
				continue
			}
			switch existing[i].State {
			case domain.AllocationStateReleased, domain.AllocationStateReleasing:
				continue
			}
			if !repo.IsShared() || existing[i].DiskID == "" {
				// 独享模式：复用母盘映射，没有差异盘，也就没有版本可比。
				alloc = &existing[i]
				return nil
			}
			oldDisk, diskErr := tx.GetDisk(ctx, existing[i].DiskID)
			if diskErr != nil || oldDisk.ParentVersion != repo.ParentVersion {
				// 版本对不上（或盘记录已读不到）：作废旧分配，下面重建。
				staleAllocID = existing[i].ID
				staleDiskID = existing[i].DiskID
				break
			}
			alloc = &existing[i]
			return nil
		}
		if staleAllocID != "" {
			// 只删分配行：差异盘与它的 iSCSI 目标交给删除任务（AllocationID 留空，
			// 避免任务回头再删一次已经不存在的分配）。
			if err := tx.DeleteAllocation(ctx, staleAllocID); err != nil {
				return err
			}
		}

		if !repo.IsShared() {
			// 独享模式：复用建库时的单盘，仅建立映射记录。
			if repo.ParentDiskID == nil {
				return apperr.New("repo.disk_missing", 500)
			}
			a := &domain.Allocation{
				RepoID: repo.ID,
				DiskID: *repo.ParentDiskID,
				UserID: userID,
				State:  domain.AllocationStateAllocated,
			}
			if err := tx.CreateAllocation(ctx, a); err != nil {
				return err
			}
			alloc = a
			return nil
		}

		// 共享模式：先用池里的差异盘，池里没有才派生。
		if repo.ParentDiskID == nil {
			return apperr.New("repo.disk_missing", 500)
		}
		parent, err := tx.GetDisk(ctx, *repo.ParentDiskID)
		if err != nil {
			return err
		}

		// 池化库的常态路径：取一块**建库时就派生好、并已发布**的空闲池位盘。
		//
		// 这是"共享数量"真正的收益点：分配在这里只写几行记录，
		// 用户点挂载时既不用等派生差异盘（JobCreateDiff），也不用等目标的首次
		// 全量下发（Windows 上一次 pushTarget 实测约 15 秒）。
		if repo.Meta.Pool {
			idle, pickErr := tx.PickIdleDiffDisk(ctx, repo.ID)
			if pickErr != nil && !isNotFound(pickErr) {
				return pickErr
			}
			// 版本必须与母盘一致：母盘做过维护（版本 +1）后，旧版本的池位盘不能复用。
			if idle != nil && idle.ParentVersion == repo.ParentVersion {
				need := parent.SizeBytes
				if err := checkQuota(repo.QuotaBytes, repo.UsedBytes, need); err != nil {
					return err
				}
				if err := checkQuota(target.QuotaBytes, target.UsedBytes, need); err != nil {
					return err
				}
				a := &domain.Allocation{
					ID:     uuid.NewString(),
					RepoID: repo.ID,
					DiskID: idle.ID,
					UserID: userID,
					State:  domain.AllocationStateAllocated,
				}
				if err := tx.CreateAllocation(ctx, a); err != nil {
					return err
				}
				// 用量预留口径与派生路径完全一致（按母盘容量预留，随后由对账按实际占用校正）。
				if _, err := tx.AddRepoUsedBytes(ctx, repo.ID, need); err != nil {
					return err
				}
				if _, err := tx.AddUserUsedBytes(ctx, userID, need); err != nil {
					return err
				}
				alloc = a
				poolDiskID = idle.ID
				return nil
			}
		}

		diffCount, err := tx.CountDiffDisks(ctx, parent.ID)
		if err != nil {
			return err
		}
		// 正在作废的旧差异盘不计入上限：否则"差异盘已满"的库将无法重建（重建=先作废旧的再建新的）。
		if staleDiskID != "" && diffCount > 0 {
			diffCount--
		}
		if repo.Meta.Pool {
			// 池化库的上限判据只用计数：它的母盘在预创建时就已经置为 derived，
			// 再让 CanDerive 校验"母盘是否 idle"只会把"池已满"报成状态机违规。
			//
			// 走到这里说明池位已全部被占用（或版本作废），也就是"共享数量"用满了。
			if repo.MaxDiffDisks > 0 && diffCount >= repo.MaxDiffDisks {
				return apperr.RepoDiffLimitExceeded(repo.MaxDiffDisks)
			}
		} else if err := domain.CanDerive(repo.Condition(), diffCount, repo.MaxDiffDisks); err != nil {
			return err
		}

		// 配额：母盘容量按逻辑大小预留，任务完成后按实际物理占用校正。
		need := parent.SizeBytes
		if err := checkQuota(repo.QuotaBytes, repo.UsedBytes, need); err != nil {
			return err
		}
		if err := checkQuota(target.QuotaBytes, target.UsedBytes, need); err != nil {
			return err
		}

		// 分配 ID 由本层生成：差异盘路径包含分配 ID，需先于磁盘记录确定。
		allocID := uuid.NewString()
		rel := filepath.Join(s.raw().Storage.DisksDir, "diffs", repo.ID, allocID+".vhdx")
		guard := diffGuard
		if !hasDiffGuard {
			// 预取失败（极少见）时退化为第一个根，保持"不阻塞分配"的既有行为。
			g, ok := guards.First()
			if !ok {
				return apperr.InvalidParam("path")
			}
			guard = g
		}
		abs, err := s.Disk.DiskRef(guard.Root, rel)
		if err != nil {
			return apperr.InvalidParam("path")
		}

		d := &domain.Disk{
			RepoID:        repo.ID,
			Kind:          domain.DiskKindDiff,
			VHDXPath:      abs,
			ParentID:      &parent.ID,
			ParentVersion: repo.ParentVersion,
			SizeBytes:     parent.SizeBytes,
			VHDType:       domain.VHDTypeDifferencing,
			State:         domain.DiskStateCreating,
		}
		if err := tx.CreateDisk(ctx, d); err != nil {
			return err
		}
		a := &domain.Allocation{
			ID:     allocID,
			RepoID: repo.ID,
			DiskID: d.ID,
			UserID: userID,
			State:  domain.AllocationStateAllocated,
		}
		if err := tx.CreateAllocation(ctx, a); err != nil {
			return err
		}

		// 预留用量，防止并发分配超卖。
		if _, err := tx.AddRepoUsedBytes(ctx, repo.ID, need); err != nil {
			return err
		}
		if _, err := tx.AddUserUsedBytes(ctx, userID, need); err != nil {
			return err
		}

		// 第一个差异盘 → 母盘进入 derived（状态迁移必须走状态机）。
		if diffCount == 0 && repo.Condition() == domain.ParentIdle {
			if err := (domain.TransitionRequest{From: repo.Condition(), To: domain.ParentDerived}).Validate(); err != nil {
				return err
			}
			if err := tx.SetParentCondition(ctx, repo.ID, domain.ParentDerived, false); err != nil {
				return err
			}
		}

		alloc = a
		diffDiskID = d.ID
		parentDiskID = parent.ID
		return nil
	})
	if err != nil {
		return nil, err
	}

	if poolDiskID != "" {
		// 把池位盘上预建的 iSCSI 目标认领给这次分配。
		//
		// 只改一行关联，完全不碰平台侧：目标与映射在建库阶段就下发好了，
		// 这正是"分配完立刻能挂"的来源。
		//
		// 失败也不回滚分配：盘已经绑好，挂载路径（Publish）还会再认领一次，用户无感 ——
		// 为一次可自愈的绑定失败去回滚已提交的分配，反而会留下"盘占了但没分配"的脏状态。
		if err := s.bindPoolTarget(ctx, poolDiskID, alloc.ID); err != nil {
			s.Log.Warn("绑定池位盘的 iSCSI 目标失败（挂载时会再次认领）",
				"repo_id", alloc.RepoID, "disk_id", poolDiskID,
				"allocation_id", alloc.ID, "error", err)
		}
	}

	if diffDiskID != "" {
		payload := diffPayload{DiskID: diffDiskID, RepoID: alloc.RepoID, ParentDiskID: parentDiskID}
		// 锁用 **RepoKey** 而不是 DiskKey：派生差异盘要独占打开母盘 VHDX，而同一个库的
		// 所有差异盘共用这一个母盘文件，回收存储库的任务（同样持 RepoKey）也在删它。
		// 用 DiskKey 的话，并发的两次派生、以及"回收"和"派生"之间都互不排斥，
		// 必然撞出 platform.sharing_violation（真实工单：后台回收时建盘全挂，
		// "The process cannot access the file because it is being used by another process"）。
		if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobCreateDiff, diffDiskID, "create_diff:"+diffDiskID, lock.RepoKey(alloc.RepoID), payload); err != nil {
			return alloc, err
		}
	}

	if staleDiskID != "" {
		// 作废旧差异盘（连带删除它的 iSCSI 目标）：这是"母盘版本对不上才释放"的唯一路径。
		// 配额上，新盘已按母盘容量预留，旧占用要等删除任务完成后才退回，中间短暂多占一份；
		// 对账（reclaim/reconcile）随后会按实际占用校正。
		stalePayload := deleteDiskPayload{DiskID: staleDiskID}
		if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobDeleteDisk, staleDiskID,
			"delete_disk:"+staleDiskID, lock.DiskKey(staleDiskID), stalePayload); err != nil {
			return alloc, err
		}
		s.audit(ctx, operatorID, "repo.rebuild_diff", "repo:"+alloc.RepoID,
			"user="+userID+" stale_disk="+staleDiskID+" new_allocation="+alloc.ID, domain.AuditResultOK)
	}

	s.audit(ctx, operatorID, "repo.allocate", "repo:"+alloc.RepoID, "user="+userID+" allocation="+alloc.ID, domain.AuditResultOK)
	return alloc, nil
}

// ListAllocations 列出存储库的全部分配。
func (s *RepoService) ListAllocations(ctx context.Context, repoID string) ([]domain.Allocation, error) {
	return s.Store.ListAllocationsByRepo(ctx, repoID)
}

// bindPoolTarget 把池位盘上预建好的 user 目标绑到这次分配上。
//
// 盘上没有目标时静默返回（预创建时 iSCSI 后端未装配、或目标阶段失败）：
// 挂载路径会按分配新建一个，功能不受影响 —— 只是那一次挂载会比较慢。
func (s *RepoService) bindPoolTarget(ctx context.Context, diskID, allocationID string) error {
	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, diskID)
	if err != nil {
		return err
	}
	for i := range targets {
		t := &targets[i]
		if t.Purpose != domain.PurposeUser {
			continue
		}
		if t.AllocationID != nil && *t.AllocationID == allocationID {
			return nil
		}
		return s.Store.SetIscsiTargetAllocation(ctx, t.ID, allocationID)
	}
	return nil
}

// setRepoPrepare 写入建库预创建的进度快照（meta.prepare），供卡片显示"创建到第几步"。
//
// 进度是**展示用**的易变数据，写失败不返回错误：任务真正的结果（磁盘的物理状态 +
// 库的 state）才是真源，为了刷新一个进度数字让整个建库任务失败是本末倒置。
func (s *RepoService) setRepoPrepare(ctx context.Context, repoID string, p *domain.RepoPrepare) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return
	}
	if p == nil {
		repo.Meta.Prepare = nil
	} else {
		cp := *p
		repo.Meta.Prepare = &cp
	}
	if err := s.Store.UpdateRepository(ctx, repo); err != nil {
		s.Log.Warn("写入建库进度失败", "repo_id", repoID, "error", err)
	}
}

// FinishRepoPrepare 把预创建完成的存储库置为 active，并清掉进度快照。
//
// 清掉而不是留一个 100%：建完之后卡片该显示正常的库状态，
// 而不是长期挂着一句"上次创建走到第 3/5 步"的历史痕迹。
func (s *RepoService) FinishRepoPrepare(ctx context.Context, repoID string) error {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return err
	}
	repo.Meta.Prepare = nil
	repo.State = domain.RepoStateActive
	return s.Store.UpdateRepository(ctx, repo)
}

// MarkRepoPrepareFailed 把预创建失败的存储库置为 error，留下可本地化的失败码。
//
// 必须落库：任务失败后不会有人再来改库状态，库就永远停在 creating ——
// 前端一直显示"创建中"，用户既分配不了也挂载不了，还看不出卡在哪一步。
func (s *RepoService) MarkRepoPrepareFailed(ctx context.Context, repoID string, cause error) error {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.State != domain.RepoStateCreating {
		// 已经建成 active（或已被删除）：不能用一次迟到的失败回调覆盖它。
		return nil
	}
	if repo.Meta.Prepare == nil {
		repo.Meta.Prepare = &domain.RepoPrepare{}
	}
	// 只落**错误码**，由前端按当前语言渲染文案：底层原始报错可能带服务端路径与命令原文，
	// 不能进前端；而服务端也不该替用户猜语言（有中英日韩四套文案）。
	repo.Meta.Prepare.Error = apperr.CodeOf(cause)
	repo.State = domain.RepoStateError
	return s.Store.UpdateRepository(ctx, repo)
}

// ReleaseAllocation 释放分配：校验无活跃租约后异步删除差异盘与分配记录。
func (s *RepoService) ReleaseAllocation(ctx context.Context, allocationID string) error {
	alloc, err := s.Store.GetAllocation(ctx, allocationID)
	if err != nil {
		return err
	}

	leases, err := s.Store.ListActiveLeasesByRepo(ctx, alloc.RepoID)
	if err != nil {
		return err
	}
	for _, l := range leases {
		if l.AllocationID == allocationID {
			return apperr.RepoHasActiveLease(1)
		}
	}

	if err := s.Store.UpdateAllocationState(ctx, allocationID, domain.AllocationStateReleasing); err != nil {
		return err
	}

	// 池化库：回池，而不是删盘。
	//
	// 为什么必须回池（见 5.14）：共享数量就是"随时可用的差异盘数"。释放即删的话
	// 池子越用越小，下次分配又回到"分完还要等建盘"，这个特性就白做了。
	//
	// 但池位盘里还留着上一个用户的数据，不能直接交给下一个用户 —— 回池必须
	// **原地重建**（删文件、重新派生），见 DiskService.runResetDiff。
	if repo, rErr := s.Store.GetRepository(ctx, alloc.RepoID); rErr == nil && repo.Meta.Pool && alloc.DiskID != "" {
		payload := resetDiffPayload{
			DiskID:       alloc.DiskID,
			RepoID:       alloc.RepoID,
			ParentDiskID: parentIDOf(repo),
			AllocationID: allocationID,
		}
		// 立刻把盘挡在分配之外：它马上就要被重建，而在此之前盘里还躺着上一个用户的
		// 数据、平台侧映射也已经被拆掉 —— 这时候被分配出去就是数据泄漏 + 挂载即坏。
		//
		// 只置状态、不删记录：池位盘的记录是"池的一格"，重建后会复用同一块盘
		// （同一个目标名与 CHAP 凭据，挂载因此不需要重新下发）。
		switch d, dErr := s.Store.GetDisk(ctx, alloc.DiskID); {
		case dErr == nil:
			d.State = domain.DiskStateCreating
			if err := s.Store.UpdateDisk(ctx, d); err != nil {
				return err
			}
		case !isNotFound(dErr):
			return dErr
		}

		// 幂等键带 allocationID 而不是 diskID：同一块池位盘会被反复分配/释放，
		// 用 diskID 作键的话第二次释放会命中第一次留下的历史任务、被"幂等"掉
		// （表现为：分配释放都成功，但盘里的旧数据没被清掉）。
		if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobResetDiff, alloc.DiskID,
			"reset_diff:"+allocationID, lock.RepoKey(alloc.RepoID), payload); err != nil {
			return err
		}
		s.audit(ctx, "", "repo.release_allocation", "allocation:"+allocationID,
			"pool=reset disk="+alloc.DiskID, domain.AuditResultOK)
		return nil
	}

	payload := deleteDiskPayload{DiskID: alloc.DiskID, AllocationID: alloc.ID}
	if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobDeleteDisk, alloc.DiskID, "delete_disk:"+alloc.DiskID, lock.DiskKey(alloc.DiskID), payload); err != nil {
		return err
	}
	s.audit(ctx, "", "repo.release_allocation", "allocation:"+allocationID, "", domain.AuditResultOK)
	return nil
}

// TransitionParent 执行母盘用途迁移。
//
// 所有迁移都必须经 domain.TransitionRequest.Validate 判定，
// 再调用 SetParentCondition（完成维护时 bumpVersion=true）。
func (s *RepoService) TransitionParent(ctx context.Context, repoID string, action ParentAction, operatorID string) error {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return err
	}
	if !repo.IsShared() {
		return apperr.New("repo.not_shared", 409).WithArg("mode", string(repo.Mode))
	}

	var to domain.ParentCondition
	switch action {
	case ParentActionDerive:
		to = domain.ParentDerived
	case ParentActionTempShare:
		to = domain.ParentTempShared
	case ParentActionUnshare, ParentActionFinishMaint, ParentActionCleanup:
		to = domain.ParentIdle
	case ParentActionMaintenance:
		to = domain.ParentMaintenance
	default:
		return apperr.InvalidParam("action")
	}

	// 动作级前置条件：状态机允许 derived→idle（那是"清理差异盘"的路径），
	// 因此"撤销共享 / 完成维护 / 清理差异盘"必须各自校验当前条件，不能只依赖 Validate。
	current := repo.Condition()
	switch action {
	case ParentActionUnshare:
		if current != domain.ParentTempShared {
			return apperr.RepoParentConditionViolation(string(current), string(domain.ParentTempShared))
		}
	case ParentActionFinishMaint:
		if current != domain.ParentMaintenance {
			return apperr.RepoParentConditionViolation(string(current), string(domain.ParentMaintenance))
		}
	case ParentActionCleanup:
		if current != domain.ParentDerived {
			return apperr.RepoParentConditionViolation(string(current), string(domain.ParentDerived))
		}
	}

	diffCount := 0
	if repo.ParentDiskID != nil {
		if n, err := s.Store.CountDiffDisks(ctx, *repo.ParentDiskID); err == nil {
			diffCount = n
		}
	}
	active, err := s.Store.CountActiveLeasesByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	req := domain.TransitionRequest{From: repo.Condition(), To: to, DiffCount: diffCount, ActiveLeases: active}
	if err := req.Validate(); err != nil {
		return err
	}

	switch action {
	case ParentActionTempShare:
		if err := s.startTempShare(ctx, repo, operatorID); err != nil {
			return err
		}
	case ParentActionUnshare:
		if err := s.stopTempShare(ctx, repo, operatorID); err != nil {
			return err
		}
	case ParentActionCleanup:
		// 状态保持 derived 直到全部子盘删除成功（见 5.4「中途失败」）。
		payload := reclaimPayload{RepoID: repo.ID, ParentDiskID: deref(repo.ParentDiskID), Finalize: reclaimFinalizeParentIdle}
		if repo.ParentDiskID != nil {
			disks, dErr := s.Store.ListDiffDisksByParent(ctx, *repo.ParentDiskID)
			if dErr != nil {
				return dErr
			}
			for _, d := range disks {
				payload.DiskIDs = append(payload.DiskIDs, d.ID)
			}
		}
		if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobReclaim, repo.ID, "reclaim_diffs:"+repo.ID, lock.RepoKey(repo.ID), payload); err != nil {
			return err
		}
	case ParentActionFinishMaint:
		if err := s.Store.SetParentCondition(ctx, repo.ID, domain.ParentIdle, true); err != nil {
			return err
		}
		if err := s.refreshParentFingerprint(ctx, repo); err != nil {
			s.Log.Warn("重算母盘指纹失败", "repo_id", repo.ID, "error", err)
		}
	default:
		if err := s.Store.SetParentCondition(ctx, repo.ID, to, false); err != nil {
			return err
		}
	}

	s.audit(ctx, operatorID, "repo.parent."+string(action), "repo:"+repoID,
		string(repo.Condition())+" -> "+string(to), domain.AuditResultOK)
	s.emit("repo", map[string]any{
		"action":           string(action),
		"repo_id":          repoID,
		"parent_condition": string(to),
		"operator_id":      operatorID,
	})
	return nil
}

// startTempShare 创建临时共享的占位分配与目标记录，并提交发布任务。
func (s *RepoService) startTempShare(ctx context.Context, repo *domain.Repository, operatorID string) error {
	if repo.ParentDiskID == nil {
		return apperr.New("repo.disk_missing", 500)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}

	name := tempTargetName(repo.ID)
	existing, err := s.Store.GetIscsiTargetByName(ctx, name)
	if err != nil && !isNotFound(err) {
		return err
	}
	if existing != nil {
		// 已存在（上次未收敛）：直接置回 temp_shared 并重新下发。
		return s.Store.SetParentCondition(ctx, repo.ID, domain.ParentTempShared, false)
	}

	alloc := &domain.Allocation{
		RepoID: repo.ID,
		DiskID: parent.ID,
		UserID: domain.TempAllocationUserID,
		State:  domain.AllocationStateAllocated,
	}
	if err := s.Store.CreateAllocation(ctx, alloc); err != nil {
		return err
	}

	diskID := parent.ID
	allocID := alloc.ID
	target := &domain.IscsiTarget{
		TargetName:     name,
		DiskID:         &diskID,
		Purpose:        domain.PurposeTempParent,
		AllocationID:   &allocID,
		AuthMode:       domain.AuthModeNone,
		Enabled:        false,
		DesiredEnabled: true,
		ReadOnly:       true,
	}
	if err := s.Store.CreateIscsiTarget(ctx, target); err != nil {
		return err
	}
	if err := s.Store.SetParentCondition(ctx, repo.ID, domain.ParentTempShared, false); err != nil {
		return err
	}

	payload := publishPayload{AllocationID: alloc.ID}
	if _, _, err := s.Jobs.EnqueueWith(ctx, domain.JobPublish, alloc.ID, "publish:"+alloc.ID, lock.TargetKey(name), payload); err != nil {
		return err
	}
	s.Log.Info("已发起母盘临时共享", "repo_id", repo.ID, "target", name, "operator", operatorID)
	return nil
}

// stopTempShare 撤销临时共享：移除映射、停用并删除目标，清理占位记录。
//
// 全部动作幂等（目标不存在视为成功，见 5.6）。
func (s *RepoService) stopTempShare(ctx context.Context, repo *domain.Repository, operatorID string) error {
	name := tempTargetName(repo.ID)
	target, err := s.Store.GetIscsiTargetByName(ctx, name)
	if err != nil && !isNotFound(err) {
		return err
	}

	if target != nil {
		var diskPath string
		if target.DiskID != nil {
			if d, dErr := s.Store.GetDisk(ctx, *target.DiskID); dErr == nil {
				diskPath = d.VHDXPath
			}
		}
		// 目标拆除走唯一实现（见 IscsiService.teardownTargetLocked）：临时共享的目标名
		// 同样是**短名**（tempTargetName），直接拿它去问平台必然 not_found —— 原来
		// "解映射 + 停用 + 删除"三步全是空转，目标一直挂在平台上。没有磁盘路径时只拆
		// 目标（映射随目标一起消失）。
		if s.IscsiSvc != nil {
			s.IscsiSvc.TeardownTarget(ctx, target, TeardownOptions{DetachDiskPath: diskPath})
		} else if s.Iscsi != nil {
			s.Log.Error("未装配 iSCSI 服务，跳过临时共享目标拆除", "target", name)
		}
		if target.AllocationID != nil {
			if err := deleteAllocationIfExists(ctx, s.Store, *target.AllocationID); err != nil {
				s.Log.Warn("清理临时分配记录失败", "allocation_id", *target.AllocationID, "error", err)
			}
		}
		if err := s.Store.DeleteIscsiTarget(ctx, target.ID); err != nil {
			return err
		}
	}

	if err := s.Store.SetParentCondition(ctx, repo.ID, domain.ParentIdle, false); err != nil {
		return err
	}
	s.Log.Info("已撤销母盘临时共享", "repo_id", repo.ID, "operator", operatorID)
	return nil
}

// refreshParentFingerprint 重算并写回母盘内容指纹。
func (s *RepoService) refreshParentFingerprint(ctx context.Context, repo *domain.Repository) error {
	if repo.ParentDiskID == nil || s.Disk == nil {
		return nil
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}
	if !s.Disk.Exists(parent.VHDXPath) {
		return nil
	}
	fp, err := s.Disk.Fingerprint(parent.VHDXPath)
	if err != nil {
		return err
	}
	if size, err := s.Disk.PhysicalSize(parent.VHDXPath); err == nil {
		parent.SizeBytes = size
	}
	parent.ContentFingerprint = fp
	return s.Store.UpdateDisk(ctx, parent)
}

// ReconcileTempShares 服务端重启后收敛临时共享（见 5.6 步骤 ④）。
func (s *RepoService) ReconcileTempShares(ctx context.Context) error {
	repos, err := s.Store.ListRepositories(ctx, "", 500, 0)
	if err != nil {
		return err
	}
	for i := range repos {
		repo := &repos[i]
		if repo.Condition() != domain.ParentTempShared {
			continue
		}
		name := tempTargetName(repo.ID)
		// 平台未装配 iSCSI 后端时（例如单元测试）无从查询实际目标，
		// 视作"服务端不存在该目标"：仅依据租约数决定是否收敛，避免 nil 解引用 panic。
		var target *platform.TargetInfo
		if s.Iscsi != nil {
			if t, tErr := s.Iscsi.GetTarget(ctx, name); tErr == nil {
				target = t
			}
		}
		active, lErr := s.Store.CountActiveLeasesByRepo(ctx, repo.ID)
		if lErr != nil {
			return lErr
		}

		if target != nil && active > 0 {
			// 仍有人连接：保持现状。
			continue
		}

		if err := s.stopTempShare(ctx, repo, "system"); err != nil {
			s.Log.Error("收敛临时共享失败", "repo_id", repo.ID, "error", err)
			continue
		}
		s.audit(ctx, "system", "repo.temp_share.auto_reaped", "repo:"+repo.ID,
			"target_exists="+boolText(target != nil), domain.AuditResultOK)
	}
	return nil
}

// pickGuardForPath 选择用于新建磁盘的根。
//
// preferPath 非空时优先让它所属的根（用于"差异盘优先与母盘同根"）；
// 若该根空间/水位不满足，再退回 Pick（可用空间最大的根）并记录日志。
func (s *RepoService) pickGuardForPath(ctx context.Context, preferPath string, need int64) (domain.PathGuard, error) {
	guards, err := s.storageGuards(ctx)
	if err != nil {
		return domain.PathGuard{}, err
	}
	if strings.TrimSpace(preferPath) != "" {
		if g, ok := guards.GuardForPath(preferPath); ok {
			if err := s.ensureVolumeFreeAt(ctx, g.Root, need); err == nil {
				return g, nil
			} else {
				s.Log.Warn("首选根不可用，改选其它根",
					"prefer_root", g.Root, "prefer_path", preferPath, "need_bytes", need, "error", err)
			}
		}
	}
	return guards.Pick(ctx, s.volumeProvider(), need)
}

// pickCreateStorage 为新建存储库选择存储与根。
//
// storageID 非空时要求该存储存在且已启用；为空时在所有启用存储中自动选根。
// 返回的 *domain.Storage 为选中的存储实体；DB 无存储记录而回退配置种子根时为 nil。
func (s *RepoService) pickCreateStorage(ctx context.Context, storageID string, need int64) (domain.PathGuard, *domain.Storage, error) {
	if storageID != "" {
		st, err := s.Store.GetStorage(ctx, storageID)
		if err != nil {
			return domain.PathGuard{}, nil, err
		}
		if !st.Enabled {
			return domain.PathGuard{}, nil, apperr.StorageDisabled().WithArg("id", st.ID)
		}
		return domain.NewPathGuard(st.Path), st, nil
	}

	roots, storages, err := s.storageSelection(ctx)
	if err != nil {
		return domain.PathGuard{}, nil, err
	}
	guard, err := domain.NewPathGuardSet(roots).Pick(ctx, s.volumeProvider(), need)
	if err != nil {
		return domain.PathGuard{}, nil, err
	}
	for i := range storages {
		if strings.EqualFold(storages[i].Path, guard.Root) {
			return guard, &storages[i], nil
		}
	}
	return guard, nil, nil
}

// storageIDOf / storageNameOf 安全读取（可能为 nil 的）存储标识，仅用于日志。
func storageIDOf(st *domain.Storage) string {
	if st == nil {
		return ""
	}
	return st.ID
}

func storageNameOf(st *domain.Storage) string {
	if st == nil {
		return ""
	}
	return st.Name
}

// ensureVolumeFreeAt 校验指定根所在卷的水位与剩余空间。
//
// 注意：这里是**物理水位**校验，与逻辑配额 checkQuota 是两套东西，不要混用。
func (s *RepoService) ensureVolumeFreeAt(ctx context.Context, root string, need int64) error {
	raw := s.raw()
	if s.Vol == nil || strings.TrimSpace(root) == "" {
		return nil
	}
	space, err := s.Vol.SpaceUsageOf(ctx, root)
	if err != nil {
		return err
	}
	free, total := space.FreeBytes, space.TotalBytes
	if total > 0 && raw.Storage.MinVolumeFreePermille > 0 {
		permille := free * 1000 / total
		if permille < raw.Storage.MinVolumeFreePermille {
			return apperr.New("storage.low_free_space", 507).
				WithArg("free_permille", permille).
				WithArg("min_permille", raw.Storage.MinVolumeFreePermille)
		}
	}
	if free < need {
		return apperr.New("storage.insufficient_space", 507).
			WithArg("free_bytes", free).
			WithArg("need_bytes", need)
	}
	return nil
}

// checkQuota 校验配额；quota <= 0 表示不限。
func checkQuota(quota, used, need int64) error {
	if quota <= 0 {
		return nil
	}
	if used+need > quota {
		return apperr.RepoQuotaExceeded(quota, used, need)
	}
	return nil
}

// dirSize 统计目录下常规文件的字节总数。
func dirSize(root string) (int64, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, apperr.InvalidParam("source_dir").WithCause(err)
	}
	if !info.IsDir() {
		return 0, apperr.InvalidParam("source_dir")
	}
	var total int64
	walkErr := filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, apperr.InvalidParam("source_dir").WithCause(walkErr)
	}
	return total, nil
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
