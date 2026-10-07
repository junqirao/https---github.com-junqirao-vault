package store

import (
	"context"
	"encoding/json"
	"fmt"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- json 辅助 ----------

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func fromJSON(s string, v any) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

// ---------- repositories ----------

const repoCols = `id, name, mode, owner_id, parent_disk_id, parent_version, parent_condition,
	max_diff_disks, quota_bytes, used_bytes, state, meta, created_at, updated_at`

// repoCapacitySQL 是"库容量"的派生表达式（库容量 = 建库时设定的容量，见 domain.Repository.CapacityBytes）。
//
// 容量不落 repositories 表：唯一真源是建库时创建的那块盘的 size_bytes，所以读取时联表派生。
// 共享库取它的母盘（parent_disk_id 指向的当前母盘）；无母盘的库（独享模式：它那块盘是
// standalone，parent_disk_id 为空）退回按 repo_id 找它自己的盘，否则独享库永远派生不出容量。
//
// 用 coalesce 套两个标量子查询而不是 join：引用它的列表查询还要 ORDER BY / LIMIT，
// 联表一旦出多行（同一库存在多块 parent/standalone 盘）同一个库就会在列表里重复出现，
// 标量子查询天然只出一行；代价是每行两次索引点查，存储库是十几个量级，可以接受。
const repoCapacitySQL = `COALESCE(
		(SELECT p.size_bytes FROM disks p WHERE p.id = r.parent_disk_id),
		(SELECT MAX(s.size_bytes) FROM disks s
		  WHERE s.repo_id = r.id AND s.kind = '` + string(domain.DiskKindStandalone) + `'),
		0)`

// repoReadCols 是"读存储库"的列清单：repoCols + 派生的 capacity_bytes。
//
// 不能并入 repoCols：那份列表同时用作 INSERT 的列清单（见 CreateRepository）。
// 引用它的查询必须把 repositories 别名为 r（repoCapacitySQL 依赖这个别名）。
const repoReadCols = `r.id, r.name, r.mode, r.owner_id, r.parent_disk_id, r.parent_version,
	r.parent_condition, r.max_diff_disks, r.quota_bytes, r.used_bytes, r.state, r.meta,
	r.created_at, r.updated_at, ` + repoCapacitySQL + ` AS capacity_bytes`

// repoRow 是 repositories 表的扁平映射，用于解决 meta 的 JSON 编解码。
type repoRow struct {
	ID              string  `db:"id"`
	Name            string  `db:"name"`
	Mode            string  `db:"mode"`
	OwnerID         string  `db:"owner_id"`
	ParentDiskID    *string `db:"parent_disk_id"`
	ParentVersion   int     `db:"parent_version"`
	ParentCondition *string `db:"parent_condition"`
	MaxDiffDisks    int     `db:"max_diff_disks"`
	QuotaBytes      int64   `db:"quota_bytes"`
	UsedBytes       int64   `db:"used_bytes"`
	// CapacityBytes 是读时派生的库容量（见 repoCapacitySQL），不是表里的列。
	CapacityBytes int64  `db:"capacity_bytes"`
	State         string `db:"state"`
	Meta          string `db:"meta"`
	CreatedAt     int64  `db:"created_at"`
	UpdatedAt     int64  `db:"updated_at"`
}

func (r repoRow) toDomain() *domain.Repository {
	repo := &domain.Repository{
		ID:            r.ID,
		Name:          r.Name,
		Mode:          domain.RepoMode(r.Mode),
		OwnerID:       r.OwnerID,
		ParentDiskID:  r.ParentDiskID,
		ParentVersion: r.ParentVersion,
		MaxDiffDisks:  r.MaxDiffDisks,
		QuotaBytes:    r.QuotaBytes,
		CapacityBytes: r.CapacityBytes,
		UsedBytes:     r.UsedBytes,
		State:         domain.RepoState(r.State),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if r.ParentCondition != nil {
		c := domain.ParentCondition(*r.ParentCondition)
		repo.ParentCondition = &c
	}
	fromJSON(r.Meta, &repo.Meta)
	return repo
}

// CreateRepository 创建存储库。
func (s *Store) CreateRepository(ctx context.Context, r *domain.Repository) error {
	if r.ID == "" {
		r.ID = newID()
	}
	now := nowMillis()
	r.CreatedAt, r.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO repositories (`+repoCols+`)
		 VALUES (:id, :name, :mode, :owner_id, :parent_disk_id, :parent_version, :parent_condition,
		         :max_diff_disks, :quota_bytes, :used_bytes, :state, :meta, :created_at, :updated_at)`,
		map[string]any{
			"id":               r.ID,
			"name":             r.Name,
			"mode":             string(r.Mode),
			"owner_id":         r.OwnerID,
			"parent_disk_id":   r.ParentDiskID,
			"parent_version":   r.ParentVersion,
			"parent_condition": nullableCondition(r),
			"max_diff_disks":   r.MaxDiffDisks,
			"quota_bytes":      r.QuotaBytes,
			"used_bytes":       r.UsedBytes,
			"state":            string(r.State),
			"meta":             toJSON(r.Meta),
			"created_at":       r.CreatedAt,
			"updated_at":       r.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.RepoNameTaken().WithArg("name", r.Name)
	}
	if err != nil {
		return fmt.Errorf("store: 创建存储库失败: %w", err)
	}
	return nil
}

func nullableCondition(r *domain.Repository) any {
	if r.ParentCondition == nil {
		return nil
	}
	return string(*r.ParentCondition)
}

// GetRepository 按 ID 查询存储库。
func (s *Store) GetRepository(ctx context.Context, id string) (*domain.Repository, error) {
	var row repoRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+repoReadCols+` FROM repositories r WHERE r.id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.RepoNotFound().WithArg("id", id) })
	}
	return row.toDomain(), nil
}

// GetRepositoryByName 按名称查询存储库。
func (s *Store) GetRepositoryByName(ctx context.Context, name string) (*domain.Repository, error) {
	var row repoRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+repoReadCols+` FROM repositories r WHERE r.name = ?`), name)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.RepoNotFound().WithArg("name", name) })
	}
	return row.toDomain(), nil
}

// LockRepository 以行锁方式读取存储库，用于配额的"读-校验-写"临界区。
//
// SQLite 依赖 `_txlock=immediate` 与单写者语义；MySQL 使用 SELECT ... FOR UPDATE。
func (s *Store) LockRepository(ctx context.Context, id string) (*domain.Repository, error) {
	var row repoRow
	query := s.forUpdateSQL(`SELECT ` + repoCols + ` FROM repositories WHERE id = ?`)
	if err := s.q.GetContext(ctx, &row, s.q.Rebind(query), id); err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.RepoNotFound().WithArg("id", id) })
	}
	return row.toDomain(), nil
}

// ListRepositories 查询存储库列表。ownerID 为空表示不过滤。
func (s *Store) ListRepositories(ctx context.Context, ownerID string, limit, offset int) ([]domain.Repository, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + repoReadCols + ` FROM repositories r`
	args := []any{}
	if ownerID != "" {
		query += ` WHERE r.owner_id = ?`
		args = append(args, ownerID)
	}
	query += ` ORDER BY r.created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	var rows []repoRow
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询存储库列表失败: %w", err)
	}
	out := make([]domain.Repository, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}

// UpdateRepository 更新存储库可变字段。
//
// 注意：parent_disk_id 也在此更新 —— 母盘磁盘记录必须先于存储库存在（disks 表对
// repositories 有外键），因此"创建存储库 → 创建母盘 → 回填 parent_disk_id"
// 是唯一可行的顺序。
func (s *Store) UpdateRepository(ctx context.Context, r *domain.Repository) error {
	r.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE repositories SET name=:name, parent_disk_id=:parent_disk_id,
		 parent_version=:parent_version, parent_condition=:parent_condition,
		 max_diff_disks=:max_diff_disks, quota_bytes=:quota_bytes, used_bytes=:used_bytes,
		 state=:state, meta=:meta, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":               r.ID,
			"name":             r.Name,
			"parent_disk_id":   r.ParentDiskID,
			"parent_version":   r.ParentVersion,
			"parent_condition": nullableCondition(r),
			"max_diff_disks":   r.MaxDiffDisks,
			"quota_bytes":      r.QuotaBytes,
			"used_bytes":       r.UsedBytes,
			"state":            string(r.State),
			"meta":             toJSON(r.Meta),
			"updated_at":       r.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.RepoNameTaken().WithArg("name", r.Name)
	}
	if err != nil {
		return fmt.Errorf("store: 更新存储库失败: %w", err)
	}
	return mustAffect(res, apperr.RepoNotFound().WithArg("id", r.ID))
}

// SetParentCondition 更新母盘条件；versionBump 为 true 时同时自增母盘版本号。
//
// 版本号自增的语义：母盘内容发生变化（完成维护更新）后必须自增，
// 使已有差异盘因 parent_version 不匹配而拒绝挂载（见 docs/implementation.md 5.4）。
func (s *Store) SetParentCondition(ctx context.Context, id string, cond domain.ParentCondition, bumpVersion bool) error {
	query := `UPDATE repositories SET parent_condition = ?, updated_at = ? WHERE id = ?`
	args := []any{string(cond), nowMillis(), id}
	if bumpVersion {
		query = `UPDATE repositories SET parent_condition = ?, parent_version = parent_version + 1, updated_at = ? WHERE id = ?`
	}
	res, err := s.q.ExecContext(ctx, s.q.Rebind(query), args...)
	if err != nil {
		return fmt.Errorf("store: 更新母盘条件失败: %w", err)
	}
	return mustAffect(res, apperr.RepoNotFound().WithArg("id", id))
}

// AddRepoUsedBytes 原子调整存储库已用量，返回调整后的值。
func (s *Store) AddRepoUsedBytes(ctx context.Context, id string, delta int64) (int64, error) {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE repositories SET used_bytes = used_bytes + ?, updated_at = ? WHERE id = ?`),
		delta, nowMillis(), id); err != nil {
		return 0, fmt.Errorf("store: 调整存储库用量失败: %w", err)
	}
	r, err := s.GetRepository(ctx, id)
	if err != nil {
		return 0, err
	}
	return r.UsedBytes, nil
}

// DeleteRepository 删除存储库记录（磁盘与目标需由业务层先行清理）。
func (s *Store) DeleteRepository(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM repositories WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除存储库失败: %w", err)
	}
	return mustAffect(res, apperr.RepoNotFound().WithArg("id", id))
}

// CountRepositories 统计存储库总数。
//
// 用于判断"系统是否被使用过"——初始化入口的判定条件之一。
func (s *Store) CountRepositories(ctx context.Context) (int, error) {
	var n int
	if err := s.q.GetContext(ctx, &n, `SELECT COUNT(*) FROM repositories`); err != nil {
		return 0, fmt.Errorf("store: 统计存储库数量失败: %w", err)
	}
	return n, nil
}

// ---------- repo members ----------

// ReplaceRepoMembers 全量替换可管理列表（仅 owner 可操作，由业务层校验）。
func (s *Store) ReplaceRepoMembers(ctx context.Context, repoID string, members []domain.RepoMember) error {
	if _, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM repo_members WHERE repo_id = ?`), repoID); err != nil {
		return fmt.Errorf("store: 清空存储库成员失败: %w", err)
	}
	for _, m := range members {
		if _, err := s.q.ExecContext(ctx,
			s.q.Rebind(`INSERT INTO repo_members (repo_id, user_id, perm) VALUES (?, ?, ?)`),
			repoID, m.UserID, string(m.Perm)); err != nil {
			return fmt.Errorf("store: 写入存储库成员失败: %w", err)
		}
	}
	return nil
}

// ListRepoMembers 列出存储库成员。
func (s *Store) ListRepoMembers(ctx context.Context, repoID string) ([]domain.RepoMember, error) {
	var out []domain.RepoMember
	err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT repo_id, user_id, perm FROM repo_members WHERE repo_id = ?`), repoID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询存储库成员失败: %w", err)
	}
	return out, nil
}

// GetRepoMember 查询单个成员权限；不存在时返回 nil。
func (s *Store) GetRepoMember(ctx context.Context, repoID, userID string) (*domain.RepoMember, error) {
	var m domain.RepoMember
	err := s.q.GetContext(ctx, &m,
		s.q.Rebind(`SELECT repo_id, user_id, perm FROM repo_members WHERE repo_id = ? AND user_id = ?`),
		repoID, userID)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查询存储库成员失败: %w", err)
	}
	return &m, nil
}

// ---------- disks ----------

const diskCols = `id, repo_id, kind, vhdx_path, parent_id, parent_version, content_fingerprint,
	size_bytes, physical_bytes, vhd_type, state, desired_state, observed_state, mounted, file_system,
	created_at, updated_at`

// CreateDisk 插入磁盘记录。
func (s *Store) CreateDisk(ctx context.Context, d *domain.Disk) error {
	if d.ID == "" {
		d.ID = newID()
	}
	now := nowMillis()
	d.CreatedAt, d.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO disks (`+diskCols+`)
		 VALUES (:id, :repo_id, :kind, :vhdx_path, :parent_id, :parent_version, :content_fingerprint,
		         :size_bytes, :physical_bytes, :vhd_type, :state, :desired_state, :observed_state,
		         :mounted, :file_system, :created_at, :updated_at)`,
		map[string]any{
			"id":                  d.ID,
			"repo_id":             d.RepoID,
			"kind":                string(d.Kind),
			"vhdx_path":           d.VHDXPath,
			"parent_id":           d.ParentID,
			"parent_version":      d.ParentVersion,
			"content_fingerprint": d.ContentFingerprint,
			"size_bytes":          d.SizeBytes,
			"physical_bytes":      d.PhysicalBytes,
			"vhd_type":            string(d.VHDType),
			"state":               string(d.State),
			"desired_state":       d.DesiredState,
			"observed_state":      d.ObservedState,
			"mounted":             d.Mounted,
			"file_system":         string(d.FileSystem),
			"created_at":          d.CreatedAt,
			"updated_at":          d.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("disk.path_taken", 409).WithArg("path", d.VHDXPath)
	}
	if err != nil {
		return fmt.Errorf("store: 创建磁盘记录失败: %w", err)
	}
	return nil
}

func (s *Store) scanDisk(ctx context.Context, query string, args ...any) (*domain.Disk, error) {
	var row struct {
		ID                 string  `db:"id"`
		RepoID             string  `db:"repo_id"`
		Kind               string  `db:"kind"`
		VHDXPath           string  `db:"vhdx_path"`
		ParentID           *string `db:"parent_id"`
		ParentVersion      int     `db:"parent_version"`
		ContentFingerprint string  `db:"content_fingerprint"`
		SizeBytes          int64   `db:"size_bytes"`
		PhysicalBytes      int64   `db:"physical_bytes"`
		VHDType            string  `db:"vhd_type"`
		State              string  `db:"state"`
		DesiredState       string  `db:"desired_state"`
		ObservedState      string  `db:"observed_state"`
		Mounted            bool    `db:"mounted"`
		FileSystem         string  `db:"file_system"`
		CreatedAt          int64   `db:"created_at"`
		UpdatedAt          int64   `db:"updated_at"`
	}
	if err := s.q.GetContext(ctx, &row, s.q.Rebind(query), args...); err != nil {
		return nil, err
	}
	return &domain.Disk{
		ID:                 row.ID,
		RepoID:             row.RepoID,
		Kind:               domain.DiskKind(row.Kind),
		VHDXPath:           row.VHDXPath,
		ParentID:           row.ParentID,
		ParentVersion:      row.ParentVersion,
		ContentFingerprint: row.ContentFingerprint,
		SizeBytes:          row.SizeBytes,
		PhysicalBytes:      row.PhysicalBytes,
		VHDType:            domain.VHDType(row.VHDType),
		State:              domain.DiskState(row.State),
		DesiredState:       row.DesiredState,
		ObservedState:      row.ObservedState,
		Mounted:            row.Mounted,
		FileSystem:         domain.FileSystem(row.FileSystem),
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}, nil
}

// GetDisk 按 ID 查询磁盘。
func (s *Store) GetDisk(ctx context.Context, id string) (*domain.Disk, error) {
	d, err := s.scanDisk(ctx, `SELECT `+diskCols+` FROM disks WHERE id = ?`, id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.DiskNotFound().WithArg("id", id) })
	}
	return d, nil
}

// GetDiskByPath 按 VHDX 路径查询磁盘。
func (s *Store) GetDiskByPath(ctx context.Context, path string) (*domain.Disk, error) {
	d, err := s.scanDisk(ctx, `SELECT `+diskCols+` FROM disks WHERE vhdx_path = ?`, path)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.DiskNotFound().WithArg("path", path) })
	}
	return d, nil
}

// ListDisksByRepo 列出某存储库下的全部磁盘。
func (s *Store) ListDisksByRepo(ctx context.Context, repoID string) ([]domain.Disk, error) {
	return s.listDisks(ctx, `SELECT `+diskCols+` FROM disks WHERE repo_id = ? ORDER BY created_at`, repoID)
}

// ListDisksByKind 按类型列出磁盘。
func (s *Store) ListDisksByKind(ctx context.Context, kind domain.DiskKind) ([]domain.Disk, error) {
	return s.listDisks(ctx, `SELECT `+diskCols+` FROM disks WHERE kind = ? ORDER BY created_at`, string(kind))
}

// ListAllDisks 列出全部磁盘记录（对账器使用）。
func (s *Store) ListAllDisks(ctx context.Context) ([]domain.Disk, error) {
	return s.listDisks(ctx, `SELECT `+diskCols+` FROM disks ORDER BY created_at`)
}

// ListDiffDisksByParent 列出某母盘派生出的全部差异盘。
func (s *Store) ListDiffDisksByParent(ctx context.Context, parentDiskID string) ([]domain.Disk, error) {
	return s.listDisks(ctx, `SELECT `+diskCols+` FROM disks WHERE parent_id = ? ORDER BY created_at`, parentDiskID)
}

func (s *Store) listDisks(ctx context.Context, query string, args ...any) ([]domain.Disk, error) {
	var rows []struct {
		ID                 string  `db:"id"`
		RepoID             string  `db:"repo_id"`
		Kind               string  `db:"kind"`
		VHDXPath           string  `db:"vhdx_path"`
		ParentID           *string `db:"parent_id"`
		ParentVersion      int     `db:"parent_version"`
		ContentFingerprint string  `db:"content_fingerprint"`
		SizeBytes          int64   `db:"size_bytes"`
		PhysicalBytes      int64   `db:"physical_bytes"`
		VHDType            string  `db:"vhd_type"`
		State              string  `db:"state"`
		DesiredState       string  `db:"desired_state"`
		ObservedState      string  `db:"observed_state"`
		Mounted            bool    `db:"mounted"`
		FileSystem         string  `db:"file_system"`
		CreatedAt          int64   `db:"created_at"`
		UpdatedAt          int64   `db:"updated_at"`
	}
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询磁盘列表失败: %w", err)
	}
	out := make([]domain.Disk, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Disk{
			ID:                 r.ID,
			RepoID:             r.RepoID,
			Kind:               domain.DiskKind(r.Kind),
			VHDXPath:           r.VHDXPath,
			ParentID:           r.ParentID,
			ParentVersion:      r.ParentVersion,
			ContentFingerprint: r.ContentFingerprint,
			SizeBytes:          r.SizeBytes,
			PhysicalBytes:      r.PhysicalBytes,
			VHDType:            domain.VHDType(r.VHDType),
			State:              domain.DiskState(r.State),
			DesiredState:       r.DesiredState,
			ObservedState:      r.ObservedState,
			Mounted:            r.Mounted,
			FileSystem:         domain.FileSystem(r.FileSystem),
			CreatedAt:          r.CreatedAt,
			UpdatedAt:          r.UpdatedAt,
		})
	}
	return out, nil
}

// CountDiffDisks 统计某母盘的差异盘数量，用于 max_diff_disks 闸门。
func (s *Store) CountDiffDisks(ctx context.Context, parentDiskID string) (int, error) {
	var n int
	err := s.q.GetContext(ctx, &n,
		s.q.Rebind(`SELECT COUNT(*) FROM disks WHERE parent_id = ?`), parentDiskID)
	if err != nil {
		return 0, fmt.Errorf("store: 统计差异盘数量失败: %w", err)
	}
	return n, nil
}

// PickIdleDiffDisk 从"差异盘池"里取一块空闲差异盘（当前没有未释放的分配）。
//
// 池化存储库（RepoMeta.Pool）在分配时优先走这里：盘与 iSCSI 目标都在建库阶段就
// 建好并发布过了，分配只是把它绑给用户 —— 用户点挂载时不需要再等建盘与首次下发。
//
// 只取 ready/published 的盘：creating 还在派生（挂载会退化成等待）、error 已经不可用、
// deleting 正在消失，交给调用方只会得到一个"要等很久"或"挂上去就坏"的分配。
// 找不到空闲盘时返回 not_found（调用方据此决定是报"池已满"还是回退到现建差异盘）。
func (s *Store) PickIdleDiffDisk(ctx context.Context, repoID string) (*domain.Disk, error) {
	d, err := s.scanDisk(ctx, `SELECT `+diskCols+` FROM disks
		 WHERE repo_id = ? AND kind = ? AND state IN (?, ?)
		   AND id NOT IN (SELECT disk_id FROM allocations WHERE state <> ?)
		 ORDER BY created_at LIMIT 1`,
		repoID, string(domain.DiskKindDiff),
		string(domain.DiskStateReady), string(domain.DiskStatePublished),
		string(domain.AllocationStateReleased))
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("repo.no_idle_diff_disk", 404).WithArg("repo_id", repoID)
		})
	}
	return d, nil
}

// UpdateDiskState 更新磁盘状态与观测值。
func (s *Store) UpdateDiskState(ctx context.Context, id string, state domain.DiskState, observed string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE disks SET state = ?, observed_state = ?, updated_at = ? WHERE id = ?`),
		string(state), observed, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新磁盘状态失败: %w", err)
	}
	return mustAffect(res, apperr.DiskNotFound().WithArg("id", id))
}

// UpdateDisk 覆盖更新磁盘的可变字段。
func (s *Store) UpdateDisk(ctx context.Context, d *domain.Disk) error {
	d.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE disks SET parent_version=:parent_version, content_fingerprint=:content_fingerprint,
		 size_bytes=:size_bytes, physical_bytes=:physical_bytes, state=:state,
		 desired_state=:desired_state, observed_state=:observed_state, mounted=:mounted, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":                  d.ID,
			"parent_version":      d.ParentVersion,
			"content_fingerprint": d.ContentFingerprint,
			"size_bytes":          d.SizeBytes,
			"physical_bytes":      d.PhysicalBytes,
			"state":               string(d.State),
			"desired_state":       d.DesiredState,
			"observed_state":      d.ObservedState,
			"mounted":             d.Mounted,
			"updated_at":          d.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 更新磁盘失败: %w", err)
	}
	return mustAffect(res, apperr.DiskNotFound().WithArg("id", d.ID))
}

// DeleteDisk 删除磁盘记录。
func (s *Store) DeleteDisk(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM disks WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除磁盘记录失败: %w", err)
	}
	return mustAffect(res, apperr.DiskNotFound().WithArg("id", id))
}

// ---------- allocations ----------

const allocCols = `id, repo_id, disk_id, user_id, state, created_at, updated_at`

// CreateAllocation 创建分配记录。
func (s *Store) CreateAllocation(ctx context.Context, a *domain.Allocation) error {
	if a.ID == "" {
		a.ID = newID()
	}
	now := nowMillis()
	a.CreatedAt, a.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO allocations (`+allocCols+`)
		 VALUES (:id, :repo_id, :disk_id, :user_id, :state, :created_at, :updated_at)`,
		map[string]any{
			"id":         a.ID,
			"repo_id":    a.RepoID,
			"disk_id":    a.DiskID,
			"user_id":    a.UserID,
			"state":      string(a.State),
			"created_at": a.CreatedAt,
			"updated_at": a.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 创建分配失败: %w", err)
	}
	return nil
}

func (s *Store) scanAlloc(ctx context.Context, query string, args ...any) (*domain.Allocation, error) {
	var row struct {
		ID        string `db:"id"`
		RepoID    string `db:"repo_id"`
		DiskID    string `db:"disk_id"`
		UserID    string `db:"user_id"`
		State     string `db:"state"`
		CreatedAt int64  `db:"created_at"`
		UpdatedAt int64  `db:"updated_at"`
	}
	if err := s.q.GetContext(ctx, &row, s.q.Rebind(query), args...); err != nil {
		return nil, err
	}
	return &domain.Allocation{
		ID: row.ID, RepoID: row.RepoID, DiskID: row.DiskID, UserID: row.UserID,
		State: domain.AllocationState(row.State), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// GetAllocation 按 ID 查询分配。
func (s *Store) GetAllocation(ctx context.Context, id string) (*domain.Allocation, error) {
	a, err := s.scanAlloc(ctx, `SELECT `+allocCols+` FROM allocations WHERE id = ?`, id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("allocation.not_found", 404).WithArg("id", id)
		})
	}
	return a, nil
}

// GetAllocationByDisk 按磁盘查询分配。
func (s *Store) GetAllocationByDisk(ctx context.Context, diskID string) (*domain.Allocation, error) {
	a, err := s.scanAlloc(ctx, `SELECT `+allocCols+` FROM allocations WHERE disk_id = ?`, diskID)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("allocation.not_found", 404).WithArg("disk_id", diskID)
		})
	}
	return a, nil
}

// ListAllocationsByRepo 列出某存储库的全部分配。
func (s *Store) ListAllocationsByRepo(ctx context.Context, repoID string) ([]domain.Allocation, error) {
	return s.listAllocs(ctx, `SELECT `+allocCols+` FROM allocations WHERE repo_id = ? ORDER BY created_at`, repoID)
}

// ListAllocationsByUser 列出某用户的全部分配。
func (s *Store) ListAllocationsByUser(ctx context.Context, userID string) ([]domain.Allocation, error) {
	return s.listAllocs(ctx, `SELECT `+allocCols+` FROM allocations WHERE user_id = ? ORDER BY created_at`, userID)
}

// ListAllAllocations 列出全部分配记录（对账器使用）。
func (s *Store) ListAllAllocations(ctx context.Context) ([]domain.Allocation, error) {
	return s.listAllocs(ctx, `SELECT `+allocCols+` FROM allocations ORDER BY created_at`)
}

func (s *Store) listAllocs(ctx context.Context, query string, args ...any) ([]domain.Allocation, error) {
	var rows []struct {
		ID        string `db:"id"`
		RepoID    string `db:"repo_id"`
		DiskID    string `db:"disk_id"`
		UserID    string `db:"user_id"`
		State     string `db:"state"`
		CreatedAt int64  `db:"created_at"`
		UpdatedAt int64  `db:"updated_at"`
	}
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询分配列表失败: %w", err)
	}
	out := make([]domain.Allocation, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Allocation{
			ID: r.ID, RepoID: r.RepoID, DiskID: r.DiskID, UserID: r.UserID,
			State: domain.AllocationState(r.State), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// UpdateAllocationState 更新分配状态。
func (s *Store) UpdateAllocationState(ctx context.Context, id string, st domain.AllocationState) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE allocations SET state = ?, updated_at = ? WHERE id = ?`),
		string(st), nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 更新分配状态失败: %w", err)
	}
	return mustAffect(res, apperr.New("allocation.not_found", 404).WithArg("id", id))
}

// DeleteAllocation 删除分配记录。
func (s *Store) DeleteAllocation(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM allocations WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除分配失败: %w", err)
	}
	return mustAffect(res, apperr.New("allocation.not_found", 404).WithArg("id", id))
}
