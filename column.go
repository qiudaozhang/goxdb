package xdb

import "time"

// Column 是可以出现在 INSERT 列表、UPDATE SET 左侧、ON CONFLICT 目标中的列。
type Column interface {
	Node
	colName() string
	colSource() Source
}

// colNode 渲染为 "表"."列"（无数据源时只有 "列"）。
type colNode struct {
	src  Source
	name string
}

func (n colNode) build(b *builder) {
	if n.src != nil {
		b.ident(n.src.qualifier())
		b.write(".")
	}
	b.ident(n.name)
}

// Col 是类型为 T 的列，内嵌 E[T]，拥有全部比较方法。
type Col[T any] struct {
	E[T]
	src  Source
	name string
}

// C 构造列。src 为所属的表 / 派生表 / CTE；为 nil 时表示不限定表名的列或输出别名，
// 例如在 ORDER BY 中引用 SELECT 的别名：xdb.C[int64](nil, "total").Desc()。
func C[T any](src Source, name string) Col[T] {
	return Col[T]{E: wrap[T](colNode{src: src, name: name}), src: src, name: name}
}

// Name 返回列名。
func (c Col[T]) Name() string { return c.name }

func (c Col[T]) colName() string   { return c.name }
func (c Col[T]) colSource() Source { return c.src }

// To 生成赋值 col = v，用于 UPDATE SET 与 upsert。
func (c Col[T]) To(v T) Assignment { return Assignment{col: c, val: valNode{v}} }

// ToExpr 生成赋值 col = 表达式。
func (c Col[T]) ToExpr(e Expr[T]) Assignment { return Assignment{col: c, val: e} }

// ToNull 生成赋值 col = NULL。
func (c Col[T]) ToNull() Assignment { return Assignment{col: c, val: rawNode("NULL")} }

// Assignment 是 SET 子句中的一项：列 = 值。
type Assignment struct {
	col Column
	val Node
}

// ---------- 字符串列 ----------

// StringCol 是字符串列，额外提供 LIKE 系列方法。
type StringCol struct{ Col[string] }

// CStr 构造字符串列。
func CStr(src Source, name string) StringCol { return StringCol{C[string](src, name)} }

// Like：col LIKE pattern（pattern 原样使用，% 与 _ 为通配符）。
func (c StringCol) Like(pattern string) Cond {
	return wrap[bool](likeNode{x: c, pattern: valNode{pattern}})
}

// NotLike：col NOT LIKE pattern。
func (c StringCol) NotLike(pattern string) Cond {
	return wrap[bool](likeNode{x: c, pattern: valNode{pattern}, not: true})
}

// ILike：不区分大小写的 LIKE。PostgreSQL 使用 ILIKE，其它方言使用 LOWER() 模拟。
func (c StringCol) ILike(pattern string) Cond {
	return wrap[bool](likeNode{x: c, pattern: valNode{pattern}, ci: true})
}

// HasPrefix：以 s 开头（s 中的 % _ \ 会被转义，按字面匹配）。
func (c StringCol) HasPrefix(s string) Cond { return c.Like(escapeLike(s) + "%") }

// HasSuffix：以 s 结尾（按字面匹配）。
func (c StringCol) HasSuffix(s string) Cond { return c.Like("%" + escapeLike(s)) }

// Contains：包含 s（按字面匹配）。
func (c StringCol) Contains(s string) Cond { return c.Like("%" + escapeLike(s) + "%") }

// ---------- 数值列 ----------

// Number 是数值类型约束。
type Number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64
}

// NumberCol 是数值列，额外提供算术与自增赋值。
type NumberCol[T Number] struct{ Col[T] }

// CNum 构造数值列。
func CNum[T Number](src Source, name string) NumberCol[T] { return NumberCol[T]{C[T](src, name)} }

func (c NumberCol[T]) Add(v T) E[T] { return arith[T](c, "+", valNode{v}) }
func (c NumberCol[T]) Sub(v T) E[T] { return arith[T](c, "-", valNode{v}) }
func (c NumberCol[T]) Mul(v T) E[T] { return arith[T](c, "*", valNode{v}) }
func (c NumberCol[T]) Div(v T) E[T] { return arith[T](c, "/", valNode{v}) }

// Incr 生成赋值 col = col + v，并发安全的计数器更新应使用它而不是“读-改-写”。
func (c NumberCol[T]) Incr(v T) Assignment { return c.ToExpr(c.Add(v)) }

// Decr 生成赋值 col = col - v。
func (c NumberCol[T]) Decr(v T) Assignment { return c.ToExpr(c.Sub(v)) }

func arith[T any](l Node, op string, r Node) E[T] {
	return wrap[T](binNode{l: l, op: op, r: r, paren: true})
}

// ---------- 时间列 ----------

// TimeCol 是时间列。
type TimeCol struct{ Col[time.Time] }

// CTime 构造时间列。
func CTime(src Source, name string) TimeCol { return TimeCol{C[time.Time](src, name)} }

// Before：col < t。
func (c TimeCol) Before(t time.Time) Cond { return c.Lt(t) }

// After：col > t。
func (c TimeCol) After(t time.Time) Cond { return c.Gt(t) }
