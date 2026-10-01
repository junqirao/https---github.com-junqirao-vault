package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/store"
)

// DefaultUploadChunkSize 是分块上传的默认分块大小（8MiB，见 docs/implementation.md 5.10）。
const DefaultUploadChunkSize int64 = 8 << 20

// uploadGCRetention 是未完成上传会话的暂存保留时长，超过后由 GC 清理。
const uploadGCRetention = 24 * time.Hour

// CreateUploadInput 是创建上传会话的入参。
type CreateUploadInput struct {
	// UserID 发起人，同时是新建存储库的 owner。
	UserID string
	// RepoName 目标存储库名。
	RepoName string
	// Mode copy | move。
	Mode     string
	Manifest domain.UploadManifest
	// RepoMode 建库模式 shared | exclusive；留空默认 shared。
	RepoMode string
	// StorageID 可选目标存储 ID；留空按"所在卷可用空间最大"自动选。
	StorageID string
	// QuotaBytes 可选应用层配额；0 表示不限。
	QuotaBytes int64
}

// UploadSession 是创建上传会话的响应。
type UploadSession struct {
	UploadID      string `json:"upload_id"`
	ChunkSize     int64  `json:"chunk_size"`
	MissingChunks []int  `json:"missing_chunks"`
	ReceivedBytes int64  `json:"received_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
}

// UploadStatus 是查询上传会话状态的响应（断点续传用）。
type UploadStatus struct {
	UploadID      string `json:"upload_id"`
	State         string `json:"state"`
	TotalFiles    int    `json:"total_files"`
	TotalBytes    int64  `json:"total_bytes"`
	ReceivedBytes int64  `json:"received_bytes"`
	MissingChunks []int  `json:"missing_chunks"`
}

// ChunkResult 是上传单个分块的响应。
type ChunkResult struct {
	Index         int   `json:"index"`
	Size          int64 `json:"size"`
	ReceivedBytes int64 `json:"received_bytes"`
	Done          bool  `json:"done"`
}

// uploadCompletePayload 是 JobUploadComplete 的任务参数。
type uploadCompletePayload struct {
	UploadID string `json:"upload_id"`
}

// uploadFile 描述清单中的一个文件在拼接流中的位置。
type uploadFile struct {
	RelPath string
	Size    int64
	Offset  int64
	OutPath string
}

// UploadService 负责分块上传会话的编排与「由目录内容建盘」的异步任务（见 5.10）。
//
// 暂存目录位于 storage.staging_dir（相对白名单根），与 disks 目录同卷，
// 完成时可直接把重组后的 tree 交给建盘任务复制（见 5.10 要点）。
type UploadService struct {
	Deps
	Disks *DiskService
	Repos *RepoService
}

// ChunkSize 返回当前生效的分块大小。
func (s *UploadService) ChunkSize() int64 { return DefaultUploadChunkSize }

// RegisterHandlers 把上传完成后建盘的任务注册到 worker。
func (s *UploadService) RegisterHandlers(w *job.Worker) {
	if w == nil {
		return
	}
	w.Register(job.HandlerFunc{T: domain.JobUploadComplete, F: s.runUploadComplete})
}

// CreateUpload 创建上传会话：校验清单、配额与卷水位，登记会话并返回缺失分块列表。
func (s *UploadService) CreateUpload(ctx context.Context, in CreateUploadInput) (*UploadSession, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return nil, apperr.InvalidParam("user_id")
	}
	name := strings.TrimSpace(in.RepoName)
	if name == "" {
		return nil, apperr.InvalidParam("repo_name")
	}
	if in.Mode != domain.UploadModeCopy && in.Mode != domain.UploadModeMove {
		return nil, apperr.InvalidParam("mode")
	}
	// 建库模式：留空默认 shared；非法值明确报错。
	repoMode := strings.TrimSpace(in.RepoMode)
	if repoMode == "" {
		repoMode = string(domain.RepoModeShared)
	}
	if !domain.RepoMode(repoMode).Valid() {
		return nil, apperr.InvalidParam("repo_mode")
	}
	if in.QuotaBytes < 0 {
		return nil, apperr.InvalidParam("quota_bytes")
	}
	storageID := strings.TrimSpace(in.StorageID)

	entries, totalFiles, totalBytes, err := normalizeManifest(in.Manifest)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := marshalManifest(uploadManifestRecord{
		Root:       in.Manifest.Root,
		Files:      entries,
		RepoMode:   repoMode,
		StorageID:  storageID,
		QuotaBytes: in.QuotaBytes,
	})
	if err != nil {
		return nil, err
	}

	// 选根 + 配额与卷水位：目标存储库尚未创建，先按用户配额与卷水位把关（见 5.10 要点）。
	// 暂存目录建在**选定根**下，保证与后续建盘同卷（complete 时的同卷语义）。
	// storage_id 显式指定时走与 RepoService.Create 相同的 storage.not_found / storage.disabled 校验。
	size := s.sizing().CalculateVHDXSize(totalBytes, 0)
	guard, err := s.pickUploadGuard(ctx, storageID, size)
	if err != nil {
		return nil, err
	}
	if err := s.ensureUploadAllowed(ctx, in.UserID, name, guard.Root, size); err != nil {
		return nil, err
	}

	raw := s.raw()
	uploadID := uuid.NewString()
	stagingRoot, err := guard.Resolve(filepath.Join(raw.Storage.StagingDir, uploadID))
	if err != nil {
		return nil, apperr.InvalidParam("staging_dir")
	}
	if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_mkdir").WithCause(err)
	}

	u := &domain.Upload{
		ID:         uploadID,
		UserID:     in.UserID,
		RepoName:   name,
		Mode:       in.Mode,
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
		StagingDir: stagingRoot,
		Manifest:   manifestJSON,
		State:      domain.UploadStateOpen,
	}
	if err := s.Store.CreateUpload(ctx, u); err != nil {
		_ = os.RemoveAll(stagingRoot)
		return nil, err
	}
	s.audit(ctx, in.UserID, "upload.create", "upload:"+uploadID,
		fmt.Sprintf("repo=%s mode=%s files=%d bytes=%d", name, in.Mode, totalFiles, totalBytes),
		domain.AuditResultOK)

	missing := allChunkIndices(totalBytes, DefaultUploadChunkSize)
	s.emit("upload", map[string]any{
		"action": "create", "upload_id": uploadID, "user_id": in.UserID, "state": domain.UploadStateOpen,
	})
	return &UploadSession{
		UploadID:      uploadID,
		ChunkSize:     DefaultUploadChunkSize,
		MissingChunks: missing,
		ReceivedBytes: 0,
		TotalBytes:    totalBytes,
	}, nil
}

// Status 返回上传会话状态与缺失分块（断点续传用）。
func (s *UploadService) Status(ctx context.Context, uploadID string) (*UploadStatus, error) {
	u, err := s.Store.GetUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	missing, received, err := s.missingChunks(ctx, u)
	if err != nil {
		return nil, err
	}
	return &UploadStatus{
		UploadID:      u.ID,
		State:         u.State,
		TotalFiles:    u.TotalFiles,
		TotalBytes:    u.TotalBytes,
		ReceivedBytes: received,
		MissingChunks: missing,
	}, nil
}

// PutChunk 写入单个分块：落盘 → 校验 SHA256 → 登记；校验失败删除该分块文件。
//
// 幂等：同 index 且校验一致时直接返回成功，不重复写入。
func (s *UploadService) PutChunk(ctx context.Context, uploadID string, index int, sha256Hex string, body io.Reader) (*ChunkResult, error) {
	u, err := s.Store.GetUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	if u.State != domain.UploadStateOpen && u.State != domain.UploadStateVerifying {
		return nil, apperr.New("upload.state_invalid", 409).WithArg("state", u.State)
	}
	total := u.TotalBytes
	count := chunkCount(total, DefaultUploadChunkSize)
	if index < 0 || index >= count {
		return nil, apperr.InvalidParam("index")
	}
	expectSize := chunkSizeAt(index, total, DefaultUploadChunkSize)

	// 幂等：已存在且校验值一致 → 直接成功。
	existing, existingSize, err := s.findChunk(ctx, u.StagingDir, uploadID, index)
	if err != nil {
		return nil, err
	}
	if existing != nil && strings.EqualFold(existing.Checksum, sha256Hex) && existingSize == expectSize {
		received, err := s.receivedBytes(ctx, u)
		if err != nil {
			return nil, err
		}
		return &ChunkResult{Index: index, Size: existingSize, ReceivedBytes: received, Done: s.isComplete(received, total)}, nil
	}

	partPath := partFilePath(u.StagingDir, index)
	if err := os.MkdirAll(u.StagingDir, 0o755); err != nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_mkdir").WithCause(err)
	}
	f, err := os.Create(partPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_write").WithCause(err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(body, expectSize+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(partPath)
		cause := copyErr
		if cause == nil {
			cause = closeErr
		}
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_write").WithCause(cause)
	}
	if written != expectSize {
		_ = os.Remove(partPath)
		return nil, apperr.New("upload.chunk_size_mismatch", 400).
			WithArg("index", index).WithArg("expected", expectSize).WithArg("actual", written)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(sha256Hex)) {
		// 校验失败：删除该分块文件且不登记（见 5.10 要点）。
		_ = os.Remove(partPath)
		s.audit(ctx, u.UserID, "upload.chunk_reject", "upload:"+uploadID,
			fmt.Sprintf("index=%d checksum_mismatch", index), domain.AuditResultError)
		return nil, apperr.New("upload.chunk_checksum_mismatch", 400).WithArg("index", index)
	}

	delta := expectSize
	if existing != nil {
		// 覆盖一个校验值不同的旧分块：先撤销原登记的字节数。
		if err := s.Store.DeleteUploadChunk(ctx, uploadID, index); err != nil {
			return nil, err
		}
		delta = expectSize - existingSize
	}
	chunk := &domain.UploadChunk{
		UploadID:   uploadID,
		ChunkIndex: index,
		RelPath:    s.chunkRelPath(u, index),
		Offset:     int64(index) * DefaultUploadChunkSize,
		Size:       expectSize,
		Checksum:   actual,
	}
	if err := s.Store.PutUploadChunk(ctx, chunk); err != nil {
		return nil, err
	}
	if err := s.Store.AddUploadReceived(ctx, uploadID, delta); err != nil {
		return nil, err
	}
	received, err := s.receivedBytes(ctx, u)
	if err != nil {
		return nil, err
	}
	return &ChunkResult{Index: index, Size: expectSize, ReceivedBytes: received, Done: s.isComplete(received, total)}, nil
}

// Complete 完成上传：串行重组文件树 → 校验 → 提交 JobUploadComplete（幂等键为 upload_id）。
func (s *UploadService) Complete(ctx context.Context, uploadID string) (*domain.Job, error) {
	u, err := s.Store.GetUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	switch u.State {
	case domain.UploadStateComplete:
		// 幂等：直接返回既有任务。
		if j, gErr := s.Store.GetJobByIdemKey(ctx, uploadCompleteIdemKey(uploadID)); gErr == nil {
			return j, nil
		}
		return nil, apperrConflictState(u.State)
	case domain.UploadStateAborted:
		return nil, apperrConflictState(u.State)
	}

	missing, _, err := s.missingChunks(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return nil, apperr.New("upload.incomplete", 409).WithArg("missing_count", len(missing))
	}

	if err := s.Store.UpdateUploadState(ctx, uploadID, domain.UploadStateVerifying); err != nil {
		return nil, err
	}
	treeDir, err := s.ensureTree(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := s.verifyTree(treeDir, u); err != nil {
		return nil, err
	}

	guard, err := s.guardForUpload(ctx, u)
	if err != nil {
		return nil, err
	}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobUploadComplete, uploadID,
		uploadCompleteIdemKey(uploadID), lock.VolumeKey(domain.VolumeID(guard.Root)),
		uploadCompletePayload{UploadID: uploadID})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, u.UserID, "upload.complete", "upload:"+uploadID, "job="+j.ID, domain.AuditResultOK)
	return j, nil
}

// Abort 中止上传并清理暂存目录（幂等）。
func (s *UploadService) Abort(ctx context.Context, uploadID string) error {
	u, err := s.Store.GetUpload(ctx, uploadID)
	if err != nil {
		return err
	}
	if u.State == domain.UploadStateComplete {
		return apperrConflictState(u.State)
	}
	if err := os.RemoveAll(u.StagingDir); err != nil {
		return apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_remove").WithCause(err)
	}
	if u.State != domain.UploadStateAborted {
		if err := s.Store.UpdateUploadState(ctx, uploadID, domain.UploadStateAborted); err != nil {
			return err
		}
	}
	s.audit(ctx, u.UserID, "upload.abort", "upload:"+uploadID, "", domain.AuditResultOK)
	return nil
}

// GCStaleUploads 清理超过 24h 未完成的会话及其暂存目录，返回清理数量。
func (s *UploadService) GCStaleUploads(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-uploadGCRetention).UnixMilli()
	stale, err := s.Store.ListStaleUploads(ctx, cutoff, 100)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for i := range stale {
		u := &stale[i]
		if u.StagingDir != "" {
			if err := os.RemoveAll(u.StagingDir); err != nil {
				s.Log.Warn("清理上传暂存目录失败", "upload_id", u.ID, "error", err)
				continue
			}
		}
		if err := s.Store.DeleteUpload(ctx, u.ID); err != nil && !isNotFound(err) {
			s.Log.Warn("删除过期上传会话失败", "upload_id", u.ID, "error", err)
			continue
		}
		cleaned++
	}
	if cleaned > 0 {
		s.audit(ctx, "system", "upload.gc", "uploads", fmt.Sprintf("count=%d", cleaned), domain.AuditResultOK)
	}
	return cleaned, nil
}

// ---- 任务处理器 ----

// runUploadComplete 由目录树创建 VHDX 并建立存储库记录（见 5.10 步骤 ⑤）。
//
// 幂等：upload.state 已为 complete 时直接返回；建盘流水线本身按 disk 状态幂等。
// mode=move 时在复制校验成功后才清理 staging（服务端侧源由客户端负责清理）。
func (s *UploadService) runUploadComplete(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p uploadCompletePayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	if p.UploadID == "" {
		return apperr.InvalidParam("upload_id")
	}
	u, err := s.Store.GetUpload(ctx, p.UploadID)
	if err != nil {
		return err
	}
	if u.State == domain.UploadStateComplete {
		return nil
	}
	if u.State == domain.UploadStateAborted {
		return apperr.New("upload.state_invalid", 409).WithArg("state", u.State)
	}

	rep.Progress(5)
	treeDir, err := s.ensureTree(ctx, u)
	if err != nil {
		return err
	}
	if err := s.verifyTree(treeDir, u); err != nil {
		return err
	}
	rep.Progress(20)

	repo, disk, err := s.ensureRepoForUpload(ctx, u, treeDir)
	if err != nil {
		return err
	}
	rep.Progress(35)

	if err := s.Disks.buildVHDXFromSource(ctx, disk, treeDir, rep); err != nil {
		return err
	}
	rep.Progress(95)

	// 记录母盘内容指纹，供后续派生差异盘时的指纹校验使用（5.4）。
	if repo.IsShared() && disk.Kind == domain.DiskKindParent && s.Disk != nil && s.Disk.Exists(disk.VHDXPath) {
		if fp, fErr := s.Disk.Fingerprint(disk.VHDXPath); fErr == nil {
			disk.ContentFingerprint = fp
			if err := s.Store.UpdateDisk(ctx, disk); err != nil {
				s.Log.Warn("写回母盘指纹失败", "disk_id", disk.ID, "error", err)
			}
		}
	}

	// 登记 iSCSI 虚拟盘：幂等，失败不影响建库结果（发布时会再次 Import）。
	if s.Iscsi != nil {
		if err := s.Iscsi.EnsureVirtualDisk(ctx, disk.VHDXPath, "vault:"+repo.Name); err != nil {
			s.Log.Warn("登记 iSCSI 虚拟盘失败", "path", disk.VHDXPath, "error", err)
		}
	}

	if err := s.Store.UpdateUploadState(ctx, u.ID, domain.UploadStateComplete); err != nil {
		return err
	}
	if u.Mode == domain.UploadModeMove {
		// 校验成功后才清理暂存（见 5.10 要点）。
		if err := os.RemoveAll(u.StagingDir); err != nil {
			s.Log.Warn("清理 move 模式暂存目录失败", "upload_id", u.ID, "error", err)
		}
	}
	rep.Progress(100)
	s.audit(ctx, u.UserID, "upload.complete.done", "upload:"+u.ID,
		"repo="+repo.ID+" disk="+disk.ID, domain.AuditResultOK)
	s.emit("upload", map[string]any{
		"action": "complete", "upload_id": u.ID, "repo_id": repo.ID, "state": domain.UploadStateComplete,
	})
	return nil
}

// ---- 内部实现 ----

// ensureRepoForUpload 幂等地为上传创建存储库与母盘记录。
//
// 建库模式（shared | exclusive）、目标存储与配额取自会话清单中持久化的选项
// （见 uploadManifestRecord），因此异步任务重试时语义稳定。
func (s *UploadService) ensureRepoForUpload(ctx context.Context, u *domain.Upload, treeDir string) (*domain.Repository, *domain.Disk, error) {
	raw := s.raw()
	rec, err := parseManifest(u.Manifest)
	if err != nil {
		return nil, nil, err
	}
	mode := domain.RepoMode(rec.RepoMode)
	if !mode.Valid() {
		mode = domain.RepoModeShared
	}

	dirBytes, err := dirSize(treeDir)
	if err != nil {
		return nil, nil, err
	}
	size := s.sizing().CalculateVHDXSize(dirBytes, 0)

	guard, err := s.uploadTargetGuard(ctx, u, rec.StorageID)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Repos.ensureVolumeFreeAt(ctx, guard.Root, size); err != nil {
		return nil, nil, err
	}

	var repo *domain.Repository
	var disk *domain.Disk
	err = s.Store.Tx(ctx, func(tx *store.Store) error {
		owner, err := tx.GetUserByID(ctx, u.UserID)
		if err != nil {
			return err
		}
		if owner.QuotaBytes > 0 && owner.UsedBytes+size > owner.QuotaBytes {
			return apperr.RepoQuotaExceeded(owner.QuotaBytes, owner.UsedBytes, size)
		}
		if rec.QuotaBytes > 0 && size > rec.QuotaBytes {
			return apperr.RepoQuotaExceeded(rec.QuotaBytes, 0, size)
		}

		existing, err := tx.GetRepositoryByName(ctx, u.RepoName)
		if err != nil && !isNotFound(err) {
			return err
		}
		if existing != nil {
			// 幂等复用：任务重试时不重复建库。
			repo = existing
			if existing.ParentDiskID != nil {
				d, dErr := tx.GetDisk(ctx, *existing.ParentDiskID)
				if dErr != nil {
					return dErr
				}
				disk = d
				return nil
			}
			// 独享库不设 ParentDiskID：复用其 standalone 盘，保证重试幂等。
			disks, dErr := tx.ListDisksByRepo(ctx, existing.ID)
			if dErr != nil {
				return dErr
			}
			if len(disks) > 0 {
				disk = &disks[0]
				return nil
			}
		} else {
			maxDiff := raw.Storage.DefaultMaxDiffDisks
			if maxDiff <= 0 {
				maxDiff = 50
			}
			repo = &domain.Repository{
				Name:         u.RepoName,
				Mode:         mode,
				OwnerID:      u.UserID,
				MaxDiffDisks: maxDiff,
				QuotaBytes:   rec.QuotaBytes,
				State:        domain.RepoStateActive,
			}
			if mode == domain.RepoModeShared {
				cond := domain.ParentIdle
				repo.ParentCondition = &cond
			}
			if err := tx.CreateRepository(ctx, repo); err != nil {
				return err
			}
		}

		kind := domain.DiskKindParent
		rel := filepath.Join(raw.Storage.DisksDir, "parents", repo.ID, "base.vhdx")
		if mode == domain.RepoModeExclusive {
			kind = domain.DiskKindStandalone
			rel = filepath.Join(raw.Storage.DisksDir, "standalone", repo.ID, "data.vhdx")
		}
		abs, err := s.Disk.DiskRef(guard.Root, rel)
		if err != nil {
			return apperr.InvalidParam("path")
		}
		disk = &domain.Disk{
			RepoID:    repo.ID,
			Kind:      kind,
			VHDXPath:  abs,
			SizeBytes: size,
			VHDType:   domain.VHDTypeDynamic,
			State:     domain.DiskStateCreating,
		}
		if err := tx.CreateDisk(ctx, disk); err != nil {
			return err
		}
		if mode == domain.RepoModeShared {
			pid := disk.ID
			repo.ParentDiskID = &pid
			if err := tx.UpdateRepository(ctx, repo); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return repo, disk, nil
}

// uploadTargetGuard 解析建库应使用的目标根。
//
//   - 会话显式指定了 storage_id → 必须与该存储根一致（暂存必须在其下），
//     否则返回 storage.staging_mismatch——这是"建盘与暂存同卷、可原子改名"的前提；
//   - 未指定 → 从 staging 反推根（沿用既有做法）。
func (s *UploadService) uploadTargetGuard(ctx context.Context, u *domain.Upload, storageID string) (domain.PathGuard, error) {
	if strings.TrimSpace(storageID) == "" {
		return s.guardForUpload(ctx, u)
	}
	st, err := s.Store.GetStorage(ctx, storageID)
	if err != nil {
		return domain.PathGuard{}, err
	}
	if !st.Enabled {
		return domain.PathGuard{}, apperr.StorageDisabled().WithArg("id", st.ID)
	}
	guards, err := s.storageGuards(ctx)
	if err != nil {
		return domain.PathGuard{}, err
	}
	if sg, ok := guards.GuardForPath(u.StagingDir); !ok || !strings.EqualFold(sg.Root, st.Path) {
		return domain.PathGuard{}, apperr.New("storage.staging_mismatch", 409).
			WithArg("staging_dir", u.StagingDir).WithArg("storage_path", st.Path)
	}
	return domain.NewPathGuard(st.Path), nil
}

// ensureTree 幂等地把分块重组为文件树；已存在且完整的树直接复用。
func (s *UploadService) ensureTree(ctx context.Context, u *domain.Upload) (string, error) {
	treeDir := filepath.Join(u.StagingDir, "tree")
	if s.treeOK(treeDir, u) {
		return treeDir, nil
	}
	files, err := s.filePlan(u, treeDir)
	if err != nil {
		return "", err
	}
	if err := assembleTree(u, files); err != nil {
		return "", err
	}
	if err := s.verifyTree(treeDir, u); err != nil {
		return "", err
	}
	return treeDir, nil
}

// treeOK 判断已存在的文件树是否与清单一致。
func (s *UploadService) treeOK(treeDir string, u *domain.Upload) bool {
	if u == nil || u.TotalFiles < 0 {
		return false
	}
	files, err := s.filePlan(u, treeDir)
	if err != nil {
		return false
	}
	return treeMatches(files)
}

// verifyTree 校验文件数与总字节与清单一致（见 5.10 步骤 ④）。
func (s *UploadService) verifyTree(treeDir string, u *domain.Upload) error {
	files, err := s.filePlan(u, treeDir)
	if err != nil {
		return err
	}
	count, bytes, err := countTree(treeDir)
	if err != nil {
		return apperr.New("upload.tree_invalid", 500).WithArg("reason", "walk").WithCause(err)
	}
	if count != u.TotalFiles || bytes != u.TotalBytes || !treeMatches(files) {
		return apperr.New("upload.tree_mismatch", 500).
			WithArg("expected_files", u.TotalFiles).WithArg("actual_files", count).
			WithArg("expected_bytes", u.TotalBytes).WithArg("actual_bytes", bytes)
	}
	return nil
}

// filePlan 根据清单构造"拼接流 → 文件"的映射（按清单顺序）。
func (s *UploadService) filePlan(u *domain.Upload, treeDir string) ([]uploadFile, error) {
	manifest, err := parseManifest(u.Manifest)
	if err != nil {
		return nil, err
	}
	out := make([]uploadFile, 0, len(manifest.Files))
	var offset int64
	for _, f := range manifest.Files {
		rel, err := sanitizeManifestPath(f.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, uploadFile{
			RelPath: rel,
			Size:    f.Size,
			Offset:  offset,
			OutPath: filepath.Join(treeDir, rel),
		})
		offset += f.Size
	}
	return out, nil
}

// missingChunks 计算缺失分块与已接收字节数（以登记为准）。
func (s *UploadService) missingChunks(ctx context.Context, u *domain.Upload) ([]int, int64, error) {
	total := chunkCount(u.TotalBytes, DefaultUploadChunkSize)
	chunks, err := s.Store.ListUploadChunks(ctx, u.ID)
	if err != nil {
		return nil, 0, err
	}
	got := make(map[int]int64, len(chunks))
	var received int64
	for _, c := range chunks {
		got[c.ChunkIndex] = c.Size
		received += c.Size
	}
	missing := make([]int, 0)
	for i := 0; i < total; i++ {
		if _, ok := got[i]; !ok {
			missing = append(missing, i)
		}
	}
	return missing, received, nil
}

// receivedBytes 读取最新的已接收字节数。
func (s *UploadService) receivedBytes(ctx context.Context, u *domain.Upload) (int64, error) {
	fresh, err := s.Store.GetUpload(ctx, u.ID)
	if err != nil {
		return 0, err
	}
	return fresh.ReceivedBytes, nil
}

// findChunk 查找已登记分块，返回记录与其文件大小（文件缺失时返回 nil）。
func (s *UploadService) findChunk(ctx context.Context, stagingDir, uploadID string, index int) (*domain.UploadChunk, int64, error) {
	chunks, err := s.Store.ListUploadChunks(ctx, uploadID)
	if err != nil {
		return nil, 0, err
	}
	for i := range chunks {
		if chunks[i].ChunkIndex != index {
			continue
		}
		info, statErr := os.Stat(partFilePath(stagingDir, index))
		if statErr != nil {
			return nil, 0, nil
		}
		return &chunks[i], info.Size(), nil
	}
	return nil, 0, nil
}

// chunkRelPath 返回分块起始位置所属文件的相对路径（仅用于登记信息）。
func (s *UploadService) chunkRelPath(u *domain.Upload, index int) string {
	files, err := s.filePlan(u, "")
	if err != nil {
		return ""
	}
	off := int64(index) * DefaultUploadChunkSize
	for i := range files {
		if off >= files[i].Offset && off < files[i].Offset+files[i].Size {
			return files[i].RelPath
		}
	}
	return ""
}

// ensureUploadAllowed 校验用户配额与目标根所在卷的水位。
func (s *UploadService) ensureUploadAllowed(ctx context.Context, userID, repoName, root string, size int64) error {
	owner, err := s.Store.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !owner.Enabled {
		return apperr.New("user.disabled", 409).WithArg("user_id", userID)
	}
	if owner.QuotaBytes > 0 && owner.UsedBytes+size > owner.QuotaBytes {
		return apperr.RepoQuotaExceeded(owner.QuotaBytes, owner.UsedBytes, size)
	}
	if _, err := s.Store.GetRepositoryByName(ctx, repoName); err == nil {
		return apperr.RepoNameTaken().WithArg("name", repoName)
	} else if !isNotFound(err) {
		return err
	}
	return s.Repos.ensureVolumeFreeAt(ctx, root, size)
}

// pickUploadGuard 为上传会话选择目标根。
//
//   - storageID 非空 → 必须存在且启用（与 RepoService.Create 相同语义：storage.not_found / storage.disabled），
//     直接使用该存储的根，保证暂存与后续建盘同根（原子改名的前提）；
//   - storageID 为空 → 在所有启用存储里按"所在卷可用空间最大"自动选根（Pick 语义）。
func (s *UploadService) pickUploadGuard(ctx context.Context, storageID string, need int64) (domain.PathGuard, error) {
	if storageID != "" {
		st, err := s.Store.GetStorage(ctx, storageID)
		if err != nil {
			return domain.PathGuard{}, err
		}
		if !st.Enabled {
			return domain.PathGuard{}, apperr.StorageDisabled().WithArg("id", st.ID)
		}
		return domain.NewPathGuard(st.Path), nil
	}
	guards, err := s.storageGuards(ctx)
	if err != nil {
		return domain.PathGuard{}, err
	}
	return guards.Pick(ctx, s.volumeProvider(), need)
}

// guardForUpload 返回上传会话应使用的目标根守卫。
//
// 处理方式：创建会话时已把 staging 建在选定根下，这里从 u.StagingDir 反推该根，
// 从而保证"建盘与暂存同卷"。这与老记录天然兼容——老记录的 staging_dir 仍落在旧的
// 单根（即 roots[0]）下，反推即可命中；只有 staging 不在任何已配置根下时才退化为
// 第一个根并记 WARN（不会让老数据失效，只是失去"同卷"这一优化）。
func (s *UploadService) guardForUpload(ctx context.Context, u *domain.Upload) (domain.PathGuard, error) {
	guards, err := s.storageGuards(ctx)
	if err != nil {
		return domain.PathGuard{}, err
	}
	if u != nil && strings.TrimSpace(u.StagingDir) != "" {
		if g, ok := guards.GuardForPath(u.StagingDir); ok {
			return g, nil
		}
		s.Log.Warn("上传暂存目录不在任何已配置根下，退化为第一个根",
			"upload_id", u.ID, "staging_dir", u.StagingDir)
	}
	if g, ok := guards.First(); ok {
		return g, nil
	}
	return domain.PathGuard{}, apperr.InvalidParam("path")
}

// isComplete 判断已接收字节是否覆盖全部内容。
func (s *UploadService) isComplete(received, total int64) bool { return received >= total }

// ---- 纯函数辅助 ----

// uploadCompleteIdemKey 生成完成任务的幂等键。
func uploadCompleteIdemKey(uploadID string) string { return "upload_complete:" + uploadID }

// apperrConflictState 构造"状态不允许"的冲突错误。
func apperrConflictState(state string) error {
	return apperr.New("upload.state_invalid", 409).WithArg("state", state)
}

// chunkCount 计算分块总数。
func chunkCount(total, chunkSize int64) int {
	if total <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((total + chunkSize - 1) / chunkSize)
}

// chunkSizeAt 返回指定索引分块的字节数。
func chunkSizeAt(index int, total, chunkSize int64) int64 {
	if total <= 0 || chunkSize <= 0 {
		return 0
	}
	offset := int64(index) * chunkSize
	if remaining := total - offset; remaining < chunkSize {
		return remaining
	}
	return chunkSize
}

// allChunkIndices 返回 0..n-1。
func allChunkIndices(total, chunkSize int64) []int {
	n := chunkCount(total, chunkSize)
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, i)
	}
	return out
}

// partFilePath 返回分块文件路径。
func partFilePath(stagingDir string, index int) string {
	return filepath.Join(stagingDir, fmt.Sprintf("%d.part", index))
}

// sanitizeManifestPath 规范化并校验清单中的相对路径（见 5.10 路径安全要点）。
func sanitizeManifestPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", apperr.InvalidParam("manifest.path")
	}
	// 统一分隔符后再判断，避免 `..\\` 之类的绕过。
	slash := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(slash, "/") {
		// 绝对路径或 UNC。
		return "", apperr.InvalidParam("manifest.path")
	}
	clean := path.Clean(slash)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", apperr.InvalidParam("manifest.path")
	}
	parts := strings.Split(clean, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", apperr.InvalidParam("manifest.path")
		}
		if strings.ContainsAny(seg, ":*?\"<>|") {
			// 冒号用于阻止 NTFS 数据流与盘符；其余为非法字符。
			return "", apperr.InvalidParam("manifest.path")
		}
		if strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, ".") {
			return "", apperr.InvalidParam("manifest.path")
		}
		if isWindowsReserved(seg) {
			return "", apperr.InvalidParam("manifest.path")
		}
	}
	return filepath.Join(parts...), nil
}

// windowsReservedNames 是 Windows 保留设备名（不含扩展名比较）。
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isWindowsReserved 判断路径段是否为 Windows 保留名（含带扩展名形式）。
func isWindowsReserved(seg string) bool {
	base := seg
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		base = seg[:i]
	}
	return windowsReservedNames[strings.ToUpper(base)]
}

// normalizeManifest 校验清单并返回规范化条目、文件数与总字节。
func normalizeManifest(m domain.UploadManifest) ([]domain.ManifestEntry, int, int64, error) {
	if len(m.Files) == 0 {
		return nil, 0, 0, apperr.InvalidParam("manifest.files")
	}
	entries := make([]domain.ManifestEntry, 0, len(m.Files))
	seen := make(map[string]bool, len(m.Files))
	var total int64
	for _, f := range m.Files {
		rel, err := sanitizeManifestPath(f.Path)
		if err != nil {
			return nil, 0, 0, err
		}
		if f.Size < 0 {
			return nil, 0, 0, apperr.InvalidParam("manifest.size")
		}
		key := strings.ToLower(filepath.ToSlash(rel))
		if seen[key] {
			return nil, 0, 0, apperr.InvalidParam("manifest.path").WithArg("reason", "duplicate")
		}
		seen[key] = true
		entries = append(entries, domain.ManifestEntry{Path: rel, Size: f.Size, Mtime: f.Mtime, SHA256: f.SHA256})
		total += f.Size
	}
	return entries, len(entries), total, nil
}

// uploadManifestRecord 是持久化到 uploads.manifest 的清单记录。
//
// 除文件清单外还携带建库选项（repo_mode / storage_id / quota_bytes）：
// uploads 表没有这些列，而"完成建库"是异步任务（读不到创建时的请求参数），
// 因此随清单一起持久化在同一个 JSON 里，供 runUploadComplete 使用。
type uploadManifestRecord struct {
	Root       string                 `json:"root"`
	Files      []domain.ManifestEntry `json:"files"`
	RepoMode   string                 `json:"repo_mode,omitempty"`
	StorageID  string                 `json:"storage_id,omitempty"`
	QuotaBytes int64                  `json:"quota_bytes,omitempty"`
}

// marshalManifest 把规范化后的清单记录序列化存储。
func marshalManifest(rec uploadManifestRecord) (string, error) {
	out, err := json.Marshal(rec)
	if err != nil {
		return "", apperr.InvalidParam("manifest").WithCause(err)
	}
	return string(out), nil
}

// parseManifest 反序列化清单记录。
func parseManifest(raw string) (*uploadManifestRecord, error) {
	var m uploadManifestRecord
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, apperr.New("upload.manifest_invalid", 500).WithCause(err)
	}
	return &m, nil
}

// treeWriter 把拼接流顺序写入文件树，跨越文件边界。
type treeWriter struct {
	files  []uploadFile
	idx    int
	f      *os.File
	remain int64
}

// newTreeWriter 打开第一个输出文件。
func newTreeWriter(files []uploadFile) (*treeWriter, error) {
	w := &treeWriter{files: files}
	if err := w.advance(); err != nil {
		return nil, err
	}
	return w, nil
}

// advance 关闭当前文件并打开下一个非空文件（空文件直接创建）。
func (w *treeWriter) advance() error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}
	for w.idx < len(w.files) {
		p := w.files[w.idx]
		if err := os.MkdirAll(filepath.Dir(p.OutPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(p.OutPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if p.Size <= 0 {
			if err := f.Close(); err != nil {
				return err
			}
			w.idx++
			continue
		}
		w.f = f
		w.remain = p.Size
		return nil
	}
	return nil
}

// Write 实现 io.Writer。
func (w *treeWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if w.f == nil {
			if w.idx >= len(w.files) {
				return total - len(p), fmt.Errorf("上传内容超过清单总大小")
			}
			if err := w.advance(); err != nil {
				return total - len(p), err
			}
			if w.f == nil {
				return total - len(p), fmt.Errorf("上传内容超过清单总大小")
			}
		}
		n := len(p)
		if int64(n) > w.remain {
			n = int(w.remain)
		}
		if _, err := w.f.Write(p[:n]); err != nil {
			return total - len(p), err
		}
		w.remain -= int64(n)
		p = p[n:]
		if w.remain == 0 {
			w.idx++
			if err := w.advance(); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

// Close 关闭当前文件句柄。
func (w *treeWriter) Close() error {
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}

// assembleTree 顺序读取全部分块并重组成文件树（幂等：先清空再重建）。
func assembleTree(u *domain.Upload, files []uploadFile) error {
	treeDir := filepath.Join(u.StagingDir, "tree")
	if err := os.RemoveAll(treeDir); err != nil {
		return apperr.New("upload.tree_invalid", 500).WithArg("reason", "clean").WithCause(err)
	}
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		return apperr.New("upload.tree_invalid", 500).WithArg("reason", "mkdir").WithCause(err)
	}
	w, err := newTreeWriter(files)
	if err != nil {
		return apperr.New("upload.tree_invalid", 500).WithArg("reason", "writer").WithCause(err)
	}
	count := chunkCount(u.TotalBytes, DefaultUploadChunkSize)
	for i := 0; i < count; i++ {
		part := partFilePath(u.StagingDir, i)
		expect := chunkSizeAt(i, u.TotalBytes, DefaultUploadChunkSize)
		info, statErr := os.Stat(part)
		if statErr != nil || info.Size() != expect {
			_ = w.Close()
			return apperr.New("upload.chunk_missing", 500).WithArg("index", i)
		}
		f, openErr := os.Open(part)
		if openErr != nil {
			_ = w.Close()
			return apperr.New("upload.chunk_missing", 500).WithArg("index", i).WithCause(openErr)
		}
		_, copyErr := io.Copy(w, f)
		_ = f.Close()
		if copyErr != nil {
			_ = w.Close()
			return apperr.New("upload.tree_invalid", 500).WithArg("reason", "assemble").WithCause(copyErr)
		}
	}
	if err := w.Close(); err != nil {
		return apperr.New("upload.tree_invalid", 500).WithArg("reason", "close").WithCause(err)
	}
	return nil
}

// treeMatches 校验树中每个文件的类型与大小与清单一致（拒绝符号链接）。
func treeMatches(files []uploadFile) bool {
	for i := range files {
		info, err := os.Lstat(files[i].OutPath)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false
		}
		if info.Size() != files[i].Size {
			return false
		}
	}
	return true
}

// countTree 统计目录树下的常规文件数与总字节数。
func countTree(root string) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.Walk(root, func(_ string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("目录树中存在符号链接")
		}
		if fi.Mode().IsRegular() {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}
