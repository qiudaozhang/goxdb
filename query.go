package xdb

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"slices"
	"sync"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/schema"
)

// ErrLockOutsideTx：在非事务 Executor 上使用 FOR UPDATE / FOR SHARE，锁会在语句结束时立即释放，毫无意义。
var ErrLockOutsideTx = errors.New("xdb: row locks (FOR UPDATE / FOR SHARE) require a transaction")

// Q 是 SELECT 查询，T 为结果行的类型。
//
// Q 是不可变的：每个方法都返回新的 Q，原对象不受影响，因此可以放心复用
// 公共的“基础查询”再分别追加条件。
//
// Q 同时是一个类型为 T 的表达式（渲染为带括号的子查询），可直接用于
// InSub、Exists、Derived、CTE 等位置。
type Q[T any] struct {
	ex       Executor
	c        selectCore
	preloads []preloadSpec
	// softDelete 是模型的软删除列（无则 nil）。
	softDelete *schema.Field
	// unscoped 为 true 时不再自动附加软删除过滤。
	unscoped bool
	lenient  bool
	err      error
}

type joinClause struct {
	kind string
	src  Source
	on   Cond
}

type unionPart struct {
	all  bool
	core *selectCore
}

type lockClause struct {
	mode string // "UPDATE" / "SHARE"
	wait string // "" / "NOWAIT" / "SKIP LOCKED"
}

type selectCore struct {
	ctes      []CTETable
	recursive bool
	distinct  bool
	cols      []Node
	from      Source
	joins     []joinClause
	where     []Cond
	groupBy   []Node
	having    []Cond
	unions    []unionPart
	orderBy   []Order
	limit     int64 // <0 表示无
	offset    int64
	lock      lockClause
	// softDelete 不为空时，在 WHERE 中附加 "<col> IS NULL"。
	softDelete *schema.Field
}

// Query 构造以模型 T 为结果、以 T 对应的表为数据源的查询，选择列表为模型的全部列。
//
//	users, err := xdb.Query[User](db).Where(U.Age.Ge(18)).OrderBy(U.ID.Desc()).Limit(20).All(ctx)
func Query[T any](ex Executor) *Q[T] {
	q := &Q[T]{ex: ex, c: selectCore{limit: -1}}
	m, err := schema.Of(reflect.TypeFor[T]())
	if err != nil {
		q.err = err
		return q
	}
	tbl := Table{name: m.Table, model: m}
	q.c.from = tbl
	q.softDelete = m.SoftDelete
	q.c.cols = allColumns(tbl, m)
	return q
}

// colList 是模型全部列的渲染模板：colNode 是值类型且源表相同，
// 因此同一模型的所有 Query 可以共享一份切片。
type colList struct {
	cols []Node
}

var colLists sync.Map // *schema.Model -> *colList

// allColumns 返回模型全部列的选择列表。
//
// 切片只读复用：选择列表在构建过程中从不被修改（Where/OrderBy 等只增删自身字段），
// 因此 Query[T] 不必每次重新构造 15 个 colNode。这消除了热路径上的一批分配。
func allColumns(tbl Table, m *schema.Model) []Node {
	// 无别名时才能共享：带别名的表需要不同的限定符。
	if tbl.alias != "" {
		cols := make([]Node, 0, len(m.Fields))
		for _, f := range m.Fields {
			cols = append(cols, colNode{src: tbl, name: f.Column})
		}
		return cols
	}
	if v, ok := colLists.Load(m); ok {
		return v.(*colList).cols
	}
	cols := make([]Node, 0, len(m.Fields))
	for _, f := range m.Fields {
		cols = append(cols, colNode{src: tbl, name: f.Column})
	}
	actual, _ := colLists.LoadOrStore(m, &colList{cols: cols})
	return actual.(*colList).cols
}

// Select 构造自定义投影的查询，结果按列名（或别名）映射到 T。
// cols 为空时为 SELECT *。作为子查询使用时 ex 可以为 nil。
//
//	xdb.Select[UserStat](db, U.ID.As("user_id"), xdb.Sum(O.Amount).As("total")).From(U).LeftJoin(...)
func Select[T any](ex Executor, cols ...Node) *Q[T] {
	return &Q[T]{ex: ex, c: selectCore{cols: cols, limit: -1}}
}

func (q *Q[T]) clone() *Q[T] { c := *q; return &c }

// ---------- 构建方法（均返回新对象） ----------

// From 设置数据源（表、派生表、CTE）。
func (q *Q[T]) From(src Source) *Q[T] { c := q.clone(); c.c.from = src; return c }

// Columns 替换选择列表。
func (q *Q[T]) Columns(cols ...Node) *Q[T] { c := q.clone(); c.c.cols = cols; return c }

// Distinct 即 SELECT DISTINCT。
func (q *Q[T]) Distinct() *Q[T] { c := q.clone(); c.c.distinct = true; return c }

// Join 即 INNER JOIN src ON cond1 AND cond2 ...。
func (q *Q[T]) Join(src Source, on ...Cond) *Q[T] { return q.join("JOIN", src, on) }

// LeftJoin 即 LEFT JOIN。
func (q *Q[T]) LeftJoin(src Source, on ...Cond) *Q[T] { return q.join("LEFT JOIN", src, on) }

// RightJoin 即 RIGHT JOIN。
func (q *Q[T]) RightJoin(src Source, on ...Cond) *Q[T] { return q.join("RIGHT JOIN", src, on) }

// CrossJoin 即 CROSS JOIN。
func (q *Q[T]) CrossJoin(src Source) *Q[T] { return q.join("CROSS JOIN", src, nil) }

func (q *Q[T]) join(kind string, src Source, on []Cond) *Q[T] {
	c := q.clone()
	cond := And(on...)
	if kind != "CROSS JOIN" && cond.IsZero() && c.err == nil {
		c.err = errors.New("xdb: " + kind + " requires an ON condition")
	}
	c.c.joins = append(slices.Clip(c.c.joins), joinClause{kind: kind, src: src, on: cond})
	return c
}

// Where 追加 WHERE 条件（与已有条件为 AND 关系），零值条件被忽略。
func (q *Q[T]) Where(conds ...Cond) *Q[T] {
	c := q.clone()
	c.c.where = append(slices.Clip(c.c.where), conds...)
	return c
}

// GroupBy 追加 GROUP BY 表达式。
func (q *Q[T]) GroupBy(exprs ...Node) *Q[T] {
	c := q.clone()
	c.c.groupBy = append(slices.Clip(c.c.groupBy), exprs...)
	return c
}

// Having 追加 HAVING 条件。
func (q *Q[T]) Having(conds ...Cond) *Q[T] {
	c := q.clone()
	c.c.having = append(slices.Clip(c.c.having), conds...)
	return c
}

// OrderBy 追加排序。
func (q *Q[T]) OrderBy(orders ...Order) *Q[T] {
	c := q.clone()
	c.c.orderBy = append(slices.Clip(c.c.orderBy), orders...)
	return c
}

// Limit 设置 LIMIT。
func (q *Q[T]) Limit(n int64) *Q[T] { c := q.clone(); c.c.limit = n; return c }

// Offset 设置 OFFSET。
func (q *Q[T]) Offset(n int64) *Q[T] { c := q.clone(); c.c.offset = n; return c }

// Page 按页码（从 1 开始）与页大小设置 LIMIT / OFFSET。
func (q *Q[T]) Page(page, size int64) *Q[T] {
	page = max(page, 1)
	return q.Limit(size).Offset((page - 1) * size)
}

// With 声明 CTE（WITH ...）。
func (q *Q[T]) With(ctes ...CTETable) *Q[T] {
	c := q.clone()
	c.c.ctes = append(slices.Clip(c.c.ctes), ctes...)
	return c
}

// WithRecursive 声明递归 CTE（WITH RECURSIVE ...）。
func (q *Q[T]) WithRecursive(ctes ...CTETable) *Q[T] {
	c := q.With(ctes...)
	c.c.recursive = true
	return c
}

// Union 即 UNION（去重）。ORDER BY / LIMIT 作用于整个组合结果，
// 被合并的查询自身不能带 ORDER BY / LIMIT / 锁。
func (q *Q[T]) Union(other *Q[T]) *Q[T] { return q.union(false, other) }

// UnionAll 即 UNION ALL。
func (q *Q[T]) UnionAll(other *Q[T]) *Q[T] { return q.union(true, other) }

func (q *Q[T]) union(all bool, other *Q[T]) *Q[T] {
	c := q.clone()
	if c.err == nil {
		c.err = other.err
	}
	oc := other.c
	if (len(oc.orderBy) > 0 || oc.limit >= 0 || oc.offset > 0 || oc.lock.mode != "" || len(oc.unions) > 0 || len(oc.ctes) > 0) && c.err == nil {
		c.err = errors.New("xdb: a UNION operand cannot have ORDER BY, LIMIT, OFFSET, locks, CTEs or nested unions")
	}
	c.c.unions = append(slices.Clip(c.c.unions), unionPart{all: all, core: &oc})
	return c
}

// ForUpdate 即 SELECT ... FOR UPDATE，只能在事务中执行。
func (q *Q[T]) ForUpdate() *Q[T] { c := q.clone(); c.c.lock.mode = "UPDATE"; return c }

// ForShare 即 SELECT ... FOR SHARE，只能在事务中执行。
func (q *Q[T]) ForShare() *Q[T] { c := q.clone(); c.c.lock.mode = "SHARE"; return c }

// NoWait 拿不到锁时立即报错（ErrLockTimeout），需配合 ForUpdate / ForShare。
func (q *Q[T]) NoWait() *Q[T] { c := q.clone(); c.c.lock.wait = "NOWAIT"; return c }

// SkipLocked 跳过已被锁定的行，常用于任务队列，需配合 ForUpdate / ForShare。
func (q *Q[T]) SkipLocked() *Q[T] { c := q.clone(); c.c.lock.wait = "SKIP LOCKED"; return c }

// softDeleteField 返回数据源的软删除列（没有则 nil）。
//
// 关键点：软删除列属于**源表**，而不是结果类型 T。因此不能只看 T——
// 例如 xdb.Select[int64](nil, P.ID).From(P) 的结果类型是 int64，
// 但软删除过滤必须来自 posts 表。只有 from 是 TableOf[M] 构造的表时才能推导；
// 手写的 xdb.T("posts") 没有模型信息，因此不会自动过滤（行为显式、可预期）。
func (q *Q[T]) softDeleteField() *schema.Field {
	if q.c.from == nil {
		return nil
	}
	if m := q.c.from.sourceModel(); m != nil {
		return m.SoftDelete
	}
	return nil
}

// Unscoped 关闭软删除过滤，让查询能看到（并操作到）已软删除的行。
//
//	xdb.Query[User](db).Unscoped().Where(U.ID.Eq(1)).One(ctx) // 也能查到已删除的行
func (q *Q[T]) Unscoped() *Q[T] { c := q.clone(); c.unscoped = true; return c }

// Lenient 返回宽松模式的查询：结果列找不到对应字段时忽略而非报错。
func (q *Q[T]) Lenient() *Q[T] { c := q.clone(); c.lenient = true; return c }

// ---------- 执行 ----------

// All 返回全部行；无结果时为空切片。声明了 Preload 时会随后加载关联。
func (q *Q[T]) All(ctx context.Context) ([]T, error) {
	out, err := All[T](ctx, q)
	if err != nil || len(q.preloads) == 0 || len(out) == 0 {
		return out, err
	}
	rv := reflect.ValueOf(out)
	if err := q.runPreloads(ctx, rv); err != nil {
		return nil, err
	}
	return out, nil
}

// One 返回恰好一行：无结果返回 ErrNotFound，多于一行返回 ErrMultipleRows。
func (q *Q[T]) One(ctx context.Context) (T, error) {
	out, err := One[T](ctx, q)
	if err != nil || len(q.preloads) == 0 {
		return out, err
	}
	rv := reflect.ValueOf([]T{out})
	if err := q.runPreloads(ctx, rv); err != nil {
		var zero T
		return zero, err
	}
	return rv.Index(0).Interface().(T), nil
}

// First 加上 LIMIT 1 后返回第一行，无结果返回 ErrNotFound。通常应配合 OrderBy 使用。
func (q *Q[T]) First(ctx context.Context) (T, error) { return One[T](ctx, q.Limit(1)) }

// Iter 流式读取。
func (q *Q[T]) Iter(ctx context.Context) iter.Seq2[T, error] { return Iter[T](ctx, q) }

// Count 返回满足条件的行数（忽略 ORDER BY；含 DISTINCT / GROUP BY / LIMIT / UNION 时
// 自动包装为子查询计数，保证结果与 All 的行数一致）。
func (q *Q[T]) Count(ctx context.Context) (int64, error) {
	if q.err != nil {
		return 0, q.err
	}
	inner := q.clone()
	inner.c.orderBy = nil
	inner.c.lock = lockClause{}
	var cq *Q[int64]
	if inner.c.distinct || len(inner.c.groupBy) > 0 || len(inner.c.unions) > 0 || inner.c.limit >= 0 || inner.c.offset > 0 {
		cq = Select[int64](q.ex, Count()).From(Derived("xdb_count", inner))
	} else {
		core := inner.c
		core.cols = []Node{Count()}
		cq = &Q[int64]{ex: q.ex, c: core}
	}
	return cq.One(ctx)
}

// Exists 判断是否存在满足条件的行。
func (q *Q[T]) Exists(ctx context.Context) (bool, error) {
	if q.err != nil {
		return false, q.err
	}
	inner := q.clone()
	inner.c.orderBy = nil
	inner.c.lock = lockClause{}
	inner.c.cols = []Node{rawNode("1")}
	return Select[bool](q.ex, Exists(inner)).One(ctx)
}

// Pluck 查询单列并返回其值列表：ids, err := xdb.Pluck(ctx, xdb.Query[User](db).Where(...), U.ID)。
func Pluck[V, T any](ctx context.Context, q *Q[T], col Expr[V]) ([]V, error) {
	c := &Q[V]{ex: q.ex, c: q.c, err: q.err}
	c.c.cols = []Node{col}
	return c.All(ctx)
}

// ToSQL 按 Executor 的方言渲染 SQL 与参数，用于调试。
func (q *Q[T]) ToSQL() (string, []any, error) {
	if q.ex == nil {
		return "", nil, ErrNoExecutor
	}
	return q.SQL(q.ex.Dialect())
}

// SQL 按指定方言渲染 SQL 与参数。
func (q *Q[T]) SQL(d dialect.Dialect) (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}
	core := q.c
	if core.from != nil && !q.unscoped {
		core.softDelete = q.softDeleteField()
	}
	b := newBuilder(d)
	core.buildSelect(b)
	return b.result()
}

func (q *Q[T]) stmt() stmt {
	return stmt{
		ex: q.ex,
		render: func(d dialect.Dialect) (string, []any, error) {
			if q.c.lock.mode != "" && q.ex.session(false).tx == nil {
				return "", nil, ErrLockOutsideTx
			}
			return q.SQL(d)
		},
		strict: !q.lenient,
	}
}

// build 让 Q 作为子查询表达式使用：(SELECT ...)。
func (q *Q[T]) build(b *builder) {
	if q.err != nil {
		b.fail(q.err)
		return
	}
	core := q.c
	if core.from != nil && !q.unscoped {
		core.softDelete = q.softDeleteField()
	}
	b.write("(")
	core.buildSelect(b)
	b.write(")")
}

// buildSelectBody 把 Q 渲染为不带外层括号的 SELECT，供 INSERT ... SELECT 使用。
func (q *Q[T]) buildSelectBody(b *builder) {
	if q.err != nil {
		b.fail(q.err)
		return
	}
	core := q.c
	if core.from != nil && !q.unscoped {
		core.softDelete = q.softDeleteField()
	}
	core.buildSelect(b)
}

func (q *Q[T]) exprType() T { var z T; return z }

// ---------- 渲染 ----------

func (c *selectCore) buildSelect(b *builder) {
	if len(c.ctes) > 0 {
		b.write("WITH ")
		if c.recursive {
			b.write("RECURSIVE ")
		}
		for i, cte := range c.ctes {
			if i > 0 {
				b.write(", ")
			}
			cte.buildDef(b)
		}
		b.write(" ")
	}
	c.buildCore(b)
	for _, u := range c.unions {
		if u.all {
			b.write(" UNION ALL ")
		} else {
			b.write(" UNION ")
		}
		u.core.buildCore(b)
	}
	if len(c.orderBy) > 0 {
		b.write(" ORDER BY ")
		for i, o := range c.orderBy {
			if i > 0 {
				b.write(", ")
			}
			o.build(b)
		}
	}
	if c.limit >= 0 {
		b.write(" LIMIT ")
		b.int(c.limit)
	}
	if c.offset > 0 {
		if c.limit < 0 && b.d.Features().OffsetRequiresLimit {
			b.write(" LIMIT 18446744073709551615")
		}
		b.write(" OFFSET ")
		b.int(c.offset)
	}
	if c.lock.mode != "" {
		b.write(" FOR " + c.lock.mode)
		if c.lock.wait != "" {
			b.write(" " + c.lock.wait)
		}
	} else if c.lock.wait != "" {
		b.fail(errors.New("xdb: NoWait / SkipLocked require ForUpdate or ForShare"))
	}
}

func (c *selectCore) buildCore(b *builder) {
	b.write("SELECT ")
	if c.distinct {
		b.write("DISTINCT ")
	}
	if len(c.cols) == 0 {
		b.write("*")
	} else {
		b.nodes(c.cols, ", ")
	}
	if c.from != nil {
		b.write(" FROM ")
		c.from.buildFrom(b)
	}
	for _, j := range c.joins {
		b.write(" " + j.kind + " ")
		j.src.buildFrom(b)
		if !j.on.IsZero() {
			b.write(" ON ")
			j.on.build(b)
		}
	}
	conds := c.where
	if c.softDelete != nil {
		col := colNode{src: c.from, name: c.softDelete.Column}
		conds = append([]Cond{wrap[bool](postfixNode{col, "IS NULL"})}, conds...)
	}
	if w := And(conds...); !w.IsZero() {
		b.write(" WHERE ")
		w.build(b)
	}
	if len(c.groupBy) > 0 {
		b.write(" GROUP BY ")
		b.nodes(c.groupBy, ", ")
	}
	if h := And(c.having...); !h.IsZero() {
		b.write(" HAVING ")
		h.build(b)
	}
}
