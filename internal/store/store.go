// Package store 是数据访问层。
//
// 分层约定（见 docs/implementation.md 6.2 / 10.2）：
//   - 业务层不得出现任何 SQL 或方言分支；
//   - 方言差异（UPSERT / INSERT IGNORE 语法）只在本包内通过 dialect 处理；
//   - 两套建表语句在 schema.go 中保持结构等价。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"vault/internal/apperr"
)

// Dialect 数据库方言。
type Dialect string

const (
	DialectSQLite Dialect = "sqlite"
	DialectMySQL  Dialect = "mysql"
)

// Options 打开数据库所需的参数。
type Options struct {
	Dialect Dialect
	// SQLitePath SQLite 数据库文件路径（必须位于本地磁盘）。
	SQLitePath string
	// MySQLDSN MySQL 连接串。
	MySQLDSN string
	// MaxOpenConns / MaxIdleConns 仅 MySQL 生效。
	MaxOpenConns int
	MaxIdleConns int
	Logger       *slog.Logger
}

// queryer 抽象 *sqlx.DB 与 *sqlx.Tx 的公共查询能力，使事务内外代码复用同一套方法。
type queryer interface {
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
	Rebind(query string) string
}

// Store 是数据访问入口。事务内使用它派生出的副本。
type Store struct {
	q       queryer
	dialect Dialect
	log     *slog.Logger

	// root 仅在事务外持有，用于开始事务与关闭数据库。
	root *sqlx.DB
}

// Open 连接数据库并完成基础配置。
func Open(ctx context.Context, opt Options) (*Store, error) {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}

	db, err := openDB(ctx, opt)
	if err != nil {
		return nil, err
	}

	if opt.Dialect == DialectMySQL {
		if opt.MaxOpenConns > 0 {
			db.SetMaxOpenConns(opt.MaxOpenConns)
		}
		if opt.MaxIdleConns > 0 {
			db.SetMaxIdleConns(opt.MaxIdleConns)
		}
		db.SetConnMaxLifetime(time.Hour)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: 数据库连通性检查失败: %w", err)
	}

	return &Store{q: db, dialect: opt.Dialect, log: opt.Logger, root: db}, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

// Dialect 返回当前方言。
func (s *Store) Dialect() Dialect { return s.dialect }

// DB 暴露底层 *sqlx.DB（仅事务外可用，供迁移与健康检查使用）。
func (s *Store) DB() *sqlx.DB { return s.root }

// Tx 在事务中执行 fn。fn 收到的是绑定到事务的 Store，其所有方法都在同一事务内。
//
// SQLite 使用 BEGIN IMMEDIATE 语义（写事务立即取写锁），避免"读-改-写"竞态；
// MySQL 通过 SELECT ... FOR UPDATE（见 Lock* 方法）达到同等效果。
func (s *Store) Tx(ctx context.Context, fn func(tx *Store) error) error {
	if s.root == nil {
		return errors.New("store: 不能在事务中嵌套开启事务")
	}
	tx, err := s.root.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	child := &Store{q: tx, dialect: s.dialect, log: s.log}

	if err := fn(child); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			s.log.Error("事务回滚失败", "error", rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return nil
}

// nowMillis 返回当前 Unix 毫秒，全库统一时间表示。
func nowMillis() int64 { return time.Now().UnixMilli() }

// newID 生成实体主键。主键统一由应用生成，避免不同数据库自增语义差异。
func newID() string { return uuid.NewString() }

// ErrNotFound 是统一的"未找到"错误，由实体方法在查询无结果时返回。
var ErrNotFound = errors.New("store: 记录不存在")

// notFound 把 sql.ErrNoRows 归一化为业务错误；其它错误原样返回。
func notFound(err error, builder func() *apperr.Error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return builder()
	}
	return err
}

// isNoRows 判断是否为"无结果"。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// mustAffect 要求影响行数 > 0，否则返回指定业务错误。
//
// 用于 UPDATE/DELETE 后判断目标是否存在，避免 handler 里重复写 RowsAffected 检查。
func mustAffect(res sql.Result, notFoundErr *apperr.Error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: 读取影响行数失败: %w", err)
	}
	if n == 0 {
		return notFoundErr
	}
	return nil
}

// isDuplicate 判断是否为唯一约束冲突。
//
// 两种驱动的错误类型不同，这里用错误信息做保守匹配；
// 命中后由调用方决定改为返回"冲突"类业务错误。
func isDuplicate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "Error 1062")
}

// placeholders 生成 n 个占位符，两种驱动都用 `?`。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '?')
	}
	return string(buf)
}

// insertIgnoreSQL 生成"存在则忽略"的插入语句。
//
// SQLite: INSERT INTO ... ON CONFLICT(cols) DO NOTHING
// MySQL:  INSERT IGNORE INTO ...
func (s *Store) insertIgnoreSQL(table string, cols, conflictCols []string) string {
	values := "(" + join(cols, ",") + ") VALUES (" + placeholders(len(cols)) + ")"
	if s.dialect == DialectMySQL {
		return "INSERT IGNORE INTO " + table + " " + values
	}
	return "INSERT INTO " + table + " " + values +
		" ON CONFLICT(" + join(conflictCols, ",") + ") DO NOTHING"
}

// upsertSQL 生成"存在则更新"的插入语句。
//
// SQLite: INSERT INTO ... ON CONFLICT(cols) DO UPDATE SET x = excluded.x
// MySQL:  INSERT INTO ... ON DUPLICATE KEY UPDATE x = VALUES(x)
func (s *Store) upsertSQL(table string, cols, conflictCols, updateCols []string) string {
	base := "INSERT INTO " + table + " (" + join(cols, ",") + ") VALUES (" + placeholders(len(cols)) + ")"

	sets := make([]string, 0, len(updateCols))
	if s.dialect == DialectMySQL {
		for _, c := range updateCols {
			sets = append(sets, c+"=VALUES("+c+")")
		}
		return base + " ON DUPLICATE KEY UPDATE " + join(sets, ",")
	}
	for _, c := range updateCols {
		sets = append(sets, c+"=excluded."+c)
	}
	return base + " ON CONFLICT(" + join(conflictCols, ",") + ") DO UPDATE SET " + join(sets, ",")
}

// forUpdateSQL 为 SELECT 追加行锁（仅 MySQL 需要；SQLite 依赖 BEGIN IMMEDIATE 与单写者语义）。
func (s *Store) forUpdateSQL(query string) string {
	if s.dialect == DialectMySQL {
		return query + " FOR UPDATE"
	}
	return query
}

func join(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	total := 0
	for _, it := range items {
		total += len(it) + len(sep)
	}
	buf := make([]byte, 0, total)
	for i, it := range items {
		if i > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, it...)
	}
	return string(buf)
}
