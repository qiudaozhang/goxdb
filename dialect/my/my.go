// Package my 提供 MySQL 专属的表达式构造器：JSON 与全文检索。
//
// 这些能力**不属于**通用 API：导入并使用本包意味着这段代码不可移植到 PostgreSQL，
// 这正是设计意图——让「使用了方言专属特性」在代码里一眼可见。
//
//	import "github.com/qiudaozhang/goxdb/dialect/my"
//
//	xdb.Query[Doc](db).Where(my.JSON(doc.Meta).Text("status").Eq("active"))
package my

import (
	"strconv"
	"strings"

	xdb "github.com/qiudaozhang/goxdb"
)

// JSON 构造一个 JSON 列（或任意表达式）的路径访问起点。
//
// 路径由代码中的常量构成；不要传入用户输入（需要动态路径时用 xdb.SQL 绑定参数）。
func JSON(col xdb.Node) JSONPath { return JSONPath{root: col} }

// JSONPath 是 MySQL JSON 上的路径链。
//
// MySQL 没有 PostgreSQL 那样的 -> 操作符，统一用
// JSON_EXTRACT(col, '$.a.b') 与 JSON_UNQUOTE(JSON_EXTRACT(...))。
// path 保存的是 JSON 路径的后缀部分（如 ".a[0].b"）。
type JSONPath struct {
	root xdb.Node
	path string
}

// Field 进入下一层对象字段，返回 JSON 值（仍可继续链）。
func (p JSONPath) Field(name string) JSONPath {
	seg := "." + escapeSegment(name)
	return JSONPath{root: p.root, path: p.path + seg}
}

// Index 取数组下标（从 0 开始）。
func (p JSONPath) Index(n int) JSONPath {
	return JSONPath{root: p.root, path: p.path + "[" + strconv.Itoa(n) + "]"}
}

// Extract 返回 JSON 值本身：JSON_EXTRACT(col, '$.path')。
func (p JSONPath) Extract() xdb.E[string] {
	return xdb.SQL[string]("JSON_EXTRACT(?, "+quote("$"+p.path)+")", p.root)
}

// Text 返回去掉引号的文本值：JSON_UNQUOTE(JSON_EXTRACT(col, '$.path'))。
func (p JSONPath) Text(name string) xdb.E[string] {
	g := p.Field(name)
	return xdb.SQL[string]("JSON_UNQUOTE(JSON_EXTRACT(?, "+quote("$"+g.path)+"))", g.root)
}

// TextAt 返回当前路径的文本值（不追加新段）。
func (p JSONPath) TextAt() xdb.E[string] {
	return xdb.SQL[string]("JSON_UNQUOTE(JSON_EXTRACT(?, "+quote("$"+p.path)+"))", p.root)
}

// HasKey 判断是否存在路径：JSON_CONTAINS_PATH(col, 'one', '$.path')。
func (p JSONPath) HasKey(name string) xdb.Cond {
	g := p.Field(name)
	return xdb.SQL[bool]("JSON_CONTAINS_PATH(?, 'one', "+quote("$"+g.path)+")", g.root)
}

// Contains 判断 JSON 是否包含给定片段：JSON_CONTAINS(col, 'json', '$.path')。
func (p JSONPath) Contains(json string) xdb.Cond {
	return xdb.SQL[bool]("JSON_CONTAINS(?, "+quote(json)+", "+quote("$"+p.path)+")", p.root)
}

// ---------------------------------------------------------------- 全文检索

// Match 构造 MATCH(col) AGAINST (?)。
//
//	mode 为 "" / "IN NATURAL LANGUAGE MODE" / "IN BOOLEAN MODE"。
func Match(col xdb.Node, against string, mode string) xdb.Cond {
	frag := "MATCH(?) AGAINST (?"
	if mode != "" {
		frag += " " + mode
	}
	return xdb.SQL[bool](frag+")", col, xdb.V(against))
}

// ---------------------------------------------------------------- 内部

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// escapeSegment 转义 JSON 路径中的特殊字符：引号与反斜杠。
func escapeSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
