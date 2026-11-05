package xdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/schema"
)

// InsertSelectQ 是 INSERT INTO ... SELECT 语句（M2）。
//
// 与 InsertQ 的区别：数据来源不是 Go 值，而是另一条查询，因此没有自增回填，
// 也不会填充 autoCreateTime / autoUpdateTime（时间应由 SELECT 侧给出）。
type InsertSelectQ[T any] struct {
	ex     Executor
	model  *schema.Model
	table  Table
	cols   []Column
	source Node
	err    error
}

// InsertSelect 构造 INSERT INTO <T 的表> ... SELECT ...。
//
// 列由 T 的模型字段决定（排除自增列），SELECT 的投影必须按同样顺序给出等量列。
//
//	xdb.InsertSelect[Archive](db,
//		xdb.Select[Archive](nil, O.ID, O.CustomerID, O.Amount).From(O).Where(O.State.Eq("old")),
//	).Exec(ctx)
func InsertSelect[T any](ex Executor, source Node) *InsertSelectQ[T] {
	q := &InsertSelectQ[T]{ex: ex, source: source}
	m, err := schema.Of(reflect.TypeFor[T]())
	if err != nil {
		q.err = err
		return q
	}
	q.model, q.table = m, Table{name: m.Table}
	return q
}

// Columns 显式指定目标列（顺序需与 SELECT 投影一致），默认为全部非自增列。
func (q *InsertSelectQ[T]) Columns(cols ...Column) *InsertSelectQ[T] {
	c := *q
	c.cols = cols
	return &c
}

func (q *InsertSelectQ[T]) clone() *InsertSelectQ[T] { c := *q; return &c }

// resolveCols 返回目标列，按列名映射到目标模型。
//
// 注意：这里不要求列属于目标表（不像 Update / Insert 的 checkColumn）：
// INSERT ... SELECT 的列天然来自源表，而目标表（如归档表）往往没有类型化描述符。
// 因此按列名解析：列名必须存在于目标模型，但不限于目标表的描述符实例。
func (q *InsertSelectQ[T]) resolveCols() ([]*schema.Field, error) {
	if q.err != nil {
		return nil, q.err
	}
	if q.cols != nil {
		fields := make([]*schema.Field, 0, len(q.cols))
		for _, c := range q.cols {
			if c == nil {
				return nil, errEmptyExpr
			}
			f, ok := q.model.Lookup(c.colName())
			if !ok {
				return nil, fmt.Errorf("%w: %s.%s", ErrUnknownField, q.model.Type, c.colName())
			}
			fields = append(fields, f)
		}
		if len(fields) == 0 {
			return nil, errors.New("xdb: INSERT ... SELECT has no columns")
		}
		return fields, nil
	}
	fields := make([]*schema.Field, 0, len(q.model.Fields))
	for _, f := range q.model.Fields {
		if f != q.model.Auto {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return nil, errors.New("xdb: INSERT ... SELECT has no columns")
	}
	return fields, nil
}

// SQL 按指定方言渲染。
func (q *InsertSelectQ[T]) SQL(d dialect.Dialect) (string, []any, error) {
	fields, err := q.resolveCols()
	if err != nil {
		return "", nil, err
	}
	if q.source == nil {
		return "", nil, errors.New("xdb: INSERT ... SELECT requires a source query")
	}
	b := newBuilder(d)
	b.write("INSERT INTO ")
	q.table.buildFrom(b)
	b.write(" (")
	for i, f := range fields {
		if i > 0 {
			b.write(", ")
		}
		b.ident(f.Column)
	}
	b.write(") ")
	// 作为 INSERT 的数据源时不需要子查询的外层括号，直接渲染 SELECT 本体。
	if s, ok := q.source.(interface{ buildSelectBody(b *builder) }); ok {
		s.buildSelectBody(b)
	} else {
		b.node(q.source)
	}
	return b.result()
}

// ToSQL 按 Executor 的方言渲染。
func (q *InsertSelectQ[T]) ToSQL() (string, []any, error) {
	if q.ex == nil {
		return "", nil, ErrNoExecutor
	}
	return q.SQL(q.ex.Dialect())
}

// Exec 执行并返回插入的行数。
func (q *InsertSelectQ[T]) Exec(ctx context.Context) (int64, error) {
	return execStmt(ctx, q.ex, q.SQL)
}
