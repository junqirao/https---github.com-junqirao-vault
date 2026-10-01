package store

import (
	"context"
	"fmt"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- iscsi targets ----------

const targetCols = `id, target_name, disk_id, purpose, allocation_id, auth_mode, chap_user,
	chap_secret_enc, reverse_chap_secret_enc, enabled, desired_enabled, read_only, created_at, updated_at`

type targetRow struct {
	ID                   string  `db:"id"`
	TargetName           string  `db:"target_name"`
	DiskID               *string `db:"disk_id"`
	Purpose              string  `db:"purpose"`
	AllocationID         *string `db:"allocation_id"`
	AuthMode             string  `db:"auth_mode"`
	ChapUser             string  `db:"chap_user"`
	ChapSecretEnc        []byte  `db:"chap_secret_enc"`
	ReverseChapSecretEnc []byte  `db:"reverse_chap_secret_enc"`
	Enabled              bool    `db:"enabled"`
	DesiredEnabled       bool    `db:"desired_enabled"`
	ReadOnly             bool    `db:"read_only"`
	CreatedAt            int64   `db:"created_at"`
	UpdatedAt            int64   `db:"updated_at"`
}

func (r targetRow) toDomain() *domain.IscsiTarget {
	return &domain.IscsiTarget{
		ID:                   r.ID,
		TargetName:           r.TargetName,
		DiskID:               r.DiskID,
		Purpose:              domain.Purpose(r.Purpose),
		AllocationID:         r.AllocationID,
		AuthMode:             domain.AuthMode(r.AuthMode),
		ChapUser:             r.ChapUser,
		ChapSecretEnc:        r.ChapSecretEnc,
		ReverseChapSecretEnc: r.ReverseChapSecretEnc,
		Enabled:              r.Enabled,
		DesiredEnabled:       r.DesiredEnabled,
		ReadOnly:             r.ReadOnly,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
	}
}

// CreateIscsiTarget 创建 iSCSI 目标记录。
func (s *Store) CreateIscsiTarget(ctx context.Context, t *domain.IscsiTarget) error {
	if t.ID == "" {
		t.ID = newID()
	}
	now := nowMillis()
	t.CreatedAt, t.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO iscsi_targets (`+targetCols+`)
		 VALUES (:id, :target_name, :disk_id, :purpose, :allocation_id, :auth_mode, :chap_user,
		         :chap_secret_enc, :reverse_chap_secret_enc, :enabled, :desired_enabled, :read_only,
		         :created_at, :updated_at)`,
		map[string]any{
			"id":                      t.ID,
			"target_name":             t.TargetName,
			"disk_id":                 t.DiskID,
			"purpose":                 string(t.Purpose),
			"allocation_id":           t.AllocationID,
			"auth_mode":               string(t.AuthMode),
			"chap_user":               t.ChapUser,
			"chap_secret_enc":         t.ChapSecretEnc,
			"reverse_chap_secret_enc": t.ReverseChapSecretEnc,
			"enabled":                 t.Enabled,
			"desired_enabled":         t.DesiredEnabled,
			"read_only":               t.ReadOnly,
			"created_at":              t.CreatedAt,
			"updated_at":              t.UpdatedAt,
		})
	if isDuplicate(err) {
		return apperr.New("iscsi.target_name_taken", 409).WithArg("target_name", t.TargetName)
	}
	if err != nil {
		return fmt.Errorf("store: 创建 iSCSI 目标失败: %w", err)
	}
	return nil
}

// GetIscsiTarget 按 ID 查询目标。
func (s *Store) GetIscsiTarget(ctx context.Context, id string) (*domain.IscsiTarget, error) {
	var row targetRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+targetCols+` FROM iscsi_targets WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.IscsiTargetNotFound().WithArg("id", id)
		})
	}
	return row.toDomain(), nil
}

// GetIscsiTargetByName 按目标名查询。
func (s *Store) GetIscsiTargetByName(ctx context.Context, name string) (*domain.IscsiTarget, error) {
	var row targetRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+targetCols+` FROM iscsi_targets WHERE target_name = ?`), name)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.IscsiTargetNotFound().WithArg("target_name", name)
		})
	}
	return row.toDomain(), nil
}

// GetIscsiTargetByAllocation 按分配查询目标（一个分配对应一个目标）。
func (s *Store) GetIscsiTargetByAllocation(ctx context.Context, allocationID string) (*domain.IscsiTarget, error) {
	var row targetRow
	err := s.q.GetContext(ctx, &row,
		s.q.Rebind(`SELECT `+targetCols+` FROM iscsi_targets WHERE allocation_id = ? ORDER BY created_at DESC LIMIT 1`),
		allocationID)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.IscsiTargetNotFound().WithArg("allocation_id", allocationID)
		})
	}
	return row.toDomain(), nil
}

// ListIscsiTargets 列出全部目标，可按 purpose 过滤。
func (s *Store) ListIscsiTargets(ctx context.Context, purpose domain.Purpose) ([]domain.IscsiTarget, error) {
	query := `SELECT ` + targetCols + ` FROM iscsi_targets`
	args := []any{}
	if purpose != "" {
		query += ` WHERE purpose = ?`
		args = append(args, string(purpose))
	}
	query += ` ORDER BY created_at`

	var rows []targetRow
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询 iSCSI 目标列表失败: %w", err)
	}
	out := make([]domain.IscsiTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}

// ListIscsiTargetsByDisk 列出映射到某磁盘的目标。
func (s *Store) ListIscsiTargetsByDisk(ctx context.Context, diskID string) ([]domain.IscsiTarget, error) {
	var rows []targetRow
	if err := s.q.SelectContext(ctx, &rows,
		s.q.Rebind(`SELECT `+targetCols+` FROM iscsi_targets WHERE disk_id = ? ORDER BY created_at`), diskID); err != nil {
		return nil, fmt.Errorf("store: 查询磁盘目标失败: %w", err)
	}
	out := make([]domain.IscsiTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r.toDomain())
	}
	return out, nil
}

// UpdateIscsiTarget 覆盖更新目标的可变字段。
func (s *Store) UpdateIscsiTarget(ctx context.Context, t *domain.IscsiTarget) error {
	t.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE iscsi_targets SET auth_mode=:auth_mode, chap_user=:chap_user,
		 chap_secret_enc=:chap_secret_enc, reverse_chap_secret_enc=:reverse_chap_secret_enc,
		 enabled=:enabled, desired_enabled=:desired_enabled, read_only=:read_only, updated_at=:updated_at
		 WHERE id=:id`,
		map[string]any{
			"id":                      t.ID,
			"auth_mode":               string(t.AuthMode),
			"chap_user":               t.ChapUser,
			"chap_secret_enc":         t.ChapSecretEnc,
			"reverse_chap_secret_enc": t.ReverseChapSecretEnc,
			"enabled":                 t.Enabled,
			"desired_enabled":         t.DesiredEnabled,
			"read_only":               t.ReadOnly,
			"updated_at":              t.UpdatedAt,
		})
	if err != nil {
		return fmt.Errorf("store: 更新 iSCSI 目标失败: %w", err)
	}
	return mustAffect(res, apperr.IscsiTargetNotFound().WithArg("id", t.ID))
}

// SetIscsiTargetEnabled 设置目标的期望/实际启用状态。
func (s *Store) SetIscsiTargetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE iscsi_targets SET enabled = ?, desired_enabled = ?, updated_at = ? WHERE id = ?`),
		enabled, enabled, nowMillis(), id)
	if err != nil {
		return fmt.Errorf("store: 设置目标启用状态失败: %w", err)
	}
	return mustAffect(res, apperr.IscsiTargetNotFound().WithArg("id", id))
}

// DeleteIscsiTarget 删除目标及其 initiator 白名单。
func (s *Store) DeleteIscsiTarget(ctx context.Context, id string) error {
	if _, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM iscsi_initiator_ids WHERE target_id = ?`), id); err != nil {
		return fmt.Errorf("store: 清理 initiator 白名单失败: %w", err)
	}
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM iscsi_targets WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除 iSCSI 目标失败: %w", err)
	}
	return mustAffect(res, apperr.IscsiTargetNotFound().WithArg("id", id))
}

// ---------- initiator ids ----------

// ReplaceInitiatorIDs 全量替换某目标的 initiator 白名单。
//
// 必须全量替换：Windows 的 `Set-IscsiServerTarget -InitiatorIds` 本身就是替换语义，
// 且模块没有增量的 Add/Remove cmdlet（见 docs/implementation.md 5.3）。
// 调用方应先读后合并，再通过本方法一次性写入。
func (s *Store) ReplaceInitiatorIDs(ctx context.Context, targetID string, ids []domain.InitiatorID) error {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`DELETE FROM iscsi_initiator_ids WHERE target_id = ?`), targetID); err != nil {
		return fmt.Errorf("store: 清空 initiator 白名单失败: %w", err)
	}
	sqlStr := s.insertIgnoreSQL("iscsi_initiator_ids",
		[]string{"target_id", "id_type", "value"},
		[]string{"target_id", "id_type", "value"})
	for _, id := range ids {
		if !id.Valid() {
			return apperr.InvalidParam("initiator_id")
		}
		if _, err := s.q.ExecContext(ctx, sqlStr, targetID, string(id.Type), id.Value); err != nil {
			return fmt.Errorf("store: 写入 initiator 白名单失败: %w", err)
		}
	}
	return nil
}

// ListInitiatorIDs 列出某目标的白名单。
func (s *Store) ListInitiatorIDs(ctx context.Context, targetID string) ([]domain.InitiatorID, error) {
	var rows []struct {
		IDType string `db:"id_type"`
		Value  string `db:"value"`
	}
	if err := s.q.SelectContext(ctx, &rows,
		s.q.Rebind(`SELECT id_type, value FROM iscsi_initiator_ids WHERE target_id = ?`), targetID); err != nil {
		return nil, fmt.Errorf("store: 查询 initiator 白名单失败: %w", err)
	}
	out := make([]domain.InitiatorID, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.InitiatorID{Type: domain.InitiatorIDType(r.IDType), Value: r.Value})
	}
	return out, nil
}

// ---------- leases ----------

const leaseCols = `id, allocation_id, target_name, user_id, client_id, owner_token,
	mount_point, state, expires_at, last_seen_at, created_at`

// UpsertLease 创建或续期租约。
//
// 以 (allocation_id, client_id) 为唯一键：同一客户端重复挂载会刷新而非新建，
// 避免出现"幽灵租约"导致误判在线（见 docs/implementation.md 5.7）。
func (s *Store) UpsertLease(ctx context.Context, l *domain.Lease) error {
	if l.ID == "" {
		l.ID = newID()
	}
	if l.CreatedAt == 0 {
		l.CreatedAt = nowMillis()
	}
	sqlStr := s.upsertSQL("leases",
		[]string{"id", "allocation_id", "target_name", "user_id", "client_id", "owner_token",
			"mount_point", "state", "expires_at", "last_seen_at", "created_at"},
		[]string{"allocation_id", "client_id"},
		[]string{"target_name", "user_id", "owner_token", "state", "expires_at", "last_seen_at"})

	_, err := s.q.ExecContext(ctx, sqlStr,
		l.ID, l.AllocationID, l.TargetName, l.UserID, l.ClientID, l.OwnerToken,
		l.MountPoint, string(l.State), l.ExpiresAt, l.LastSeenAt, l.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: 写入租约失败: %w", err)
	}
	return nil
}

// GetLease 按 ID 查询租约。
func (s *Store) GetLease(ctx context.Context, id string) (*domain.Lease, error) {
	return s.scanLease(ctx, `SELECT `+leaseCols+` FROM leases WHERE id = ?`, id)
}

// GetLeaseByAllocationClient 按 (分配, 客户端) 查询租约。
func (s *Store) GetLeaseByAllocationClient(ctx context.Context, allocationID, clientID string) (*domain.Lease, error) {
	return s.scanLease(ctx,
		`SELECT `+leaseCols+` FROM leases WHERE allocation_id = ? AND client_id = ?`, allocationID, clientID)
}

func (s *Store) scanLease(ctx context.Context, query string, args ...any) (*domain.Lease, error) {
	var row struct {
		ID           string `db:"id"`
		AllocationID string `db:"allocation_id"`
		TargetName   string `db:"target_name"`
		UserID       string `db:"user_id"`
		ClientID     string `db:"client_id"`
		OwnerToken   string `db:"owner_token"`
		MountPoint   string `db:"mount_point"`
		State        string `db:"state"`
		ExpiresAt    int64  `db:"expires_at"`
		LastSeenAt   int64  `db:"last_seen_at"`
		CreatedAt    int64  `db:"created_at"`
	}
	if err := s.q.GetContext(ctx, &row, s.q.Rebind(query), args...); err != nil {
		return nil, notFound(err, func() *apperr.Error { return apperr.LeaseNotFound() })
	}
	return &domain.Lease{
		ID: row.ID, AllocationID: row.AllocationID, TargetName: row.TargetName,
		UserID: row.UserID, ClientID: row.ClientID, OwnerToken: row.OwnerToken,
		MountPoint: row.MountPoint, State: domain.LeaseState(row.State),
		ExpiresAt: row.ExpiresAt, LastSeenAt: row.LastSeenAt, CreatedAt: row.CreatedAt,
	}, nil
}

// ListActiveLeasesByRepo 列出某存储库下处于 active 的租约，用于判定"是否有人在使用"。
func (s *Store) ListActiveLeasesByRepo(ctx context.Context, repoID string) ([]domain.Lease, error) {
	var rows []struct {
		ID           string `db:"id"`
		AllocationID string `db:"allocation_id"`
		TargetName   string `db:"target_name"`
		UserID       string `db:"user_id"`
		ClientID     string `db:"client_id"`
		MountPoint   string `db:"mount_point"`
		State        string `db:"state"`
		ExpiresAt    int64  `db:"expires_at"`
		LastSeenAt   int64  `db:"last_seen_at"`
		CreatedAt    int64  `db:"created_at"`
	}
	query := `SELECT l.id, l.allocation_id, l.target_name, l.user_id, l.client_id, l.mount_point,
	                 l.state, l.expires_at, l.last_seen_at, l.created_at
	          FROM leases l JOIN allocations a ON a.id = l.allocation_id
	          WHERE a.repo_id = ? AND l.state = ?`
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query), repoID, string(domain.LeaseStateActive)); err != nil {
		return nil, fmt.Errorf("store: 查询活跃租约失败: %w", err)
	}
	out := make([]domain.Lease, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Lease{
			ID: r.ID, AllocationID: r.AllocationID, TargetName: r.TargetName,
			UserID: r.UserID, ClientID: r.ClientID, MountPoint: r.MountPoint,
			State: domain.LeaseState(r.State), ExpiresAt: r.ExpiresAt,
			LastSeenAt: r.LastSeenAt, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// CountActiveLeasesByRepo 统计某存储库的活跃租约数量，用于状态迁移前置校验。
func (s *Store) CountActiveLeasesByRepo(ctx context.Context, repoID string) (int, error) {
	var n int
	query := `SELECT COUNT(*) FROM leases l JOIN allocations a ON a.id = l.allocation_id
	          WHERE a.repo_id = ? AND l.state = ?`
	if err := s.q.GetContext(ctx, &n, s.q.Rebind(query), repoID, string(domain.LeaseStateActive)); err != nil {
		return 0, fmt.Errorf("store: 统计活跃租约失败: %w", err)
	}
	return n, nil
}

// ListExpiredLeases 列出已过期的活跃租约，供 Reaper 处理。
func (s *Store) ListExpiredLeases(ctx context.Context, now int64, limit int) ([]domain.Lease, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows []struct {
		ID           string `db:"id"`
		AllocationID string `db:"allocation_id"`
		TargetName   string `db:"target_name"`
		UserID       string `db:"user_id"`
		ClientID     string `db:"client_id"`
		State        string `db:"state"`
		ExpiresAt    int64  `db:"expires_at"`
		LastSeenAt   int64  `db:"last_seen_at"`
		CreatedAt    int64  `db:"created_at"`
	}
	query := `SELECT id, allocation_id, target_name, user_id, client_id, state,
	                 expires_at, last_seen_at, created_at
	          FROM leases WHERE state = ? AND expires_at < ? ORDER BY expires_at LIMIT ?`
	if err := s.q.SelectContext(ctx, &rows, s.q.Rebind(query),
		string(domain.LeaseStateActive), now, limit); err != nil {
		return nil, fmt.Errorf("store: 查询过期租约失败: %w", err)
	}
	out := make([]domain.Lease, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Lease{
			ID: r.ID, AllocationID: r.AllocationID, TargetName: r.TargetName,
			UserID: r.UserID, ClientID: r.ClientID, State: domain.LeaseState(r.State),
			ExpiresAt: r.ExpiresAt, LastSeenAt: r.LastSeenAt, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// UpdateLeaseState 更新租约状态。
func (s *Store) UpdateLeaseState(ctx context.Context, id string, st domain.LeaseState) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE leases SET state = ? WHERE id = ?`), string(st), id)
	if err != nil {
		return fmt.Errorf("store: 更新租约状态失败: %w", err)
	}
	return mustAffect(res, apperr.LeaseNotFound().WithArg("id", id))
}

// TouchLease 续期租约（心跳）。
//
// 允许续期的状态：active（在线）与 expired（离线，见 app.LeaseService.ReapExpired）——
// 后者续期后由调用方复位为 active，这是"断网/重启后自动恢复"的关键：
// 若这里只认 active，任何一次过期都会让此后所有心跳永久返回 lease_revoked，
// 客户端随即主动卸载一个完好的挂载（真实事故：盘符消失、服务端目标仍在）。
//
// revoked / released 是终态，不属于允许续期的状态：命中 0 行即返回 lease_revoked，
// 客户端据此主动卸载。
func (s *Store) TouchLease(ctx context.Context, id string, expiresAt int64) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE leases SET expires_at = ?, last_seen_at = ? WHERE id = ? AND state IN (?, ?)`),
		expiresAt, nowMillis(), id,
		string(domain.LeaseStateActive), string(domain.LeaseStateExpired))
	if err != nil {
		return fmt.Errorf("store: 续期租约失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 租约不存在、已过期又被清理、或已被吊销：明确返回业务错误，避免客户端继续认为在线。
		return apperr.LeaseRevoked()
	}
	return nil
}

// SetLeaseMountPoint 记录客户端上报的挂载点。
func (s *Store) SetLeaseMountPoint(ctx context.Context, id, mountPoint string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE leases SET mount_point = ? WHERE id = ?`), mountPoint, id)
	if err != nil {
		return fmt.Errorf("store: 更新挂载点失败: %w", err)
	}
	return mustAffect(res, apperr.LeaseNotFound().WithArg("id", id))
}
