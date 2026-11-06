package xdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

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
	model   *schema.Model
	table   Table
	joins   []joinClause
	sets    []Assignment
	where   []Cond
	touched []reflect.Value // 需要填充 autoUpdateTime 的可寻址模型（UpdateModel）
	// autoCols 记录由 UpdateModel 从模型快照取值的列名。只有这些列会被自动时间戳刷新：
	// 手动 Set 的值属于调用方的显式意图，必须保留。
	autoCols map[string]bool
	allRows  bool
	err      error
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
	q.model = m
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
			// 主键不进 SET；autoCreateTime 列只在插入时写入，更新时保留原值。
			// autoUpdateTime 保留在列表里，其值会在执行前被替换为当前时间。
			if f.Has("pk") || f == m.CreateTime {
				continue
			}
			fields = append(fields, f)
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
		if q.autoCols == nil {
			q.autoCols = map[string]bool{}
		}
		q.autoCols[f.Column] = true
	}
	q.where = pkConds(q.table, m, rv)
	q.touched = []reflect.Value{rv}
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

// Join 为多表 UPDATE 添加连接：MySQL 渲染为 UPDATE t JOIN u ON ... SET ...，
// PostgreSQL 渲染为 UPDATE t SET ... FROM u WHERE ...。on 为连接条件。
//
//	xdb.Update[Order](db).
//		Join(U, U.ID.EqCol(O.UserID)).
//		Set(O.State.To("frozen")).
//		Where(U.Status.Eq(0)).
func (q *UpdateQ[T]) Join(src Source, on ...Cond) *UpdateQ[T] {
	c := q.clone()
	cond := And(on...)
	if cond.IsZero() && c.err == nil {
		c.err = errors.New("xdb: UPDATE ... JOIN requires an ON condition")
	}
	c.joins = append(slices.Clip(c.joins), joinClause{kind: "JOIN", src: src, on: cond})
	return c
}

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
	q.buildTarget(b)
	b.write(" SET ")
	buildAssignments(b, q.table, q.sets)
	extra := q.buildJoinFrom(b)
	if !where.IsZero() || len(extra) > 0 {
		b.write(" WHERE ")
		And(append(append([]Cond{}, extra...), q.where...)...).build(b)
	}
	return b.result()
}

// buildTarget 写入 UPDATE 的目标表。MySQL 的多表 UPDATE 把 JOIN 直接接在表名后；
// PostgreSQL 则是 UPDATE a SET ... FROM b。
func (q *UpdateQ[T]) buildTarget(b *builder) { buildUpdateTarget(b, q.table, q.joins) }

// buildJoinFrom 为 PostgreSQL 渲染 FROM 子句（多表 UPDATE）并返回需要并入 WHERE 的连接条件；
// MySQL 已在 buildTarget 中内联 JOIN + ON，此处不输出。
func (q *UpdateQ[T]) buildJoinFrom(b *builder) []Cond { return buildUpdateJoinFrom(b, q.joins) }

// buildUpdateTarget 写入 UPDATE 的目标表；内联风格（MySQL）的方言在此输出 JOIN ... ON。
// UpdateQ 与软删除的 DeleteQ 共用。
func buildUpdateTarget(b *builder, table Table, joins []joinClause) {
	table.buildFrom(b)
	if b.d.Features().UpdateJoinStyle != dialect.UpdateJoinInline {
		return
	}
	for _, j := range joins {
		b.write(" " + j.kind + " ")
		j.src.buildFrom(b)
		b.write(" ON ")
		j.on.build(b)
	}
}

// buildUpdateJoinFrom 为 FROM 风格（PostgreSQL）的方言输出 FROM 子句，
// 返回需要并入 WHERE 的连接条件；内联风格或无连接时不输出。
func buildUpdateJoinFrom(b *builder, joins []joinClause) []Cond {
	if len(joins) == 0 || b.d.Features().UpdateJoinStyle == dialect.UpdateJoinInline {
		return nil
	}
	b.write(" FROM ")
	on := make([]Cond, 0, len(joins))
	for i, j := range joins {
		if i > 0 {
			b.write(", ")
		}
		j.src.buildFrom(b)
		on = append(on, j.on)
	}
	return on
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
//
// 由 UpdateModel 构造且模型带 autoUpdateTime 字段时，会在执行前把该字段设为当前时间
// 并加入 SET（若调用方显式 Set 过该列则尊重调用方的值）。
func (q *UpdateQ[T]) Exec(ctx context.Context) (int64, error) {
	// BeforeUpdate 只在 UpdateModel 这类持有具体模型的场景触发；
	// 按条件批量 Update[T] 不触发行级回调（无法知道会命中哪些行）。
	for _, row := range q.touched {
		if err := beforeUpdate(ctx, row.Addr().Interface()); err != nil {
			return 0, wrapCallback("BeforeUpdate", err)
		}
	}
	n, err := execStmt(ctx, q.ex, q.renderForExec)
	if err != nil {
		return n, err
	}
	for _, row := range q.touched {
		if err := afterUpdate(ctx, row.Addr().Interface()); err != nil {
			return n, wrapCallback("AfterUpdate", err)
		}
	}
	return n, nil
}

// renderForExec 先填充 autoUpdateTime，再渲染；只用于执行路径，ToSQL / SQL 保持不变。
func (q *UpdateQ[T]) renderForExec(d dialect.Dialect) (string, []any, error) {
	q = q.stampUpdateTime()
	return q.SQL(d)
}

// stampUpdateTime 返回一个副本：把 autoUpdateTime 列的赋值替换为当前时间并回写模型。
// 已由调用方显式 Set 过该列时不干预（显式优先），但仍会把模型字段同步成该值。
func (q *UpdateQ[T]) stampUpdateTime() *UpdateQ[T] {
	if q.err != nil || q.model == nil || q.model.UpdateTime == nil || len(q.touched) == 0 {
		return q
	}
	col := q.model.UpdateTime.Column
	now := q.now()
	idx := slices.IndexFunc(q.sets, func(a Assignment) bool {
		return a.col != nil && a.col.colName() == col
	})
	if idx < 0 || !q.autoCols[col] {
		// 该列不在本次 UPDATE 中，或由调用方显式 Set（显式意图优先）。
		return q
	}
	c := q.clone()
	c.sets = slices.Clone(q.sets)
	c.sets[idx].val = valNode{now}
	for _, row := range q.touched {
		setZeroTime(row, q.model.UpdateTime, now, true)
	}
	return c
}

func (q *UpdateQ[T]) now() time.Time {
	if q.ex != nil {
		return q.ex.session(false).db.now()
	}
	return time.Now().Truncate(time.Microsecond)
}

// DeleteQ 是 DELETE 语句，不可变。
type DeleteQ[T any] struct {
	ex    Executor
	table Table
	joins []joinClause
	where []Cond
	// softDelete 是模型的软删除列；非 nil 时 Delete 渲染为 UPDATE 该列。
	softDelete *schema.Field
	unscoped   bool
	allRows    bool
	err        error
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
	q.softDelete = m.SoftDelete
	return q
}

// Unscoped 让删除真正物理删除，即使模型声明了 softDelete 列。
func (q *DeleteQ[T]) Unscoped() *DeleteQ[T] { c := q.clone(); c.unscoped = true; return c }

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

// Join 为多表 DELETE 添加连接：MySQL 渲染为 DELETE t FROM t JOIN u ON ...，
// PostgreSQL 渲染为 DELETE FROM t USING u WHERE ...。
//
//	xdb.Delete[Order](db).Join(U, U.ID.EqCol(O.UserID)).Where(U.Status.Eq(0))
func (q *DeleteQ[T]) Join(src Source, on ...Cond) *DeleteQ[T] {
	c := q.clone()
	cond := And(on...)
	if cond.IsZero() && c.err == nil {
		c.err = errors.New("xdb: DELETE ... JOIN requires an ON condition")
	}
	c.joins = append(slices.Clip(c.joins), joinClause{kind: "JOIN", src: src, on: cond})
	return c
}

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
	// 软删除：DELETE 改写为 UPDATE ... SET <col> = CURRENT_TIMESTAMP WHERE <col> IS NULL AND ...。
	// 带 Join 时按多表 UPDATE 渲染，与 UpdateQ.Join 的方言差异一致。
	if q.softDelete != nil && !q.unscoped {
		b.write("UPDATE ")
		buildUpdateTarget(b, q.table, q.joins)
		b.write(" SET ")
		if len(q.joins) > 0 && b.d.Features().UpdateJoinStyle == dialect.UpdateJoinInline {
			// MySQL 多表 UPDATE 中裸列名可能有歧义（连接表也有 deleted_at）。
			// PostgreSQL 的 SET 不允许表限定，FROM 风格因此不加。
			b.ident(q.table.qualifier())
			b.write(".")
		}
		b.ident(q.softDelete.Column)
		b.write(" = ")
		b.node(nowNode{})
		extra := buildUpdateJoinFrom(b, q.joins)
		// 只标记尚未删除的行：重复删除不覆盖首次删除时间，受影响行数也如实反映。
		alive := wrap[bool](postfixNode{colNode{src: q.table, name: q.softDelete.Column}, "IS NULL"})
		b.write(" WHERE ")
		And(append(append(extra, alive), q.where...)...).build(b)
		return b.result()
	}
	var extra []Cond
	if len(q.joins) == 0 {
		b.write("DELETE FROM ")
		q.table.buildFrom(b)
	} else if b.d.Features().UpdateJoinStyle == dialect.UpdateJoinInline {
		// MySQL：DELETE t FROM t JOIN u ON ...
		b.write("DELETE ")
		b.ident(q.table.qualifier())
		b.write(" FROM ")
		q.table.buildFrom(b)
		for _, j := range q.joins {
			b.write(" " + j.kind + " ")
			j.src.buildFrom(b)
			b.write(" ON ")
			j.on.build(b)
		}
	} else {
		// PostgreSQL：DELETE FROM t USING u WHERE <on>
		b.write("DELETE FROM ")
		q.table.buildFrom(b)
		b.write(" USING ")
		extra = make([]Cond, 0, len(q.joins))
		for i, j := range q.joins {
			if i > 0 {
				b.write(", ")
			}
			j.src.buildFrom(b)
			extra = append(extra, j.on)
		}
	}
	if !where.IsZero() || len(extra) > 0 {
		b.write(" WHERE ")
		And(append(extra, q.where...)...).build(b)
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
