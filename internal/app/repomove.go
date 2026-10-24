package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
	"vault/internal/store"
)

// moveRepoPhase 是迁移任务的阶段（写进 meta.move，供卡片显示"现在在做什么"）。
const (
	moveRepoPhasePreparing  = "preparing"
	moveRepoPhaseCopying    = "copying"
	moveRepoPhaseFinalizing = "finalizing"
)

// moveCopyBufferSize 是复制磁盘数据的读块大小。
//
// 4MiB 是吞吐与"取消后多久能停住"之间的取舍：块太大时取消要等一整块读完，
// 块太小时系统调用次数明显上升。
const moveCopyBufferSize = 4 << 20

// moveRepoEntry 是一块磁盘的迁移快照。
//
// 路径在入队时就固定下来（而不是任务执行时再推导），原因：
//   - 任务重试/续跑必须复制到同一个目标位置，否则会散落多份半成品；
//   - 目标引用（Linux 的 /dev/mapper/<vg>-<lv>）由入队时的存储拓扑决定。
type moveRepoEntry struct {
	DiskID string `json:"disk_id"`
	// SourceRef / DestRef 源与目标的平台引用（Windows 为 VHDX 绝对路径，Linux 为 LV 引用）。
	SourceRef string `json:"source_ref"`
	DestRef   string `json:"dest_ref"`
	// SizeBytes 盘的逻辑容量；Linux 块设备复制时按它读满整盘。
	SizeBytes int64 `json:"size_bytes"`
	// CopyBytes 预计要搬运的字节数（Windows 取文件大小，Linux 取逻辑容量）。
	CopyBytes int64 `json:"copy_bytes"`
	// PublishAfter 迁移前这块盘上有启用中的 iSCSI 目标：迁移完成后要重新发布。
	//
	// 复制前必须先拆目标（平台会一直打开 VHDX/LV），否则复制/删除都会撞占用；
	// 拆掉的记录由本字段决定是否在原位置重建。
	PublishAfter bool `json:"publish_after,omitempty"`
	// AlreadyCopied 源文件已不在、目标文件已存在：上一次迁移在"已复制、还没切库路径"
	// 之间中断（例如旧版本只更新了路径字段却没落库）。本次直接复用目标文件，不再复制。
	AlreadyCopied bool `json:"already_copied,omitempty"`
}

// moveRepoPayload 是 JobMoveRepo 的入参。
type moveRepoPayload struct {
	RepoID string `json:"repo_id"`
	// FromStorageID / FromStorageName / ToStorageID / ToStorageName 仅用于进度展示。
	FromStorageID   string          `json:"from_storage_id,omitempty"`
	FromStorageName string          `json:"from_storage_name,omitempty"`
	ToStorageID     string          `json:"to_storage_id"`
	ToStorageName   string          `json:"to_storage_name,omitempty"`
	Entries         []moveRepoEntry `json:"entries"`
}

// Move 发起存储库迁移：复制全部磁盘到目标存储，复制完成后删除源文件。
func (s *RepoService) Move(ctx context.Context, repoID, toStorageID, operatorID string) (*domain.Job, error) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.State == domain.RepoStateMoving || repo.Meta.Move != nil {
		return nil, apperr.New("repo.move_in_progress", 409).WithArg("repo_id", repoID)
	}
	if repo.State != domain.RepoStateActive {
		return nil, apperr.New("repo.state_invalid", 409).WithArg("state", string(repo.State))
	}
	// 维护态 / 临时共享态下母盘可能正被服务端或客户端打开：这时复制母盘必然撞占用，
	// 而且维护中的内容尚未定稿，搬过去只会得到一个半成品。
	if repo.IsShared() && repo.Condition() != domain.ParentIdle {
		return nil, apperr.New("repo.state_invalid", 409).WithArg("state", string(repo.Condition()))
	}
	// 与分配、挂载、删除等整库操作串行：下面的校验（无租约/无挂载/无目标）只有在
	// 拿锁之后才可靠，否则新的分配可能在"校验通过"与"切成 moving"之间挤进来。
	unlock := s.Locks.Acquire(lock.RepoKey(repoID))
	defer unlock()

	toStorageID = strings.TrimSpace(toStorageID)
	if toStorageID == "" {
		return nil, apperr.InvalidParam("storage_id")
	}
	target, err := s.Store.GetStorage(ctx, toStorageID)
	if err != nil {
		return nil, err
	}
	if !target.Enabled {
		return nil, apperr.StorageDisabled().WithArg("id", target.ID)
	}
	if !s.storageMountReady(target.ID) {
		return nil, apperr.StorageNotMounted().WithArg("id", target.ID)
	}

	disks, err := s.Store.ListDisksByRepo(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		return nil, apperr.New("repo.disk_missing", 500)
	}
	if active, err := s.Store.CountActiveLeasesByRepo(ctx, repoID); err != nil {
		return nil, err
	} else if active > 0 {
		return nil, apperr.RepoHasActiveLease(active)
	}
	for i := range disks {
		if disks[i].Mounted {
			return nil, apperr.DiskBusy().WithArg("disk_id", disks[i].ID)
		}
	}

	// 源存储：优先按母盘定位（独享库没有母盘，退回第一块盘）。
	var sourcePath string
	if repo.ParentDiskID != nil {
		if d, err := s.Store.GetDisk(ctx, *repo.ParentDiskID); err == nil {
			sourcePath = d.VHDXPath
		}
	}
	if sourcePath == "" {
		sourcePath = disks[0].VHDXPath
	}
	fromStorage, fromRoot := s.storageForPath(ctx, sourcePath)
	if fromStorage != nil && fromStorage.ID == target.ID {
		return nil, apperr.New("repo.move_same_storage", 409).WithArg("storage_id", target.ID)
	}
	if fromRoot != "" && strings.EqualFold(filepath.Clean(fromRoot), filepath.Clean(target.Path)) {
		return nil, apperr.New("repo.move_same_storage", 409).WithArg("storage_id", target.ID)
	}

	entries := make([]moveRepoEntry, 0, len(disks))
	var totalBytes int64
	for i := range disks {
		d := &disks[i]
		dst, err := s.moveDestRef(ctx, target.Path, d.VHDXPath)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(filepath.Clean(dst), filepath.Clean(d.VHDXPath)) {
			// 这块盘本来就在目标存储里（同一存储的不同根也可能落到同一个引用）：
			// 不参与搬运，更不能"复制到自己身上"。
			continue
		}
		alreadyCopied := false
		cp := s.moveCopyBytes(d)
		if s.Disk != nil && !s.Disk.Exists(d.VHDXPath) {
			// 上一次迁移已经复制完成、但库路径还没切过去就中断了（旧版本遗留）。
			// 目标文件在就直接复用；两边都没有才是真的丢盘。
			if !s.Disk.Exists(dst) {
				return nil, apperr.DiskNotFound().WithArg("id", d.ID)
			}
			alreadyCopied, cp = true, 0
		}
		totalBytes += cp
		entries = append(entries, moveRepoEntry{
			DiskID:        d.ID,
			SourceRef:     d.VHDXPath,
			DestRef:       dst,
			SizeBytes:     d.SizeBytes,
			CopyBytes:     cp,
			PublishAfter:  s.diskHasEnabledTarget(ctx, d.ID),
			AlreadyCopied: alreadyCopied,
		})
	}
	if len(entries) == 0 {
		return nil, apperr.New("repo.move_same_storage", 409).WithArg("storage_id", target.ID)
	}

	// 复制期间源与目标会同时存在，目标卷必须放得下整份数据。
	if err := s.ensureVolumeFreeAt(ctx, target.Path, totalBytes); err != nil {
		return nil, err
	}

	move := &domain.RepoMove{
		FromStorageID:   storageIDOf(fromStorage),
		FromStorageName: storageNameOf(fromStorage),
		ToStorageID:     target.ID,
		ToStorageName:   target.Name,
		Phase:           moveRepoPhasePreparing,
		FilesTotal:      len(entries),
		TotalBytes:      totalBytes,
	}

	payload := moveRepoPayload{
		RepoID:          repo.ID,
		FromStorageID:   move.FromStorageID,
		FromStorageName: move.FromStorageName,
		ToStorageID:     target.ID,
		ToStorageName:   target.Name,
		Entries:         entries,
	}
	// 幂等键带随机串：中止后重试要用新任务；相同 lock_key 负责串行化。
	//
	// 先入队、再切状态：本方法此刻持有 RepoKey，worker 领取后也要等同一把锁，
	// 因此不会出现"任务已经开始搬文件、库却还是 active"的窗口；反过来写则会在
	// 两次写库之间留下一个没有 JobID 的 moving 快照（界面既中止不了也看不到任务）。
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobMoveRepo, repo.ID,
		"move_repo:"+repo.ID+":"+uuid.NewString(), lock.RepoKey(repo.ID), payload)
	if err != nil {
		return nil, err
	}
	move.JobID = j.ID
	repo.Meta.Move = move
	repo.State = domain.RepoStateMoving
	if err := s.Store.SetRepositoryMetaState(ctx, repo.ID, repo.Meta, repo.State); err != nil {
		// 状态没切过去就不能让任务继续跑：那会在 active 的库上开始复制。
		_ = s.Jobs.Cancel(ctx, j.ID)
		return nil, err
	}

	s.audit(ctx, operatorID, "repo.move.start", "repo:"+repo.ID,
		"from="+move.FromStorageName+" to="+target.Name+" job="+j.ID, domain.AuditResultOK)
	return j, nil
}

// CancelMove 中止该库正在进行的迁移。
//
// 为什么要单独的接口而不是复用 /v1/jobs/{id}/cancel：后者只对超级管理员开放，
// 而迁移是库所有者/管理成员自己发起的操作 —— 发起人应当能自己中止。
func (s *RepoService) CancelMove(ctx context.Context, repoID, operatorID string) error {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return err
	}
	if repo.Meta.Move == nil || strings.TrimSpace(repo.Meta.Move.JobID) == "" {
		return nil
	}
	jobID := repo.Meta.Move.JobID
	if err := s.Jobs.Cancel(ctx, jobID); err != nil {
		return err
	}
	s.audit(ctx, operatorID, "repo.move.cancel", "repo:"+repoID, "job="+jobID, domain.AuditResultOK)
	// 排队中的任务会被 Cancel 直接置为取消、不会进入 handler：这里补一次状态恢复。
	// 正在执行的任务由 handler 的收尾逻辑负责（两边都幂等）。
	if j, err := s.Store.GetJob(ctx, jobID); err == nil && j.State.IsTerminal() {
		var p moveRepoPayload
		if decodePayload(j.Payload, &p) == nil && p.RepoID != "" {
			s.abortMove(context.WithoutCancel(ctx), p)
		} else {
			s.restoreRepoAfterMove(context.WithoutCancel(ctx), repoID)
		}
	}
	return nil
}

// RegisterMoveHandler 注册迁移任务处理器（由 RepoService.RegisterHandlers 调用）。
func (s *RepoService) registerMoveHandler(w *job.Worker) {
	if w == nil {
		return
	}
	w.Register(job.HandlerFunc{T: domain.JobMoveRepo, F: s.runMoveRepo})
}

// runMoveRepo 是 JobMoveRepo 的实现。
func (s *RepoService) runMoveRepo(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p moveRepoPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		if isNotFound(err) {
			// 库在排队期间被删掉了：没有可迁移的对象。
			return nil
		}
		return err
	}

	// 进程在"数据库已切到目标、源文件还没删完"之间退出时，恢复的任务会重新跑一遍。
	// 这时**绝不能**再复制/回滚：目标已是真源，源可能只剩一部分。直接走收尾即可。
	if s.moveAlreadyApplied(ctx, p) {
		rep.Progress(96)
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		s.afterMove(postCtx, p)
		s.finishMove(postCtx, p.RepoID)
		rep.Progress(100)
		s.Log.Info("存储库迁移已在之前的尝试中完成，补齐收尾", "repo_id", p.RepoID)
		return nil
	}

	// 重试时状态可能已被上一次尝试恢复为 active：重新切成 moving，并重建进度快照。
	move := &domain.RepoMove{
		JobID:           j.ID,
		FromStorageID:   p.FromStorageID,
		FromStorageName: p.FromStorageName,
		ToStorageID:     p.ToStorageID,
		ToStorageName:   p.ToStorageName,
		Phase:           moveRepoPhasePreparing,
		FilesTotal:      len(p.Entries),
		TotalBytes:      totalCopyBytes(p.Entries),
	}
	repo.State = domain.RepoStateMoving
	repo.Meta.Move = move
	if err := s.Store.SetRepositoryMetaState(ctx, repo.ID, repo.Meta, repo.State); err != nil {
		return err
	}

	finalized := false
	defer func() {
		if finalized {
			return
		}
		s.abortMove(context.WithoutCancel(ctx), p)
	}()

	// ① 拆掉会打开磁盘文件的目标。没有这一步，Windows 上复制/删除源 VHDX 会撞共享冲突。
	if len(p.Entries) > 0 {
		rep.Progress(2)
		for i := range p.Entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.teardownMoveTargets(ctx, &p.Entries[i])
		}
	}

	// ② 逐块复制。
	move.Phase = moveRepoPhaseCopying
	var copiedTotal int64
	lastPercent := -1
	lastJobPercent := -1
	lastMetaAt := time.Time{}
	for i := range p.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &p.Entries[i]
		if e.AlreadyCopied {
			// 源已不在、目标已在：上一次尝试其实已经复制完，只差切库路径。
			move.FilesDone = i + 1
			s.saveMoveProgress(ctx, p.RepoID, move)
			continue
		}
		entryBase := copiedTotal
		copied, err := s.copyMoveEntry(ctx, e, func(done int64) {
			totalDone := entryBase + done
			move.Bytes = totalDone
			pct := movePercent(totalDone, move.TotalBytes)
			if pct != lastPercent && time.Since(lastMetaAt) >= 500*time.Millisecond {
				lastPercent, lastMetaAt = pct, time.Now()
				s.saveMoveProgress(ctx, p.RepoID, move)
			}
			// 复制占任务进度的 5% ~ 90%；只在整数百分比前进时写一次，
			// 否则每 4MiB 一条 UPDATE 会把任务表刷爆（与建盘复制同一口径）。
			jobPct := 5 + 85*pct/100
			if jobPct != lastJobPercent {
				lastJobPercent = jobPct
				rep.Progress(jobPct)
			}
		})
		if err != nil {
			return err
		}
		copiedTotal += copied
		move.FilesDone = i + 1
		move.Bytes = copiedTotal
		s.saveMoveProgress(ctx, p.RepoID, move)
	}

	// ③ 切换数据库中的磁盘路径与库状态（原子）。
	move.Phase = moveRepoPhaseFinalizing
	s.saveMoveProgress(ctx, p.RepoID, move)
	rep.Progress(92)
	if err := s.finalizeMove(ctx, p); err != nil {
		return err
	}
	finalized = true
	rep.Progress(96)

	// ④ 后续动作即便用户点了中止也要做完：数据已经在目标位置，源文件删除与目标重建
	//    只是收尾，半途停住会留下"库已切走、源还占着空间"的中间态。
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	s.afterMove(postCtx, p)
	s.finishMove(postCtx, p.RepoID)
	rep.Progress(100)

	s.audit(context.WithoutCancel(ctx), "", "repo.move.done", "repo:"+p.RepoID,
		"to="+p.ToStorageName+" files="+strconv.Itoa(len(p.Entries)), domain.AuditResultOK)
	s.Log.Info("存储库迁移完成", "repo_id", p.RepoID,
		"from", p.FromStorageName, "to", p.ToStorageName, "disks", len(p.Entries))
	s.emit("repo", map[string]any{
		"action":     "moved",
		"repo_id":    p.RepoID,
		"storage_id": p.ToStorageID,
	})
	return nil
}

// moveAlreadyApplied 判断数据库里的磁盘路径是否已经全部切到目标。
//
// 判据是"每一条迁移条目对应的磁盘路径都等于 DestRef"：finalizeMove 在一个事务里
// 写完全部路径与库状态，所以这里只可能是全有或全无。
func (s *RepoService) moveAlreadyApplied(ctx context.Context, p moveRepoPayload) bool {
	if len(p.Entries) == 0 {
		return false
	}
	for i := range p.Entries {
		d, err := s.Store.GetDisk(ctx, p.Entries[i].DiskID)
		if err != nil || !strings.EqualFold(filepath.Clean(d.VHDXPath), filepath.Clean(p.Entries[i].DestRef)) {
			return false
		}
	}
	return true
}

// teardownMoveTargets 拆除某块盘的全部 iSCSI 目标与虚拟盘登记，释放对底层文件的占用。
//
// 不区分目标是否启用：停用的目标同样可能保留虚拟盘登记（平台侧文件仍被打开），
// 而复制阶段不能容忍任何占用。是否重建由 moveRepoEntry.PublishAfter 决定。
func (s *RepoService) teardownMoveTargets(ctx context.Context, e *moveRepoEntry) {
	if s.IscsiSvc != nil {
		s.IscsiSvc.TeardownDiskTargets(ctx, e.DiskID)
	}
	if s.Iscsi != nil {
		if err := s.Iscsi.RemoveVirtualDisk(ctx, e.SourceRef); err != nil {
			s.Log.Warn("迁移前移除虚拟盘登记失败（继续复制）",
				"disk_id", e.DiskID, "ref", e.SourceRef, "error", err)
		}
	}
}

// copyMoveEntry 把一块磁盘从源复制到目标；返回实际搬运的字节数。
func (s *RepoService) copyMoveEntry(ctx context.Context, e *moveRepoEntry, onProgress func(done int64)) (int64, error) {
	if s.Disk == nil {
		return 0, errNotImplemented
	}
	if strings.HasPrefix(e.SourceRef, "/dev/") || strings.HasPrefix(e.DestRef, "/dev/") {
		return s.copyMoveDevice(ctx, e, onProgress)
	}
	return s.copyMoveFile(ctx, e, onProgress)
}

// copyMoveFile 复制普通文件（Windows 的 VHDX / 目录模式存储）。
//
// 用自带的流式复制而不是 platform.Clone：迁移必须**保留磁盘标识**与差异盘的父链，
// 而 Clone 会在复制末尾重置标识。
func (s *RepoService) copyMoveFile(ctx context.Context, e *moveRepoEntry, onProgress func(done int64)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(e.DestRef), 0o755); err != nil {
		return 0, err
	}
	// 上一次尝试可能留下了半成品：先清掉再从头复制，保证重试幂等。
	if _, err := os.Stat(e.DestRef); err == nil {
		if err := os.Remove(e.DestRef); err != nil {
			return 0, err
		}
	}
	return copyFileStream(ctx, e.SourceRef, e.DestRef, e.CopyBytes, onProgress)
}

// copyMoveDevice 复制块设备（Linux 的 LVM thin LV）。
//
// 跨卷组无法用 thin snapshot 直接挪动，只能新建一块同容量的 LV 再把数据写过去。
func (s *RepoService) copyMoveDevice(ctx context.Context, e *moveRepoEntry, onProgress func(done int64)) (int64, error) {
	if s.Disk.Exists(e.DestRef) {
		if err := s.Disk.Delete(ctx, e.DestRef); err != nil {
			return 0, err
		}
	}
	if err := s.Disk.Create(ctx, e.DestRef, e.SizeBytes); err != nil {
		return 0, err
	}
	if err := s.Disk.Activate(ctx, e.SourceRef, false); err != nil {
		return 0, err
	}
	if err := s.Disk.Activate(ctx, e.DestRef, false); err != nil {
		return 0, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := s.Disk.Deactivate(cleanup, e.DestRef); err != nil {
			s.Log.Warn("迁移后停用目标盘失败", "ref", e.DestRef, "error", err)
		}
		if err := s.Disk.Deactivate(cleanup, e.SourceRef); err != nil {
			s.Log.Warn("迁移后停用源盘失败", "ref", e.SourceRef, "error", err)
		}
	}()
	return copyFileStream(ctx, e.SourceRef, e.DestRef, e.CopyBytes, onProgress)
}

// copyFileStream 流式复制 src → dst，并在取消时尽快返回。
//
// 全零块只用 Seek 跳过（普通文件最后再 Truncate 到真实长度）：VHDX/LV 都是稀疏的，
// 逐字节写零会把目标撑成逻辑容量那么大。取消时**不删除目标**：调用方（abortMove）
// 统一清理，这样重试与中止只有一条清理路径。
func copyFileStream(ctx context.Context, src, dst string, totalHint int64, onProgress func(done int64)) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	isRegular := false
	if info, statErr := in.Stat(); statErr == nil {
		isRegular = info.Mode().IsRegular()
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Close() }()

	buf := make([]byte, moveCopyBufferSize)
	var processed int64
	var pendingZero int64
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			processed += int64(n)
			if allZero(chunk) {
				pendingZero += int64(n)
			} else {
				if pendingZero > 0 {
					if _, err := out.Seek(pendingZero, io.SeekCurrent); err != nil {
						return processed, err
					}
					pendingZero = 0
				}
				if _, err := out.Write(chunk); err != nil {
					return processed, err
				}
			}
			if onProgress != nil {
				onProgress(processed)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return processed, readErr
		}
	}
	if pendingZero > 0 {
		if _, err := out.Seek(pendingZero, io.SeekCurrent); err != nil {
			return processed, err
		}
	}
	if isRegular {
		// 尾部全零块没有真正写入，文件长度要用 Truncate 补到源长度。
		if err := out.Truncate(processed); err != nil {
			return processed, err
		}
	}
	if totalHint > 0 && processed > totalHint {
		processed = totalHint
	}
	return processed, out.Close()
}

// allZero 判断一个字节块是否全为零。
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// finalizeMove 在一个事务里把磁盘路径切到目标。
//
// 库状态仍保持 moving、meta.move 也保留：后面的收尾（重建目标、删除源文件）同样属于
// 迁移过程，期间不能让客户端挂载到正在收尾的库上。全部做完后由 finishMove 恢复 active。
func (s *RepoService) finalizeMove(ctx context.Context, p moveRepoPayload) error {
	return s.Store.Tx(ctx, func(tx *store.Store) error {
		if _, err := tx.LockRepository(ctx, p.RepoID); err != nil {
			return err
		}
		for i := range p.Entries {
			if err := tx.UpdateDiskPath(ctx, p.Entries[i].DiskID, p.Entries[i].DestRef); err != nil {
				return err
			}
		}
		return nil
	})
}

// finishMove 在收尾完成后把库恢复为 active，并清掉迁移进度快照。
func (s *RepoService) finishMove(ctx context.Context, repoID string) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		s.Log.Warn("迁移收尾后读取存储库失败", "repo_id", repoID, "error", err)
		return
	}
	repo.Meta.Move = nil
	repo.State = domain.RepoStateActive
	if err := s.Store.SetRepositoryMetaState(ctx, repoID, repo.Meta, repo.State); err != nil {
		s.Log.Warn("迁移收尾后恢复存储库状态失败", "repo_id", repoID, "error", err)
	}
}

// afterMove 是迁移成功后的收尾：重建目标、删除源、重写盘上的元数据标签。
func (s *RepoService) afterMove(ctx context.Context, p moveRepoPayload) {
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		s.Log.Warn("迁移收尾时读取存储库失败", "repo_id", p.RepoID, "error", err)
		return
	}
	// 先把路径切换后目标侧的 iSCSI 目标重新发布：预创建的池位盘应当继续"分配即用"。
	if s.IscsiSvc != nil {
		for i := range p.Entries {
			if !p.Entries[i].PublishAfter {
				continue
			}
			disk, err := s.Store.GetDisk(ctx, p.Entries[i].DiskID)
			if err != nil {
				s.Log.Warn("迁移后重建 iSCSI 目标时读取磁盘失败",
					"disk_id", p.Entries[i].DiskID, "error", err)
				continue
			}
			if err := s.IscsiSvc.EnsureDiskTarget(ctx, repo, disk); err != nil {
				s.Log.Warn("迁移后重建 iSCSI 目标失败（下次挂载会现场下发）",
					"disk_id", disk.ID, "error", err)
			}
		}
	}
	// 删除源文件：失败只告警 —— 数据已经在目标位置，残留文件由孤儿扫描兜底。
	for i := range p.Entries {
		if strings.EqualFold(filepath.Clean(p.Entries[i].SourceRef), filepath.Clean(p.Entries[i].DestRef)) {
			continue
		}
		if err := s.removeMoveSource(ctx, p.Entries[i].SourceRef); err != nil {
			s.Log.Warn("迁移后删除源磁盘失败（可由孤儿磁盘清理）",
				"ref", p.Entries[i].SourceRef, "error", err)
		}
	}
	// 盘上的 sidecar 跟着路径走：重新写一遍，换机器导入时仍然认得出这个库。
	s.refreshRepoMeta(ctx, repo)
}

// abortMove 回滚一次未完成的迁移：清理目标侧半成品、恢复库状态、重建被拆掉的目标。
func (s *RepoService) abortMove(ctx context.Context, p moveRepoPayload) {
	cleanup, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for i := range p.Entries {
		s.removeMoveDestination(cleanup, &p.Entries[i])
	}
	s.restoreRepoAfterMove(cleanup, p.RepoID)
	if s.IscsiSvc == nil {
		return
	}
	repo, err := s.Store.GetRepository(cleanup, p.RepoID)
	if err != nil {
		return
	}
	for i := range p.Entries {
		if !p.Entries[i].PublishAfter {
			continue
		}
		disk, err := s.Store.GetDisk(cleanup, p.Entries[i].DiskID)
		if err != nil {
			continue
		}
		if err := s.IscsiSvc.EnsureDiskTarget(cleanup, repo, disk); err != nil {
			s.Log.Warn("中止迁移后重建 iSCSI 目标失败（下次挂载会现场下发）",
				"disk_id", disk.ID, "error", err)
		}
	}
}

// restoreRepoAfterMove 把库从 moving 恢复为 active 并清掉进度快照。
func (s *RepoService) restoreRepoAfterMove(ctx context.Context, repoID string) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return
	}
	if repo.Meta.Move == nil && repo.State != domain.RepoStateMoving {
		return
	}
	repo.Meta.Move = nil
	repo.State = domain.RepoStateActive
	if err := s.Store.SetRepositoryMetaState(ctx, repoID, repo.Meta, repo.State); err != nil {
		s.Log.Warn("恢复存储库迁移状态失败", "repo_id", repoID, "error", err)
	}
}

// removeMoveDestination 删除目标侧的半成品。
func (s *RepoService) removeMoveDestination(ctx context.Context, e *moveRepoEntry) {
	if strings.EqualFold(filepath.Clean(e.SourceRef), filepath.Clean(e.DestRef)) {
		return
	}
	if strings.HasPrefix(e.DestRef, "/dev/") {
		if s.Disk != nil {
			if err := s.Disk.Delete(ctx, e.DestRef); err != nil {
				s.Log.Warn("清理迁移目标盘失败", "ref", e.DestRef, "error", err)
			}
		}
		return
	}
	if err := os.Remove(e.DestRef); err != nil && !os.IsNotExist(err) {
		s.Log.Warn("清理迁移目标文件失败", "path", e.DestRef, "error", err)
	}
	removeDiskMetaFile(s.Log, e.DestRef)
}

// removeMoveSource 删除迁移后的源磁盘（含 sidecar）。失败由调用方只告警。
func (s *RepoService) removeMoveSource(ctx context.Context, ref string) error {
	if s.Disk == nil {
		return nil
	}
	removeDiskMetaFile(s.Log, ref)
	return s.Disk.Delete(ctx, ref)
}

// saveMoveProgress 写入迁移进度快照（失败只告警：进度不该影响任务本身）。
func (s *RepoService) saveMoveProgress(ctx context.Context, repoID string, move *domain.RepoMove) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return
	}
	cp := *move
	repo.Meta.Move = &cp
	repo.State = domain.RepoStateMoving
	if err := s.Store.SetRepositoryMetaState(ctx, repoID, repo.Meta, repo.State); err != nil {
		s.Log.Warn("写入迁移进度失败", "repo_id", repoID, "error", err)
	}
}

// StorageOf 返回该库磁盘当前所在的存储（供详情页隐藏"当前存储"选项）。
//
// 找不到时返回 nil；Linux 上按卷组匹配，Windows 上按路径前缀匹配。
func (s *RepoService) StorageOf(ctx context.Context, repo *domain.Repository) (*domain.Storage, error) {
	if repo == nil {
		return nil, nil
	}
	var path string
	if repo.ParentDiskID != nil {
		if d, err := s.Store.GetDisk(ctx, *repo.ParentDiskID); err == nil {
			path = d.VHDXPath
		}
	}
	if path == "" {
		disks, err := s.Store.ListDisksByRepo(ctx, repo.ID)
		if err != nil {
			return nil, err
		}
		if len(disks) > 0 {
			path = disks[0].VHDXPath
		}
	}
	if path == "" {
		return nil, nil
	}
	st, _ := s.storageForPath(ctx, path)
	return st, nil
}

// storageForPath 定位某个磁盘引用/路径所属的存储。
func (s *RepoService) storageForPath(ctx context.Context, path string) (*domain.Storage, string) {
	storages, err := s.Store.ListStorages(ctx)
	if err == nil {
		var best *domain.Storage
		bestRoot := ""
		for i := range storages {
			root := strings.TrimSpace(storages[i].Path)
			if root == "" || !domain.UnderRoot(root, path) {
				continue
			}
			if best == nil || len(root) > len(bestRoot) {
				bestRoot = root
				st := storages[i]
				best = &st
			}
		}
		if best != nil {
			return best, bestRoot
		}
		// Linux：磁盘引用是 /dev/mapper/<vg>-<lv>，不在存储根之下，按卷组归属匹配。
		if pb, ok := s.Disk.(platform.PooledDiskBackend); ok {
			if pool := pb.PoolOf(path); pool != "" {
				for i := range storages {
					if strings.TrimSpace(storages[i].Path) == "" {
						continue
					}
					if pb.PoolOf(storages[i].Path) == pool {
						st := storages[i]
						return &st, st.Path
					}
				}
			}
		}
	}
	// 没有 DB 存储记录时回退配置里的种子根（只用于展示，不参与迁移校验）。
	var bestRoot string
	for _, root := range s.raw().Storage.EffectiveRoots() {
		if domain.UnderRoot(root, path) && len(root) > len(bestRoot) {
			bestRoot = root
		}
	}
	return nil, bestRoot
}

// moveDestRef 生成目标存储中的磁盘引用。
func (s *RepoService) moveDestRef(ctx context.Context, targetRoot, srcRef string) (string, error) {
	if strings.HasPrefix(srcRef, "/dev/") {
		_, lv, ok := splitMapperRef(srcRef)
		if !ok {
			return "", apperr.InvalidParam("path")
		}
		// Linux 的 LV 名与 rel 一一对应；把 LV 名原样当成 rel 再映射一次即可保持布局。
		return s.Disk.DiskRef(targetRoot, lv)
	}
	_, rootPath := s.storageForPath(ctx, srcRef)
	if strings.TrimSpace(rootPath) == "" {
		return "", apperr.InvalidParam("path").WithCause(errors.New("源磁盘不在任何存储根下"))
	}
	rel, err := filepath.Rel(rootPath, srcRef)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", apperr.InvalidParam("path")
	}
	return s.Disk.DiskRef(targetRoot, rel)
}

// splitMapperRef 把 /dev/mapper/<vg>-<lv> 拆成 vg 与 lv（VG/LV 名不含 '-'）。
func splitMapperRef(ref string) (vg, lv string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(ref), "/dev/mapper/")
	if !found {
		return "", "", false
	}
	i := strings.IndexByte(rest, '-')
	if i <= 0 || i >= len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// moveCopyBytes 估算一块盘要搬运的字节数。
func (s *RepoService) moveCopyBytes(d *domain.Disk) int64 {
	if d == nil {
		return 0
	}
	if strings.HasPrefix(d.VHDXPath, "/dev/") {
		// 块设备按逻辑容量整盘读写（稀疏零块用 Seek 跳过）。
		return d.SizeBytes
	}
	if info, err := os.Stat(d.VHDXPath); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return info.Size()
	}
	if d.PhysicalBytes > 0 {
		return d.PhysicalBytes
	}
	return d.SizeBytes
}

// diskHasEnabledTarget 判断某块盘上是否有启用中的 iSCSI 目标。
func (s *RepoService) diskHasEnabledTarget(ctx context.Context, diskID string) bool {
	targets, err := s.Store.ListIscsiTargetsByDisk(ctx, diskID)
	if err != nil {
		return false
	}
	for i := range targets {
		if targets[i].Enabled || targets[i].DesiredEnabled {
			return true
		}
	}
	return false
}

// totalCopyBytes 汇总一次迁移要搬运的字节数。
func totalCopyBytes(entries []moveRepoEntry) int64 {
	var total int64
	for i := range entries {
		total += entries[i].CopyBytes
	}
	return total
}

// movePercent 按字节算进度；总字节为 0（空盘）时按文件数给一个保守值。
func movePercent(done, total int64) int {
	if total <= 0 {
		return 0
	}
	pct := int(done * 100 / total)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
