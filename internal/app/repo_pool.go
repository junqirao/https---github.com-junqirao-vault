package app

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
)

// 建库预创建（JobPrepareRepo）与释放回池（JobResetDiff）—— 这两件事共同支撑"共享数量"。
//
// 建库时按共享数量把差异盘池一次性建好并发布，之后分配只是把池里的一块盘绑给用户：
// 用户点挂载时不必再等"派生差异盘 + 首次下发 iSCSI 目标"（Windows 上一次全量下发
// 实测约 15 秒，是挂载慢的主因）。
//
// 释放时把这块盘**原地重建**回池（而不是删掉）：池子始终有"共享数量"块随取随用的盘，
// 否则池子越用越小，下次分配又回到需要等待的老路径。重建是必须的 —— 盘里还留着
// 上一个用户的数据，直接复用会把它泄给下一个用户。

// MarkPrepareFailedByJob 从 JobPrepareRepo 的 payload 里取出库 ID 并回滚库状态。
//
// 放在 app 层而不是装配方（cmd）：payload 的字段结构属于这一层，
// 让 main 里再去解析一遍 JSON 只会把两处结构绑死。
func (s *RepoService) MarkPrepareFailedByJob(ctx context.Context, payload string, cause error) error {
	var p prepareRepoPayload
	if err := decodePayload(payload, &p); err != nil || p.RepoID == "" {
		return nil
	}
	return s.MarkRepoPrepareFailed(ctx, p.RepoID, cause)
}

// runPrepareRepo 是 JobPrepareRepo 的实现：
// 母盘 → 共享数量个池位盘 → 同数量的 iSCSI 目标。
//
// 每个阶段都把进度写进 repositories.meta.prepare（而不只是任务里的 progress 数字）：
// 卡片要显示的是"正在派生差异盘 3/5"这种人话步骤，而且任务重试后进度数字会来回跳。
//
// 全程幂等：重试时已建好的母盘、已派生好的池位盘都会被复用，不会重建（重建会抹掉数据）。
func (s *DiskService) runPrepareRepo(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p prepareRepoPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}

	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			s.Log.Info("存储库记录已不存在，跳过预创建", "repo_id", p.RepoID)
			return nil
		}
		return err
	}
	// 幂等出口：已经建完（active）说明之前成功过，重复入队 / 重试时什么都不用做。
	if repo.State == domain.RepoStateActive {
		return nil
	}
	if repo.State != domain.RepoStateCreating {
		// 已被删除或置为其它状态：不要继续往里建盘，否则会留下无人认领的池位盘。
		s.Log.Info("存储库不在创建中，跳过预创建", "repo_id", repo.ID, "state", repo.State)
		return nil
	}
	parent, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		return err
	}
	if s.Disk == nil || s.Vol == nil {
		return errNotImplemented
	}

	count := p.Count
	if count <= 0 {
		count = repo.MaxDiffDisks
	}
	if count <= 0 {
		count = 1
	}

	// ---- 阶段一：母盘 ----
	s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
		Phase: domain.RepoPhaseParent, Done: 0, Total: 1,
	})
	rep.Progress(2)
	// 母盘自己的进度（0-100）压进建库总进度的 2%-38% 区间，
	// 否则它内部一次 Progress(100) 就把总进度顶满了。
	if err := s.buildVHDXFromSource(ctx, parent, p.SourceDir,
		job.ReporterFunc(func(v int) { rep.Progress(2 + 36*v/100) })); err != nil {
		return err
	}
	s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
		Phase: domain.RepoPhaseParent, Done: 1, Total: 1,
	})
	rep.Progress(38)

	// ---- 阶段二：池位盘（先全部派生好）----
	//
	// 逐块串行：每块差异盘都要**独占打开母盘 VHDX**，并发派生只会互相抢锁失败。
	s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
		Phase: domain.RepoPhaseDiffs, Done: 0, Total: count,
	})
	disks := make([]*domain.Disk, 0, count)
	for i := 0; i < count; i++ {
		idx := i
		d, err := s.ensurePoolDiffDisk(ctx, repo, parent, idx,
			job.ReporterFunc(func(v int) { rep.Progress(38 + (36*idx+36*v/100)/count) }))
		if err != nil {
			return err
		}
		disks = append(disks, d)
		s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
			Phase: domain.RepoPhaseDiffs, Done: idx + 1, Total: count,
		})
		rep.Progress(38 + 36*(idx+1)/count)
	}

	// 池位盘已经存在 → 母盘进入 derived。
	//
	// 这里不走 domain.CanDerive：它的判据是"母盘还处于 idle"，而批量派生是服务端
	// 自己的建库动作（不是用户触发的派生），语义上不需要那道用户侧校验。
	if repo.Condition() == domain.ParentIdle {
		if err := s.Store.SetParentCondition(ctx, repo.ID, domain.ParentDerived, false); err != nil {
			return err
		}
	}

	// ---- 阶段三：iSCSI 目标 ----
	//
	// 逐个建好并**启用**。这是整条链路最慢的一步（Windows 上一次下发约 15 秒），
	// 放在这里做完，用户分配到手的盘就是"连上即可用"。
	s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
		Phase: domain.RepoPhaseTargets, Done: 0, Total: len(disks),
	})
	if s.IscsiSvc == nil {
		// 平台没装配 iSCSI 后端（Linux/测试环境）：池位只建盘，发布留给挂载路径。
		s.Log.Warn("iSCSI 服务未装配，池位盘只建盘不做发布", "repo_id", repo.ID)
	} else {
		for i, d := range disks {
			if err := s.IscsiSvc.EnsurePoolTarget(ctx, repo, d); err != nil {
				return err
			}
			s.Repos.setRepoPrepare(ctx, repo.ID, &domain.RepoPrepare{
				Phase: domain.RepoPhaseTargets, Done: i + 1, Total: len(disks),
			})
			rep.Progress(74 + 26*(i+1)/len(disks))
		}
	}

	// ---- 完成：库进入 active，进度快照清掉 ----
	if err := s.Repos.FinishRepoPrepare(ctx, repo.ID); err != nil {
		return err
	}
	rep.Progress(100)
	s.audit(ctx, "", "repo.prepare.done", "repo:"+repo.ID,
		fmt.Sprintf("pool=%d", len(disks)), domain.AuditResultOK)
	return nil
}

// ensurePoolDiffDisk 保证"第 index 个池位盘"存在且已派生好（幂等），并返回它。
//
// 复用而不是重建：任务重试、以及进程重启后 running → pending 的重跑都会走到这里，
// 重建会把已经建好（甚至已经被分配出去）的盘抹掉。
//
// index 按 created_at 顺序定位已有的池位盘，因此"上次建到第 3 块就挂了"也能正确续建，
// 而不会从头再来一遍。
func (s *DiskService) ensurePoolDiffDisk(ctx context.Context, repo *domain.Repository,
	parent *domain.Disk, index int, rep job.Reporter) (*domain.Disk, error) {
	existing, err := s.Store.ListDiffDisksByParent(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	if index < len(existing) {
		d := existing[index]
		return &d, s.ensureDiffDisk(ctx, &d, parent, repo, rep)
	}

	guard, err := s.Repos.pickGuardForPath(ctx, parent.VHDXPath, parent.SizeBytes)
	if err != nil {
		return nil, err
	}
	// 路径用独立随机名（而不是 disk.ID）：盘记录由 CreateDisk 生成 ID，而路径得在此之前
	// 定好。固定路径也让"释放回池后的原地重建"天然落在同一个文件上。
	abs, err := s.Disk.DiskRef(guard.Root,
		filepath.Join(s.raw().Storage.DisksDir, "diffs", repo.ID, uuid.NewString()+".vhdx"))
	if err != nil {
		return nil, apperr.InvalidParam("path")
	}

	parentID := parent.ID
	d := &domain.Disk{
		RepoID:        repo.ID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      abs,
		ParentID:      &parentID,
		ParentVersion: repo.ParentVersion,
		SizeBytes:     parent.SizeBytes,
		VHDType:       domain.VHDTypeDifferencing,
		State:         domain.DiskStateCreating,
	}
	if err := s.Store.CreateDisk(ctx, d); err != nil {
		return nil, err
	}
	return d, s.ensureDiffDisk(ctx, d, parent, repo, rep)
}

// runResetDiff 是 JobResetDiff 的实现：把释放出来的池位盘**原地重建**回池。
//
// 盘里还留着上一个用户的数据，所以顺序不能省：
//
//	解映射 → 删文件 → 重新派生 → 清空 initiator 白名单 → 重建并启用目标
//
// 为什么原地重建而不是"删盘 + 建新盘"：目标名、IQN 与 CHAP 凭据都跟着这块盘走，
// 原地重建让下一个用户直接复用已经下发过的目标 —— 挂载时不再有首次下发那十几秒；
// 而换一块新盘意味着平台侧要走一次完整的"删目标 + 建目标"。
func (s *DiskService) runResetDiff(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p resetDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}

	// 上一个用户的 ID 要在删掉分配记录**之前**拿：它用于重建后按实际占用重算用量。
	userID := s.allocatedUserID(ctx, p.DiskID)
	if err := s.removeAllocation(ctx, p.AllocationID); err != nil {
		return err
	}

	disk, err := s.Store.GetDisk(ctx, p.DiskID)
	if err != nil {
		if isNotFound(err) {
			// 盘记录已经没了（例如整库在期间被删除）：没有可重建的对象。
			return nil
		}
		return err
	}
	if s.Disk == nil {
		return errNotImplemented
	}

	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			s.Log.Info("存储库已不存在，跳过回池重建", "repo_id", p.RepoID, "disk_id", disk.ID)
			return nil
		}
		return err
	}
	parent, err := s.Store.GetDisk(ctx, p.ParentDiskID)
	if err != nil {
		if isNotFound(err) {
			// 母盘不在了（整库正在回收）：把这块盘连同记录一起清掉，别在池里留个僵尸。
			s.Log.Warn("母盘已不存在，改为删除池位盘", "disk_id", disk.ID, "parent_disk_id", p.ParentDiskID)
			return s.deleteDiskArtifacts(ctx, disk.ID, "")
		}
		return err
	}

	// 挡住分配：重建期间这块盘上的数据/映射都不完整，被分配出去就是事故。
	// （释放流程已经把它置为 creating，这里再置一次是因为失败重试可能让它被对账改成了 error。）
	disk.State = domain.DiskStateCreating
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}
	rep.Progress(5)

	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, disk.ID)
	if err != nil && !isNotFound(err) {
		return err
	}

	// 1) 解映射 + 删除文件。
	//
	// 复用删盘的同一条路径：必须先在平台侧拆掉映射再删文件，否则 Windows 会以
	// "文件被占用"失败（Linux 上则是内核拒绝 lvremove）。
	if err := s.deleteDiskPhysical(ctx, disk.VHDXPath, targetNamesOf(targets)); err != nil {
		return err
	}
	rep.Progress(25)

	// 2) 清空 initiator 白名单。上一个用户的客户端 IQN 留着的话，下一个用户
	//    （IQN 不同）会被挡在门外 —— 表现为"能连上目标，但登录失败"。
	//
	// 只改 DB、不在这里下发：紧接着的目标重建会把新白名单一起带下去，少一次下发。
	for i := range targets {
		if err := s.Store.ReplaceInitiatorIDs(ctx, targets[i].ID, []domain.InitiatorID{}); err != nil {
			return err
		}
	}

	// 3) 重新派生（必须真的重建：旧文件里是上一个用户的数据）。
	if err := s.createDiffWithRetry(ctx, disk.VHDXPath, parent.VHDXPath); err != nil {
		return err
	}
	rep.Progress(60)

	if size, err := s.Disk.PhysicalSize(disk.VHDXPath); err == nil {
		disk.PhysicalBytes = size
	}
	disk.ParentVersion = repo.ParentVersion
	disk.State = domain.DiskStateReady
	if err := s.Store.UpdateDisk(ctx, disk); err != nil {
		return err
	}

	// 4) 重新发布：盘子换了，平台侧必须重新 Import + 映射 + 建启用状态。
	//
	// 下发记账不必在这里手工清：上一步的 deleteDiskPhysical 走的是统一拆除路径，
	// 删目标时已把记账一起清掉（见 IscsiService.teardownTargetLocked → clearAppliedByName）。
	// 它是按目标短名查 DB 再清，与寻址名（完整 IQN）无关；一旦残留就会
	// "以为已下发过"而跳过，让下一个用户连上一个根本不存在的目标。
	if s.IscsiSvc != nil {
		if err := s.IscsiSvc.EnsurePoolTarget(ctx, repo, disk); err != nil {
			return err
		}
	} else {
		s.Log.Warn("iSCSI 服务未装配，池位盘只重建文件", "repo_id", repo.ID, "disk_id", disk.ID)
	}
	rep.Progress(95)

	// 5) 按实际占用重算用量（重建后差异盘几乎是空的，释放时预留的母盘容量要还回去）。
	s.resyncUsage(ctx, disk.RepoID, userID)
	rep.Progress(100)
	s.audit(ctx, userID, "repo.pool.reset", "disk:"+disk.ID,
		"allocation="+p.AllocationID, domain.AuditResultOK)
	return nil
}
