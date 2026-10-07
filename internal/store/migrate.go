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
//	3 → 新增 storage_volumes 表（存储的底层卷登记，见 docs/implementation.md 5.1）；
//	4 → iscsi_targets 新增"已下发记账"三列（applied_fingerprint / applied_at / actual_iqn）：
//	    让"目标是否已按当前期望状态下发到平台"成为**可跨重启**的判据，
//	    挂载路径不必每次重跑 PowerShell 下发（见 docs/implementation.md 5.3）；
//	5 → storage_volumes 新增 pool_ref：记录存储落在哪个**存储池**（"<vg>/<thin_pool>"）。
//	    一台机器可以有多个存储池，创建存储时用户选择或新建
//	    （见 docs/implementation.md 5.14 Linux（LVM thin + LIO）/ 5.15 Linux 存储）。
const currentSchemaVersion = 5

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
	if err := addColumns(ctx, tx, s.dialect); err != nil {
		_ = tx.Rollback()
		return err
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

// addColumns 给**老库**补上后来新增的列。
//
// 建表语句里的列只对新建的库生效：CREATE TABLE IF NOT EXISTS 不会改动已存在的表，
// 因此老库必须显式 ALTER。执行前先探测列是否已存在，保证重复执行幂等
// （新库的 CREATE TABLE 已经带上这些列，探测后直接跳过）。
func addColumns(ctx context.Context, q queryer, dialect Dialect) error {
	// 表名/列名全部来自本文件的常量，不接受外部输入。
	type target struct {
		table  string
		column string
		sqlite string
		mysql  string
	}
	columns := []target{
		{"iscsi_targets", "applied_fingerprint",
			`ALTER TABLE iscsi_targets ADD COLUMN applied_fingerprint TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE iscsi_targets ADD COLUMN applied_fingerprint VARCHAR(64) NOT NULL DEFAULT ''`},
		{"iscsi_targets", "applied_at",
			`ALTER TABLE iscsi_targets ADD COLUMN applied_at INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE iscsi_targets ADD COLUMN applied_at BIGINT NOT NULL DEFAULT 0`},
		{"iscsi_targets", "actual_iqn",
			`ALTER TABLE iscsi_targets ADD COLUMN actual_iqn TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE iscsi_targets ADD COLUMN actual_iqn VARCHAR(255) NOT NULL DEFAULT ''`},
		{"storage_volumes", "pool_ref",
			`ALTER TABLE storage_volumes ADD COLUMN pool_ref TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_volumes ADD COLUMN pool_ref VARCHAR(160) NOT NULL DEFAULT ''`},
	}
	for _, c := range columns {
		exists, err := columnExists(ctx, q, dialect, c.table, c.column)
		if err != nil {
			return fmt.Errorf("store: 探测列 %s.%s 失败: %w", c.table, c.column, err)
		}
		if exists {
			continue
		}
		stmt := c.sqlite
		if dialect == DialectMySQL {
			stmt = c.mysql
		}
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: 补列 %s.%s 失败: %w", c.table, c.column, err)
		}
	}
	return nil
}

// columnExists 判断列是否已存在（方言差异只在本包内处理）。
func columnExists(ctx context.Context, q queryer, dialect Dialect, table, column string) (bool, error) {
	var n int
	if dialect == DialectMySQL {
		err := q.GetContext(ctx, &n, q.Rebind(
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`), table, column)
		if err != nil {
			return false, err
		}
		return n > 0, nil
	}
	// SQLite 没有 information_schema：PRAGMA 的表名只能拼字面量（此处是包内常量）。
	if err := q.GetContext(ctx, &n, q.Rebind(
		`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = ?`), column); err != nil {
		return false, err
	}
	return n > 0, nil
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
