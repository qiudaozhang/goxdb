// Package codegen 为 goxdb 模型生成类型化的表与列描述符。
//
// 它通过标准库的 go/ast 解析目标包里带 `xdb` 标签的结构体，
// 输出一个 *_gen.go 文件，其中每个模型对应一个匿名 struct 变量。
package codegen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qiudaozhang/goxdb/internal/schema"
)

// Config 控制生成行为。
type Config struct {
	Dir string // 包含模型源码的目录
	Pkg string // 生成的包名；为空时沿用目录中的包名
	Out string // 输出文件名，默认 xdb_gen.go
}

// ModelInfo 是从源码中解析出的一个模型。
type ModelInfo struct {
	Name    string   // Go 类型名
	Table   string   // 表名（由 TableName() 或 snake_case 复数推导）
	Columns []Column // 列描述符；顺序与模型字段一致
	Rels    []Rel    // 关联
	HasPK   bool
}

// Column 是一个列的生成信息。
type Column struct {
	GoName string // 字段名
	Column string // 列名
	Kind   string // xdb.NumberCol / xdb.StringCol / xdb.TimeCol / xdb.Col
	Type   string // 类型参数（仅 Col[T] 与 NumberCol[T] 使用）
}

// Rel 是一个关联的生成信息。
type Rel struct {
	GoName string
	Kind   string
	FK     string
}

// Run 解析目录并写出生成文件。
func Run(cfg Config) error {
	if cfg.Out == "" {
		cfg.Out = "xdb_gen.go"
	}
	models, pkg, err := ParseDir(cfg.Dir)
	if err != nil {
		return err
	}
	if cfg.Pkg != "" {
		pkg = cfg.Pkg
	}
	if pkg == "" {
		return fmt.Errorf("cannot determine package name; pass -pkg")
	}
	src, err := Render(pkg, models)
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.Dir, cfg.Out)
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return err
	}
	return nil
}

// ParseDir 解析目录中的全部 Go 文件（跳过生成文件与测试文件），
// 返回带 xdb 标签的模型。也返回目录中的包名。
func ParseDir(dir string) ([]ModelInfo, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", err
	}
	fset := token.NewFileSet()
	var models []ModelInfo
	var pkg string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", name, err)
		}
		if pkg == "" {
			pkg = f.Name.Name
		}
		models = append(models, extract(f)...)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, pkg, nil
}

// extract 从单个文件里提取模型。
func extract(f *ast.File) []ModelInfo {
	// 先收集 TableName() 的返回值，用于覆盖表名推导。
	tables := map[string]string{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TableName" || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		recv := receiverName(fn.Recv.List[0].Type)
		if recv == "" || fn.Body == nil || len(fn.Body.List) != 1 {
			continue
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		if lit, ok := ret.Results[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				tables[recv] = s
			}
		}
	}

	var out []ModelInfo
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			m, ok := extractModel(ts.Name.Name, st)
			if !ok {
				continue
			}
			if t, has := tables[m.Name]; has {
				m.Table = t
			} else {
				m.Table = schema.Plural(schema.SnakeCase(m.Name))
			}
			out = append(out, m)
		}
	}
	return out
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// extractModel 把结构体转换为列与关联描述。没有 xdb 标签时返回 false：
// 只有显式标注过的模型才会生成描述符，避免误把 DTO 当作表。
func extractModel(name string, st *ast.StructType) (ModelInfo, bool) {
	m := ModelInfo{Name: name}
	tagged := false
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue // 跳过嵌入字段（v1 只支持直接字段）
		}
		if field.Tag == nil {
			continue
		}
		raw, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			continue
		}
		tag, ok := lookupTag(raw, schema.TagName)
		if !ok || tag == "-" {
			continue
		}
		tagged = true
		colName, opts := splitTag(tag)

		if kind, isRel := opts["rel"]; isRel {
			m.Rels = append(m.Rels, Rel{GoName: field.Names[0].Name, Kind: kind, FK: opts["fk"]})
			continue
		}
		if colName == "" {
			colName = schema.SnakeCase(field.Names[0].Name)
		}
		c := Column{GoName: field.Names[0].Name, Column: colName}
		typ := typeString(field.Type)
		base := typ
		if strings.HasPrefix(base, "*") {
			base = base[1:]
		}
		switch {
		case base == "string":
			c.Kind = "xdb.StringCol"
		case base == "time.Time":
			c.Kind = "xdb.TimeCol"
		case isNumber(base):
			c.Kind = "xdb.NumberCol"
			c.Type = base
		default:
			c.Kind = "xdb.Col"
			c.Type = typ
		}
		if _, isPK := opts["pk"]; isPK {
			m.HasPK = true
		}
		m.Columns = append(m.Columns, c)
	}
	if !tagged || len(m.Columns) == 0 {
		return ModelInfo{}, false
	}
	return m, true
}

func lookupTag(raw, key string) (string, bool) {
	for raw != "" {
		var part string
		if i := strings.IndexByte(raw, ' '); i >= 0 {
			part, raw = raw[:i], raw[i+1:]
			raw = strings.TrimLeft(raw, " ")
		} else {
			part, raw = raw, ""
		}
		if !strings.HasPrefix(part, key+":") {
			continue
		}
		v := part[len(key)+1:]
		if s, err := strconv.Unquote(v); err == nil {
			return s, true
		}
		return v, true
	}
	return "", false
}

func splitTag(tag string) (string, map[string]string) {
	parts := strings.Split(tag, ",")
	name := strings.TrimSpace(parts[0])
	rest := parts[1:]
	if strings.Contains(name, ":") {
		name, rest = "", parts
	}
	opts := map[string]string{}
	for _, p := range rest {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, ":")
		opts[k] = v
	}
	return name, opts
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + typeString(t.Key) + "]" + typeString(t.Value)
	case *ast.InterfaceType:
		return "any"
	}
	return "any"
}

func isNumber(t string) bool {
	switch t {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64":
		return true
	}
	return false
}

// Render 生成源码。输出固定格式，便于 diff 与 gofmt 稳定。
func Render(pkg string, models []ModelInfo) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by goxdb. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	fmt.Fprintf(&b, "import (\n\txdb \"github.com/qiudaozhang/goxdb\"\n)\n\n")

	for _, m := range models {
		v := varName(m.Name)
		fmt.Fprintf(&b, "// %s 是 %s 的类型化表描述符。\n", v, m.Name)
		fmt.Fprintf(&b, "var %s = struct {\n\txdb.Table\n", v)
		for _, c := range m.Columns {
			fmt.Fprintf(&b, "\t%s %s\n", c.GoName, colType(c))
		}
		fmt.Fprintf(&b, "}{}\n\n")
	}

	for _, m := range models {
		v := varName(m.Name)
		fmt.Fprintf(&b, "func init() {\n")
		fmt.Fprintf(&b, "\t%s.Table = xdb.TableOf[%s]()\n", v, m.Name)
		for _, c := range m.Columns {
			fmt.Fprintf(&b, "\t%s.%s = %s\n", v, c.GoName, colCtor(c, v))
		}
		fmt.Fprintf(&b, "}\n\n")
	}

	src, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n%s", err, b.String())
	}
	return src, nil
}

func colType(c Column) string {
	switch c.Kind {
	case "xdb.NumberCol":
		return "xdb.NumberCol[" + c.Type + "]"
	case "xdb.Col":
		return "xdb.Col[" + c.Type + "]"
	}
	return c.Kind
}

func colCtor(c Column, tableVar string) string {
	src := tableVar + ".Table"
	switch c.Kind {
	case "xdb.NumberCol":
		return "xdb.CNum[" + c.Type + "](" + src + ", " + strconv.Quote(c.Column) + ")"
	case "xdb.StringCol":
		return "xdb.CStr(" + src + ", " + strconv.Quote(c.Column) + ")"
	case "xdb.TimeCol":
		return "xdb.CTime(" + src + ", " + strconv.Quote(c.Column) + ")"
	}
	return "xdb.C[" + c.Type + "](" + src + ", " + strconv.Quote(c.Column) + ")"
}

// varName 把类型名转成短的描述符变量名：User -> U，UserProfile -> UP，
// Product -> P。非导出类型（首字母小写）或缩略名与类型名相同时，
// 回退为 "<Type>Table"，避免与类型声明冲突。
func varName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	v := b.String()
	if v == "" || v == name {
		return name + "Table"
	}
	return v
}
