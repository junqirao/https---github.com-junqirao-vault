package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- storages ----------

// storageCols 是 storages 表的列清单，供 NamedExec 复用。
const storageCols = `id, name, path, enabled, created_at, updated_at`

// storageDuplicate 把唯一约束冲突归一为"名称/路径已存在"业务错误。
//
// 判定依据：两种驱动在冲突信息里都会带上表列名（SQLite 为
// `UNIQUE constraint failed: storages.path`，MySQL 为 `... for key 'storages.path'`）。
// 应用层已做大小写不敏感的前置校验，这里是并发竞争下的兜底。
func storageDuplicate(err error, st *domain.Storage) *apperr.Error {
	if strings.Contains(err.Error(), "storages.path") {
		return apperr.New("storage.path_taken", 409).WithArg("path", st.Path)
	}
	return apperr.New("storage.name_taken", 409).WithArg("name", st.Name)
}

// CreateStorage 插入存储。
func (s *Store) CreateStorage(ctx context.Context, st *domain.Storage) error {
	if st.ID == "" {
		st.ID = newID()
	}
	now := nowMillis()
	st.CreatedAt, st.UpdatedAt = now, now

	_, err := s.q.NamedExecContext(ctx,
		`INSERT INTO storages (`+storageCols+`)
		 VALUES (:id, :name, :path, :enabled, :created_at, :updated_at)`, st)
	if isDuplicate(err) {
		return storageDuplicate(err, st)
	}
	if err != nil {
		return fmt.Errorf("store: 创建存储失败: %w", err)
	}
	return nil
}

// ListStorages 列出全部存储（按创建时间、名称排序）。
func (s *Store) ListStorages(ctx context.Context) ([]domain.Storage, error) {
	var out []domain.Storage
	if err := s.q.SelectContext(ctx, &out,
		`SELECT `+storageCols+` FROM storages ORDER BY created_at, name`); err != nil {
		return nil, fmt.Errorf("store: 查询存储列表失败: %w", err)
	}
	return out, nil
}

// GetStorage 按 ID 查询存储。
func (s *Store) GetStorage(ctx context.Context, id string) (*domain.Storage, error) {
	var st domain.Storage
	err := s.q.GetContext(ctx, &st,
		s.q.Rebind(`SELECT `+storageCols+` FROM storages WHERE id = ?`), id)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("storage.not_found", 404).WithArg("id", id)
		})
	}
	return &st, nil
}

// GetStorageByName 按名称查询存储（大小写不敏感）。
func (s *Store) GetStorageByName(ctx context.Context, name string) (*domain.Storage, error) {
	var st domain.Storage
	err := s.q.GetContext(ctx, &st,
		s.q.Rebind(`SELECT `+storageCols+` FROM storages WHERE LOWER(name) = LOWER(?)`), name)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("storage.not_found", 404).WithArg("name", name)
		})
	}
	return &st, nil
}

// GetStorageByPath 按路径查询存储（大小写不敏感）。
//
// 调用方应先对路径做 filepath.Clean，保证与库内规范形式一致。
func (s *Store) GetStorageByPath(ctx context.Context, path string) (*domain.Storage, error) {
	var st domain.Storage
	err := s.q.GetContext(ctx, &st,
		s.q.Rebind(`SELECT `+storageCols+` FROM storages WHERE LOWER(path) = LOWER(?)`), path)
	if err != nil {
		return nil, notFound(err, func() *apperr.Error {
			return apperr.New("storage.not_found", 404).WithArg("path", path)
		})
	}
	return &st, nil
}

// UpdateStorage 覆盖更新存储的可变字段（name / path / enabled）。
func (s *Store) UpdateStorage(ctx context.Context, st *domain.Storage) error {
	st.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE storages SET name=:name, path=:path, enabled=:enabled, updated_at=:updated_at
		 WHERE id=:id`, st)
	if isDuplicate(err) {
		return storageDuplicate(err, st)
	}
	if err != nil {
		return fmt.Errorf("store: 更新存储失败: %w", err)
	}
	return mustAffect(res, apperr.New("storage.not_found", 404).WithArg("id", st.ID))
}

// DeleteStorage 删除存储记录（**只删记录，绝不删除磁盘文件**）。
func (s *Store) DeleteStorage(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, s.q.Rebind(`DELETE FROM storages WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("store: 删除存储失败: %w", err)
	}
	return mustAffect(res, apperr.New("storage.not_found", 404).WithArg("id", id))
}

// CountStorages 统计存储总数（用于"是否已初始化种子"的判定）。
func (s *Store) CountStorages(ctx context.Context) (int, error) {
	var n int
	if err := s.q.GetContext(ctx, &n, `SELECT COUNT(*) FROM storages`); err != nil {
		return 0, fmt.Errorf("store: 统计存储数量失败: %w", err)
	}
	return n, nil
}

// StorageDiskStat 是某个存储路径下（含子目录）的磁盘数量与占用统计。
type StorageDiskStat struct {
	// DiskCount 磁盘记录数。
	DiskCount int
	// UsedBytes 占用：优先各盘 physical_bytes（实际占用），未采样（<=0）时回退 size_bytes。
	UsedBytes int64
}

// CountDisksUnderPaths 统计每个给定目录下（含子目录、按目录边界、大小写不敏感）
// 的磁盘数量与占用。
//
// 匹配规则与 domain.UnderRoot 一致：`D:\a` 匹配 `D:\a\b.vhdx`，但不匹配 `D:\ab\b.vhdx`。
// 同一磁盘归属"最具体"（路径最长）的那个目录，避免父/子目录被重复计数。
// 返回 map 的键为规范化后的查询键（filepath.Clean + 小写）；空列表返回空 map。
func (s *Store) CountDisksUnderPaths(ctx context.Context, paths []string) (map[string]StorageDiskStat, error) {
	out := make(map[string]StorageDiskStat, len(paths))
	if len(paths) == 0 {
		return out, nil
	}

	// 规范化查询键，去重后保留字典序，保证"最长匹配"并列时结果稳定。
	type root struct{ key, clean string }
	roots := make([]root, 0, len(paths))
	for _, p := range paths {
		clean := filepath.Clean(strings.TrimSpace(p))
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if _, dup := out[key]; dup {
			continue
		}
		out[key] = StorageDiskStat{}
		roots = append(roots, root{key: key, clean: clean})
	}
	if len(roots) == 0 {
		return out, nil
	}

	var rows []struct {
		VHDXPath      string `db:"vhdx_path"`
		SizeBytes     int64  `db:"size_bytes"`
		PhysicalBytes int64  `db:"physical_bytes"`
	}
	if err := s.q.SelectContext(ctx, &rows,
		`SELECT vhdx_path, size_bytes, physical_bytes FROM disks`); err != nil {
		return nil, fmt.Errorf("store: 统计存储磁盘占用失败: %w", err)
	}

	for _, r := range rows {
		best := -1
		for i := range roots {
			if !domain.UnderRoot(roots[i].clean, r.VHDXPath) {
				continue
			}
			if best < 0 || len(roots[i].clean) > len(roots[best].clean) {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		stat := out[roots[best].key]
		stat.DiskCount++
		used := r.PhysicalBytes
		if used <= 0 {
			used = r.SizeBytes
		}
		stat.UsedBytes += used
		out[roots[best].key] = stat
	}
	return out, nil
}
