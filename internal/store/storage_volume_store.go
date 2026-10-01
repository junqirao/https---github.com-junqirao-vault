package store

import (
	"context"
	"fmt"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// ---------- storage_volumes ----------

// storageVolumeCols 是 storage_volumes 表的列清单，供 NamedExec 复用。
const storageVolumeCols = `storage_id, kind, managed, state, ref, file_system, size_bytes, created_at, updated_at`

// CreateStorageVolume 插入存储卷登记（主键为 storage_id，与 storages 一对一）。
func (s *Store) CreateStorageVolume(ctx context.Context, v *domain.StorageVolume) error {
	now := nowMillis()
	v.CreatedAt, v.UpdatedAt = now, now

	if _, err := s.q.NamedExecContext(ctx,
		`INSERT INTO storage_volumes (`+storageVolumeCols+`)
		 VALUES (:storage_id, :kind, :managed, :state, :ref, :file_system, :size_bytes, :created_at, :updated_at)`,
		v); err != nil {
		return fmt.Errorf("store: 创建存储卷登记失败: %w", err)
	}
	return nil
}

// GetStorageVolume 按存储 ID 查询卷登记；**不存在时返回 nil, nil**。
//
// "无记录"是正常状态（目录模式），因此不视为错误（与 GetRepoMember 同约定）。
func (s *Store) GetStorageVolume(ctx context.Context, storageID string) (*domain.StorageVolume, error) {
	var v domain.StorageVolume
	err := s.q.GetContext(ctx, &v,
		s.q.Rebind(`SELECT `+storageVolumeCols+` FROM storage_volumes WHERE storage_id = ?`), storageID)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查询存储卷登记失败: %w", err)
	}
	return &v, nil
}

// ListStorageVolumes 列出全部存储卷登记（按创建时间、storage_id 排序）。
func (s *Store) ListStorageVolumes(ctx context.Context) ([]domain.StorageVolume, error) {
	var out []domain.StorageVolume
	if err := s.q.SelectContext(ctx, &out,
		`SELECT `+storageVolumeCols+` FROM storage_volumes ORDER BY created_at, storage_id`); err != nil {
		return nil, fmt.Errorf("store: 查询存储卷列表失败: %w", err)
	}
	return out, nil
}

// ListStorageVolumesByState 按状态列出存储卷登记（用于识别 pending 的无主卷）。
func (s *Store) ListStorageVolumesByState(ctx context.Context, state string) ([]domain.StorageVolume, error) {
	var out []domain.StorageVolume
	if err := s.q.SelectContext(ctx, &out,
		s.q.Rebind(`SELECT `+storageVolumeCols+` FROM storage_volumes WHERE state = ? ORDER BY created_at`),
		state); err != nil {
		return nil, fmt.Errorf("store: 按状态查询存储卷失败: %w", err)
	}
	return out, nil
}

// UpdateStorageVolume 覆盖更新卷登记的可变字段（kind / managed / state / ref / file_system / size_bytes）。
func (s *Store) UpdateStorageVolume(ctx context.Context, v *domain.StorageVolume) error {
	v.UpdatedAt = nowMillis()
	res, err := s.q.NamedExecContext(ctx,
		`UPDATE storage_volumes
		 SET kind=:kind, managed=:managed, state=:state, ref=:ref,
		     file_system=:file_system, size_bytes=:size_bytes, updated_at=:updated_at
		 WHERE storage_id=:storage_id`, v)
	if err != nil {
		return fmt.Errorf("store: 更新存储卷登记失败: %w", err)
	}
	return mustAffect(res, apperr.New("storage.not_found", 404).WithArg("id", v.StorageID))
}

// DeleteStorageVolume 删除卷登记（**只删记录，不删除底层卷**；卷的删除由编排层负责）。
func (s *Store) DeleteStorageVolume(ctx context.Context, storageID string) error {
	res, err := s.q.ExecContext(ctx,
		s.q.Rebind(`DELETE FROM storage_volumes WHERE storage_id = ?`), storageID)
	if err != nil {
		return fmt.Errorf("store: 删除存储卷登记失败: %w", err)
	}
	return mustAffect(res, apperr.New("storage.not_found", 404).WithArg("id", storageID))
}
