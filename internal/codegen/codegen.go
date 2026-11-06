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
	"io"
	"os"
	"path"
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
	// Warn 接收不影响生成但需要用户知道的提示（例如某个模型无法生成零反射扫描器）。
	// 为 nil 时写到 os.Stderr。
	Warn io.Writer
}

// ModelInfo 是从源码中解析出的一个模型。
type ModelInfo struct {
	Name    string   // Go 类型名
	Table   string   // 表名（由 TableName() 或 snake_case 复数推导）
	Columns []Column // 列描述符；顺序与模型字段一致
	Rels    []Rel    // 关联
	HasPK   bool
	// Warnings 非空表示生成器看不全模型的列集（例如嵌入了外部包的结构体）：
	// 仍生成已知列的描述符，但不生成 NewRowScanner，扫描直接走反射。
	Warnings []string
}

// Column 是一个列的生成信息。
type Column struct {
	GoName string // 描述符字段名：沿用 Go 字段提升，与其它列重名时加嵌入路径前缀
	Sel    string // 从模型根到该字段的 Go 选择器，如 "Base.ID"；非嵌入时等于 GoName
	Column string // 列名
	Kind   string // xdb.NumberCol / xdb.StringCol / xdb.TimeCol / xdb.Col
	Type   string // 类型参数（仅 Col[T] 与 NumberCol[T] 使用）
	// Imports 是 Type 中用到的包：限定符 -> 导入路径（仅 xdb.Col 需要）。
	Imports map[string]string
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
	warn := cfg.Warn
	if warn == nil {
		warn = os.Stderr
	}
	for _, m := range models {
		for _, w := range m.Warnings {
			fmt.Fprintf(warn, "goxdb: warning: %s: %s\n", m.Name, w)
		}
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
	models, err := extract(files)
	if err != nil {
		return nil, "", err
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, pkg, nil
}

// extract 从一个包的全部文件里提取模型。
func extract(files []*ast.File) ([]ModelInfo, error) {
	// 先收集 TableName() 的返回值（用于覆盖表名推导）、包内类型索引（用于展开嵌入字段）
	// 以及带 Scan 方法的类型（运行时把它们当作标量列而不展开）。
	tables := map[string]string{}
	idx := pkgIndex{types: map[string]typeDecl{}, scanners: map[string]bool{}}
	for _, f := range files {
		imports := fileImports(f)
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if name, table, ok := tableNameMethod(d); ok {
					tables[name] = table
				}
				if d.Name.Name == "Scan" && d.Recv != nil && len(d.Recv.List) > 0 {
					if r := receiverName(d.Recv.List[0].Type); r != "" {
						idx.scanners[r] = true
					}
				}
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						idx.types[ts.Name.Name] = typeDecl{spec: ts, imports: imports}
					}
				}
			}
		}
	}

	var out []ModelInfo
	for _, f := range files {
		imports := fileImports(f)
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.TypeParams != nil {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				m, ok, err := extractModel(ts.Name.Name, st, imports, idx)
				if err != nil {
					return nil, err
				}
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
	return out, nil
}

// fileImports 返回文件里的导入：限定符 -> 路径。
// 无别名时按惯例从路径推断包名（去掉 /vN 与 .vN 后缀）；点导入与空白导入忽略。
func fileImports(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		if im.Name != nil {
			name = im.Name.Name
		} else {
			name = guessPkgName(p)
		}
		if name == "_" || name == "." {
			continue
		}
		m[name] = p
	}
	return m
}

func guessPkgName(p string) string {
	base := path.Base(p)
	if len(base) > 1 && base[0] == 'v' && isDigits(base[1:]) && path.Dir(p) != "." {
		base = path.Base(path.Dir(p))
	}
	if i := strings.LastIndex(base, ".v"); i > 0 && isDigits(base[i+2:]) {
		base = base[:i]
	}
	return base
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// typeDecl 是包内的一个类型声明及其所在文件的导入表。
type typeDecl struct {
	spec    *ast.TypeSpec
	imports map[string]string
}

// pkgIndex 是包内类型的索引，用于判断嵌入字段该展开还是当作单列。
type pkgIndex struct {
	types    map[string]typeDecl
	scanners map[string]bool // 声明了 Scan 方法的类型（实现 sql.Scanner，运行时视为标量）
}

// embedKind 是嵌入字段在运行时 schema 中的处理方式。
type embedKind int

const (
	embedScalar  embedKind = iota // 当作单列（schema.IsScalar 为真）
	embedStruct                   // 递归展开其字段
	embedUnknown                  // 外部包的结构体：只靠语法树无法得知字段
)

// classify 按 schema.walk 的规则判断嵌入类型 e：非结构体与实现 sql.Scanner 的
// 结构体（含 time.Time、sql.Null*）是标量列，其余结构体展开。
func (p pkgIndex) classify(e ast.Expr, imports map[string]string, depth int) (embedKind, *ast.StructType, map[string]string) {
	if depth > 16 {
		return embedUnknown, nil, nil
	}
	switch t := e.(type) {
	case *ast.StarExpr:
		return p.classify(t.X, imports, depth+1)
	case *ast.ParenExpr:
		return p.classify(t.X, imports, depth+1)
	case *ast.Ident:
		d, ok := p.types[t.Name]
		if !ok {
			return embedScalar, nil, nil // 预声明类型
		}
		if p.scanners[t.Name] {
			return embedScalar, nil, nil
		}
		if d.spec.TypeParams != nil {
			return embedUnknown, nil, nil
		}
		switch u := d.spec.Type.(type) {
		case *ast.StructType:
			return embedStruct, u, d.imports
		case *ast.Ident, *ast.SelectorExpr, *ast.StarExpr, *ast.ParenExpr, *ast.IndexExpr, *ast.IndexListExpr:
			// type B A / type B = A：底层类型决定是否展开；
			// 定义类型不继承方法，这里只在 B 自身检查了 Scan。
			return p.classify(u, d.imports, depth+1)
		}
		return embedScalar, nil, nil // 切片、映射、函数等
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return embedUnknown, nil, nil
		}
		switch imp := imports[pkg.Name]; {
		case imp == "time" && t.Sel.Name == "Time":
			return embedScalar, nil, nil
		case imp == "database/sql" && strings.HasPrefix(t.Sel.Name, "Null"):
			return embedScalar, nil, nil
		}
		return embedUnknown, nil, nil
	case *ast.IndexExpr: // 泛型实例，如 sql.Null[int64]
		if k, _, _ := p.classify(t.X, imports, depth+1); k == embedScalar {
			if _, isSel := t.X.(*ast.SelectorExpr); isSel {
				return embedScalar, nil, nil
			}
		}
		return embedUnknown, nil, nil
	}
	return embedUnknown, nil, nil
}

// colCand 是展开过程中的候选列，冲突消解规则与运行时 schema 一致。
type colCand struct {
	c     Column
	depth int    // 嵌入层级，模型自身字段为 0
	path  string // 嵌入路径拼接名（如 "Base"），用于描述符改名
	pk    bool
	named bool // 标签里显式写了列名（冲突消解时优先）
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
func extractModel(name string, st *ast.StructType, imports map[string]string, idx pkgIndex) (ModelInfo, bool, error) {
	m := ModelInfo{Name: name}
	var cands []colCand
	w := &walker{idx: idx, visiting: map[*ast.StructType]bool{st: true}, out: &cands, warnings: &m.Warnings}
	tagged := w.collect(st, imports, "", "", 0, &m.Rels)
	if w.err != nil {
		return ModelInfo{}, false, fmt.Errorf("%s: %w", name, w.err)
	}
	if !tagged {
		return ModelInfo{}, false, nil
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
		if len(best) == 1 {
			kept = append(kept, best[0])
			continue
		}
		// 同层多个时，显式写了列名的唯一一个胜出（与 schema.dominant 相同）。
		var named []colCand
		for _, c := range best {
			if c.named {
				named = append(named, c)
			}
		}
		if len(named) == 1 {
			kept = append(kept, named[0])
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
		return ModelInfo{}, false, nil
	}
	return m, true, nil
}

// walker 是一次模型展开的状态。
type walker struct {
	idx      pkgIndex
	visiting map[*ast.StructType]bool
	out      *[]colCand
	warnings *[]string
	err      error
}

// collect 遍历结构体字段，把列追加到 out、关联追加到 rels（仅模型自身层级）。
// sel 是从模型根到 st 的选择器前缀，path 是嵌入路径拼接名，imports 是 st 所在文件的导入表。
// 返回是否见到 xdb 标签。
func (w *walker) collect(st *ast.StructType, imports map[string]string, sel, path string, depth int, rels *[]Rel) bool {
	tagged := false
	for _, field := range st.Fields.List {
		tag, hasTag := fieldTag(field)
		if hasTag && tag == "-" {
			continue
		}
		var goName string
		if len(field.Names) == 0 {
			// 匿名（嵌入）字段：与 schema.walk 一致——标签里没写列名的结构体递归展开，
			// 其余（标量类型、实现 sql.Scanner 的结构体、写了列名的嵌入）当作一列。
			goName = embeddedTypeName(field.Type)
			if goName == "" {
				continue
			}
			if !hasTag || tagName(tag) == "" {
				kind, sub, subImports := w.idx.classify(field.Type, imports, 0)
				switch kind {
				case embedUnknown:
					if ast.IsExported(goName) || !isPointer(field.Type) {
						*w.warnings = append(*w.warnings, fmt.Sprintf(
							"embedded field %s%s: cannot resolve its columns from source "+
								"(struct from another package?); descriptors omit them and "+
								"scanning falls back to reflection", sel, typeString(field.Type)))
					}
					continue
				case embedStruct:
					// 未导出的嵌入指针无法由框架分配，运行时会跳过，这里同样跳过。
					if isPointer(field.Type) && !ast.IsExported(goName) {
						continue
					}
					if w.visiting[sub] {
						continue
					}
					w.visiting[sub] = true
					if w.collect(sub, subImports, sel+goName+".", path+goName, depth+1, nil) {
						tagged = true
					}
					delete(w.visiting, sub)
					continue
				}
			}
		} else {
			goName = field.Names[0].Name
		}
		if !ast.IsExported(goName) {
			continue
		}
		// 与运行时一致：导出字段即使没有标签也是列（列名取 snake_case）；
		// 不想映射的字段要显式写 xdb:"-"。是否算模型仍只看有没有出现过 xdb 标签。
		if hasTag {
			tagged = true
		}
		colName, opts := splitTag(tag)
		if kind, isRel := opts["rel"]; isRel {
			if rels != nil { // 嵌入结构体里的关联不提升到上层模型
				*rels = append(*rels, Rel{GoName: goName, Kind: kind, FK: opts["fk"]})
			}
			continue
		}
		named := colName != ""
		if colName == "" {
			colName = schema.SnakeCase(goName)
		}
		c := newColumn(goName, colName, field.Type)
		c.Sel = sel + goName
		if c.Kind == "xdb.Col" {
			imps, err := typeImports(field.Type, imports)
			if err != nil {
				if w.err == nil {
					w.err = fmt.Errorf("field %s%s: %w", sel, goName, err)
				}
				continue
			}
			c.Imports = imps
		}
		_, pk := opts["pk"]
		*w.out = append(*w.out, colCand{c: c, depth: depth, path: path, pk: pk, named: named})
	}
	return tagged
}

func isPointer(e ast.Expr) bool { _, ok := e.(*ast.StarExpr); return ok }

// typeImports 找出类型表达式里用到的包限定符并映射到导入路径。
func typeImports(e ast.Expr, imports map[string]string) (map[string]string, error) {
	var out map[string]string
	var err error
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		p, ok := imports[pkg.Name]
		if !ok {
			if err == nil {
				err = fmt.Errorf("cannot resolve package %q; import it with an explicit name", pkg.Name)
			}
			return false
		}
		if out == nil {
			out = map[string]string{}
		}
		out[pkg.Name] = p
		return false
	})
	return out, err
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

// embeddedTypeName 返回嵌入字段的字段名（即类型名，不含包限定符与类型参数）。
func embeddedTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return embeddedTypeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return embeddedTypeName(t.X)
	case *ast.IndexListExpr:
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
	case *ast.IndexExpr:
		return typeString(t.X) + "[" + typeString(t.Index) + "]"
	case *ast.IndexListExpr:
		args := make([]string, len(t.Indices))
		for i, x := range t.Indices {
			args[i] = typeString(x)
		}
		return typeString(t.X) + "[" + strings.Join(args, ", ") + "]"
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
	imports, err := renderImports(models)
	if err != nil {
		return nil, err
	}
	b.WriteString(imports)

	names := varNames(models)
	for _, m := range models {
		v := names[m.Name]
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
		if len(m.Warnings) == 0 {
			renderScanner(&b, m)
		}
	}

	for _, m := range models {
		v := names[m.Name]
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

// renderImports 汇总生成文件需要的导入：xdb、扫描器用的 fmt，以及列类型引用的包。
func renderImports(models []ModelInfo) (string, error) {
	const xdbPath = "github.com/qiudaozhang/goxdb"
	want := map[string]string{"xdb": xdbPath}
	for _, m := range models {
		if len(m.Warnings) == 0 {
			want["fmt"] = "fmt"
		}
		for _, c := range m.Columns {
			for q, p := range c.Imports {
				if prev, ok := want[q]; ok && prev != p {
					return "", fmt.Errorf("%s.%s: package name %q refers to both %q and %q; use distinct import names", m.Name, c.GoName, q, prev, p)
				}
				want[q] = p
			}
		}
	}
	names := make([]string, 0, len(want))
	for q := range want {
		names = append(names, q)
	}
	// 标准库一组、其余一组，与 goimports 的习惯一致。
	std := func(p string) bool { return !strings.Contains(strings.SplitN(p, "/", 2)[0], ".") }
	sort.Slice(names, func(i, j int) bool {
		pi, pj := want[names[i]], want[names[j]]
		if std(pi) != std(pj) {
			return std(pi)
		}
		return pi < pj
	})
	var b strings.Builder
	b.WriteString("import (\n")
	for i, q := range names {
		p := want[q]
		if i > 0 && std(want[names[i-1]]) && !std(p) {
			b.WriteString("\n")
		}
		if q == guessPkgName(p) && q != "xdb" {
			fmt.Fprintf(&b, "\t%s\n", strconv.Quote(p))
		} else {
			fmt.Fprintf(&b, "\t%s %s\n", q, strconv.Quote(p))
		}
	}
	b.WriteString(")\n\n")
	return b.String(), nil
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

// varNames 为每个模型选定描述符变量名。短名（varName）在多个模型间重复，
// 或与某个模型的类型名相同时，这些模型都改用 <Type>Table，保证生成代码能编译。
// 结果只取决于模型集合本身，与解析顺序无关。
func varNames(models []ModelInfo) map[string]string {
	count := map[string]int{}
	types := map[string]bool{}
	for _, m := range models {
		count[varName(m.Name)]++
		types[m.Name] = true
	}
	out := make(map[string]string, len(models))
	for _, m := range models {
		v := varName(m.Name)
		if count[v] > 1 || (types[v] && v != m.Name+"Table") {
			v = m.Name + "Table"
		}
		out[m.Name] = v
	}
	return out
}
