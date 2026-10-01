package store

import (
	"context"
	"fmt"
)

// currentSchemaVersion 是当前代码期望的 schema 版本。
//
// 新增迁移时：追加对应的建表/变更语句到 schema.go，并把该值 +1。
//
// 版本历史：
//
//	1 → 初始 schema；
//	2 → 新增 storages 表（可在线管理的"存储"，见 docs/implementation.md 5.1）；
//	3 → 新增 storage_volumes 表（存储的底层卷登记，见 docs/implementation.md 5.1）。
const currentSchemaVersion = 3

// Migrate 建表并记录 schema 版本。
//
// 迁移策略：语句全部使用 IF NOT EXISTS，天然幂等；
// 版本号只用于判断"是否需要执行一轮全量语句"，以及未来做增量升级。
//
// 老库升级说明：`applied >= currentSchemaVersion` 时直接返回；老库（applied=1 或 2）
// 会重新执行一轮**全量**语句，由于每条都是 CREATE ... IF NOT EXISTS，
// 已存在的表/索引不会被改动，只会补齐缺失的表与索引，随后写入新版本号。
// 因此从任意老版本升级都是"增量 + 幂等"的，不需要单独的回填脚本。
func (s *Store) Migrate(ctx context.Context) error {
	if s.root == nil {
		return fmt.Errorf("store: 迁移必须在事务外执行")
	}

	// 先确保版本表存在。
	if _, err := s.root.ExecContext(ctx, s.versionTableSQL()); err != nil {
		return fmt.Errorf("store: 创建版本表失败: %w", err)
	}

	var applied int
	if err := s.root.GetContext(ctx, &applied,
		s.root.Rebind("SELECT COALESCE(MAX(version), 0) FROM schema_version")); err != nil {
		return fmt.Errorf("store: 读取 schema 版本失败: %w", err)
	}
	if applied >= currentSchemaVersion {
		return nil
	}

	stmts := sqliteSchema
	if s.dialect == DialectMySQL {
		stmts = mysqlSchema
	}

	// 建表放在单个事务里，失败可整体回滚。
	tx, err := s.root.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启迁移事务失败: %w", err)
	}
	for i, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: 执行迁移语句 #%d 失败: %w", i, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		tx.Rebind("INSERT INTO schema_version (version, applied_at) VALUES (?, ?)"),
		currentSchemaVersion, nowMillis()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: 记录 schema 版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交迁移失败: %w", err)
	}
	return nil
}

func (s *Store) versionTableSQL() string {
	if s.dialect == DialectMySQL {
		return `CREATE TABLE IF NOT EXISTS schema_version (
			version    BIGINT PRIMARY KEY,
			applied_at BIGINT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	return `CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`
}
