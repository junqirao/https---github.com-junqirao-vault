package store

import (
	"context"
	"path/filepath"
	"testing"

	"vault/internal/domain"
)

// openTestStore 在临时目录打开一个 SQLite 库，并注册清理。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, Options{
		Dialect:    DialectSQLite,
		SQLitePath: filepath.Join(t.TempDir(), "vault.db"),
	})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// tableExists 判断指定表是否存在。
func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.DB().GetContext(context.Background(), &n,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name); err != nil {
		t.Fatalf("查询表 %s 是否存在失败: %v", name, err)
	}
	return n > 0
}

func schemaVersionOf(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.DB().GetContext(context.Background(), &v,
		"SELECT COALESCE(MAX(version), 0) FROM schema_version"); err != nil {
		t.Fatalf("读取 schema 版本失败: %v", err)
	}
	return v
}

// 全新库迁移后应建出 storage_volumes 并写入当前版本号。
func TestMigrateFreshCreatesStorageVolumes(t *testing.T) {
	s := openTestStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !tableExists(t, s, "storage_volumes") {
		t.Fatal("storage_volumes 表未创建")
	}
	if got := schemaVersionOf(t, s); got != currentSchemaVersion {
		t.Fatalf("schema 版本 = %d，期望 %d", got, currentSchemaVersion)
	}
}

// 老库（v2，仅有 storages 表）升级到 v3：补齐 storage_volumes，且重复迁移幂等。
func TestMigrateFromV2AddsStorageVolumes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// 造一个 v2 老库：只有版本表（记录 2）与 storages 表。
	if _, err := s.DB().ExecContext(ctx, `CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("创建版本表失败: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at) VALUES (2, 0)`); err != nil {
		t.Fatalf("写入 v2 版本号失败: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `CREATE TABLE storages (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL UNIQUE,
		enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("创建 storages 表失败: %v", err)
	}
	if tableExists(t, s, "storage_volumes") {
		t.Fatal("前置条件错误：storage_volumes 不应存在")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("从 v2 迁移失败: %v", err)
	}
	if !tableExists(t, s, "storage_volumes") {
		t.Fatal("从 v2 迁移后 storage_volumes 表未创建")
	}
	if got := schemaVersionOf(t, s); got != currentSchemaVersion {
		t.Fatalf("迁移后 schema 版本 = %d，期望 %d", got, currentSchemaVersion)
	}

	// 再跑一次应直接返回（幂等），且版本不变。
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	if got := schemaVersionOf(t, s); got != currentSchemaVersion {
		t.Fatalf("重复迁移后 schema 版本 = %d，期望 %d", got, currentSchemaVersion)
	}
}

// storage_volumes 的基本增删改查，以及"无记录 = 目录模式"的 nil 约定。
func TestStorageVolumeCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	st := &domain.Storage{Name: "st1", Path: "/srv/vault/st1"}
	if err := s.CreateStorage(ctx, st); err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}

	// 尚未登记卷信息时，读取应返回 nil（目录模式）。
	got, err := s.GetStorageVolume(ctx, st.ID)
	if err != nil {
		t.Fatalf("查询不存在的卷登记失败: %v", err)
	}
	if got != nil {
		t.Fatalf("期望 nil（目录模式），实际 %+v", got)
	}

	vol := &domain.StorageVolume{
		StorageID:  st.ID,
		Kind:       domain.StorageVolumeKindThin,
		Managed:    true,
		State:      domain.StorageVolumeStatePending,
		Ref:        "/dev/mapper/vl_st_" + st.ID,
		FileSystem: "ext4",
		SizeBytes:  10 << 30,
	}
	if err := s.CreateStorageVolume(ctx, vol); err != nil {
		t.Fatalf("创建卷登记失败: %v", err)
	}
	if vol.CreatedAt == 0 || vol.UpdatedAt == 0 {
		t.Fatal("创建卷登记未回填时间戳")
	}

	got, err = s.GetStorageVolume(ctx, st.ID)
	if err != nil || got == nil {
		t.Fatalf("查询卷登记失败: %v / %+v", err, got)
	}
	if got.Kind != domain.StorageVolumeKindThin || !got.Managed || got.SizeBytes != 10<<30 {
		t.Fatalf("卷登记字段不符: %+v", got)
	}

	// pending → ready
	got.State = domain.StorageVolumeStateReady
	if err := s.UpdateStorageVolume(ctx, got); err != nil {
		t.Fatalf("更新卷登记失败: %v", err)
	}
	ready, err := s.ListStorageVolumesByState(ctx, domain.StorageVolumeStateReady)
	if err != nil {
		t.Fatalf("按状态查询失败: %v", err)
	}
	if len(ready) != 1 || ready[0].StorageID != st.ID {
		t.Fatalf("ready 状态卷数量不符: %+v", ready)
	}
	pending, err := s.ListStorageVolumesByState(ctx, domain.StorageVolumeStatePending)
	if err != nil {
		t.Fatalf("按状态查询失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending 状态卷应为空: %+v", pending)
	}

	all, err := s.ListStorageVolumes(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("列出卷登记失败: %v / %+v", err, all)
	}

	if err := s.DeleteStorageVolume(ctx, st.ID); err != nil {
		t.Fatalf("删除卷登记失败: %v", err)
	}
	if got, err := s.GetStorageVolume(ctx, st.ID); err != nil || got != nil {
		t.Fatalf("删除后应为 nil: %v / %+v", err, got)
	}
	// 再删应报未找到（幂等性由编排层保证，store 层显式报错）。
	if err := s.DeleteStorageVolume(ctx, st.ID); err == nil {
		t.Fatal("重复删除卷登记应报未找到")
	}
}

// 删除存储记录时，卷登记应随外键级联一并删除（SQLite 侧）。
func TestStorageVolumeCascadeOnStorageDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	st := &domain.Storage{Name: "st2", Path: "/srv/vault/st2"}
	if err := s.CreateStorage(ctx, st); err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	if err := s.CreateStorageVolume(ctx, &domain.StorageVolume{
		StorageID: st.ID,
		Kind:      domain.StorageVolumeKindDir,
		State:     domain.StorageVolumeStateReady,
	}); err != nil {
		t.Fatalf("创建卷登记失败: %v", err)
	}

	if err := s.DeleteStorage(ctx, st.ID); err != nil {
		t.Fatalf("删除存储失败: %v", err)
	}
	if got, err := s.GetStorageVolume(ctx, st.ID); err != nil || got != nil {
		t.Fatalf("级联删除后卷登记应为 nil: %v / %+v", err, got)
	}
}
