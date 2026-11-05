package xdb

// Expr 是结果类型为 T 的 SQL 表达式。类型参数只在编译期起作用：
// 例如 Col[int64].EqCol 只接受 Expr[int64]，把字符串列与整数列比较会直接编译失败。
type Expr[T any] interface {
	Node
	exprType() T
}

// E 是类型为 T 的表达式值，提供比较、排序、别名等方法。
// 列（Col）、聚合函数、子查询包装等都返回或内嵌 E。
type E[T any] struct {
	n Node
}

// Cond 是布尔条件。零值表示“无条件”，在 Where / And / Or 中会被忽略，
// 便于用 xdb.If 拼装动态条件。
type Cond = E[bool]

func (e E[T]) build(b *builder) { b.node(e.n) }
func (e E[T]) exprType() T      { var z T; return z }

// IsZero 报告表达式是否为零值（对 Cond 而言即“无条件”）。
func (e E[T]) IsZero() bool { return e.n == nil }

func wrap[T any](n Node) E[T] { return E[T]{n: n} }

func cmp(l Node, op string, r Node) Cond { return wrap[bool](binNode{l: l, op: op, r: r}) }

// ---------- 与值比较 ----------

func (e E[T]) Eq(v T) Cond { return cmp(e, "=", valNode{v}) }
func (e E[T]) Ne(v T) Cond { return cmp(e, "<>", valNode{v}) }
func (e E[T]) Gt(v T) Cond { return cmp(e, ">", valNode{v}) }
func (e E[T]) Ge(v T) Cond { return cmp(e, ">=", valNode{v}) }
func (e E[T]) Lt(v T) Cond { return cmp(e, "<", valNode{v}) }
func (e E[T]) Le(v T) Cond { return cmp(e, "<=", valNode{v}) }

// Between：e BETWEEN lo AND hi（闭区间）。
func (e E[T]) Between(lo, hi T) Cond {
	return wrap[bool](betweenNode{x: e, lo: valNode{lo}, hi: valNode{hi}})
}

// NotBetween：e NOT BETWEEN lo AND hi。
func (e E[T]) NotBetween(lo, hi T) Cond {
	return wrap[bool](betweenNode{x: e, lo: valNode{lo}, hi: valNode{hi}, not: true})
}

// In：e IN (v1, v2, ...)。空列表时生成恒假条件 (1 = 0)，语义正确且 SQL 合法。
func (e E[T]) In(vs ...T) Cond {
	if len(vs) == 0 {
		return wrap[bool](rawNode("(1 = 0)"))
	}
	return cmp(e, "IN", listOf(vs))
}

// NotIn：e NOT IN (v1, v2, ...)。空列表时生成恒真条件 (1 = 1)。
func (e E[T]) NotIn(vs ...T) Cond {
	if len(vs) == 0 {
		return wrap[bool](rawNode("(1 = 1)"))
	}
	return cmp(e, "NOT IN", listOf(vs))
}

func listOf[T any](vs []T) listNode {
	out := make(listNode, len(vs))
	for i, v := range vs {
		out[i] = v
	}
	return out
}

func (e E[T]) IsNull() Cond    { return wrap[bool](postfixNode{e, "IS NULL"}) }
func (e E[T]) IsNotNull() Cond { return wrap[bool](postfixNode{e, "IS NOT NULL"}) }

// ---------- 与表达式（列、函数、子查询）比较，两侧类型必须一致 ----------

func (e E[T]) EqCol(o Expr[T]) Cond { return cmp(e, "=", o) }
func (e E[T]) NeCol(o Expr[T]) Cond { return cmp(e, "<>", o) }
func (e E[T]) GtCol(o Expr[T]) Cond { return cmp(e, ">", o) }
func (e E[T]) GeCol(o Expr[T]) Cond { return cmp(e, ">=", o) }
func (e E[T]) LtCol(o Expr[T]) Cond { return cmp(e, "<", o) }
func (e E[T]) LeCol(o Expr[T]) Cond { return cmp(e, "<=", o) }

// InSub：e IN (子查询)，子查询的结果类型必须与 e 一致。
func (e E[T]) InSub(q *Q[T]) Cond { return cmp(e, "IN", q) }

// NotInSub：e NOT IN (子查询)。注意 SQL 语义：子查询结果含 NULL 时 NOT IN 恒不成立。
func (e E[T]) NotInSub(q *Q[T]) Cond { return cmp(e, "NOT IN", q) }

// ---------- 输出与排序 ----------

// As 为选择列表中的表达式指定输出别名。
func (e E[T]) As(alias string) Node { return aliasNode{x: e, alias: alias} }

// Asc 升序。
func (e E[T]) Asc() Order { return Order{n: e} }

// Desc 降序。
func (e E[T]) Desc() Order { return Order{n: e, desc: true} }

// Order 是 ORDER BY 中的一项。
type Order struct {
	n    Node
	desc bool
}

func (o Order) build(b *builder) {
	b.node(o.n)
	if o.desc {
		b.write(" DESC")
	}
}

// ---------- 条件组合 ----------

// And 组合条件，零值条件被忽略；全部为零值时返回零值。
func And(cs ...Cond) Cond { return group("AND", cs) }

// Or 组合条件，零值条件被忽略；全部为零值时返回零值。
func Or(cs ...Cond) Cond { return group("OR", cs) }

func group(op string, cs []Cond) Cond {
	var items []Node
	for _, c := range cs {
		if !c.IsZero() {
			items = append(items, c)
		}
	}
	switch len(items) {
	case 0:
		return Cond{}
	case 1:
		return items[0].(Cond)
	}
	return wrap[bool](groupNode{op: op, items: items})
}

// Not 取反；零值条件返回零值。
func Not(c Cond) Cond {
	if c.IsZero() {
		return c
	}
	return wrap[bool](prefixNode{"NOT", groupNode{op: "AND", items: []Node{c}}})
}

// If 在 ok 为 true 时返回 c，否则返回零值条件（会被忽略），用于动态查询：
//
//	Where(
//		xdb.If(req.Name != "", U.Name.Contains(req.Name)),
//		xdb.If(req.MinAge > 0, U.Age.Ge(req.MinAge)),
//	)
func If(ok bool, c Cond) Cond {
	if ok {
		return c
	}
	return Cond{}
}
