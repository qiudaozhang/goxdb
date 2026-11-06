package xdb

import (
	"errors"
	"strconv"
	"strings"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

// errEmptyExpr：使用了零值表达式（例如未初始化的列）。
var errEmptyExpr = errors.New("xdb: empty expression (zero value column or condition)")

// Node 是可以渲染为 SQL 片段的语法节点：列、表达式、条件、子查询等。
// 接口含未导出方法，自定义片段请使用 xdb.SQL。
type Node interface {
	build(b *builder)
}

// builder 把语法树渲染为 SQL 文本与参数。占位符直接生成方言原生形式，
// 子查询与外层共享同一个参数序列，因此 PostgreSQL 的 $n 编号始终连续正确。
type builder struct {
	d    dialect.Dialect
	sb   strings.Builder
	args []any
	err  error
	// identBuf 是 ident 的中转缓冲，避免每个标识符一个临时字符串。
	identBuf []byte
	// phBuf 是 Placeholder 的中转缓冲（PostgreSQL 的 $n 需要格式化）。
	phBuf []byte
}

func newBuilder(d dialect.Dialect) *builder { return &builder{d: d} }

func (b *builder) write(s string) { b.sb.WriteString(s) }

// ident 把标识符写入输出缓冲。
//
// 走方言的 AppendIdent 而不是 QuoteIdent：后者每个标识符都会产生一个临时字符串，
// 一次查询十几个列名就会贡献十几次分配。这里直接追加到 strings.Builder 的缓冲里。
func (b *builder) ident(s string) {
	// Builder 没有可写的字节切片访问器，只能借一个复用的缓冲中转。
	b.identBuf = b.d.AppendIdent(b.identBuf[:0], s)
	b.sb.Write(b.identBuf)
}

func (b *builder) arg(v any) {
	b.args = append(b.args, v)
	// PostgreSQL 的占位符是 "$" + strconv.Itoa(n)，会分配；
	// MySQL 返回常量 "?"，走 WriteString 不会分配。
	b.phBuf = appendPlaceholder(b.d, b.phBuf[:0], len(b.args))
	b.sb.Write(b.phBuf)
}

// appendPlaceholder 把第 n 个占位符写入 dst。
// 占位符只有 "$<digits>"（PostgreSQL）与 "?"（MySQL）两种形态，
// 直接拼接可以绕开 strconv.Itoa 的字符串分配。
func appendPlaceholder(d dialect.Dialect, dst []byte, n int) []byte {
	p := d.Placeholder(n)
	if len(p) == 1 {
		return append(dst, p[0])
	}
	return append(dst, p...)
}

func (b *builder) int(n int64) { b.sb.WriteString(strconv.FormatInt(n, 10)) }

func (b *builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *builder) node(n Node) {
	if n == nil {
		b.fail(errEmptyExpr)
		return
	}
	n.build(b)
}

func (b *builder) nodes(ns []Node, sep string) {
	for i, n := range ns {
		if i > 0 {
			b.write(sep)
		}
		b.node(n)
	}
}

func (b *builder) result() (string, []any, error) {
	if b.err != nil {
		return "", nil, b.err
	}
	return b.sb.String(), b.args, nil
}

// ---------- 基础节点 ----------

// valNode 是一个绑定参数。
type valNode struct{ v any }

func (n valNode) build(b *builder) { b.arg(n.v) }

// nowNode 渲染为当前时间函数。MySQL 与 PostgreSQL 都支持 CURRENT_TIMESTAMP，
// 因此这里不需要方言分支（保持为节点以便将来扩展）。
type nowNode struct{}

func (nowNode) build(b *builder) { b.write("CURRENT_TIMESTAMP") }

// rawNode 是原样输出的 SQL 文本（仅框架内部使用固定文本）。
type rawNode string

func (n rawNode) build(b *builder) { b.write(string(n)) }

// identNode 是一个带引号的标识符（例如输出列别名）。
type identNode string

func (n identNode) build(b *builder) { b.ident(string(n)) }

// binNode 是二元运算：l op r。
type binNode struct {
	l     Node
	op    string
	r     Node
	paren bool
}

func (n binNode) build(b *builder) {
	if n.paren {
		b.write("(")
	}
	b.node(n.l)
	b.write(" " + n.op + " ")
	b.node(n.r)
	if n.paren {
		b.write(")")
	}
}

// postfixNode：x IS NULL 等。
type postfixNode struct {
	x  Node
	op string
}

func (n postfixNode) build(b *builder) {
	b.node(n.x)
	b.write(" " + n.op)
}

// prefixNode：NOT (x)、EXISTS (subquery) 等。
type prefixNode struct {
	op string
	x  Node
}

func (n prefixNode) build(b *builder) {
	b.write(n.op + " ")
	b.node(n.x)
}

// groupNode：(a AND b AND c)。
//
// 为避免每次渲染都分配一份 []Node，groupNode 可以直接持有一段连续的 Cond 切片
// （conds 非空时用它，否则用 items）。
type groupNode struct {
	op    string
	items []Node
	conds []Cond
}

func (n groupNode) build(b *builder) {
	b.write("(")
	if n.conds != nil {
		for i, c := range n.conds {
			if i > 0 {
				b.write(" " + n.op + " ")
			}
			b.node(c)
		}
	} else {
		b.nodes(n.items, " "+n.op+" ")
	}
	b.write(")")
}

// listNode：(?, ?, ?)。
type listNode []any

func (n listNode) build(b *builder) {
	b.write("(")
	for i, v := range n {
		if i > 0 {
			b.write(", ")
		}
		b.arg(v)
	}
	b.write(")")
}

// betweenNode：x BETWEEN a AND b。
type betweenNode struct {
	x, lo, hi Node
	not       bool
}

func (n betweenNode) build(b *builder) {
	b.node(n.x)
	if n.not {
		b.write(" NOT")
	}
	b.write(" BETWEEN ")
	b.node(n.lo)
	b.write(" AND ")
	b.node(n.hi)
}

// funcNode：NAME(DISTINCT a, b)。
type funcNode struct {
	name     string
	args     []Node
	distinct bool
}

func (n funcNode) build(b *builder) {
	b.write(n.name + "(")
	if n.distinct {
		b.write("DISTINCT ")
	}
	b.nodes(n.args, ", ")
	b.write(")")
}

// aliasNode：expr AS "alias"。
type aliasNode struct {
	x     Node
	alias string
}

func (n aliasNode) build(b *builder) {
	b.node(n.x)
	b.write(" AS ")
	b.ident(n.alias)
}

// sqlNode 是用户提供的 SQL 片段：其中的 "?" 若对应 Node 则内嵌渲染，
// 否则作为参数绑定（切片自动展开）。
type sqlNode struct {
	frag string
	args []any
}

func (n sqlNode) build(b *builder) {
	i := 0
	err := sqlbind.Walk(b.d.Syntax(), n.frag, b.write, func() error {
		if i >= len(n.args) {
			return sqlbind.ErrArgCount
		}
		a := n.args[i]
		i++
		switch v := a.(type) {
		case Node:
			b.node(v)
			return nil
		case sqlbind.AsIs:
			b.arg(v.V)
			return nil
		}
		if vals, ok := sqlbind.Expand(a); ok {
			if len(vals) == 0 {
				return sqlbind.ErrEmptySlice
			}
			for k, v := range vals {
				if k > 0 {
					b.write(", ")
				}
				b.arg(v)
			}
			return nil
		}
		b.arg(a)
		return nil
	})
	if err == nil && i != len(n.args) {
		err = sqlbind.ErrArgCount
	}
	if err != nil {
		b.fail(err)
	}
}

// likeNode：x [NOT] LIKE p；不区分大小写时在不支持 ILIKE 的方言上使用 LOWER() 模拟。
type likeNode struct {
	x       Node
	pattern Node
	not     bool
	ci      bool
}

func (n likeNode) build(b *builder) {
	op := "LIKE"
	if n.not {
		op = "NOT LIKE"
	}
	if n.ci && b.d.Features().ILike {
		op = "I" + op
		if n.not {
			op = "NOT ILIKE"
		}
		n.ci = false
	}
	if n.ci {
		b.write("LOWER(")
		b.node(n.x)
		b.write(") " + op + " LOWER(")
		b.node(n.pattern)
		b.write(")")
		return
	}
	b.node(n.x)
	b.write(" " + op + " ")
	b.node(n.pattern)
}

// escapeLike 转义 LIKE 模式中的通配符，使输入按字面匹配。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
