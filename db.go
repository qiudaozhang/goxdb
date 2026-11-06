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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
)

// DB 是数据库句柄，内部持有连接池，可并发使用，应在整个程序生命周期中复用。
type DB struct {
	std *sql.DB
	// replica 是只读副本连接池；为 nil 时所有读也走主库。
	replica  *sql.DB
	d        dialect.Dialect
	hooks    []Hook
	logger   *slog.Logger
	strictTx bool

	autoIncMu   sync.Mutex // 保护 autoIncStep
	autoIncStep int64      // MySQL auto_increment_increment 缓存，0 表示未读取
	now         func() time.Time
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
	now      func() time.Time
	replica  *sql.DB
}

// WithReplica 配置只读副本连接池，用于读写分离。
//
// 只读查询（All / One / Iter / Count / Exists / Pluck、以及以 SELECT 开头的 Raw 语句）会走副本；
// 写操作与事务**始终**走主库——事务一旦开启就固定在主库，避免读到不一致的快照。
//
// 主从延迟意味着刚写入的数据可能立刻读不到。需要强一致读时，在事务中读取
// （事务固定在主库），或用 xdb.Primary(db) 显式指定主库执行器。
func WithReplica(std *sql.DB) Option { return func(o *options) { o.replica = std } }

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

// WithClock 替换框架读取“当前时间”的方式，影响 autoCreateTime / autoUpdateTime 的填充。
// 默认为 time.Now，测试中可注入固定时钟使写入结果可预测。返回值同样会被截断到微秒。
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// microClock 把时钟截断到微秒并去掉单调时钟读数：MySQL DATETIME(6) / PostgreSQL TIMESTAMP
// 最多存微秒，而 Linux 上 time.Now 带纳秒。不截断时，自动填充后模型里的值与读回的值不相等，
// 用 == 或 Equal 比较、做缓存键或乐观锁条件都会出错。
func microClock(now func() time.Time) func() time.Time {
	return func() time.Time { return now().Truncate(time.Microsecond) }
}

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
	if o.now == nil {
		o.now = time.Now
	}
	o.now = microClock(o.now)
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
	if o.replica != nil && o.replica == std {
		o.replica = nil // 同一个池没有必要区分
	}
	return &DB{std: std, replica: o.replica, d: d, hooks: o.hooks, logger: o.logger, strictTx: o.strictTx, now: o.now}
}

// Dialect 返回当前方言。
func (db *DB) Dialect() dialect.Dialect { return db.d }

// SQL 返回底层 *sql.DB，用于框架未覆盖的场景。
func (db *DB) SQL() *sql.DB { return db.std }

// Ping 校验数据库连通性。
func (db *DB) Ping(ctx context.Context) error { return db.std.PingContext(ctx) }

// Stats 返回连接池统计。
func (db *DB) Stats() sql.DBStats { return db.std.Stats() }

// Close 关闭主库与副本连接池（副本未配置时只关主库）。
func (db *DB) Close() error {
	if db.replica != nil {
		return errors.Join(db.std.Close(), db.replica.Close())
	}
	return db.std.Close()
}

// session 按操作类型选择连接池：只读查询优先走副本，写与事务始终走主库。
func (db *DB) session(readOnly bool) session {
	if readOnly && db.replica != nil {
		return session{db: db, conn: db.replica}
	}
	return session{db: db, conn: db.std}
}

// Replica 返回只读副本连接池（未配置时为 nil）。
func (db *DB) Replica() *sql.DB { return db.replica }

// Primary 返回主库连接池。
func (db *DB) Primary() *sql.DB { return db.std }

// primaryExecutor 强制所有操作走主库的 Executor 包装。
type primaryExecutor struct{ *DB }

func (p primaryExecutor) session(bool) session {
	return session{db: p.DB, conn: p.DB.std}
}

// Primary 返回一个强制走主库的执行器，用于读写分离下的强一致读：
//
//	n, err := xdb.One[int64](ctx, xdb.Query[int64](xdb.Primary(db), ...))
func Primary(db *DB) Executor { return primaryExecutor{db} }
