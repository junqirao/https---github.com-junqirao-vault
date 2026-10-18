package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- jobs ----------

const jobCols = `id, type, ref_id, idem_key, payload, state, progress, attempt, last_error,
	lock_key, created_at, started_at, finished_at`

type jobRow struct {
	ID         string `db:"id"`
	Type       string `db:"type"`
	RefID      string `db:"ref_id"`
	IdemKey    string `db:"idem_key"`
	Payload    string `db:"payload"`
	State      string `db:"state"`
	Progress   int    `db:"progress"`
	Attempt    int    `db:"attempt"`
	LastError  string `db:"last_error"`
	LockKey    string `db:"lock_key"`
	CreatedAt  int64  `db:"created_at"`
	StartedAt  int64  `db:"started_at"`
	FinishedAt int64  `db:"finished_at"`
}

func (r jobRow) toDomain() *domain.Job {
	return &domain.Job{
		ID: r.ID, Type: domain.JobType(r.Type), RefID: r.RefID, IdemKey: r.IdemKey,
		Payload: r.Payload, State: domain.JobState(r.State), Progress: r.Progress,
		Attempt: r.Attempt, LastError: r.LastError, LockKey: r.LockKey,
		CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

// CreateJob 创建任务。
//
// 幂等：若 idem_key 已存在，返回已存在任务的 ID 与 created=false，
// 调用方据此直接复用而不重复执行（见 docs/implementation.md 5.12）。
func (s *Store) CreateJob(ctx context.Context, j *domain.Job) (id string, created bool, err error) {
	if j.ID == "" {
		j.ID = newID()
	}
	if j.CreatedAt == 0 {
		j.CreatedAt = nowMillis()
	}
	if j.State == "" {
		j.State = domain.JobStatePending
	}
	if j.Payload == "" {
		j.Payload = "{}"
	}

	_, err = s.q.NamedExecContext(ctx,
		`INSERT INTO jobs (`+jobCols+`)
		 VALUES (:id, :type, :ref_id, :idem_key, :payload, :state, :progress, :attempt, :last_error,
		         :lock_key, :created_at, :started_at, :finished_at)`,
		map[string]any{
			"id":          j.ID,
			"type":        string(j.Type),
			"ref_id":      j.RefID,
			"idem_key":    j.IdemKey,
			"payload":     j.Payload,
			"state":       string(j.State),
			"progress":    j.Progress,
			"attempt":     j.Attempt,
			"last_error":  j.LastError,
			"lock_key":    j.LockKey,
			"created_at":  j.CreatedAt,
			"started_at":  j.StartedAt,
			"finished_at": j.FinishedAt,
		})
	if err == nil {
		return j.ID, true, nil
	}
	if !isDuplicate(err) {
		return "", false, fmt.Errorf("store: 创建任务失败: %w", err)
	}
	if j.IdemKey == "" {
		return "", false, fmt.Errorf("store: 创建任务失败: %w", err)
	}

	// 命中幂等键：回查已有任务并复用。
	existing, getErr := s.GetJobByIdemKey(ctx, j.IdemKey)
	if getErr != nil {
		return "", false, fmt.Errorf("store: 幂等任务回查失败: %w", getErr)
	}
	return existing.ID, false, nil
}

// GetJob 按 ID 查询任务。
func (s *Store) GetJob(ctx context.Context, id string) (*domain.Job, error) {
	var row jobRow
	err := s.q.GetContext(ctx, &row, s.q.Rebind(`SELECT `+jobCols+` FROM jobs WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.JobNotFound().WithArg("id", id) })
	}
	return row.toDomain(), nil
}

// GetJobByIdemKey 按幂等键查询任务。
func (s *Store) GetJobByIdemKey(ctx context.Context, key string) (*domain.Job, error) {
	var row jobRow
	err := s.q.GetContext(ctx, &row, s.q.Rebind(`SELECT `+jobCols+` FROM jobs WHERE idem_key = ?`), key)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.JobNotFound().WithArg("idem_key", key) })
	}
	return row.toDomain(), nil
}

// GetLatestJobByRef 查询以 refID 为引用对象的最近一条任务；不存在时返回 nil, nil。
//
// 建盘任务的 ref_id 就是磁盘 ID（见 app 包的 EnqueueWith 调用），
// 对账据此判断"这个建盘中的磁盘，其建盘任务是不是已经彻底失败了"。
// 返回 nil（而非错误）表示"没有相关任务"，调用方必须把它与"任务失败"区分开：
// 例如上传建库流程里的磁盘同样是 creating，但它的任务 ref_id 是上传会话 ID。
func (s *Store) GetLatestJobByRef(ctx context.Context, refID string) (*domain.Job, error) {
	if strings.TrimSpace(refID) == "" {
		return nil, nil
	}
	var row jobRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+jobCols+` FROM jobs WHERE ref_id = ? ORDER BY created_at DESC LIMIT 1`), refID)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 按引用查询任务失败: %w", err)
	}
	return row.toDomain(), nil
}

// ClaimJobs 取出一批待执行任务并原子置为 running。
//
// 采用"先查后更新"两步：SQLite 在单写者语义下安全；MySQL 下由 update 的
// `state = 'pending'` 条件保证只有一个 worker 能抢占成功。
func (s *Store) ClaimJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	var rows []jobRow
	query := `SELECT ` + jobCols + ` FROM jobs WHERE state = ? ORDER BY created_at LIMIT ?`
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), string(domain.JobStatePending), limit); err != nil {
		return nil, fmt.Errorf("store: 查询待执行任务失败: %w", err)
	}

	now := nowMillis()
	out := make([]domain.Job, 0, len(rows))
	for _, r := range rows {
		res, err := s.q.ExecContext(ctx,
			s.q.Rebind(`UPDATE jobs SET state = ?, started_at = ?, attempt = attempt + 1
			            WHERE id = ? AND state = ?`),
			string(domain.JobStateRunning), now, r.ID, string(domain.JobStatePending))
		if err != nil {
			return nil, fmt.Errorf("store: 抢占任务失败: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // 被其他 worker 抢走
		}
		j := r.toDomain()
		j.State = domain.JobStateRunning
		j.StartedAt = now
		j.Attempt++
		out = append(out, *j)
	}
	return out, nil
}

// FinishJob 标记任务结束。
func (s *Store) FinishJob(ctx context.Context, id string, st domain.JobState, lastErr string) error {
	progress := 100
	if st != domain.JobStateSucceeded {
		progress = 0
	}
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE jobs SET state = ?, last_error = ?, progress = ?, finished_at = ? WHERE id = ?`),
		string(st), lastErr, progress, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 结束任务失败: %w", err)
	}
	return mustAffect(res, apperr.JobNotFound().WithArg("id", id))
}

// UpdateJobProgress 更新任务进度（0-100）。
func (s *Store) UpdateJobProgress(ctx context.Context, id string, progress int) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	_, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE jobs SET progress = ? WHERE id = ?`), progress, id)
	if err != nil {
		return fmt.Errorf("store: 更新任务进度失败: %w", err)
	}
	return nil
}

// ResetRunningJobs 服务端重启时把"运行中"的任务重置为待执行，实现断点重试。
func (s *Store) ResetRunningJobs(ctx context.Context) (int64, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE jobs SET state = ?, started_at = 0 WHERE state = ?`),
		string(domain.JobStatePending), string(domain.JobStateRunning))
	if err != nil {
		return 0, fmt.Errorf("store: 重置运行中任务失败: %w", err)
	}
	return res.RowsAffected()
}

// ResetJobToPending 把失败的任务退回待执行，用于重试（保留 attempt 计数）。
func (s *Store) ResetJobToPending(ctx context.Context, id, lastErr string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE jobs SET state = ?, last_error = ?, started_at = 0 WHERE id = ?`),
		string(domain.JobStatePending), lastErr, id)
	if err != nil {
		return fmt.Errorf("store: 重置任务失败: %w", err)
	}
	return mustAffect(res, apperr.JobNotFound().WithArg("id", id))
}

// RequeueJob 把**已失败/已取消**的任务重新置为待执行，供"同一条任务再跑一次"的场景使用
// （例如上传完成建库任务失败后，用户重试上传：暂存目录还在，任务却已钉死在终态）。
//
// 与 ResetJobToPending 的两点区别，都是刻意为之：
//   - 只重置 failed/cancelled（状态条件写在 SQL 里保证原子性）：调用方无法预知此刻任务
//     是否正被 worker 执行，把 running/pending 重置会造成重复投递；succeeded 更不能重置。
//   - 把 attempt 清零：ResetJobToPending 是 worker **内部重试**，attempt 必须保留（否则
//     最大尝试次数形同虚设）；而这里语义是"用户重新发起一次"，必须归还完整的重试预算 ——
//     否则一条 attempt 已用尽（= MaxAttempt）的失败任务重置后，第一次失败就又直接终结。
//
// 返回 true 表示确实完成了重置（未重置时调用方应复用既有任务）。
func (s *Store) RequeueJob(ctx context.Context, id, lastErr string) (bool, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE jobs
		            SET state = ?, progress = 0, attempt = 0, last_error = ?,
		                started_at = 0, finished_at = 0
		            WHERE id = ? AND state IN (?, ?)`),
		string(domain.JobStatePending), lastErr, id,
		string(domain.JobStateFailed), string(domain.JobStateCancelled))
	if err != nil {
		return false, fmt.Errorf("store: 重新入队任务失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 重新入队任务失败: %w", err)
	}
	return n > 0, nil
}

// ListJobs 查询任务列表，state 为空表示不过滤。
func (s *Store) ListJobs(ctx context.Context, state domain.JobState, limit, offset int) ([]domain.Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + jobCols + ` FROM jobs`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, string(state))
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	var rows []jobRow
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询任务列表失败: %w", err)
	}
	out := make([]domain.Job, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}

// ---------- uploads ----------

const uploadCols = `id, user_id, repo_name, mode, total_files, total_bytes, received_bytes,
	staging_dir, manifest, state, created_at, updated_at`

// CreateUpload 创建上传会话。
func (s *Store) CreateUpload(ctx context.Context, u *domain.Upload) error {
	if u.ID == "" {
		u.ID = newID()
	}
	now := nowMillis()
	u.CreatedAt, u.UpdatedAt = now, now
	if u.State == "" {
		u.State = domain.UploadStateOpen
	}
	if u.Manifest == "" {
		u.Manifest = "{}"
	}

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO uploads (`+uploadCols+`)
		 VALUES (:id, :user_id, :repo_name, :mode, :total_files, :total_bytes, :received_bytes,
		         :staging_dir, :manifest, :state, :created_at, :updated_at)`,
		map[string]any{
			"id":             u.ID,
			"user_id":        u.UserID,
			"repo_name":      u.RepoName,
			"mode":           u.Mode,
			"total_files":    u.TotalFiles,
			"total_bytes":    u.TotalBytes,
			"received_bytes": u.ReceivedBytes,
			"staging_dir":    u.StagingDir,
			"manifest":       u.Manifest,
			"state":          u.State,
			"created_at":     u.CreatedAt,
			"updated_at":     u.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 创建上传会话失败: %w", err)
	}
	return nil
}

// GetUpload 按 ID 查询上传会话。
func (s *Store) GetUpload(ctx context.Context, id string) (*domain.Upload, error) {
	var row struct {
		ID            string `db:"id"`
		UserID        string `db:"user_id"`
		RepoName      string `db:"repo_name"`
		Mode          string `db:"mode"`
		TotalFiles    int    `db:"total_files"`
		TotalBytes    int64  `db:"total_bytes"`
		ReceivedBytes int64  `db:"received_bytes"`
		StagingDir    string `db:"staging_dir"`
		Manifest      string `db:"manifest"`
		State         string `db:"state"`
		CreatedAt     int64  `db:"created_at"`
		UpdatedAt     int64  `db:"updated_at"`
	}
	err := s.q.GetContext(ctx, &row, s.q.Rebind(`SELECT `+uploadCols+` FROM uploads WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.UploadNotFound().WithArg("id", id) })
	}
	return &domain.Upload{
		ID: row.ID, UserID: row.UserID, RepoName: row.RepoName, Mode: row.Mode,
		TotalFiles: row.TotalFiles, TotalBytes: row.TotalBytes, ReceivedBytes: row.ReceivedBytes,
		StagingDir: row.StagingDir, Manifest: row.Manifest, State: row.State,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// UpdateUploadState 更新上传会话状态。
func (s *Store) UpdateUploadState(ctx context.Context, id, state string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE uploads SET state = ?, updated_at = ? WHERE id = ?`),
		state, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新上传状态失败: %w", err)
	}
	return mustAffect(res, apperr.UploadNotFound().WithArg("id", id))
}

// UpdateUploadManifest 覆盖上传会话的清单（复用旧会话时把本次的建库选项写回去）。
//
// 只动 manifest 一列：TotalFiles/TotalBytes 不变（复用前提就是内容一致），
// StagingDir 更不动（已上传的分块就落在那儿）。
func (s *Store) UpdateUploadManifest(ctx context.Context, id, manifest string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE uploads SET manifest = ?, updated_at = ? WHERE id = ?`),
		manifest, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新上传清单失败: %w", err)
	}
	return mustAffect(res, apperr.UploadNotFound().WithArg("id", id))
}

// AddUploadReceived 累加已接收字节数。
func (s *Store) AddUploadReceived(ctx context.Context, id string, delta int64) error {
	_, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE uploads SET received_bytes = received_bytes + ?, updated_at = ? WHERE id = ?`),
		delta, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 累加上传字节数失败: %w", err)
	}
	return nil
}

// ListUploadsByUser 列出某用户的上传会话。
func (s *Store) ListUploadsByUser(ctx context.Context, userID, state string) ([]domain.Upload, error) {
	query := `SELECT ` + uploadCols + ` FROM uploads WHERE user_id = ?`
	args := []any{userID}
	if state != "" {
		query += ` AND state = ?`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC`

	var rows []struct {
		ID            string `db:"id"`
		UserID        string `db:"user_id"`
		RepoName      string `db:"repo_name"`
		Mode          string `db:"mode"`
		TotalFiles    int    `db:"total_files"`
		TotalBytes    int64  `db:"total_bytes"`
		ReceivedBytes int64  `db:"received_bytes"`
		StagingDir    string `db:"staging_dir"`
		Manifest      string `db:"manifest"`
		State         string `db:"state"`
		CreatedAt     int64  `db:"created_at"`
		UpdatedAt     int64  `db:"updated_at"`
	}
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询上传列表失败: %w", err)
	}
	out := make([]domain.Upload, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Upload{
			ID: r.ID, UserID: r.UserID, RepoName: r.RepoName, Mode: r.Mode,
			TotalFiles: r.TotalFiles, TotalBytes: r.TotalBytes, ReceivedBytes: r.ReceivedBytes,
			StagingDir: r.StagingDir, Manifest: r.Manifest, State: r.State,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteUpload 删除上传会话及其分块记录。
func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	if _, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ?`), id); err != nil {
		return fmt.Errorf("store: 清理上传分块失败: %w", err)
	}
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM uploads WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除上传会话失败: %w", err)
	}
	return mustAffect(res, apperr.UploadNotFound().WithArg("id", id))
}

// ListStaleUploads 列出**最后有动静**早于 cutoff 且尚未完成的会话，供 GC 清理暂存目录。
//
// 已 complete 的会话不在此列（其暂存由完成流程负责回收，残留另有 ListFinishedUploadsWithStaging 兜底）。
//
// 判定用 updated_at 而不是 created_at：每收到一个分块都会刷新它（见 AddUploadReceived），
// 也就是"这个会话还有人在喂"。按 created_at 算的话，一个大目录传了一整天、刚过 24h 还在传，
// 它的暂存目录会被 GC 从**正在进行的上传**脚下删掉 —— 用户看到的是"传到一半突然全失败"。
func (s *Store) ListStaleUploads(ctx context.Context, cutoff int64, limit int) ([]domain.Upload, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + uploadCols + ` FROM uploads WHERE updated_at < ? AND state <> ? ORDER BY updated_at LIMIT ?`
	var rows []struct {
		ID            string `db:"id"`
		UserID        string `db:"user_id"`
		RepoName      string `db:"repo_name"`
		Mode          string `db:"mode"`
		TotalFiles    int    `db:"total_files"`
		TotalBytes    int64  `db:"total_bytes"`
		ReceivedBytes int64  `db:"received_bytes"`
		StagingDir    string `db:"staging_dir"`
		Manifest      string `db:"manifest"`
		State         string `db:"state"`
		CreatedAt     int64  `db:"created_at"`
		UpdatedAt     int64  `db:"updated_at"`
	}
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), cutoff, domain.UploadStateComplete, limit); err != nil {
		return nil, fmt.Errorf("store: 查询过期上传会话失败: %w", err)
	}
	out := make([]domain.Upload, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Upload{
			ID: r.ID, UserID: r.UserID, RepoName: r.RepoName, Mode: r.Mode,
			TotalFiles: r.TotalFiles, TotalBytes: r.TotalBytes, ReceivedBytes: r.ReceivedBytes,
			StagingDir: r.StagingDir, Manifest: r.Manifest, State: r.State,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// UploadStagingRemnant 是一条"会话已结束、但暂存路径还挂着"的记录（GC 回收用）。
//
// 只带 GC 需要的两个字段：整条 uploads 行的映射（含几十 MB 的 manifest）在这个流程里
// 毫无用处，而它每次扫描都要查一批。
type UploadStagingRemnant struct {
	ID         string
	StagingDir string
}

// ListFinishedUploadsWithStaging 列出已结束（complete/aborted）但暂存路径还没清空的会话。
//
// 用途：兜底回收残留的暂存目录（见 UploadService.gcLeftoverStaging）。正常完成时
// runUploadComplete 自己就会删，这里兜的是"那一次没删成"：目录被占用、进程正好退出、
// 或者某个版本只在 move 模式删 —— 后者会让**默认 copy 模式**的每一次上传都留下一整棵树，
// 用户服务器上"staging 占了几百 G"就是这么来的。
//
// 两个关键细节：
//   - 用 updated_at 判定（"最后有动静"）而不是 created_at，给刚完成的会话留一段宽限期：
//     完成流程是"先标记 complete、再删目录"，两动作之间有个很短的窗口，没有宽限期就可能
//     两边同时对着同一棵树 RemoveAll；
//   - 调用方回收成功后会清空 staging_dir（见 ClearUploadStagingDir），这条会话便**离开本
//     查询的窗口**。否则 LIMIT 每轮都是同一批（它们的目录早就不存在了），后面的永远轮不上。
func (s *Store) ListFinishedUploadsWithStaging(ctx context.Context, cutoff int64, limit int) ([]UploadStagingRemnant, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []struct {
		ID         string `db:"id"`
		StagingDir string `db:"staging_dir"`
	}
	query := `SELECT id, staging_dir FROM uploads
		WHERE staging_dir <> '' AND state IN (?, ?) AND updated_at < ?
		ORDER BY updated_at LIMIT ?`
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query),
		domain.UploadStateComplete, domain.UploadStateAborted, cutoff, limit); err != nil {
		return nil, fmt.Errorf("store: 查询待回收的暂存目录失败: %w", err)
	}
	out := make([]UploadStagingRemnant, 0, len(rows))
	for _, r := range rows {
		out = append(out, UploadStagingRemnant{ID: r.ID, StagingDir: r.StagingDir})
	}
	return out, nil
}

// ClearUploadStagingDir 清空一条会话的暂存路径（GC 确认目录已回收后调用）。
//
// 刻意**不动 updated_at**：那一列的语义是"这个会话最后有上传活动"（GC 的过期判定、
// "5 分钟内还被喂过分块"的判定都读它），不是通用的行修改时间。
// 幂等：行已经不在时静默返回（GC 与其它清理路径可能同时收尾同一条会话）。
func (s *Store) ClearUploadStagingDir(ctx context.Context, id string) error {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE uploads SET staging_dir = '' WHERE id = ?`), id); err != nil {
		return fmt.Errorf("store: 清空上传暂存路径失败: %w", err)
	}
	return nil
}

// ---------- upload chunks ----------

// CountActiveUploadsUnderPath 统计暂存目录位于 root 之下、且尚未结束的上传会话数量。
//
// "进行中"= state ∈ {open, verifying}（complete / failed / aborted 视为已结束）。
// 用途：卸载或删除存储的底层卷前把关——暂存目录就建在该存储卷上，
// 卷一卸下，进行中的写入会静默落到宿主根文件系统上（见 app.StorageService）。
func (s *Store) CountActiveUploadsUnderPath(ctx context.Context, root string) (int, error) {
	clean := filepath.Clean(strings.TrimSpace(root))
	if clean == "" {
		return 0, nil
	}
	var rows []struct {
		StagingDir string `db:"staging_dir"`
	}
	if err := s.q.SelectContext(ctx, &rows,
		s.q.Rebind(`SELECT staging_dir FROM uploads WHERE state IN (?, ?)`),
		domain.UploadStateOpen, domain.UploadStateVerifying); err != nil {
		return 0, fmt.Errorf("store: 查询进行中上传失败: %w", err)
	}
	n := 0
	for _, r := range rows {
		if strings.TrimSpace(r.StagingDir) == "" {
			continue
		}
		if domain.UnderRoot(clean, r.StagingDir) {
			n++
		}
	}
	return n, nil
}

// PutUploadChunk 登记分块。重复上传同一索引时直接忽略（幂等）。
func (s *Store) PutUploadChunk(ctx context.Context, c *domain.UploadChunk) error {
	if c.ReceivedAt == 0 {
		c.ReceivedAt = nowMillis()
	}
	sqlStr := s.insertIgnoreSQL("upload_chunks",
		[]string{"upload_id", "chunk_index", "rel_path", "offset", "size", "checksum", "received_at"},
		[]string{"upload_id", "chunk_index"})
	if _, err := s.q.ExecContext(ctx, sqlStr,
		c.UploadID, c.ChunkIndex, c.RelPath, c.Offset, c.Size, c.Checksum, c.ReceivedAt); err != nil {
		return fmt.Errorf("store: 登记上传分块失败: %w", err)
	}
	return nil
}

// DeleteUploadChunk 删除单个分块登记（幂等，用于覆盖已存在但校验不一致的分块）。
func (s *Store) DeleteUploadChunk(ctx context.Context, uploadID string, index int) error {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?`), uploadID, index); err != nil {
		return fmt.Errorf("store: 删除上传分块失败: %w", err)
	}
	return nil
}

// ListUploadChunks 列出某上传会话已收到的分块。
func (s *Store) ListUploadChunks(ctx context.Context, uploadID string) ([]domain.UploadChunk, error) {
	var out []domain.UploadChunk
	err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT upload_id, chunk_index, rel_path, offset, size, checksum, received_at
		            FROM upload_chunks WHERE upload_id = ? ORDER BY chunk_index`), uploadID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询上传分块失败: %w", err)
	}
	return out, nil
}

// UploadChunkExists 判断某分块是否已接收（用于断点续传）。
func (s *Store) UploadChunkExists(ctx context.Context, uploadID string, index int) (bool, error) {
	var n int
	err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?`),
		uploadID, index)
	if err != nil {
		return false, fmt.Errorf("store: 查询上传分块失败: %w", err)
	}
	return n > 0, nil
}

// GetUploadChunk 查询单个分块的登记（不存在返回 nil, nil）。
//
// 与"把整个会话的分块全拉回来再线性找"（ListUploadChunks）相比，这是热路径上该用的那一个：
// 每上传一个 8MiB 分块都要做一次幂等判定，而一个 100GB 的库有上万条分块记录。
func (s *Store) GetUploadChunk(ctx context.Context, uploadID string, index int) (*domain.UploadChunk, error) {
	var out []domain.UploadChunk
	err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT upload_id, chunk_index, rel_path, offset, size, checksum, received_at
		            FROM upload_chunks WHERE upload_id = ? AND chunk_index = ?`), uploadID, index)
	if err != nil {
		return nil, fmt.Errorf("store: 查询上传分块失败: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// ListUploadChunksInRange 列出某会话在 [from, to] 闭区间内已收到的分块（按索引升序）。
//
// 用来回答"某个文件覆盖的那些分块到齐了没有、它们的校验值是什么"（见 app.UploadService 的
// readyFiles）：文件在拼接流里的位置决定区间，逐个分块去查会把上传拖成 O(分块数²)。
func (s *Store) ListUploadChunksInRange(ctx context.Context, uploadID string, from, to int) ([]domain.UploadChunk, error) {
	var out []domain.UploadChunk
	err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT upload_id, chunk_index, rel_path, offset, size, checksum, received_at
		            FROM upload_chunks WHERE upload_id = ? AND chunk_index >= ? AND chunk_index <= ?
		            ORDER BY chunk_index`), uploadID, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: 查询上传分块失败: %w", err)
	}
	return out, nil
}

// CountUploadChunksInRange 统计某会话在 [from, to] 闭区间内已登记的分块数。
//
// 只数不取，成因见 ListUploadChunksInRange：判定"到齐了没有"只需要数量，
// 而校验值只有在确实到齐时才值得取回来（见 readyFiles 的两步做法）。
func (s *Store) CountUploadChunksInRange(ctx context.Context, uploadID string, from, to int) (int, error) {
	var n int
	err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM upload_chunks WHERE upload_id = ? AND chunk_index >= ? AND chunk_index <= ?`),
		uploadID, from, to)
	if err != nil {
		return 0, fmt.Errorf("store: 统计上传分块失败: %w", err)
	}
	return n, nil
}

// DeleteUploadChunksInRange 删除某会话在 [from, to] 闭区间内的分块登记，返回删除条数。
//
// 用于作废"已登记但文件树对不上"的那部分登记（见 upload.go 的 healTreeMismatch）：
// 作废后客户端把这些分块重传一遍即可自愈，不必整库重传。
func (s *Store) DeleteUploadChunksInRange(ctx context.Context, uploadID string, from, to int) (int, error) {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`DELETE FROM upload_chunks WHERE upload_id = ? AND chunk_index >= ? AND chunk_index <= ?`),
		uploadID, from, to)
	if err != nil {
		return 0, fmt.Errorf("store: 删除上传分块失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------- audit ----------

// AppendAudit 写入审计记录。
func (s *Store) AppendAudit(ctx context.Context, a *domain.AuditLog) error {
	if a.ID == "" {
		a.ID = newID()
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = nowMillis()
	}
	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO audit_logs (id, user_id, action, resource, detail, ip, result, created_at)
		 VALUES (:id, :user_id, :action, :resource, :detail, :ip, :result, :created_at)`, a)
	if err != nil {
		return fmt.Errorf("store: 写入审计失败: %w", err)
	}
	return nil
}

// ListAuditLogs 查询审计记录。
func (s *Store) ListAuditLogs(ctx context.Context, userID, action string, limit, offset int) ([]domain.AuditLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, user_id, action, resource, detail, ip, result, created_at FROM audit_logs WHERE 1=1`
	args := []any{}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if action != "" {
		query += ` AND action = ?`
		args = append(args, action)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	var out []domain.AuditLog
	if err := s.q.SelectContext(ctx, &out, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询审计失败: %w", err)
	}
	return out, nil
}

// PurgeAuditBefore 清理过期的审计记录，返回删除行数。
func (s *Store) PurgeAuditBefore(ctx context.Context, before int64) (int64, error) {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM audit_logs WHERE created_at < ?`), before)
	if err != nil {
		return 0, fmt.Errorf("store: 清理审计失败: %w", err)
	}
	return res.RowsAffected()
}
