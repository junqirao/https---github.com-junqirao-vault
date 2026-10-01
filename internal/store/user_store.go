package store

import (
	"context"
	"fmt"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- users ----------

// userCols 是 users 表的列清单，供 NamedExec 复用。
const userCols = `id, username, role, password_hash, enabled, quota_bytes, used_bytes, remark, created_at, updated_at`

// CreateUser 插入用户。
func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	if u.ID == "" {
		u.ID = newID()
	}
	now := nowMillis()
	u.CreatedAt, u.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO users (`+userCols+`)
		 VALUES (:id, :username, :role, :password_hash, :enabled, :quota_bytes, :used_bytes, :remark, :created_at, :updated_at)`,
		u)
	if isDuplicate(err) {
		return apperr.New("user.username_taken", 409).WithArg("username", u.Username)
	}
	if err != nil {
		return fmt.Errorf("store: 创建用户失败: %w", err)
	}
	return nil
}

// GetUserByID 按 ID 查询用户。
func (s *Store) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	var u domain.User
	err := s.q.GetContext(ctx, &u,
		s.q.Rebind(`SELECT `+userCols+` FROM users WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("user.not_found", 404).WithArg("id", id)
		})
	}
	return &u, nil
}

// GetUserByUsername 按用户名查询用户。
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*domain.User, error) {
	var u domain.User
	err := s.q.GetContext(ctx, &u,
		s.q.Rebind(`SELECT `+userCols+` FROM users WHERE username = ?`), username)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("user.not_found", 404).WithArg("username", username)
		})
	}
	return &u, nil
}

// ListUsers 列出用户，可按关键字模糊匹配用户名。
func (s *Store) ListUsers(ctx context.Context, keyword string, limit, offset int) ([]domain.User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + userCols + ` FROM users`
	args := []any{}
	if keyword != "" {
		query += ` WHERE username LIKE ?`
		args = append(args, "%"+keyword+"%")
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	var out []domain.User
	if err := s.q.SelectContext(ctx, &out, s.q.Rebind(query), args...); err != nil {
		return nil, fmt.Errorf("store: 查询用户列表失败: %w", err)
	}
	return out, nil
}

// CountUsers 统计用户数。
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.q.GetContext(ctx, &n, `SELECT COUNT(*) FROM users`); err != nil {
		return 0, fmt.Errorf("store: 统计用户数失败: %w", err)
	}
	return n, nil
}

// UpdateUser 覆盖更新用户的可变字段。
func (s *Store) UpdateUser(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE users SET username=:username, role=:role, password_hash=:password_hash,
		 enabled=:enabled, quota_bytes=:quota_bytes, remark=:remark, updated_at=:updated_at
		 WHERE id=:id`, u)
	if err != nil {
		return fmt.Errorf("store: 更新用户失败: %w", err)
	}
	return mustAffect(res, apperr.New("user.not_found", 404).WithArg("id", u.ID))
}

// DeleteUser 删除用户。
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM users WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除用户失败: %w", err)
	}
	return mustAffect(res, apperr.New("user.not_found", 404).WithArg("id", id))
}

// AddUserUsedBytes 原子调整用户已用量（delta 可正可负），并返回调整后的值。
func (s *Store) AddUserUsedBytes(ctx context.Context, id string, delta int64) (int64, error) {
	if _, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE users SET used_bytes = used_bytes + ?, updated_at = ? WHERE id = ?`),
		delta, nowMillis(), id); err != nil {
		return 0, fmt.Errorf("store: 调整用户用量失败: %w", err)
	}
	u, err := s.GetUserByID(ctx, id)
	if err != nil {
		return 0, err
	}
	return u.UsedBytes, nil
}

// ---------- certificates ----------

const certCols = `id, user_id, serial, fingerprint, spki_sha256, status, bound_ip, bound_mac, not_before, not_after, created_at`

// CreateCertificate 登记客户端证书。
func (s *Store) CreateCertificate(ctx context.Context, c *domain.Certificate) error {
	if c.ID == "" {
		c.ID = newID()
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = nowMillis()
	}
	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO certificates (`+certCols+`)
		 VALUES (:id, :user_id, :serial, :fingerprint, :spki_sha256, :status, :bound_ip, :bound_mac, :not_before, :not_after, :created_at)`,
		c)
	if isDuplicate(err) {
		return apperr.New("cert.already_exists", 409).WithArg("serial", c.Serial)
	}
	if err != nil {
		return fmt.Errorf("store: 登记证书失败: %w", err)
	}
	return nil
}

// GetCertificateByFingerprint 按证书指纹查询（用于认证时定位身份）。
func (s *Store) GetCertificateByFingerprint(ctx context.Context, fingerprint string) (*domain.Certificate, error) {
	var c domain.Certificate
	err := s.q.GetContext(ctx, &c,
		s.q.Rebind(`SELECT `+certCols+` FROM certificates WHERE fingerprint = ?`), fingerprint)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("cert.not_found", 404).WithArg("fingerprint", fingerprint)
		})
	}
	return &c, nil
}

// GetCertificateBySPKI 按公钥指纹查询。
//
// 续期时证书指纹会变、SPKI 不变，因此**续期审批与身份绑定都应使用本方法**。
func (s *Store) GetCertificateBySPKI(ctx context.Context, spki string) (*domain.Certificate, error) {
	var c domain.Certificate
	err := s.q.GetContext(ctx, &c,
		s.q.Rebind(`SELECT `+certCols+` FROM certificates WHERE spki_sha256 = ? ORDER BY created_at DESC LIMIT 1`), spki)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("cert.not_found", 404).WithArg("spki", spki)
		})
	}
	return &c, nil
}

// ListCertificatesByUser 列出某用户的全部证书。
func (s *Store) ListCertificatesByUser(ctx context.Context, userID string) ([]domain.Certificate, error) {
	var out []domain.Certificate
	err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT `+certCols+` FROM certificates WHERE user_id = ? ORDER BY created_at DESC`), userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户证书失败: %w", err)
	}
	return out, nil
}

// RevokeCertificate 吊销证书。
func (s *Store) RevokeCertificate(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`UPDATE certificates SET status = ? WHERE id = ?`),
		domain.CertificateStatusRevoked, id)
	if err != nil {
		return fmt.Errorf("store: 吊销证书失败: %w", err)
	}
	return mustAffect(res, apperr.New("cert.not_found", 404).WithArg("id", id))
}

// ---------- settings ----------

// GetSetting 读取配置项；不存在时返回空字符串与 false。
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.q.GetContext(ctx, &v, s.q.Rebind(`SELECT v FROM settings WHERE k = ?`), key)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: 读取配置项失败: %w", err)
	}
	return v, true, nil
}

// SetSetting 写入配置项（存在则更新）。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	sql := s.upsertSQL("settings", []string{"k", "v", "updated_at"}, []string{"k"}, []string{"v", "updated_at"})
	if _, err := s.q.ExecContext(ctx, sql, key, value, nowMillis()); err != nil {
		return fmt.Errorf("store: 写入配置项失败: %w", err)
	}
	return nil
}

// AllSettings 返回全部配置项。
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows := []struct {
		K string `db:"k"`
		V string `db:"v"`
	}{}
	if err := s.q.SelectContext(ctx, &rows, `SELECT k, v FROM settings`); err != nil {
		return nil, fmt.Errorf("store: 读取配置项失败: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.K] = r.V
	}
	return out, nil
}

// ClaimOnce 原子地声明一个一次性标记，用于"只允许成功一次"的流程。
//
// 返回值 claimed 为 true 表示本次调用成功占有该键；false 表示此前已被占用。
//
// 实现依赖 settings 表 k 列的主键约束：
// 并发调用时数据库只会让一个 INSERT 成功，其余被忽略（RowsAffected=0），
// 因此**不需要额外加锁**即可保证初始化之类流程不会被并发抢注。
func (s *Store) ClaimOnce(ctx context.Context, key, value string) (bool, error) {
	sqlStr := s.insertIgnoreSQL("settings", []string{"k", "v", "updated_at"}, []string{"k"})
	res, err := s.q.ExecContext(ctx, sqlStr, key, value, nowMillis())
	if err != nil {
		return false, fmt.Errorf("store: 声明一次性标记失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	return n > 0, nil
}
