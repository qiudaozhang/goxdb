package xdb

// ---------- 值与原始片段 ----------

// V 把一个 Go 值包装为参数表达式。
func V[T any](v T) E[T] { return wrap[T](valNode{v}) }

// SQL 构造自定义 SQL 片段，结果类型为 T。片段中的 "?" 依次对应 args：
// 若参数是表达式（列、函数、子查询……）则内嵌渲染，否则作为绑定参数（切片自动展开）。
//
//	xdb.SQL[int64]("COALESCE(?, ?)", O.Amount, 0)
//	xdb.SQL[bool]("? @> ?", P.Tags, xdb.AsIs(tags))
func SQL[T any](frag string, args ...any) E[T] { return wrap[T](sqlNode{frag: frag, args: args}) }

// Func 调用任意 SQL 函数，结果类型为 T：xdb.Func[string]("UPPER", U.Name)。
func Func[T any](name string, args ...Node) E[T] {
	return wrap[T](funcNode{name: name, args: args})
}

// ---------- 聚合 ----------

// Count 即 COUNT(*)。
func Count() E[int64] { return wrap[int64](rawNode("COUNT(*)")) }

// CountOf 即 COUNT(x)，不统计 NULL。
func CountOf(x Node) E[int64] { return wrap[int64](funcNode{name: "COUNT", args: []Node{x}}) }

// CountDistinct 即 COUNT(DISTINCT x)。
func CountDistinct(x Node) E[int64] {
	return wrap[int64](funcNode{name: "COUNT", args: []Node{x}, distinct: true})
}

// Sum 即 SUM(x)。注意无匹配行时结果为 NULL，可配合 Coalesce 使用。
func Sum[T any](x Expr[T]) E[T] { return wrap[T](funcNode{name: "SUM", args: []Node{x}}) }

// Avg 即 AVG(x)，结果按 float64 处理。
func Avg[T any](x Expr[T]) E[float64] { return wrap[float64](funcNode{name: "AVG", args: []Node{x}}) }

// Min 即 MIN(x)。
func Min[T any](x Expr[T]) E[T] { return wrap[T](funcNode{name: "MIN", args: []Node{x}}) }

// Max 即 MAX(x)。
func Max[T any](x Expr[T]) E[T] { return wrap[T](funcNode{name: "MAX", args: []Node{x}}) }

// ---------- 常用函数 ----------

// Coalesce 即 COALESCE(x, fallback)。
func Coalesce[T any](x Expr[T], fallback T) E[T] {
	return wrap[T](funcNode{name: "COALESCE", args: []Node{x, valNode{fallback}}})
}

// Cast 即 CAST(x AS sqlType)，把表达式转换为指定 SQL 类型，结果按 T 扫描。
//
// 典型用途：SUM(bigint) 在 PostgreSQL 返回 NUMERIC、在 MySQL 返回 DECIMAL，
// 直接扫入 int64 会失败，需显式转换：
//
//	total := xdb.Cast[int64]("BIGINT", xdb.Sum(o.Amount))
//
// sqlType 由调用方给出，是一个显式的可移植性选择点。
func Cast[T any, S any](sqlType string, x Expr[S]) E[T] {
	return wrap[T](castNode{x: x, typ: sqlType})
}

type castNode struct {
	x   Node
	typ string
}

func (n castNode) build(b *builder) {
	b.write("CAST(")
	b.node(n.x)
	b.write(" AS " + b.d.CastType(n.typ) + ")")
}

// Lower 即 LOWER(x)。
func Lower(x Expr[string]) E[string] { return wrap[string](funcNode{name: "LOWER", args: []Node{x}}) }

// Upper 即 UPPER(x)。
func Upper(x Expr[string]) E[string] { return wrap[string](funcNode{name: "UPPER", args: []Node{x}}) }

// Add 即 (a + b)，两侧类型必须一致。
func Add[T Number](a, b Expr[T]) E[T] { return arith[T](a, "+", b) }

// Subtract 即 (a - b)。
func Subtract[T Number](a, b Expr[T]) E[T] { return arith[T](a, "-", b) }

// Multiply 即 (a * b)。
func Multiply[T Number](a, b Expr[T]) E[T] { return arith[T](a, "*", b) }

// Divide 即 (a / b)。
func Divide[T Number](a, b Expr[T]) E[T] { return arith[T](a, "/", b) }

// ---------- 子查询 ----------

// Exists 即 EXISTS (子查询)。
func Exists(q Node) Cond { return wrap[bool](prefixNode{"EXISTS", q}) }

// NotExists 即 NOT EXISTS (子查询)。
func NotExists(q Node) Cond { return wrap[bool](prefixNode{"NOT EXISTS", q}) }

// Scalar 把返回单行单列的子查询作为表达式使用，可放在选择列表或条件中：
//
//	cnt := xdb.Scalar(xdb.Select[int64](nil, xdb.Count()).From(O).Where(O.UserID.EqCol(U.ID)))
//	xdb.Select[UserStat](db, U.ID, cnt.As("order_cnt")).From(U)
func Scalar[T any](q *Q[T]) E[T] { return wrap[T](q) }

// ---------- upsert ----------

// Excluded 在 upsert 的 DoUpdateSet 中引用“本次试图插入的值”：
// PostgreSQL 渲染为 EXCLUDED."col"，MySQL 渲染为行别名 `xdb_new`.`col`。
//
//	OnConflict(U.Email).DoUpdateSet(U.LoginCount.ToExpr(xdb.Add[int64](U.LoginCount, xdb.Excluded(U.LoginCount.Col))))
func Excluded[T any](c Col[T]) E[T] { return wrap[T](excludedNode{name: c.name}) }

type excludedNode struct{ name string }

func (n excludedNode) build(b *builder) {
	switch b.d.Features().Upsert {
	case upsertOnDuplicateKey:
		b.ident(mysqlRowAlias)
		b.write(".")
	default:
		b.write("EXCLUDED.")
	}
	b.ident(n.name)
}
