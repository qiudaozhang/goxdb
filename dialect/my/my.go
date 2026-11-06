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
	path, args := strLit("$" + p.path)
	return xdb.SQL[string]("JSON_EXTRACT(?, "+path+")", append([]any{p.root}, args...)...)
}

// Text 返回去掉引号的文本值：JSON_UNQUOTE(JSON_EXTRACT(col, '$.path'))。
func (p JSONPath) Text(name string) xdb.E[string] {
	return p.Field(name).TextAt()
}

// TextAt 返回当前路径的文本值（不追加新段）。
func (p JSONPath) TextAt() xdb.E[string] {
	path, args := strLit("$" + p.path)
	return xdb.SQL[string]("JSON_UNQUOTE(JSON_EXTRACT(?, "+path+"))", append([]any{p.root}, args...)...)
}

// HasKey 判断是否存在路径：JSON_CONTAINS_PATH(col, 'one', '$.path')。
func (p JSONPath) HasKey(name string) xdb.Cond {
	g := p.Field(name)
	path, args := strLit("$" + g.path)
	return xdb.SQL[bool]("JSON_CONTAINS_PATH(?, 'one', "+path+")", append([]any{g.root}, args...)...)
}

// Contains 判断 JSON 是否包含给定片段：JSON_CONTAINS(col, 'json', '$.path')。
func (p JSONPath) Contains(json string) xdb.Cond {
	doc, dargs := strLit(json)
	path, pargs := strLit("$" + p.path)
	args := append(append([]any{p.root}, dargs...), pargs...)
	return xdb.SQL[bool]("JSON_CONTAINS(?, "+doc+", "+path+")", args...)
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

// strLit 返回 SQL 字符串常量的片段及其参数。
//
// 通常内联为 '...'，使表达式与生成列 / 函数索引上的表达式逐字一致；
// 含反斜杠时改为绑定参数——MySQL 如何解释字面量里的反斜杠取决于
// sql_mode 的 NO_BACKSLASH_ESCAPES，构建 SQL 时无法得知。
func strLit(s string) (string, []any) {
	if strings.Contains(s, `\`) {
		return "?", []any{s}
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

// escapeSegment 把键名写成 JSON 路径的成员段。
// 合法的 ECMAScript 标识符原样输出（$.status）；其余（含 -、空格、引号等）
// 必须用双引号包起来，内部的 " 与 \ 用反斜杠转义（$."first-name"）。
func escapeSegment(s string) string {
	if isIdent(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_' || c == '$' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i > 0 && '0' <= c && c <= '9':
		default:
			return false
		}
	}
	return true
}
