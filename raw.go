package xdb

import (
	"context"
	"database/sql"
	"iter"
	"reflect"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/scan"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

// Statement 是可执行并返回结果集的语句，由 Raw 以及后续的查询构建器实现。
// 配合泛型函数 All / One / Iter 使用。
type Statement interface {
	stmt() stmt
}

type stmt struct {
	ex     Executor
	render func(d dialect.Dialect) (string, []any, error)
	strict bool
}

// query 渲染并执行查询。
func (s stmt) query(ctx context.Context) (*Rows, error) {
	if s.ex == nil {
		return nil, ErrNoExecutor
	}
	q, a, err := s.render(s.ex.Dialect())
	if err != nil {
		return nil, err
	}
	return doQuery(ctx, s.ex, q, a)
}

func (s stmt) exec(ctx context.Context) (sql.Result, error) {
	if s.ex == nil {
		return nil, ErrNoExecutor
	}
	q, a, err := s.render(s.ex.Dialect())
	if err != nil {
		return nil, err
	}
	return doExec(ctx, s.ex, q, a)
}

// RawQuery 是原始 SQL 语句。占位符统一写 "?"，会按方言自动改写
// （PostgreSQL 为 $n）；切片参数会自动展开为 "?, ?, ?"，用于 IN 查询。
// 字面量 "?" 请写作 "??"。
type RawQuery struct {
	s stmt
}

// Raw 构造原始 SQL 语句。
//
//	xdb.Raw(db, "SELECT * FROM users WHERE id IN (?) AND age > ?", ids, 18)
func Raw(ex Executor, query string, args ...any) *RawQuery {
	render := func(d dialect.Dialect) (string, []any, error) { return sqlbind.Bind(d, query, args) }
	return &RawQuery{s: stmt{ex: ex, render: render, strict: true}}
}

// Lenient 返回宽松模式的副本：结果列找不到对应字段时忽略而不是报错。
// 默认是严格模式，以便尽早发现列名拼写错误。
func (r *RawQuery) Lenient() *RawQuery {
	c := *r
	c.s.strict = false
	return &c
}

func (r *RawQuery) stmt() stmt { return r.s }

// Exec 执行不返回结果集的语句。
func (r *RawQuery) Exec(ctx context.Context) (sql.Result, error) {
	return r.s.exec(ctx)
}

// Rows 执行查询并返回结果集，调用方必须关闭。
func (r *RawQuery) Rows(ctx context.Context) (*Rows, error) {
	return r.s.query(ctx)
}

// Scan 执行查询并把结果写入 dest，dest 支持：
//
//   - *[]T、*[]*T：读取全部行（无结果时为空切片）
//   - *T：读取恰好一行，无结果返回 ErrNotFound，多于一行返回 ErrMultipleRows
//
// 其中 T 可以是结构体、标量（含 time.Time、sql.Scanner 实现）或 map[string]any。
func (r *RawQuery) Scan(ctx context.Context, dest any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrInvalidDest
	}
	target := rv.Elem()
	tt := target.Type()
	if tt.Kind() == reflect.Slice && tt.Elem().Kind() != reflect.Uint8 {
		c, err := openCursor(ctx, r.s, tt.Elem())
		if err != nil {
			return err
		}
		out := reflect.MakeSlice(tt, 0, 8)
		for c.rows.Next() {
			v := reflect.New(tt.Elem()).Elem()
			if err := c.scan(v); err != nil {
				return c.close(err)
			}
			out = reflect.Append(out, v)
		}
		if err := c.close(nil); err != nil {
			return err
		}
		target.Set(out)
		return nil
	}
	v := reflect.New(tt).Elem()
	if err := scanOne(ctx, r.s, v); err != nil {
		return err
	}
	target.Set(v)
	return nil
}

// All 执行语句并返回全部行；无结果时返回空切片（非 nil）。
func All[T any](ctx context.Context, s Statement) ([]T, error) {
	c, err := openCursor(ctx, s.stmt(), reflect.TypeFor[T]())
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, 8)
	for c.rows.Next() {
		var x T
		if err := c.scan(reflect.ValueOf(&x).Elem()); err != nil {
			return nil, c.close(err)
		}
		if err := afterFind(ctx, &x); err != nil {
			return nil, c.close(wrapCallback("AfterFind", err))
		}
		out = append(out, x)
	}
	if err := c.close(nil); err != nil {
		return nil, err
	}
	return out, nil
}

// One 执行语句并返回恰好一行：无结果返回 ErrNotFound，多于一行返回 ErrMultipleRows。
func One[T any](ctx context.Context, s Statement) (T, error) {
	var x T
	if err := scanOne(ctx, s.stmt(), reflect.ValueOf(&x).Elem()); err != nil {
		var zero T
		return zero, err
	}
	if err := afterFind(ctx, &x); err != nil {
		var zero T
		return zero, wrapCallback("AfterFind", err)
	}
	return x, nil
}

// Iter 以流式方式逐行读取，适合大结果集；提前 break 会自动关闭结果集。
//
//	for u, err := range xdb.Iter[User](ctx, q) {
//		if err != nil { return err }
//		...
//	}
//
// 注意：在事务中迭代期间不能在同一事务上执行其它语句（会返回 ErrTxConcurrentUse）。
func Iter[T any](ctx context.Context, s Statement) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		c, err := openCursor(ctx, s.stmt(), reflect.TypeFor[T]())
		if err != nil {
			yield(zero, err)
			return
		}
		for c.rows.Next() {
			var x T
			if err := c.scan(reflect.ValueOf(&x).Elem()); err != nil {
				yield(zero, c.close(err))
				return
			}
			if err := afterFind(ctx, &x); err != nil {
				yield(zero, c.close(wrapCallback("AfterFind", err)))
				return
			}
			if !yield(x, nil) {
				_ = c.close(nil)
				return
			}
		}
		if err := c.close(nil); err != nil {
			yield(zero, err)
		}
	}
}

func scanOne(ctx context.Context, s stmt, dst reflect.Value) error {
	c, err := openCursor(ctx, s, dst.Type())
	if err != nil {
		return err
	}
	if !c.rows.Next() {
		if err := c.close(nil); err != nil {
			return err
		}
		return ErrNotFound
	}
	if err := c.scan(dst); err != nil {
		return c.close(err)
	}
	if c.rows.Next() {
		return c.close(ErrMultipleRows)
	}
	return c.close(nil)
}

type cursor struct {
	rows *Rows
	plan *scan.Plan
}

func openCursor(ctx context.Context, s stmt, t reflect.Type) (*cursor, error) {
	rows, err := s.query(ctx)
	if err != nil {
		return nil, err
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	plan, err := scan.NewPlan(t, cols, s.strict)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	return &cursor{rows: rows, plan: plan}, nil
}

func (c *cursor) scan(dst reflect.Value) error { return c.plan.ScanRow(c.rows, dst) }

// close 关闭结果集并合并错误：优先返回调用方传入的错误，其次是迭代错误，最后是关闭错误。
func (c *cursor) close(err error) error {
	iterErr := c.rows.Err()
	closeErr := c.rows.Close()
	switch {
	case err != nil:
		return err
	case iterErr != nil:
		return iterErr
	default:
		return closeErr
	}
}
