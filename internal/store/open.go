package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// 纯 Go SQLite 驱动（见 docs/implementation.md 10.4：禁止使用 mattn/go-sqlite3 以免引入 cgo）。
	_ "modernc.org/sqlite"

	"github.com/jmoiron/sqlx"

	// MySQL 纯 Go 驱动。
	_ "github.com/go-sql-driver/mysql"
)

// driverName 返回 database/sql 使用的驱动名。
func driverName(d Dialect) string {
	if d == DialectMySQL {
		return "mysql"
	}
	return "sqlite"
}

// openDB 按方言打开数据库连接。
func openDB(ctx context.Context, opt Options) (*sqlx.DB, error) {
	switch opt.Dialect {
	case DialectMySQL:
		db, err := sqlx.Open("mysql", opt.MySQLDSN)
		if err != nil {
			return nil, fmt.Errorf("store: 打开 MySQL 失败: %w", err)
		}
		return db, nil

	case DialectSQLite, "":
		// 全新部署时数据库文件所在目录通常还不存在（例如默认的 data/）。
		// SQLite 不会自动创建父目录，会直接报 "unable to open database file (14)"，
		// 且默认日志只写文件、错误不易被发现。这里主动建好，保证开箱即可启动。
		if dir := filepath.Dir(opt.SQLitePath); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, fmt.Errorf("store: 创建数据库目录 %s 失败: %w", dir, mkErr)
			}
		}
		dsn, err := sqliteDSN(opt.SQLitePath)
		if err != nil {
			return nil, err
		}
		db, err := sqlx.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("store: 打开 SQLite 失败: %w", err)
		}
		// SQLite 单写者语义：限制连接数，减少 SQLITE_BUSY。
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		return db, nil

	default:
		return nil, fmt.Errorf("store: 不支持的数据库方言 %q", opt.Dialect)
	}
}

// sqliteDSN 组装 SQLite 连接串。
//
// 必须显式设置（见 docs/implementation.md 10.2）：
//   - journal_mode=WAL  提升读写并发，避免长事务阻塞读；
//   - busy_timeout      写锁竞争时等待而非立即报错；
//   - foreign_keys=ON   启用外键约束（SQLite 默认关闭）；
//   - synchronous=NORMAL 在 WAL 下兼顾安全与性能；
//   - _txlock=immediate 让写事务立即取写锁，实现"分配防超卖"所需的语义。
func sqliteDSN(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", fmt.Errorf("store: 未配置 SQLite 数据库路径")
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("store: SQLite 数据库必须位于本地磁盘，不允许网络路径: %q", p)
	}

	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(ON)",
		"_pragma=synchronous(NORMAL)",
		"_txlock=immediate",
	}
	// file: URI 形式才能携带参数；路径中的反斜杠在 URI 中需转义。
	u := "file:" + strings.ReplaceAll(p, `\`, "/")
	return u + "?" + strings.Join(pragmas, "&"), nil
}

// MaintenanceSQLite 执行 SQLite 的定期维护（WAL 检查点 + 空间回收）。
//
// 由定时任务调用，避免 WAL 无限增长。
func (s *Store) MaintenanceSQLite(ctx context.Context) error {
	if s.dialect != DialectSQLite || s.root == nil {
		return nil
	}
	if _, err := s.root.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: WAL checkpoint 失败: %w", err)
	}
	return nil
}

// escapeDSNValue 供需要拼装 DSN 的场景使用。
func escapeDSNValue(v string) string { return url.QueryEscape(v) }
