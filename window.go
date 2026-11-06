package xdb

import (
	"fmt"
	"strconv"
)

// 窗口函数（M3）。
//
// 用法：构造一个窗口函数，用 Over 指定分区与排序，再 As 起别名放进投影：
//
//	rowNum := xdb.RowNumber().Over(xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.Amount.Desc()))
//	rows, err := xdb.Select[RankedOrder](db, O.ID, O.Amount, rowNum.As("rn")).From(O).All(ctx)
//
// 窗口函数只能出现在 SELECT 列表与 ORDER BY 中，不能用于 WHERE / HAVING
// （SQL 的求值顺序决定），需要过滤时请用派生表包一层。

// Window 描述 OVER 子句：PARTITION BY + ORDER BY + 框架（frame）。
type Window struct {
	partitionBy []Node
	orderBy     []Order
	frame       string
}

// Win 构造空窗口：OVER ()。
func Win() Window { return Window{} }

// PartitionBy 追加 PARTITION BY 表达式。
func (w Window) PartitionBy(exprs ...Node) Window {
	w.partitionBy = append(append([]Node(nil), w.partitionBy...), exprs...)
	return w
}

// OrderBy 追加窗口内的 ORDER BY。
func (w Window) OrderBy(orders ...Order) Window {
	w.orderBy = append(append([]Order(nil), w.orderBy...), orders...)
	return w
}

// Frame 设置窗口框架，参数是原样 SQL，例如：
//
//	.Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
//
// 框架语法在 MySQL 8 与 PostgreSQL 之间一致，故这里不做方言翻译。
func (w Window) Frame(f string) Window { w.frame = f; return w }

func (w Window) build(b *builder) {
	b.write("OVER (")
	n := 0
	if len(w.partitionBy) > 0 {
		b.write("PARTITION BY ")
		b.nodes(w.partitionBy, ", ")
		n++
	}
	if len(w.orderBy) > 0 {
		if n > 0 {
			b.write(" ")
		}
		b.write("ORDER BY ")
		for i, o := range w.orderBy {
			if i > 0 {
				b.write(", ")
			}
			o.build(b)
		}
		n++
	}
	if w.frame != "" {
		if n > 0 {
			b.write(" ")
		}
		b.write(w.frame)
	}
	b.write(")")
}

// WindowFunc 是「函数名 + 参数 + 窗口」的通用节点，T 是结果列的 Go 类型。
//
// 它本身就是 Expr[T]（未调用 Over 时渲染为 OVER ()），导出是为了让调用方
// 能把窗口函数存进变量、字段或作为参数传递，例如：
//
//	var rank xdb.WindowFunc[int64] = xdb.Rank()
type WindowFunc[T any] struct {
	name string
	args []Node
	win  Window
	err  error // 构造期参数错误，渲染时报告
}

func (n WindowFunc[T]) build(b *builder) {
	if n.err != nil {
		b.fail(n.err)
		return
	}
	b.write(n.name + "(")
	b.nodes(n.args, ", ")
	b.write(") ")
	n.win.build(b)
}

func (n WindowFunc[T]) exprType() T { var z T; return z }

// Over 设置窗口，返回可用于投影的表达式。
func (n WindowFunc[T]) Over(w Window) E[T] { n.win = w; return wrap[T](n) }

// OverExpr 直接返回表达式（等价于 Over），便于链式书写。
func (n WindowFunc[T]) OverExpr(w Window) E[T] { return n.Over(w) }

// intArg 渲染正整数字面量参数；min 以下的值记为构造期错误。
func intArg(fn string, v, min int) (Node, error) {
	if v < min {
		return nil, fmt.Errorf("xdb: %s argument must be >= %d, got %d", fn, min, v)
	}
	return rawNode(strconv.Itoa(v)), nil
}

// ---------- 排名函数 ----------

// RowNumber 即 ROW_NUMBER()：分区内从 1 开始连续编号，不并列。
func RowNumber() WindowFunc[int64] { return WindowFunc[int64]{name: "ROW_NUMBER"} }

// Rank 即 RANK()：并列时名次相同，随后跳号。
func Rank() WindowFunc[int64] { return WindowFunc[int64]{name: "RANK"} }

// DenseRank 即 DENSE_RANK()：并列时名次相同，后续不跳号。
func DenseRank() WindowFunc[int64] { return WindowFunc[int64]{name: "DENSE_RANK"} }

// PercentRank 即 PERCENT_RANK()。
func PercentRank() WindowFunc[float64] { return WindowFunc[float64]{name: "PERCENT_RANK"} }

// CumeDist 即 CUME_DIST()。
func CumeDist() WindowFunc[float64] { return WindowFunc[float64]{name: "CUME_DIST"} }

// Ntile 即 NTILE(n)：把分区划分为 n 组。
// n 必须 >= 1，否则渲染时报错。
func Ntile(n int) WindowFunc[int64] {
	a, err := intArg("NTILE", n, 1)
	return WindowFunc[int64]{name: "NTILE", args: []Node{a}, err: err}
}

// Lag 即 LAG(expr, offset)：取分区内前 offset 行的值（默认 1）。
// offset 为 0 时省略（数据库默认 1）；负数在渲染时报错。
// 结果是 *T：越过分区边界的行得到 NULL。
func Lag[T any](x Expr[T], offset int) WindowFunc[*T] {
	args := []Node{x}
	if offset == 0 {
		return WindowFunc[*T]{name: "LAG", args: args}
	}
	a, err := intArg("LAG", offset, 1)
	return WindowFunc[*T]{name: "LAG", args: append(args, a), err: err}
}

// Lead 即 LEAD(expr, offset)：取分区内后 offset 行的值（默认 1）。
// offset 为 0 时省略（数据库默认 1）；负数在渲染时报错。
// 结果是 *T：越过分区边界的行得到 NULL。
func Lead[T any](x Expr[T], offset int) WindowFunc[*T] {
	args := []Node{x}
	if offset == 0 {
		return WindowFunc[*T]{name: "LEAD", args: args}
	}
	a, err := intArg("LEAD", offset, 1)
	return WindowFunc[*T]{name: "LEAD", args: append(args, a), err: err}
}

// FirstValue 即 FIRST_VALUE(expr)。
func FirstValue[T any](x Expr[T]) WindowFunc[T] {
	return WindowFunc[T]{name: "FIRST_VALUE", args: []Node{x}}
}

// LastValue 即 LAST_VALUE(expr)。注意默认框架下它等于当前行，
// 通常需要配合 Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING")。
func LastValue[T any](x Expr[T]) WindowFunc[T] {
	return WindowFunc[T]{name: "LAST_VALUE", args: []Node{x}}
}

// NthValue 即 NTH_VALUE(expr, n)，n 从 1 开始。
//
// 结果是 *T：默认框架（到当前行为止）下，分区前 n-1 行还看不到第 n 行，值为 NULL。
func NthValue[T any](x Expr[T], n int) WindowFunc[*T] {
	a, err := intArg("NTH_VALUE", n, 1)
	return WindowFunc[*T]{name: "NTH_VALUE", args: []Node{x, a}, err: err}
}

// ---------- 聚合窗口函数 ----------

// SumOver 即 SUM(expr) OVER (...)。
func SumOver[T any](x Expr[T]) WindowFunc[T] {
	return WindowFunc[T]{name: "SUM", args: []Node{x}}
}

// CountOver 即 COUNT(expr) OVER (...)。
func CountOver(x Node) WindowFunc[int64] {
	return WindowFunc[int64]{name: "COUNT", args: []Node{x}}
}

// AvgOver 即 AVG(expr) OVER (...)。
func AvgOver[T any](x Expr[T]) WindowFunc[float64] {
	return WindowFunc[float64]{name: "AVG", args: []Node{x}}
}

// MinOver 即 MIN(expr) OVER (...)。
func MinOver[T any](x Expr[T]) WindowFunc[T] {
	return WindowFunc[T]{name: "MIN", args: []Node{x}}
}

// MaxOver 即 MAX(expr) OVER (...)。
func MaxOver[T any](x Expr[T]) WindowFunc[T] {
	return WindowFunc[T]{name: "MAX", args: []Node{x}}
}
