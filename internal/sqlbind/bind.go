// Package sqlbind 把原始 SQL 中的 "?" 占位符改写为方言原生形式，并展开切片参数；
// 字符串字面量、带引号的标识符和注释中的内容保持不变。
package sqlbind

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/qiudaozhang/goxdb/dialect"
)

var (
	// ErrArgCount："?" 的个数与参数个数不一致。
	ErrArgCount = errors.New("goxdb: placeholder/argument count mismatch")
	// ErrEmptySlice：切片参数为空。"IN ()" 是非法 SQL，
	// 而静默替换为 NULL 会让 "NOT IN" 的语义出错，因此直接报错。
	ErrEmptySlice = errors.New("goxdb: empty slice argument")
)

// AsIs 包装一个值，使其即便是切片也作为单个参数绑定（不展开）。
type AsIs struct{ V any }

// Walk 扫描 SQL 文本：普通文本（含字符串、标识符、注释）交给 text，
// 每个 "?" 占位符调用一次 ph；"??" 作为字面量 "?" 交给 text。
func Walk(syn dialect.Syntax, query string, text func(string), ph func() error) error {
	n := len(query)
	start := 0 // 尚未输出的普通文本起点
	flush := func(end int) {
		if end > start {
			text(query[start:end])
		}
	}
	for i := 0; i < n; {
		c := query[i]
		switch {
		case c == '\'':
			i = skipQuoted(query, i, '\'', syn.BackslashEscape || isEString(query, i, syn))
		case c == '"':
			i = skipQuoted(query, i, '"', syn.DoubleQuoteString && syn.BackslashEscape)
		case c == '`' && syn.BacktickIdent:
			i = skipQuoted(query, i, '`', false)
		case c == '-' && i+1 < n && query[i+1] == '-' &&
			(!syn.DashCommentNeedsSpace || i+2 >= n || isSpace(query[i+2])):
			i = skipLine(query, i)
		case c == '#' && syn.HashComment:
			i = skipLine(query, i)
		case c == '/' && i+1 < n && query[i+1] == '*':
			i = skipBlock(query, i)
		case c == '$' && syn.DollarQuote:
			if j, ok := skipDollar(query, i); ok {
				i = j
			} else {
				i++
			}
		case c == '?':
			flush(i)
			if i+1 < n && query[i+1] == '?' {
				text("?")
				i += 2
			} else {
				if err := ph(); err != nil {
					return err
				}
				i++
			}
			start = i
		default:
			i++
		}
	}
	flush(n)
	return nil
}

// Bind 按方言 d 改写 query。字面量 "?" 可写作 "??"
// （用于 PostgreSQL jsonb 的 ? 运算符）。
func Bind(d dialect.Dialect, query string, args []any) (string, []any, error) {
	if len(args) == 0 && strings.IndexByte(query, '?') < 0 {
		return query, args, nil
	}
	var (
		b      strings.Builder
		out    = make([]any, 0, len(args))
		argIdx int
	)
	b.Grow(len(query) + 8)
	bindOne := func(v any) {
		out = append(out, v)
		b.WriteString(d.Placeholder(len(out)))
	}
	err := Walk(d.Syntax(), query, func(s string) { b.WriteString(s) }, func() error {
		if argIdx >= len(args) {
			return fmt.Errorf("%w: more placeholders than %d args", ErrArgCount, len(args))
		}
		a := args[argIdx]
		argIdx++
		if vals, ok := Expand(a); ok {
			if len(vals) == 0 {
				return fmt.Errorf("%w (argument #%d)", ErrEmptySlice, argIdx)
			}
			for k, v := range vals {
				if k > 0 {
					b.WriteString(", ")
				}
				bindOne(v)
			}
			return nil
		}
		if w, ok := a.(AsIs); ok {
			a = w.V
		}
		bindOne(a)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if argIdx != len(args) {
		return "", nil, fmt.Errorf("%w: %d placeholders, %d args", ErrArgCount, argIdx, len(args))
	}
	return b.String(), out, nil
}

var valuerType = reflect.TypeFor[driver.Valuer]()

// Expand 返回需要展开为逗号分隔占位符列表的切片参数元素。
func Expand(a any) ([]any, bool) {
	if a == nil {
		return nil, false
	}
	switch a.(type) {
	case []byte, AsIs, driver.Valuer:
		return nil, false
	}
	rv := reflect.ValueOf(a)
	if rv.Kind() != reflect.Slice || rv.Type().Implements(valuerType) {
		return nil, false
	}
	if rv.Type().Elem().Kind() == reflect.Uint8 { // 具名的 []byte 类型不展开
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isEString(q string, i int, syn dialect.Syntax) bool {
	if !syn.EscapeStringPrefix || i == 0 {
		return false
	}
	p := q[i-1]
	if p != 'e' && p != 'E' {
		return false
	}
	// 确认 E 是独立前缀，而不是某个标识符的结尾
	return i < 2 || !isIdentChar(q[i-2])
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// skipQuoted 返回从 q[i] 开始的引用串结束引号之后的下标。
// 连续两个引号视为转义；未闭合时一直到文本末尾。
func skipQuoted(q string, i int, quote byte, backslash bool) int {
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			if backslash {
				j++
			}
		case quote:
			if j+1 < len(q) && q[j+1] == quote {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(q)
}

func skipLine(q string, i int) int {
	if j := strings.IndexByte(q[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(q)
}

func skipBlock(q string, i int) int {
	// PostgreSQL 的块注释可以嵌套，MySQL 不行；
	// 但对格式正确的 MySQL 注释按嵌套方式扫描也不会出错。
	depth := 0
	for j := i; j+1 < len(q); j++ {
		switch {
		case q[j] == '/' && q[j+1] == '*':
			depth++
			j++
		case q[j] == '*' && q[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(q)
}

// skipDollar 处理 $tag$...$tag$ 字符串；当 q[i] 并非美元引用开头
// （例如 "$1" 位置参数）时返回 false。
func skipDollar(q string, i int) (int, bool) {
	if i > 0 && isIdentChar(q[i-1]) {
		return 0, false
	}
	j := i + 1
	for j < len(q) && q[j] != '$' {
		c := q[j]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 || (j > i+1 && c >= '0' && c <= '9')) {
			return 0, false
		}
		j++
	}
	if j >= len(q) {
		return 0, false
	}
	tag := q[i : j+1]
	end := strings.Index(q[j+1:], tag)
	if end < 0 {
		return len(q), true
	}
	return j + 1 + end + len(tag), true
}
