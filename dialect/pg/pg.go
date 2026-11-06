// Package pg 提供 PostgreSQL 专属的表达式构造器：JSONB 与数组。
//
// 这些能力**不属于**通用 API：导入并使用本包意味着这段代码不可移植到 MySQL，
// 这正是设计意图——让「使用了方言专属特性」在代码里一眼可见。
//
//	import "github.com/qiudaozhang/goxdb/dialect/pg"
//
//	xdb.Query[Doc](db).Where(pg.JSONB(doc.Meta).Text("status").Eq("active"))
package pg

import (
	"strconv"
	"strings"

	xdb "github.com/qiudaozhang/goxdb"
)

// JSONB 构造一个 JSONB 列（或任意表达式）的路径访问起点。
//
// 所有段名与键名都会被转义后内联为字面量；它们只应来自代码中的常量，
// 不要传入用户输入（需要动态键时请用 xdb.SQL 绑定参数）。
func JSONB(col xdb.Node) JSONPath { return JSONPath{root: col} }

// JSONPath 是 JSONB 上的路径链。
type JSONPath struct{ root xdb.Node }

// Field 进入下一层对象字段，渲染为 -> 'name'（结果仍是 jsonb，可继续链）。
func (p JSONPath) Field(name string) JSONPath {
	return JSONPath{root: sqlNode2("? -> "+quote(name), p.root)}
}

// Index 取数组下标（从 0 开始），渲染为 -> n。
func (p JSONPath) Index(n int) JSONPath {
	return JSONPath{root: sqlNode2("? -> "+strconv.Itoa(n), p.root)}
}

// Text 取出文本值，渲染为 ->> 'name'，结果为 text。
func (p JSONPath) Text(name string) xdb.E[string] {
	return xdb.SQL[string]("? ->> "+quote(name), p.root)
}

// TextIndex 取出数组下标的文本值，渲染为 ->> n。
func (p JSONPath) TextIndex(n int) xdb.E[string] {
	return xdb.SQL[string]("? ->> "+strconv.Itoa(n), p.root)
}

// HasKey 判断顶层是否存在键，渲染为 jsonb_exists(col, 'key')。
//
// 注意：PostgreSQL 的 `?` 操作符与 goxdb 的占位符冲突，所以这里用等价函数，
// 而不是在 Raw SQL 里写 `??`（那样对使用方不友好且易错）。
func (p JSONPath) HasKey(key string) xdb.Cond {
	return xdb.SQL[bool]("jsonb_exists(?, "+quote(key)+")", p.root)
}

// Contains 判断是否包含给定 JSON：col @> 'json'::jsonb。
func (p JSONPath) Contains(json string) xdb.Cond {
	return xdb.SQL[bool]("? @> "+quote(json)+"::jsonb", p.root)
}

// ---------------------------------------------------------------- 数组

// Array 构造数组列（如 text[]、bigint[]）的表达式起点。
func Array(col xdb.Node) ArrayExpr { return ArrayExpr{col: col} }

// ArrayExpr 是 PostgreSQL 数组表达式。
type ArrayExpr struct{ col xdb.Node }

// Any 判断元素是否在数组中：elem = ANY(arr)。
func (a ArrayExpr) Any(elem xdb.Node) xdb.Cond {
	return xdb.SQL[bool]("? = ANY(?)", elem, a.col)
}

// Contains 判断数组是否包含全部元素：arr @> ARRAY[...]。
// 没有元素时渲染为 arr @> '{}'（恒真；ARRAY[] 在 PostgreSQL 中无法推断类型）。
func (a ArrayExpr) Contains(elems ...xdb.Node) xdb.Cond {
	if len(elems) == 0 {
		return xdb.SQL[bool]("? @> '{}'", a.col)
	}
	return xdb.SQL[bool]("? @> ARRAY[?]", a.col, elements(elems))
}

// Overlaps 判断数组与给定元素是否有交集：arr && ARRAY[...]。
// 没有元素时渲染为 arr && '{}'（恒假）。
func (a ArrayExpr) Overlaps(elems ...xdb.Node) xdb.Cond {
	if len(elems) == 0 {
		return xdb.SQL[bool]("? && '{}'", a.col)
	}
	return xdb.SQL[bool]("? && ARRAY[?]", a.col, elements(elems))
}

// Len 返回数组长度：array_length(arr, 1)。
func (a ArrayExpr) Len() xdb.E[int] {
	return xdb.SQL[int]("array_length(?, 1)", a.col)
}

// ---------------------------------------------------------------- 内部

// quote 生成 SQL 字符串字面量（仅用于代码中的常量键名）。
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// elements 把多个节点拼成 ARRAY[...] 的元素列表（至少一个）。用 SQL 片段的 "?"
// 内嵌渲染，从而复用核心的参数编号与引号逻辑。
func elements(ns []xdb.Node) xdb.Node {
	frag := strings.TrimSuffix(strings.Repeat("?, ", len(ns)), ", ")
	args := make([]any, len(ns))
	for i, n := range ns {
		args[i] = n
	}
	return xdb.SQL[xdb.Node](frag, args...)
}

// sqlNode2 是只含一个参数的内部片段构造（避免重复书写 xdb.SQL 的类型参数）。
func sqlNode2(frag string, n xdb.Node) xdb.E[xdb.Node] {
	return xdb.SQL[xdb.Node](frag, n)
}
