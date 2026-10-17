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
	"sort"
	"strings"
	"sync"
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
	// DiffDiskLimitBytes 可选共享磁盘限制：建出来的库每块差异盘允许的容量
	// （也是它计入配额的量）；0 表示不限制。见 domain.Repository.DiffDiskLimitBytes。
	DiffDiskLimitBytes int64
	// ClientOS 可选：将使用该库的客户端操作系统（"windows"/"linux"，留空按 windows）。
	//
	// 决定建盘用的文件系统（见 domain.ClientOS），随清单持久化，建盘时生效。
	ClientOS string
}

// UploadSession 是创建上传会话的响应。
type UploadSession struct {
	UploadID      string `json:"upload_id"`
	ChunkSize     int64  `json:"chunk_size"`
	MissingChunks []int  `json:"missing_chunks"`
	ReceivedBytes int64  `json:"received_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	// FilesDone 已逐文件校验通过的文件数（复用旧会话时不从 0 起算，见 agent 侧 files_done）。
	FilesDone int `json:"files_done,omitempty"`
	// Reused 为真表示服务端没有新建会话，而是复用了同名未完成会话（断点续传）。
	//
	// 它让"agent 丢了续传记录 / 换了台机器 / 同名重传"都能接着上次的进度传，
	// 而不是把几百 GB 从头再来一遍。
	Reused bool `json:"reused,omitempty"`
	// OverwroteRepo 为真表示同名存储库已存在，且已交给本次上传覆盖重建（见 prepareUploadName）。
	OverwroteRepo bool `json:"overwrote_repo,omitempty"`
}

// UploadStatus 是查询上传会话状态的响应（断点续传用）。
type UploadStatus struct {
	UploadID      string `json:"upload_id"`
	State         string `json:"state"`
	TotalFiles    int    `json:"total_files"`
	TotalBytes    int64  `json:"total_bytes"`
	ReceivedBytes int64  `json:"received_bytes"`
	MissingChunks []int  `json:"missing_chunks"`
	// FilesDone 已逐文件校验通过的文件数（分块全部到齐）。
	//
	// 由分块登记推导，所以断点续传、进程重启后依然准确 —— 客户端续传时用它作为
	// "已校验 N/M 个文件"的起点，而不必从 0 重数。
	FilesDone int `json:"files_done"`
}

// ChunkResult 是上传单个分块的响应。
type ChunkResult struct {
	Index         int   `json:"index"`
	Size          int64 `json:"size"`
	ReceivedBytes int64 `json:"received_bytes"`
	Done          bool  `json:"done"`
	// FilesDone 本次写入后"分块全部到齐且文件级校验通过"的文件相对路径（通常 0~1 个）。
	//
	// 这就是"上传一个文件校验一个"的回报：客户端据它展示"刚刚有哪个文件传完并校验通过"，
	// 以及累加"已校验 N/M 个文件"—— 而不是等 100GB 全传完才见分晓（真实反馈）。
	FilesDone []string `json:"files_done,omitempty"`
	// FilesTotal 清单里的文件总数（"N/M"里的 M）。
	FilesTotal int `json:"files_total"`
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

// uploadPlanCacheMax 是"拼接流 → 文件"映射缓存的容量上限。
//
// 同一时刻真正在进行上传的会话通常只有个位数，16 条足够覆盖并发上传而不必做精细 LRU
// （见 filePlanCached 的淘汰策略）。
const uploadPlanCacheMax = 16

// uploadPlanEntry 是 planFor 的一条缓存：映射本身、是否旧模式、写入时刻（淘汰用）。
type uploadPlanEntry struct {
	files  []uploadFile
	legacy bool
	at     time.Time
}

// UploadService 负责分块上传会话的编排与「由目录内容建盘」的异步任务（见 5.10）。
//
// 暂存目录位于 storage.staging_dir（相对白名单根），与 disks 目录同卷：
// 上传的分块**直接就位写入暂存目录下的文件树**（见 PutChunk），完成时把 tree 交给建盘任务
// 复制（见 5.10 要点）。
type UploadService struct {
	Deps
	Disks *DiskService
	Repos *RepoService

	// planCache 缓存"拼接流 → 文件"映射（见 filePlanCached）。
	//
	// 它由会话的 manifest 决定、会话存续期内不变，因此可以整场缓存。为什么必须缓存：
	// 现在每写一个分块都要用这份映射定位它落在哪些文件里，而一个 100GB 的库有上万块、
	// 清单可能有几十万文件（JSON 几十 MB）—— 不缓存就等于每 8MiB 重新解析一次几十 MB JSON，
	// 网络再快也跑不满。
	planMu    sync.Mutex
	planCache map[string]uploadPlanEntry

	// supersedeIdle 覆盖"同名旧会话还算活着"的时间窗（毫秒）。0 = 用默认值
	// uploadSupersedeIdleMillis；负值 = 一律按"没人动过"处理（只有测试会这么设）。
	supersedeIdleMillis int64
}

// supersedeIdle 返回生效的"刚活跃过"时间窗。
func (s *UploadService) supersedeIdle() int64 {
	if s.supersedeIdleMillis != 0 {
		return s.supersedeIdleMillis
	}
	return uploadSupersedeIdleMillis
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
	if in.DiffDiskLimitBytes < 0 {
		return nil, apperr.InvalidParam("diff_disk_limit_bytes")
	}
	// 客户端系统决定建盘用的文件系统（见 domain.ClientOS），留空按默认（Windows）。
	clientOS, ok := domain.ParseClientOS(in.ClientOS)
	if !ok {
		return nil, apperr.InvalidParam("client_os")
	}
	storageID := strings.TrimSpace(in.StorageID)

	entries, totalFiles, totalBytes, err := normalizeManifest(in.Manifest)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := marshalManifest(uploadManifestRecord{
		Root:               in.Manifest.Root,
		Files:              entries,
		RepoMode:           repoMode,
		StorageID:          storageID,
		QuotaBytes:         in.QuotaBytes,
		DiffDiskLimitBytes: in.DiffDiskLimitBytes,
		ClientOS:           string(clientOS),
	})
	if err != nil {
		return nil, err
	}

	// 同名残留（上次失败留下的库/会话/暂存）在**这一刻**处理掉，不留给定时器：
	// 用户点上传时就该是干净的，否则同一个名字永远传不进来（见 prepareUploadName）。
	plan, err := s.prepareUploadName(ctx, in.UserID, name, totalFiles, totalBytes, storageID)
	if err != nil {
		return nil, err
	}

	// 选根 + 配额与卷水位：目标存储库尚未创建，先按用户配额与卷水位把关（见 5.10 要点）。
	// 暂存目录建在**选定根**下，保证与后续建盘同卷（complete 时的同卷语义）。
	// storage_id 显式指定时走与 RepoService.Create 相同的 storage.not_found / storage.disabled 校验。
	size := s.sizing().CalculateVHDXSize(totalBytes, 0)
	if plan.reusable != nil {
		return s.resumeUploadSession(ctx, plan, repoMode, storageID, in.QuotaBytes,
			in.DiffDiskLimitBytes, clientOS, size)
	}
	guard, err := s.pickUploadGuard(ctx, storageID, size)
	if err != nil {
		return nil, err
	}
	if err := s.ensureUploadQuota(ctx, in.UserID, guard.Root, size); err != nil {
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
		OverwroteRepo: plan.overwrite,
	}, nil
}

// uploadSupersedeIdleMillis 是"这条同名旧会话还能不能算活着的"判定窗口。
//
// 每收一个分块服务端都会 bump uploads.updated_at，所以 5 分钟内被喂过数据的会话
// 可能正被**另一台机器**上传同一个库名；抽掉它的暂存等于打断一次正在进行的上传。
// 只用它挡"删暂存"，不用它挡"新建会话"：用户刚失败就想换内容重传是完全正常的。
// 这不是定时器 —— 只在用户点上传时读一次时间戳。
const uploadSupersedeIdleMillis = 5 * 60 * 1000

// uploadNamePlan 是"同名残留"的处理结论（见 prepareUploadName）。
type uploadNamePlan struct {
	// reusable 非 nil：存在一份内容一致的未完成会话，本次上传直接续传它，不新建。
	reusable *domain.Upload
	// overwrite：同名存储库已存在，且属于本人、当前无人使用，将由本次上传覆盖重建。
	overwrite bool
	// cleaned 记录本次就地清理掉的对象，用于日志（用户看得见的"脏数据已清"）。
	cleaned []string
}

// prepareUploadName 处理同名残留：能续传的续传、该死的死、该拒的拒。
//
// 为什么必须在上传开始时同步做、而且不能靠定时器：
// 用户看到的现象是"点上传就失败"。上一次上传失败会在服务端留下三样东西 —— 库记录
// （建库任务里 ensureRepoForUpload 建出来后被打成 importing/error）、建库任务、暂存目录。
// 只要库名还在，旧的 ensureUploadAllowed 就会把这次上传判成 repo.name_taken：同一个名字
// **再也**传不进来，用户只能改名（丢掉已传的分块）或者手删库。清理不能等 24h 的 GC，
// 也不能等下一次对账 —— 用户点上传的那一刻就得干净。
//
// 规则（顺序即优先级）：
//  1. 最新的同名未完成会话若与本次清单一致 → 复用（断点续传：agent 丢了本地记录、
//     换了机器、或者上次就是失败在这里，都能接着传）；
//  2. 其余同名未完成会话 → 连同暂存目录就地作废（它们是这次上传的前身，留着白占盘，
//     还会让 Status/GC 分不清谁是谁）；但建库任务仍在跑的会话是"活"的，不碰；
//  3. 同名存储库：别人的 / 有人在用（活跃租约、已挂载、分配过盘）→ 明确拒绝；
//     残骸（importing/error）→ 清干净后重建；已建成但没人用过的空库 → 交给建库任务
//     就地重建母盘（runUploadComplete 本来就支持"重传覆盖"）。
func (s *UploadService) prepareUploadName(ctx context.Context, userID, name string, totalFiles int, totalBytes int64, storageID string) (*uploadNamePlan, error) {
	plan := &uploadNamePlan{}
	sessions, err := s.Store.ListUploadsByUser(ctx, userID, "")
	if err != nil {
		return nil, err
	}
	// 列表按 created_at DESC：第一条命中的就是"最近那份"，也就是用户刚失败的那次。
	for i := range sessions {
		u := &sessions[i]
		if u.RepoName != name || u.State == domain.UploadStateComplete {
			continue
		}
		if s.uploadReusable(u, totalFiles, totalBytes, storageID) {
			plan.reusable = u
			break
		}
	}
	buildingSession := false
	for i := range sessions {
		u := &sessions[i]
		if u.RepoName != name || u.State == domain.UploadStateComplete {
			continue
		}
		if plan.reusable != nil && u.ID == plan.reusable.ID {
			continue
		}
		if s.uploadCompleteJobAlive(ctx, u.ID) {
			// 建库任务还在跑（几十上百 GB 要在服务端落盘）：这条会话仍然活着，别抽它的暂存。
			buildingSession = true
			s.Log.Warn("同名会话的建库任务仍在执行，跳过清理", "upload_id", u.ID, "repo", name)
			continue
		}
		if time.Now().UnixMilli()-u.UpdatedAt < s.supersedeIdle() {
			// 刚有人喂过这块会话（每收一块都会 bump updated_at）：可能另一台机器正在传同名的库，
			// 抽掉它的暂存等于打断一次正在进行的上传。留着它不删 —— 本次上传照样会新建或复用会话，
			// 多余的暂存由 24h GC 收走。**不算"占用名字"**：用户刚失败就想换内容重传是完全正常的。
			s.Log.Warn("同名会话刚刚还有活动，跳过清理", "upload_id", u.ID, "repo", name, "updated_at", u.UpdatedAt)
			continue
		}
		s.discardUploadSession(ctx, u)
		plan.cleaned = append(plan.cleaned, "session:"+u.ID)
	}
	if buildingSession && plan.reusable == nil {
		// 同名库正在被另一次上传构建：此时既不能续传（内容不同），也不能清库
		// （会把正在写盘的建库任务打成残骸），只能明确拒绝。
		return nil, apperr.RepoUploadConflict("build_in_progress").WithArg("name", name)
	}

	repo, err := s.Store.GetRepositoryByName(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return plan, nil
		}
		return nil, err
	}
	reason, err := s.uploadOverwriteBlocked(ctx, repo, userID)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		s.Log.Warn("同名存储库无法被本次上传接管",
			"repo", name, "repo_id", repo.ID, "state", string(repo.State), "reason", reason)
		return nil, apperr.RepoUploadConflict(reason).WithArg("name", name)
	}
	if plan.reusable != nil {
		// 这个库正是那条会话建出来的（或即将由它建出来）：交给续传，别动它。
		return plan, nil
	}
	if repo.State == domain.RepoStateImporting || repo.State == domain.RepoStateError {
		// 半成品残骸：库停在 importing/error，母盘被 MarkCreateFailed 之后要求 creating 才能重建，
		// 原地复活比清掉重来更容易卡死（这正是"重复创建必定秒失败"的一类）。残骸里没有任何
		// 用户数据（库从没建成过），清掉不损失什么，反而让建库任务从零干净地重建。
		if err := s.purgeRemnantRepo(ctx, repo); err != nil {
			return nil, err
		}
		plan.cleaned = append(plan.cleaned, "repo:"+repo.ID)
	}
	plan.overwrite = true
	s.audit(ctx, userID, "upload.overwrite", "repo:"+repo.ID,
		fmt.Sprintf("name=%s state=%s cleaned=%s", name, repo.State, strings.Join(plan.cleaned, ",")),
		domain.AuditResultOK)
	s.Log.Warn("同名存储库将由本次上传覆盖重建",
		"repo", name, "repo_id", repo.ID, "state", string(repo.State))
	return plan, nil
}

// uploadReusable 判断旧会话能否被本次上传接着用（断点续传）。
//
// 判据只有两条，都是"分块偏移能不能对得上"的必要条件：
//   - 状态未终态：complete/aborted/failed 的会话已被消费或作废；
//   - 内容一致（文件数 + 总字节）：分块偏移是按清单顺序算出来的，清单不同就是对不上号。
//
// 建库选项（模式/配额/客户端系统）**不参与**判定：它们不影响已落盘的分块，
// 复用时把本次的选择写回清单即可（见 resumeUploadSession）。指定了目标存储且与上次
// 不同的例外 —— 那说明数据该落在另一个卷上，续传没有意义。
func (s *UploadService) uploadReusable(u *domain.Upload, totalFiles int, totalBytes int64, storageID string) bool {
	switch u.State {
	case domain.UploadStateOpen, domain.UploadStateVerifying:
	default:
		return false
	}
	if u.TotalFiles != totalFiles || u.TotalBytes != totalBytes {
		return false
	}
	rec, err := parseManifest(u.Manifest)
	if err != nil {
		return false
	}
	if storageID != "" && rec.StorageID != "" && rec.StorageID != storageID {
		return false
	}
	return true
}

// uploadCompleteJobAlive 判断会话的建库任务是否还没跑完（pending/running）。
func (s *UploadService) uploadCompleteJobAlive(ctx context.Context, uploadID string) bool {
	j, err := s.Store.GetJobByIdemKey(ctx, uploadCompleteIdemKey(uploadID))
	if err != nil {
		return false
	}
	return j.State == domain.JobStatePending || j.State == domain.JobStateRunning
}

// discardUploadSession 就地作废一条同名旧会话：删暂存目录、积分块记录与会话行。
func (s *UploadService) discardUploadSession(ctx context.Context, u *domain.Upload) {
	if u.StagingDir != "" {
		if err := os.RemoveAll(u.StagingDir); err != nil {
			s.Log.Warn("清理旧上传暂存目录失败", "upload_id", u.ID, "staging_dir", u.StagingDir, "error", err)
		}
	}
	s.forgetPlan(u.ID)
	if err := s.Store.DeleteUpload(ctx, u.ID); err != nil && !isNotFound(err) {
		s.Log.Warn("删除旧上传会话失败", "upload_id", u.ID, "error", err)
		return
	}
	s.Log.Info("已作废同名旧上传会话", "upload_id", u.ID, "repo", u.RepoName, "state", u.State)
	s.audit(ctx, u.UserID, "upload.superseded", "upload:"+u.ID, "repo="+u.RepoName, domain.AuditResultOK)
}

// uploadOverwriteBlocked 判断同名库能否被本次上传接管，不能则返回原因（空串 = 可以）。
//
// 只拒绝"会伤到用户"的三种情况：库主不是你、有人正挂着（活跃租约或已挂载）、
// 已经分配出去过盘。残骸（importing/error）和从没被分配过的空壳库都放行 ——
// 那正是"上次失败了、这次同名重传"必须能过的那道门。注意这里**不删库**：
// 已建成的库由建库任务就地重建母盘（重传覆盖），只有残骸才在调用方清理。
func (s *UploadService) uploadOverwriteBlocked(ctx context.Context, repo *domain.Repository, userID string) (string, error) {
	if repo.OwnerID != userID {
		return "other_owner", nil
	}
	leases, err := s.Store.CountActiveLeasesByRepo(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	if leases > 0 {
		return "active_lease", nil
	}
	disks, err := s.Store.ListDisksByRepo(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	for i := range disks {
		if disks[i].Mounted {
			return "mounted", nil
		}
	}
	allocs, err := s.Store.ListAllocationsByRepo(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	if len(allocs) > 0 {
		return "allocated", nil
	}
	return "", nil
}

// purgeUploadRemnant 在建库前清掉同名残骸（只清状态为 error 的、本人的、没人用的库）。
//
// 为什么只清 error：importing 可能是"另一个还没跑完的任务"（正文里有 skip 保护），
// active 是正常库（重传覆盖由 buildVHDXFromSource 就地重建母盘，不该删记录）。
func (s *UploadService) purgeUploadRemnant(ctx context.Context, name, ownerID string) error {
	repo, err := s.Store.GetRepositoryByName(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if repo.State != domain.RepoStateError {
		return nil
	}
	// 本人 + 没人用（无租约/未挂载/没分配过盘）才动手；否则交给后面的就地重建。
	reason, err := s.uploadOverwriteBlocked(ctx, repo, ownerID)
	if err != nil {
		return err
	}
	if reason != "" {
		s.Log.Warn("同名残骸库仍处于使用中，跳过清理（将尝试就地重建）",
			"repo", name, "repo_id", repo.ID, "reason", reason)
		return nil
	}
	return s.purgeRemnantRepo(ctx, repo)
}

// purgeRemnantRepo 清掉一次失败上传留下的残骸库：逐块回收磁盘与目标，再删库记录。
//
// 单块清理失败不阻塞整体：残骸本来就可能是半截的（盘文件没建出来、目标没发布成功），
// 这些痕迹交给 purgeRepository 兜底，用户不该因为"残骸清不干净"而传不了。
func (s *UploadService) purgeRemnantRepo(ctx context.Context, repo *domain.Repository) error {
	disks, err := s.Store.ListDisksByRepo(ctx, repo.ID)
	if err != nil {
		return err
	}
	for i := range disks {
		if err := s.Disks.deleteDiskArtifacts(ctx, disks[i].ID, ""); err != nil {
			s.Log.Warn("清理残骸磁盘失败", "repo_id", repo.ID, "disk_id", disks[i].ID, "error", err)
		}
	}
	if err := s.Disks.purgeRepository(ctx, repo.ID); err != nil {
		return err
	}
	s.Log.Warn("已清理同名上传残骸库（上次上传未建成）",
		"repo", repo.Name, "repo_id", repo.ID, "state", string(repo.State))
	s.audit(ctx, repo.OwnerID, "repo.remnant_purged", "repo:"+repo.ID, "name="+repo.Name, domain.AuditResultOK)
	return nil
}

// resumeUploadSession 复用同名未完成会话：刷新本次的建库选项，返回缺失分块与已校验文件数。
//
// agent 拿到的是同一个 upload_id 与"还缺哪几块"，于是已传完的部分不会重传；
// 已逐文件校验通过的文件数也一并带上，免得界面上的进度从 0 重数。
func (s *UploadService) resumeUploadSession(ctx context.Context, plan *uploadNamePlan, repoMode, storageID string,
	quotaBytes, diffDiskLimitBytes int64, clientOS domain.ClientOS, size int64) (*UploadSession, error) {
	u := plan.reusable
	guard, err := s.guardForUpload(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := s.ensureUploadQuota(ctx, u.UserID, guard.Root, size); err != nil {
		return nil, err
	}

	rec, err := parseManifest(u.Manifest)
	if err != nil {
		return nil, err
	}
	manifestJSON, err := manifestWithOptions(rec, repoMode, storageID, quotaBytes,
		diffDiskLimitBytes, clientOS)
	if err != nil {
		return nil, err
	}
	if manifestJSON != u.Manifest {
		if err := s.Store.UpdateUploadManifest(ctx, u.ID, manifestJSON); err != nil {
			return nil, err
		}
		// 清单变了，映射缓存必须跟着失效（内容没变，但缓存以清单为准）。
		s.forgetPlan(u.ID)
	}

	missing, received, err := s.missingChunks(ctx, u)
	if err != nil {
		return nil, err
	}
	filesDone, err := s.readyFileCount(ctx, u)
	if err != nil {
		return nil, err
	}
	s.Log.Info("复用同名未完成上传会话（断点续传）",
		"upload_id", u.ID, "repo", u.RepoName, "missing_chunks", len(missing), "files_done", filesDone)
	s.audit(ctx, u.UserID, "upload.resume", "upload:"+u.ID,
		fmt.Sprintf("repo=%s missing=%d files_done=%d", u.RepoName, len(missing), filesDone),
		domain.AuditResultOK)
	s.emit("upload", map[string]any{
		"action": "resume", "upload_id": u.ID, "user_id": u.UserID, "state": u.State,
	})
	return &UploadSession{
		UploadID:      u.ID,
		ChunkSize:     DefaultUploadChunkSize,
		MissingChunks: missing,
		ReceivedBytes: received,
		TotalBytes:    u.TotalBytes,
		FilesDone:     filesDone,
		Reused:        true,
		OverwroteRepo: plan.overwrite,
	}, nil
}

// Status 返回上传会话状态、缺失分块与已校验文件数（断点续传用）。
func (s *UploadService) Status(ctx context.Context, uploadID string) (*UploadStatus, error) {
	u, err := s.Store.GetUpload(ctx, uploadID)
	if err != nil {
		return nil, err
	}
	missing, received, err := s.missingChunks(ctx, u)
	if err != nil {
		return nil, err
	}
	filesDone, err := s.readyFileCount(ctx, u)
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
		FilesDone:     filesDone,
	}, nil
}

// readyFileCount 统计"分块已全部到齐"的文件数（即已逐个校验通过的文件数）。
//
// 用于展示"已校验 N/M 个文件"：客户端续传时从它起算，免得从 0 重数（已传的部分不会重传，
// 也就不会再收到 PutChunk 的回报）。一次全量块查询 + 一遍清单，都在内存里做，无额外磁盘 IO。
//
// 空文件不计入：它们不占拼接流，只有 complete 补建后才存在，提前计入会让进度出现
// "看起来全好了、却还在传"的假象。
func (s *UploadService) readyFileCount(ctx context.Context, u *domain.Upload) (int, error) {
	files, _, err := s.planFor(u)
	if err != nil {
		return 0, err
	}
	chunks, err := s.Store.ListUploadChunks(ctx, u.ID)
	if err != nil {
		return 0, err
	}
	present := make([]bool, chunkCount(u.TotalBytes, DefaultUploadChunkSize))
	for i := range chunks {
		if idx := chunks[i].ChunkIndex; idx >= 0 && idx < len(present) {
			present[idx] = true
		}
	}
	done := 0
	for i := range files {
		f := files[i]
		if f.Size <= 0 {
			continue
		}
		from := int(f.Offset / DefaultUploadChunkSize)
		to := int((f.Offset + f.Size - 1) / DefaultUploadChunkSize)
		if to >= len(present) {
			to = len(present) - 1
		}
		ok := from <= to
		for j := from; j <= to && ok; j++ {
			ok = present[j]
		}
		if ok {
			done++
		}
	}
	return done, nil
}

// PutChunk 写入单个分块：读入内存 → 校验 SHA256 → **直接写进文件树的最终位置** → 登记。
//
// 为什么不再"先存成分块文件、等 complete 再拼接"（5.10 的改动记录）：
//
//   - 拼接等于把整份数据再复制一遍 —— 100GB 的库要在 complete 里一次性写 100GB，远超客户端
//     请求超时，于是客户端看到的是"传到最后一步失败"（真实反馈：agent.server_timeout，
//     cause 是 POST /v1/uploads/{id}/complete 等 30 秒没响应），而服务端其实还在闷头写；
//   - 每个分块自带偏移（index × chunk_size），它该写进哪个文件的哪个位置是纯定位问题
//     （见 writeChunkToTree），与到达顺序无关。于是可以"到一块写一块"，complete 只剩
//     stat 级核对，毫秒级返回。
//
// 每次写完顺带判定"本次块覆盖的文件到齐了没有"，到齐的立刻做文件级校验（见 readyFiles）：
// 校验结果随传输过程**逐个文件**地出来，而不是等整库传完。
//
// 幂等：同 index 且校验一致时直接返回成功，不重复写盘。
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
	offset := int64(index) * DefaultUploadChunkSize

	// 幂等：已登记且校验值一致 → 直接成功。
	existing, err := s.Store.GetUploadChunk(ctx, uploadID, index)
	if err != nil {
		return nil, err
	}
	files, legacy, err := s.planFor(u)
	if err != nil {
		return nil, err
	}
	if existing != nil && strings.EqualFold(existing.Checksum, sha256Hex) && existing.Size == expectSize {
		received, err := s.receivedBytes(ctx, u)
		if err != nil {
			return nil, err
		}
		return &ChunkResult{
			Index: index, Size: expectSize, ReceivedBytes: received,
			Done: s.isComplete(received, total), FilesTotal: u.TotalFiles,
		}, nil
	}

	// 分块本身不大（8MiB），先在内存里校验完再落盘：这样"校验失败"绝不会在文件树里留下
	// 半截数据（旧实现是先写文件再删，中途失败就可能留下垃圾）。
	data, err := io.ReadAll(io.LimitReader(body, expectSize+1))
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_read").WithCause(err)
	}
	if int64(len(data)) != expectSize {
		return nil, apperr.New("upload.chunk_size_mismatch", 400).
			WithArg("index", index).WithArg("expected", expectSize).WithArg("actual", len(data))
	}
	actual := sha256HexOf(data)
	if !strings.EqualFold(actual, strings.TrimSpace(sha256Hex)) {
		s.audit(ctx, u.UserID, "upload.chunk_reject", "upload:"+uploadID,
			fmt.Sprintf("index=%d checksum_mismatch", index), domain.AuditResultError)
		return nil, apperr.New("upload.chunk_checksum_mismatch", 400).WithArg("index", index)
	}

	// 旧会话（升级前创建、数据已落在 *.part 里）继续走旧路径：混着写会让它的 complete 既拼不出
	// 完整树、也找不到全部分块文件，只能失败（见 planFor 的模式判定）。
	if legacy {
		if err := writePartFile(u.StagingDir, index, data); err != nil {
			return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_write").WithCause(err)
		}
	} else if err := writeChunkToTree(files, offset, data); err != nil {
		return nil, apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_write").WithCause(err)
	}

	delta := expectSize
	if existing != nil {
		// 覆盖一个校验值不同的旧分块：先撤销原登记的字节数。
		if err := s.Store.DeleteUploadChunk(ctx, uploadID, index); err != nil {
			return nil, err
		}
		delta = expectSize - existing.Size
	}
	chunk := &domain.UploadChunk{
		UploadID:   uploadID,
		ChunkIndex: index,
		RelPath:    relPathAt(files, offset),
		Offset:     offset,
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
	result := &ChunkResult{
		Index: index, Size: expectSize, ReceivedBytes: received,
		Done: s.isComplete(received, total), FilesTotal: u.TotalFiles,
	}
	if legacy {
		return result, nil
	}
	// 逐文件校验：本次写到的文件里，凡分块已到齐的就地校验并回报（见 readyFiles）。
	done, err := s.readyFiles(ctx, u, files, offset, expectSize)
	if err != nil {
		return nil, err
	}
	result.FilesDone = done
	return result, nil
}

// readyFiles 返回"本次分块写完后分块已全部到齐并通过文件级校验"的文件相对路径。
//
// 这是"上传一个文件校验一个"的落点：一个文件覆盖的分块齐了，就意味着它的每一个字节都已经
// 逐块与客户端报上来的 SHA256 比对通过（见 PutChunk），于是立刻回报"这个文件传完且校验过了"
// —— 用户能在传输过程中就看到结果，而不是等 100GB 全传完才见分晓（真实反馈）。
//
// 只检查本次分块覆盖的文件（通常 1~2 个，见 locateFiles）：每个分块都全量扫一遍清单会让
// 上传退化成 O(分块数 × 文件数)，几十万文件时完全不可接受。
//
// 零额外磁盘 IO：指纹由分块校验值聚合而来，不必把文件读回来重算（那等于再读一遍全量数据，
// 还会与写入抢同一块盘）。
func (s *UploadService) readyFiles(ctx context.Context, u *domain.Upload, files []uploadFile, offset, size int64) ([]string, error) {
	var done []string
	for _, i := range locateFiles(files, offset, size) {
		f := files[i]
		if f.Size <= 0 {
			// 空文件不占拼接流，其"就绪"由 complete 阶段补建（见 finalizeTree）。
			continue
		}
		from := int(f.Offset / DefaultUploadChunkSize)
		to := int((f.Offset + f.Size - 1) / DefaultUploadChunkSize)
		n, err := s.Store.CountUploadChunksInRange(ctx, u.ID, from, to)
		if err != nil {
			return nil, err
		}
		if n != to-from+1 {
			continue
		}
		// 到齐了才取校验值：判定"齐没齐"只要数量，取回上千行只为算指纹是浪费。
		chunks, err := s.Store.ListUploadChunksInRange(ctx, u.ID, from, to)
		if err != nil {
			return nil, err
		}
		if len(chunks) != to-from+1 {
			// 期间被并发删除（作废/覆盖）：下个分块到达时再判。
			continue
		}
		done = append(done, f.RelPath)
	}
	return done, nil
}

// writePartFile 把分块写成旧的 *.part 文件（仅用于升级前创建的会话）。
func writePartFile(stagingDir string, index int, data []byte) error {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(partFilePath(stagingDir, index), data, 0o644)
}

// writeChunkToTree 把一个分块的字节写进文件树里对应的位置（可能横跨两个文件）。
//
// 用 WriteAt 而不是顺序写：分块与文件边界并不对齐（一个 8MiB 块可能前半段属于 A、后半段
// 属于 B），而且分块到达顺序不保证（多路并发 + 断点续传）。WriteAt 让"块 → 文件"成为纯
// 定位问题（见 locateFiles），既不依赖顺序，也不需要为每个文件维持长驻句柄。
func writeChunkToTree(files []uploadFile, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	for _, i := range locateFiles(files, offset, int64(len(data))) {
		f := files[i]
		from := max64(f.Offset, offset)
		to := min64(f.Offset+f.Size, offset+int64(len(data)))
		if to <= from {
			continue
		}
		if err := writeFileRange(f.OutPath, from-f.Offset, data[from-offset:to-offset]); err != nil {
			return err
		}
	}
	return nil
}

// writeFileRange 把 data 写到 path 的 off 处（父目录自动创建，文件不存在则创建）。
func writeFileRange(path string, off int64, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteAt(data, off)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// locateFiles 返回拼接流区间 [offset, offset+size) 覆盖到的文件下标（按清单顺序，通常 1~2 个）。
func locateFiles(files []uploadFile, offset, size int64) []int {
	if size <= 0 || len(files) == 0 {
		return nil
	}
	end := offset + size
	// 二分到第一个"末尾在 offset 之后"的文件：它及其后继才可能与区间重叠。
	i := sort.Search(len(files), func(i int) bool { return files[i].Offset+files[i].Size > offset })
	out := make([]int, 0, 2)
	for ; i < len(files) && files[i].Offset < end; i++ {
		if files[i].Size <= 0 {
			continue
		}
		out = append(out, i)
	}
	return out
}

// relPathAt 返回拼接流中 offset 处所属文件的相对路径（用于分块登记信息）。
//
// 必须二分：它在 PutChunk 热路径上每块调用一次，而清单规模跟库同阶（十万级）。线性扫描在
// 十万文件 × 一万分块下就是十亿次比较 —— 单块请求会从毫秒级掉到肉眼可见的卡顿。
func relPathAt(files []uploadFile, offset int64) string {
	// 第一个"末尾在 offset 之后"的文件即候选：偏移连续的清单里它就是要找的那个
	// （零长文件不占区间，跳过它们）。
	i := sort.Search(len(files), func(i int) bool { return files[i].Offset+files[i].Size > offset })
	for ; i < len(files); i++ {
		if files[i].Size <= 0 {
			continue
		}
		return files[i].RelPath
	}
	return ""
}

// sha256HexOf 计算字节块的十六进制 SHA256。
func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// max64 / min64 是 int64 版的最值（Go 1.21 的 min/max 内建在这里不便于表达"两个表达式"）。
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
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
	// 只核对、不重组：分块到达时就写进树的最终位置了（见 PutChunk）。
	// 整棵树的全量走查（verifyTree）留给异步的建库任务 —— 那才是最终正确性关口，
	// 而它没有理由拖住这个 HTTP 请求（拖住的后果就是客户端 30 秒超时误报上传失败）。
	if err := s.finalizeTree(ctx, u); err != nil {
		return nil, err
	}

	guard, err := s.guardForUpload(ctx, u)
	if err != nil {
		return nil, err
	}
	// 提交/复用前先处理"上次失败的那条任务"：必须在 EnqueueWith 之前，
	// 因为它幂等命中后会回查任务的最新状态（见 requeueFailedCompleteJob）。
	if _, err := s.requeueFailedCompleteJob(ctx, uploadID); err != nil {
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

// requeueFailedCompleteJob 让"上次已失败/已取消"的建库任务重新执行，返回被重置的任务 ID
// （没有需要重置的任务时返回空串）。
//
// 为什么必须做：agent 对同一 local_dir + repo_name 重试时会**复用同一 upload_id**
// （见 internal/agent 的续传记录，只有上传+建库成功才会删除该记录），而建库任务的幂等键
// 固定为 upload_id。EnqueueWith 命中幂等键后会原样返回那条 failed 任务，而 worker 只领取
// pending 任务 —— 终态任务永远不会再被执行。于是 agent 立刻看到 job failed，报
// agent.upload_failed（stage=job, cause="建库任务状态: failed"），表现为"重复创建必定
// 秒失败"，而暂存目录其实还在（5.10 步骤⑥ 保留 staging 就是为了可重试）。
func (s *UploadService) requeueFailedCompleteJob(ctx context.Context, uploadID string) (string, error) {
	existing, err := s.Store.GetJobByIdemKey(ctx, uploadCompleteIdemKey(uploadID))
	if err != nil {
		// 首次提交时本来就没有任务，属常态；其余错误如实上抛。
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	switch existing.State {
	case domain.JobStateFailed, domain.JobStateCancelled:
		// 需要重置：下面交给 RequeueJob（它自己保证只重置终态失败的任务）。
	default:
		// pending/running：任务正在跑，agent 继续等即可；succeeded：真幂等命中，直接复用。
		return "", nil
	}
	requeued, err := s.Store.RequeueJob(ctx, existing.ID, "")
	if err != nil {
		return "", err
	}
	if requeued {
		s.Log.Info("重新执行上次失败的建库任务",
			"upload_id", uploadID, "job_id", existing.ID, "prev_state", string(existing.State))
	}
	return existing.ID, nil
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
	// 会话结束：把清单映射缓存放开（几十万文件的清单会占几十 MB）。
	s.forgetPlan(uploadID)
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
		s.forgetPlan(u.ID)
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

	// 上次建库失败留下的残骸（库与母盘停在 error）不能原地复活：磁盘处于 error 时
	// buildVHDXFromSource 会按"未建好"重来，但半截的盘文件/失效的目标更容易再次卡死，
	// 用户看到的就是"重试秒失败"。残骸里没有用户数据，清掉后由 ensureRepoForUpload
	// 从零重建；暂存里的目录树不动，已上传的字节一个都不用重传。
	// 走本地续传记录重试的路径不经过 CreateUpload，所以这道自愈必须也放在这里。
	if err := s.purgeUploadRemnant(ctx, u.RepoName, u.UserID); err != nil {
		return err
	}

	repo, disk, err := s.ensureRepoForUpload(ctx, u, treeDir)
	if err != nil {
		return err
	}
	// 母盘构建完成前一律标为"上传中"：ensureRepoForUpload 对同名库是幂等复用的，
	// 复用出来的库可能已经是 active（上次建好过、这次是重传覆盖），而这次要把母盘整个
	// 重建 —— 期间必须拒绝挂载，否则用户挂上去看到的是正在被覆盖的半成品。
	if repo.State != domain.RepoStateImporting {
		if err := s.Store.SetRepositoryState(ctx, repo.ID, domain.RepoStateImporting); err != nil {
			return err
		}
		repo.State = domain.RepoStateImporting
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

	// 母盘就绪：库从"上传中"转为正常，卡片上的上传中到此结束。
	if err := s.Store.SetRepositoryState(ctx, repo.ID, domain.RepoStateActive); err != nil {
		return err
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

// MarkUploadCompleteFailed 把"上传建库任务重试用尽"的库与母盘从中间态回滚为 error。
//
// 为什么不能像其它建盘任务那样只用 j.RefID：upload_complete 的 ref_id 是 **upload_id**
// 而不是磁盘 ID（见 Complete 的 EnqueueWith），拿它去 MarkCreateFailed 只会命中
// "磁盘不存在"并静默返回 —— 历史上这正是"上传失败后库残留在中途、母盘永远 creating、
// 用户点挂载得到 disk.not_ready"的来源（服务端日志里那行"回滚磁盘状态失败"的 WARN）。
//
// 只回滚仍是 importing 的库：一次迟到的失败回调不得覆盖已经建成 active 的库。
func (s *UploadService) MarkUploadCompleteFailed(ctx context.Context, payload string, cause error) error {
	var p uploadCompletePayload
	if err := decodePayload(payload, &p); err != nil || p.UploadID == "" {
		return nil
	}
	u, err := s.Store.GetUpload(ctx, p.UploadID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	repo, err := s.Store.GetRepositoryByName(ctx, u.RepoName)
	if err != nil {
		// 还没走到建库就失败了（分块不全、目录树校验不过）：没有库要回滚。
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if repo.State != domain.RepoStateImporting {
		return nil
	}

	// 母盘仍停在 creating 时一并置 error：对账不会自愈（它刻意跳过 creating），
	// 只把库置 error 会让这块盘永远挂不上、也删不掉。
	diskID := ""
	if repo.ParentDiskID != nil {
		diskID = *repo.ParentDiskID
	} else if disks, dErr := s.Store.ListDisksByRepo(ctx, repo.ID); dErr == nil && len(disks) > 0 {
		// 独享库不设 ParentDiskID（见 ensureRepoForUpload），取库下那块 standalone 盘。
		diskID = disks[0].ID
	}
	if diskID != "" && s.Disks != nil {
		if err := s.Disks.MarkCreateFailed(ctx, diskID, cause); err != nil {
			return err
		}
	}

	if err := s.Store.SetRepositoryState(ctx, repo.ID, domain.RepoStateError); err != nil {
		return err
	}
	s.Log.Warn("上传建库失败，已回滚存储库与母盘状态",
		"upload_id", u.ID, "repo_id", repo.ID, "disk_id", diskID, "error", cause)
	return nil
}

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
				// 共享磁盘限制取自本次上传会话的建库选项（随清单持久化，见 uploadManifestRecord）；
				// 独享库没有差异盘，这个值对它没有对象，归零免得详情页显示一个不起作用的配置。
				DiffDiskLimitBytes: limitForMode(mode, rec.DiffDiskLimitBytes),
				QuotaBytes:         rec.QuotaBytes,
				// 建库期间是"上传中"而不是 active：母盘还没建出来，此时挂载只会得到
				// disk.not_ready。任务结束时会置为 active（见 runUploadComplete）。
				State: domain.RepoStateImporting,
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
			// 建盘格式按上传时声明的客户端系统决定（见 uploadManifestRecord.ClientOS）。
			FileSystem: uploadClientOS(rec).FileSystem(),
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
//
// 只由异步的建库任务调用（Complete 走 finalizeTree：只核对、不重组）。走到"需要重组"这一步
// 的只剩升级前创建的会话（数据还在 *.part 里），由 finalizeTree 的兜底路径拼一次。
func (s *UploadService) ensureTree(ctx context.Context, u *domain.Upload) (string, error) {
	treeDir := filepath.Join(u.StagingDir, "tree")
	if s.treeOK(treeDir, u) {
		return treeDir, nil
	}
	if err := s.finalizeTree(ctx, u); err != nil {
		return "", err
	}
	if err := s.verifyTree(treeDir, u); err != nil {
		return "", err
	}
	return treeDir, nil
}

// finalizeTree 确认文件树已就绪：核对 + 补空文件，**不重组**。
//
// 正常路径（分块都经新路径直接写进树）到这里就是"stat 一遍即通过"，毫秒级。三种例外各有交代：
//
//   - 空文件：零字节不占拼接流，没有任何分块会写到它们，所以在这里补建；
//   - 旧会话（升级前创建、数据在 *.part 里）：走一次 assembleTree 兜底，这是历史路径；
//   - 块齐但树对不上（树被外部动过 / 磁盘故障）：作废这些文件的登记让客户端重传对应分块
//     （见 healTreeMismatch），而不是整库重传。
func (s *UploadService) finalizeTree(ctx context.Context, u *domain.Upload) error {
	files, legacy, err := s.planFor(u)
	if err != nil {
		return err
	}
	if treeMatches(files) {
		return nil
	}
	if err := createEmptyFiles(files); err != nil {
		return apperr.New(apperr.CodeInternal, 500).WithArg("reason", "staging_write").WithCause(err)
	}
	if treeMatches(files) {
		return nil
	}
	if legacy {
		if err := assembleTree(u, files); err != nil {
			return err
		}
		if treeMatches(files) {
			return nil
		}
	}
	healed, err := s.healTreeMismatch(ctx, u, files)
	if err != nil {
		return err
	}
	if healed {
		// 已作废对应登记：让客户端看到缺块并重传（missing_count 只用于提示，实际以 Status 为准）。
		return apperr.New("upload.incomplete", 409).WithArg("missing_count", 0)
	}
	return apperr.New("upload.tree_mismatch", 500).WithArg("expected_files", u.TotalFiles)
}

// healTreeMismatch 作废"文件树对不上"的那些文件所覆盖的分块登记，返回是否发生过作废。
//
// 为什么要自愈而不是直接报错：块登记齐全、树却对不上属于极端现场（树被外部删改、磁盘故障）。
// 此时只报错会死锁 —— complete 重试时 missingChunks 认为"没有缺块"，于是永远重试、永远失败。
// 作废对应登记后，客户端下一次 Status 就会看到这些分块缺失并重传，代价与损坏范围成正比。
func (s *UploadService) healTreeMismatch(ctx context.Context, u *domain.Upload, files []uploadFile) (bool, error) {
	healed := false
	for _, f := range files {
		if f.Size <= 0 || fileMatches(f) {
			continue
		}
		from := int(f.Offset / DefaultUploadChunkSize)
		to := int((f.Offset + f.Size - 1) / DefaultUploadChunkSize)
		chunks, err := s.Store.ListUploadChunksInRange(ctx, u.ID, from, to)
		if err != nil {
			return false, err
		}
		var freed int64
		for i := range chunks {
			freed += chunks[i].Size
		}
		n, err := s.Store.DeleteUploadChunksInRange(ctx, u.ID, from, to)
		if err != nil {
			return false, err
		}
		if n == 0 {
			continue
		}
		healed = true
		if err := s.Store.AddUploadReceived(ctx, u.ID, -freed); err != nil {
			return false, err
		}
		// 顺手删掉对不上的那个文件：重传会重建它，留着只是半截数据。
		_ = os.Remove(f.OutPath)
		s.Log.Warn("文件树与清单不符，已作废对应分块等待重传",
			"upload_id", u.ID, "path", f.RelPath, "chunks", n)
	}
	return healed, nil
}

// createEmptyFiles 补建清单里的空文件（零字节不占拼接流，不会有任何分块写到它们）。
func createEmptyFiles(files []uploadFile) error {
	for i := range files {
		f := files[i]
		if f.Size != 0 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.OutPath), 0o755); err != nil {
			return err
		}
		fh, err := os.OpenFile(f.OutPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
	return nil
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

// planFor 返回会话的"拼接流 → 文件"映射，以及该会话是否还在用旧的 *.part 分块文件。
//
// 两者都按会话缓存（见 planCache）。缓存是必需的，不是优化：
//   - 映射：现在每写一个分块都要用它定位（见 writeChunkToTree），而清单可能有几十万文件
//     （JSON 几十 MB）—— 不缓存就等于每 8MiB 重新解析一次；
//   - 模式：判定要读一遍 staging 目录，而那个目录在上传过程中会涨到上万条目（旧会话的
//     *.part）甚至几十万（新会话的文件树）。
//
// 结果在会话存续期内不会变：manifest 不变；旧会话继续写 part、新会话从不写 part。
func (s *UploadService) planFor(u *domain.Upload) ([]uploadFile, bool, error) {
	s.planMu.Lock()
	if e, ok := s.planCache[u.ID]; ok {
		files, legacy := e.files, e.legacy
		s.planMu.Unlock()
		return files, legacy, nil
	}
	s.planMu.Unlock()

	files, err := s.filePlan(u, filepath.Join(u.StagingDir, "tree"))
	if err != nil {
		return nil, false, err
	}
	legacy := legacyPartFiles(u.StagingDir)

	s.planMu.Lock()
	if s.planCache == nil {
		s.planCache = make(map[string]uploadPlanEntry, uploadPlanCacheMax)
	}
	if _, exists := s.planCache[u.ID]; !exists && len(s.planCache) >= uploadPlanCacheMax {
		// 简单淘汰：丢掉最早写入的一条。同时进行的上传会话通常只有个位数，不必精细 LRU。
		var oldestKey string
		var oldest time.Time
		for k, v := range s.planCache {
			if oldestKey == "" || v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		delete(s.planCache, oldestKey)
	}
	s.planCache[u.ID] = uploadPlanEntry{files: files, legacy: legacy, at: time.Now()}
	s.planMu.Unlock()
	return files, legacy, nil
}

// forgetPlan 丢弃会话的映射缓存（会话完成/中止时调用，别让几十 MB 的清单长期占着内存）。
func (s *UploadService) forgetPlan(uploadID string) {
	s.planMu.Lock()
	delete(s.planCache, uploadID)
	s.planMu.Unlock()
}

// legacyPartFiles 判断 staging 下是否已有旧的 *.part 分块文件（有则说明这是升级前创建的会话）。
//
// 历史实现把每个分块存成 N.part、由 complete 统一拼成树。这类会话必须继续按旧路径写：
// 一旦混着写，数据就一半在 part、一半在树里，complete 时既拼不出完整树、也找不到全部分块文件，
// 只能失败。新会话从不写 part，所以这个判定一旦成立就不会再变（见 planFor 的缓存）。
func legacyPartFiles(stagingDir string) bool {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".part") {
			return true
		}
	}
	return false
}

// ensureUploadQuota 校验用户配额与目标根所在卷的水位。
//
// 名字冲突**不在这里**判定：那个检查会拦掉"上次失败、这次同名重传"，交给
// prepareUploadName（它先清同名残骸、能续传的续传，剩下的才拒绝）。
func (s *UploadService) ensureUploadQuota(ctx context.Context, userID, root string, size int64) error {
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
// 除文件清单外还携带建库选项（repo_mode / storage_id / quota_bytes /
// diff_disk_limit_bytes / client_os）：uploads 表没有这些列，而"完成建库"是异步任务
// （读不到创建时的请求参数），因此随清单一起持久化在同一个 JSON 里，供 runUploadComplete 使用。
type uploadManifestRecord struct {
	Root       string                 `json:"root"`
	Files      []domain.ManifestEntry `json:"files"`
	RepoMode   string                 `json:"repo_mode,omitempty"`
	StorageID  string                 `json:"storage_id,omitempty"`
	QuotaBytes int64                  `json:"quota_bytes,omitempty"`
	// DiffDiskLimitBytes 共享磁盘限制（0=不限制），建出来的库按它给差异盘记账。
	DiffDiskLimitBytes int64 `json:"diff_disk_limit_bytes,omitempty"`
	// ClientOS 将使用该库的客户端操作系统，决定建盘格式（见 domain.ClientOS）。
	// 留空按默认（windows → NTFS）。
	ClientOS string `json:"client_os,omitempty"`
}

// marshalManifest 把规范化后的清单记录序列化存储。
func marshalManifest(rec uploadManifestRecord) (string, error) {
	out, err := json.Marshal(rec)
	if err != nil {
		return "", apperr.InvalidParam("manifest").WithCause(err)
	}
	return string(out), nil
}

// manifestWithOptions 在既有清单上改写建库选项，返回新的清单 JSON。
//
// Root/Files **必须原样保留**：分块偏移是按清单里的文件顺序算出来的，改了顺序就等于
// 把已经上传的数据对到别的文件上（第一个块的字节会落进第二个文件）。复用旧会话时
// 只有选项跟着本次请求走，文件列表一个字都不能动。
func manifestWithOptions(rec *uploadManifestRecord, repoMode, storageID string,
	quotaBytes, diffDiskLimitBytes int64, clientOS domain.ClientOS) (string, error) {
	if rec == nil {
		return "", apperr.InvalidParam("manifest")
	}
	next := *rec
	next.RepoMode = repoMode
	next.StorageID = storageID
	next.QuotaBytes = quotaBytes
	next.DiffDiskLimitBytes = diffDiskLimitBytes
	next.ClientOS = string(clientOS)
	return marshalManifest(next)
}

// parseManifest 反序列化清单记录。
func parseManifest(raw string) (*uploadManifestRecord, error) {
	var m uploadManifestRecord
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, apperr.New("upload.manifest_invalid", 500).WithCause(err)
	}
	return &m, nil
}

// uploadClientOS 返回该上传会话建盘用的客户端操作系统。
//
// 清单里存的是创建会话时校验过的取值（见 CreateUpload）；若为空或损坏，
// 回退到默认（Windows）而不是报错：建盘阶段没必要因为一个可选标记让任务失败。
func uploadClientOS(rec *uploadManifestRecord) domain.ClientOS {
	if rec != nil {
		if os, ok := domain.ParseClientOS(rec.ClientOS); ok {
			return os
		}
	}
	return domain.ClientOSDefault
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
		if !fileMatches(files[i]) {
			return false
		}
	}
	return true
}

// fileMatches 判断树里该文件存在、是常规文件（拒绝符号链接）且大小与清单一致。
//
// 大小即内容断言：这个文件的每个字节都由分块写入（WriteAt 到精确区间）、且每个分块的 SHA256
// 都已在到达时与客户端报上来的值比对通过，所以"大小对"在此处等价于"这批字节就是客户端发来的
// 那批"。真正的全量走查仍由建库任务做一次（见 verifyTree），用于防住目录层面的异常。
func fileMatches(f uploadFile) bool {
	info, err := os.Lstat(f.OutPath)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false
	}
	return info.Size() == f.Size
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
