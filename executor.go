package xdb

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

// Executor 是语句的执行者，由 *DB 与 *Tx 实现。
//
// 所有查询构建器都以 Executor 作为第一个参数，因此同一段代码在事务内外
// 无需任何修改即可复用。接口含未导出方法，外部类型无法实现，
// 从而保证所有执行路径都经过 Hook、错误归一化与事务状态检查。
type Executor interface {
	Dialect() dialect.Dialect
	// session 返回本次操作的执行上下文。readOnly 为 true 表示这是只读查询，
	// 读写分离时可能路由到只读副本；事务始终使用主库。
	session(readOnly bool) session
}

type sqlConn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type session struct {
	db   *DB
	conn sqlConn
	tx   *Tx // 非事务时为 nil
}

// AsIs 包装一个参数，使其即使是切片也不会被展开为多个占位符。
// 典型用途：PostgreSQL 的数组参数 `WHERE id = ANY(?)`。
func AsIs(v any) any { return sqlbind.AsIs{V: v} }

// enter 在执行语句前做状态检查，返回结束时需要调用的释放函数。
func (s session) enter(ctx context.Context) (release func(), err error) {
	if s.tx != nil {
		return s.tx.acquire()
	}
	if err := s.db.checkMisuse(ctx); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// doExec 执行已渲染完成（占位符为方言原生形式）的语句。
func doExec(ctx context.Context, ex Executor, q string, a []any) (sql.Result, error) {
	s := ex.session(false)
	release, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	ev := &QueryEvent{Op: OpExec, Query: q, Args: a, InTx: s.tx != nil, Start: time.Now(), Rows: -1}
	hctx := s.db.before(ctx, ev)
	res, err := s.conn.ExecContext(hctx, q, a...)
	err = s.db.wrapErr(err, q)
	if s.tx != nil {
		err = s.tx.noteErr(err)
	}
	if err == nil {
		if n, e := res.RowsAffected(); e == nil {
			ev.Rows = n
		}
	}
	ev.Err = err
	s.db.after(hctx, ev)
	return res, err
}

// doQuery 执行已渲染完成的查询。readOnly 为 true 时可能路由到只读副本；
// INSERT/UPDATE/DELETE ... RETURNING 虽然返回结果集，但属于写操作，必须传 false。
func doQuery(ctx context.Context, ex Executor, q string, a []any, readOnly bool) (*Rows, error) {
	s := ex.session(readOnly)
	release, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}

	ev := &QueryEvent{Op: OpQuery, Query: q, Args: a, InTx: s.tx != nil, Start: time.Now(), Rows: -1}
	hctx := s.db.before(ctx, ev)
	rows, err := s.conn.QueryContext(hctx, q, a...)
	if err != nil {
		err = s.db.wrapErr(err, q)
		if s.tx != nil {
			err = s.tx.noteErr(err)
		}
		ev.Err = err
		s.db.after(hctx, ev)
		release()
		return nil, err
	}
	r := &Rows{Rows: rows, db: s.db, query: q}
	r.finish = func(err error, n int64) {
		if s.tx != nil {
			_ = s.tx.noteErr(err) // 死锁也可能在逐行读取时才报告；Err() 的返回值不在此处改写
		}
		ev.Err = err
		ev.Rows = n
		s.db.after(hctx, ev)
		release()
	}
	return r, nil
}

// Rows 包装 *sql.Rows：错误经过归一化；关闭（或读完）时触发 Hook 的 After
// 并释放事务占用。使用方式与 *sql.Rows 相同，务必 defer rows.Close()。
type Rows struct {
	*sql.Rows
	db     *DB
	query  string
	n      int64
	once   sync.Once
	finish func(err error, n int64)
}

// Next 前进到下一行；读完时自动关闭结果集并结束本次查询。
//
// 注意：为了保证事务中连接占用的释放时机确定，读完即关闭，
// 因此不支持多结果集（NextResultSet）；需要时请使用 DB.SQL() 走原生接口。
func (r *Rows) Next() bool {
	if r.Rows.Next() {
		r.n++
		return true
	}
	_ = r.Close()
	return false
}

// Close 关闭结果集，可重复调用。
func (r *Rows) Close() error {
	err := r.Rows.Close()
	r.done()
	return err
}

// Err 返回迭代过程中的错误（已归一化）。
func (r *Rows) Err() error { return r.db.wrapErr(r.Rows.Err(), r.query) }

func (r *Rows) done() {
	r.once.Do(func() { r.finish(r.Err(), r.n) })
}
