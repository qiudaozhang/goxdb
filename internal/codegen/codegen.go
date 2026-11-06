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
	GoName string // 描述符字段名：沿用 Go 字段提升，与其它列重名时加嵌入路径前缀
	Sel    string // 从模型根到该字段的 Go 选择器，如 "Base.ID"；非嵌入时等于 GoName
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
	var files []*ast.File
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
		files = append(files, f)
	}
	// 按整个包提取：嵌入的结构体与 TableName() 方法可能定义在别的文件里。
	models := extract(files)
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, pkg, nil
}

// extract 从一个包的全部文件里提取模型。
func extract(files []*ast.File) []ModelInfo {
	// 先收集 TableName() 的返回值（用于覆盖表名推导）与包内结构体索引（用于展开嵌入字段）。
	tables := map[string]string{}
	idx := structIndex{}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if name, table, ok := tableNameMethod(d); ok {
					tables[name] = table
				}
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							idx[ts.Name.Name] = st
						}
					}
				}
			}
		}
	}

	var out []ModelInfo
	for _, f := range files {
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
				m, ok := extractModel(ts.Name.Name, st, idx)
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
	}
	return out
}

// tableNameMethod 识别形如 func (T) TableName() string { return "x" } 的方法。
func tableNameMethod(fn *ast.FuncDecl) (recv, table string, ok bool) {
	if fn.Name.Name != "TableName" || fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", "", false
	}
	recv = receiverName(fn.Recv.List[0].Type)
	if recv == "" || fn.Body == nil || len(fn.Body.List) != 1 {
		return "", "", false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", "", false
	}
	lit, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", "", false
	}
	table, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", "", false
	}
	return recv, table, true
}

// receiverName 返回方法接收者的类型名（T 或 *T）。
func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// structIndex 是包内结构体定义的索引，用于展开嵌入字段。
type structIndex map[string]*ast.StructType

// colCand 是展开过程中的候选列，冲突消解规则与运行时 schema 一致。
type colCand struct {
	c     Column
	depth int    // 嵌入层级，模型自身字段为 0
	path  string // 嵌入路径拼接名（如 "Base"），用于描述符改名
	pk    bool
}

// extractModel 把结构体转换为列与关联描述。没有 xdb 标签时返回 false：
// 只有显式标注过的模型才会生成描述符，避免误把 DTO 当作表。
//
// 嵌入字段（含导出的嵌入指针）会递归展开，并按 schema 的规则消解同名列：
// 层级最浅者胜出，同层多个则全部丢弃。两边不一致会让生成的扫描器与框架的
// 列集对不上——那比「不支持」更糟。
//
// 描述符字段名沿用 Go 的字段提升（Base.ID → U.ID）；与其它列的描述符重名时
// 才加上嵌入路径前缀（U.BaseID）。
func extractModel(name string, st *ast.StructType, idx structIndex) (ModelInfo, bool) {
	m := ModelInfo{Name: name}
	var cands []colCand
	tagged := collect(st, idx, "", "", 0, map[string]bool{name: true}, &cands, &m.Rels)
	if !tagged {
		return ModelInfo{}, false
	}

	// 按列名分组，保持首次出现的顺序（与 schema.parse 相同）。
	groups := map[string][]colCand{}
	var order []string
	for _, c := range cands {
		if _, seen := groups[c.c.Column]; !seen {
			order = append(order, c.c.Column)
		}
		groups[c.c.Column] = append(groups[c.c.Column], c)
	}
	var kept []colCand
	for _, col := range order {
		g := groups[col]
		minDepth := g[0].depth
		for _, c := range g[1:] {
			minDepth = min(minDepth, c.depth)
		}
		var best []colCand
		for _, c := range g {
			if c.depth == minDepth {
				best = append(best, c)
			}
		}
		if len(best) == 1 { // 生成器只看带标签的字段，同层多个即为歧义
			kept = append(kept, best[0])
		}
	}

	// 描述符名冲突时，嵌入来的列改用带路径前缀的名字。
	count := map[string]int{}
	for _, c := range kept {
		count[c.c.GoName]++
	}
	for _, c := range kept {
		if count[c.c.GoName] > 1 && c.path != "" {
			c.c.GoName = c.path + c.c.GoName
		}
		if c.pk {
			m.HasPK = true
		}
		m.Columns = append(m.Columns, c.c)
	}
	if len(m.Columns) == 0 {
		return ModelInfo{}, false
	}
	return m, true
}

// collect 遍历结构体字段，把列追加到 out、关联追加到 rels（仅模型自身层级）。
// sel 是从模型根到 st 的选择器前缀，path 是嵌入路径拼接名。返回是否见到 xdb 标签。
func collect(st *ast.StructType, idx structIndex, sel, path string, depth int, visiting map[string]bool, out *[]colCand, rels *[]Rel) bool {
	tagged := false
	for _, field := range st.Fields.List {
		tag, hasTag := fieldTag(field)
		if hasTag && tag == "-" {
			continue
		}
		if len(field.Names) == 0 {
			// 匿名（嵌入）字段：与 schema.walk 一致，只展开同包可见的结构体；
			// 未导出的嵌入指针无法由框架分配，运行时会跳过，这里同样跳过。
			typ := embeddedTypeName(field.Type)
			sub, ok := idx[typ]
			if !ok || visiting[typ] || (hasTag && tagName(tag) != "") {
				continue
			}
			if _, isPtr := field.Type.(*ast.StarExpr); isPtr && !ast.IsExported(typ) {
				continue
			}
			visiting[typ] = true
			if collect(sub, idx, sel+typ+".", path+typ, depth+1, visiting, out, nil) {
				tagged = true
			}
			delete(visiting, typ)
			continue
		}
		if !hasTag || !ast.IsExported(field.Names[0].Name) {
			continue
		}
		tagged = true
		colName, opts := splitTag(tag)
		goName := field.Names[0].Name
		if kind, isRel := opts["rel"]; isRel {
			if rels != nil { // 嵌入结构体里的关联不提升到上层模型
				*rels = append(*rels, Rel{GoName: goName, Kind: kind, FK: opts["fk"]})
			}
			continue
		}
		if colName == "" {
			colName = schema.SnakeCase(goName)
		}
		c := newColumn(goName, colName, field.Type)
		c.Sel = sel + goName
		_, pk := opts["pk"]
		*out = append(*out, colCand{c: c, depth: depth, path: path, pk: pk})
	}
	return tagged
}

// fieldTag 返回字段的 xdb 标签值。
func fieldTag(field *ast.Field) (string, bool) {
	if field.Tag == nil {
		return "", false
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return "", false
	}
	return lookupTag(raw, schema.TagName)
}

// tagName 返回标签中的列名部分。
func tagName(tag string) string {
	name, _ := splitTag(tag)
	return name
}

// embeddedTypeName 返回嵌入字段的类型名；外部包（含选择器）返回空。
func embeddedTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedTypeName(t.X)
	}
	return ""
}

// newColumn 按 Go 类型推断列描述符的种类与类型参数。
func newColumn(goName, colName string, expr ast.Expr) Column {
	c := Column{GoName: goName, Sel: goName, Column: colName}
	typ := typeString(expr)
	base := strings.TrimPrefix(typ, "*")
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
	return c
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
	fmt.Fprintf(&b, "import (\n\t\"fmt\"\n\n\txdb \"github.com/qiudaozhang/goxdb\"\n)\n\n")

	for _, m := range models {
		v := varName(m.Name)
		fmt.Fprintf(&b, "// %s 是 %s 的类型化表描述符。\n", v, m.Name)
		fmt.Fprintf(&b, "var %s = struct {\n\txdb.Table\n", v)
		for _, c := range m.Columns {
			fmt.Fprintf(&b, "\t%s %s\n", c.GoName, colType(c))
		}
		fmt.Fprintf(&b, "}{}\n\n")
	}

	// 零反射扫描器：把列顺序与字段地址固定在编译期，
	// 逐行扫描退化为直接写入 &m.Field，省掉反射取址与接口装箱。
	for _, m := range models {
		renderScanner(&b, m)
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

// renderScanner 为模型生成 xdb.RowScanner 的实现。
func renderScanner(b *bytes.Buffer, m ModelInfo) {
	recv := strings.ToLower(m.Name[:1])
	scan := lowerFirst(m.Name)
	fmt.Fprintf(b, "// NewRowScanner 实现 xdb.RowScanner：按编译期固定的列顺序填写字段地址，\n")
	fmt.Fprintf(b, "// 逐行扫描不经过反射。列不能一一匹配时返回 nil，由框架回退到反射路径。\n")
	fmt.Fprintf(b, "func (%s *%s) NewRowScanner(columns []string) (xdb.ScanRows, error) {\n", recv, m.Name)
	fmt.Fprintf(b, "\tif len(columns) != %d {\n\t\treturn nil, nil\n\t}\n", len(m.Columns))
	for i, c := range m.Columns {
		fmt.Fprintf(b, "\tif columns[%d] != %s {\n\t\treturn nil, nil\n\t}\n", i, strconv.Quote(c.Column))
	}
	fmt.Fprintf(b, "\treturn &%sScanner{}, nil\n}\n\n", scan)

	fmt.Fprintf(b, "// %sScanner 是 %s 的零反射行扫描器。\n", scan, m.Name)
	fmt.Fprintf(b, "type %sScanner struct {\n\tdest [%d]any\n}\n\n", scan, len(m.Columns))
	fmt.Fprintf(b, "func (s *%sScanner) ScanDest(dst any) ([]any, error) {\n", scan)
	fmt.Fprintf(b, "\tm, ok := dst.(*%s)\n\tif !ok {\n\t\treturn nil, fmt.Errorf(\"goxdb: %s scanner got %%T\", dst)\n\t}\n", m.Name, m.Name)
	for i, c := range m.Columns {
		// 嵌入字段走完整选择器（&m.Base.ID）；途经的嵌入指针由框架在
		// 调用 ScanDest 前分配（schema.Model.EmbedPtrs）。
		fmt.Fprintf(b, "\ts.dest[%d] = &m.%s\n", i, c.Sel)
	}
	fmt.Fprintf(b, "\treturn s.dest[:], nil\n}\n\n")
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
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
