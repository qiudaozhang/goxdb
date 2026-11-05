// Package xdb 是 goxdb 面向用户的入口包。
//
// 基本用法：
//
//	import (
//		"github.com/qiudaozhang/goxdb"
//		_ "github.com/qiudaozhang/goxdb/dialect/postgres"
//	)
//
//	db, err := xdb.Open("postgres", dsn)
//	users, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT * FROM users WHERE age > ?", 18))
//
//	err = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
//		_, err := xdb.Raw(tx, "UPDATE ...").Exec(ctx)
//		return err
//	})
package xdb

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
)

// DB 是数据库句柄，内部持有连接池，可并发使用，应在整个程序生命周期中复用。
type DB struct {
	std      *sql.DB
	d        dialect.Dialect
	hooks    []Hook
	logger   *slog.Logger
	strictTx bool

	autoIncMu   sync.Mutex // 保护 autoIncStep
	autoIncStep int64      // MySQL auto_increment_increment 缓存，0 表示未读取
}

// Pool 是连接池配置，零值字段表示保持 database/sql 的默认值。
type Pool struct {
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
	MaxIdleTime time.Duration
}

type options struct {
	hooks    []Hook
	logger   *slog.Logger
	strictTx bool
	pool     *Pool
}

// Option 配置 DB。
type Option func(*options)

// WithHook 注册查询拦截器。
func WithHook(h ...Hook) Option { return func(o *options) { o.hooks = append(o.hooks, h...) } }

// WithLogger 设置框架内部告警（如事务误用）使用的日志器，默认 slog.Default()。
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithPool 设置连接池参数。
func WithPool(p Pool) Option { return func(o *options) { o.pool = &p } }

// WithStrictTx 开启严格事务模式：当 context 处于某个事务中，却直接用 *DB
// 执行语句时返回 ErrExecutorMisuse，而不仅是记录告警。建议在开发和测试环境开启。
func WithStrictTx() Option { return func(o *options) { o.strictTx = true } }

// Open 按方言名打开数据库。方言需要先通过导入方言包注册，例如：
//
//	import _ "github.com/qiudaozhang/goxdb/dialect/mysql"
//
// Open 不会建立连接，如需立即校验连通性请调用 Ping。
func Open(dialectName, dsn string, opts ...Option) (*DB, error) {
	d, ok := dialect.Lookup(dialectName)
	if !ok {
		return nil, fmt.Errorf("xdb: unknown dialect %q (registered: %v); did you import the dialect package?", dialectName, dialect.Names())
	}
	std, err := sql.Open(d.DriverName(), dsn)
	if err != nil {
		return nil, err
	}
	return New(std, d, opts...), nil
}

// New 用已有的 *sql.DB 构造 DB，便于接入已存在的连接池。
func New(std *sql.DB, d dialect.Dialect, opts ...Option) *DB {
	if std == nil || d == nil {
		panic("xdb: New requires a non-nil *sql.DB and dialect")
	}
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	if p := o.pool; p != nil {
		if p.MaxOpen > 0 {
			std.SetMaxOpenConns(p.MaxOpen)
		}
		if p.MaxIdle > 0 {
			std.SetMaxIdleConns(p.MaxIdle)
		}
		if p.MaxLifetime > 0 {
			std.SetConnMaxLifetime(p.MaxLifetime)
		}
		if p.MaxIdleTime > 0 {
			std.SetConnMaxIdleTime(p.MaxIdleTime)
		}
	}
	return &DB{std: std, d: d, hooks: o.hooks, logger: o.logger, strictTx: o.strictTx}
}

// Dialect 返回当前方言。
func (db *DB) Dialect() dialect.Dialect { return db.d }

// SQL 返回底层 *sql.DB，用于框架未覆盖的场景。
func (db *DB) SQL() *sql.DB { return db.std }

// Ping 校验数据库连通性。
func (db *DB) Ping(ctx context.Context) error { return db.std.PingContext(ctx) }

// Stats 返回连接池统计。
func (db *DB) Stats() sql.DBStats { return db.std.Stats() }

// Close 关闭连接池。
func (db *DB) Close() error { return db.std.Close() }

func (db *DB) session() session { return session{db: db, conn: db.std} }
