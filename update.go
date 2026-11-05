package xdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/schema"
)

var (
	// ErrMissingWhere：UPDATE / DELETE 没有任何 WHERE 条件。确需全表操作请调用 AllRows()。
	ErrMissingWhere = errors.New("xdb: UPDATE/DELETE without WHERE; call AllRows() to affect every row")
	// ErrNoPrimaryKey：按模型更新/删除时，模型没有带 pk 选项的字段。
	ErrNoPrimaryKey = errors.New("xdb: model has no primary key field (tag option pk)")
)

// UpdateQ 是 UPDATE 语句，不可变。
type UpdateQ[T any] struct {
	ex      Executor
	table   Table
	sets    []Assignment
	where   []Cond
	allRows bool
	err     error
}

// Update 构造针对模型 T 对应表的 UPDATE。只更新显式 Set 的列，绝不做零值推断。
//
//	n, err := xdb.Update[User](db).Set(U.Name.To("tom"), U.Age.Incr(1)).Where(U.ID.Eq(1)).Exec(ctx)
func Update[T any](ex Executor) *UpdateQ[T] {
	q := &UpdateQ[T]{ex: ex}
	m, err := schema.Of(reflect.TypeFor[T]())
	if err != nil {
		q.err = err
		return q
	}
	q.table = Table{name: m.Table}
	return q
}

// UpdateModel 按主键更新模型：默认更新全部非主键列，传入 cols 时只更新这些列。
// 返回的 UpdateQ 可以继续追加条件，例如乐观锁：.Where(U.Version.Eq(old))。
func UpdateModel[T any](ex Executor, v *T, cols ...Column) *UpdateQ[T] {
	q := Update[T](ex)
	if q.err != nil {
		return q
	}
	if v == nil {
		q.err = errors.New("xdb: UpdateModel with nil model")
		return q
	}
	m, _ := schema.Of(reflect.TypeFor[T]())
	if len(m.PK) == 0 {
		q.err = ErrNoPrimaryKey
		return q
	}
	rv := reflect.ValueOf(v).Elem()
	var fields []*schema.Field
	if len(cols) == 0 {
		for _, f := range m.Fields {
			if !f.Has("pk") {
				fields = append(fields, f)
			}
		}
	} else {
		for _, c := range cols {
			if err := checkColumn(q.table, c); err != nil {
				q.err = err
				return q
			}
			f, ok := m.Lookup(c.colName())
			if !ok {
				q.err = fmt.Errorf("%w: %s.%s", ErrUnknownField, m.Type, c.colName())
				return q
			}
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		q.err = errors.New("xdb: UpdateModel has no columns to update")
		return q
	}
	for _, f := range fields {
		q.sets = append(q.sets, Assignment{col: C[any](q.table, f.Column), val: valNode{fieldValue(rv, f.Index)}})
	}
	q.where = pkConds(q.table, m, rv)
	return q
}

func pkConds(t Table, m *schema.Model, rv reflect.Value) []Cond {
	conds := make([]Cond, len(m.PK))
	for i, f := range m.PK {
		conds[i] = C[any](t, f.Column).Eq(fieldValue(rv, f.Index))
	}
	return conds
}

func (q *UpdateQ[T]) clone() *UpdateQ[T] { c := *q; return &c }

// Set 追加赋值。
func (q *UpdateQ[T]) Set(as ...Assignment) *UpdateQ[T] {
	c := q.clone()
	c.sets = append(slices.Clip(c.sets), as...)
	return c
}

// Where 追加条件（AND）。
func (q *UpdateQ[T]) Where(conds ...Cond) *UpdateQ[T] {
	c := q.clone()
	c.where = append(slices.Clip(c.where), conds...)
	return c
}

// AllRows 明确允许无 WHERE 的全表更新。
func (q *UpdateQ[T]) AllRows() *UpdateQ[T] { c := q.clone(); c.allRows = true; return c }

// SQL 按指定方言渲染。
func (q *UpdateQ[T]) SQL(d dialect.Dialect) (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}
	if len(q.sets) == 0 {
		return "", nil, errors.New("xdb: UPDATE without SET")
	}
	where := And(q.where...)
	if where.IsZero() && !q.allRows {
		return "", nil, ErrMissingWhere
	}
	b := newBuilder(d)
	b.write("UPDATE ")
	q.table.buildFrom(b)
	b.write(" SET ")
	buildAssignments(b, q.table, q.sets)
	if !where.IsZero() {
		b.write(" WHERE ")
		where.build(b)
	}
	return b.result()
}

// ToSQL 按 Executor 的方言渲染。
func (q *UpdateQ[T]) ToSQL() (string, []any, error) {
	if q.ex == nil {
		return "", nil, ErrNoExecutor
	}
	return q.SQL(q.ex.Dialect())
}

// Exec 执行更新，返回受影响行数。
// 注意 MySQL 默认只统计值真正发生变化的行。
func (q *UpdateQ[T]) Exec(ctx context.Context) (int64, error) {
	return execStmt(ctx, q.ex, q.SQL)
}

// DeleteQ 是 DELETE 语句，不可变。
type DeleteQ[T any] struct {
	ex      Executor
	table   Table
	where   []Cond
	allRows bool
	err     error
}

// Delete 构造针对模型 T 对应表的 DELETE。
func Delete[T any](ex Executor) *DeleteQ[T] {
	q := &DeleteQ[T]{ex: ex}
	m, err := schema.Of(reflect.TypeFor[T]())
	if err != nil {
		q.err = err
		return q
	}
	q.table = Table{name: m.Table}
	return q
}

// DeleteModel 按主键删除模型。
func DeleteModel[T any](ex Executor, v *T) *DeleteQ[T] {
	q := Delete[T](ex)
	if q.err != nil {
		return q
	}
	if v == nil {
		q.err = errors.New("xdb: DeleteModel with nil model")
		return q
	}
	m, _ := schema.Of(reflect.TypeFor[T]())
	if len(m.PK) == 0 {
		q.err = ErrNoPrimaryKey
		return q
	}
	q.where = pkConds(q.table, m, reflect.ValueOf(v).Elem())
	return q
}

func (q *DeleteQ[T]) clone() *DeleteQ[T] { c := *q; return &c }

// Where 追加条件（AND）。
func (q *DeleteQ[T]) Where(conds ...Cond) *DeleteQ[T] {
	c := q.clone()
	c.where = append(slices.Clip(c.where), conds...)
	return c
}

// AllRows 明确允许无 WHERE 的全表删除。
func (q *DeleteQ[T]) AllRows() *DeleteQ[T] { c := q.clone(); c.allRows = true; return c }

// SQL 按指定方言渲染。
func (q *DeleteQ[T]) SQL(d dialect.Dialect) (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}
	where := And(q.where...)
	if where.IsZero() && !q.allRows {
		return "", nil, ErrMissingWhere
	}
	b := newBuilder(d)
	b.write("DELETE FROM ")
	q.table.buildFrom(b)
	if !where.IsZero() {
		b.write(" WHERE ")
		where.build(b)
	}
	return b.result()
}

// ToSQL 按 Executor 的方言渲染。
func (q *DeleteQ[T]) ToSQL() (string, []any, error) {
	if q.ex == nil {
		return "", nil, ErrNoExecutor
	}
	return q.SQL(q.ex.Dialect())
}

// Exec 执行删除，返回受影响行数。
func (q *DeleteQ[T]) Exec(ctx context.Context) (int64, error) {
	return execStmt(ctx, q.ex, q.SQL)
}

func execStmt(ctx context.Context, ex Executor, render func(dialect.Dialect) (string, []any, error)) (int64, error) {
	res, err := stmt{ex: ex, render: render}.exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
