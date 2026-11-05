// Package schema 解析并缓存结构体元数据，用于把结果行映射到 Go 值。
// 每个类型只解析一次，之后的查找无锁。
package schema

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
)

// TagName 是 goxdb 使用的结构体标签键。
const TagName = "xdb"

// Field 描述结构体中映射到列的一个字段。
type Field struct {
	Column  string            // 列名
	GoName  string            // 以点分隔的 Go 字段路径，用于错误信息
	Index   []int             // reflect 索引路径（可穿过嵌入结构体）
	Type    reflect.Type      // 字段类型
	Options map[string]string // 其余标签选项，如 pk、auto、rel:has_many
}

// Has 判断是否存在某个标签选项。
func (f *Field) Has(opt string) bool { _, ok := f.Options[opt]; return ok }

// RelationKind 是关联的种类。
type RelationKind string

const (
	// HasMany：一对多，外键在对方表上（user -> orders.user_id）。
	HasMany RelationKind = "has_many"
	// HasOne：一对一，外键在对方表上。
	HasOne RelationKind = "has_one"
	// BelongsTo：属于，外键在本表上（order.user_id -> users.id）。
	BelongsTo RelationKind = "belongs_to"
)

// Relation 是模型上的一个关联字段（标签含 rel 选项，不是列）。
type Relation struct {
	GoName string       // 字段名（用于错误信息与 Preload 定位）
	Index  []int        // reflect 索引路径
	Kind   RelationKind // 关联种类
	FK     string       // 外键列名
	Type   reflect.Type // 字段类型（可为切片、指针或结构体）
	Target reflect.Type // 关联目标的元素类型
}

// Model 是结构体类型解析后的元数据。
type Model struct {
	Type   reflect.Type
	Table  string   // 表名：TableName() 方法的返回值，或类型名的 snake_case 复数
	Fields []*Field // 按声明顺序排列的列字段
	PK     []*Field // 带 pk 选项的字段
	Auto   *Field   // 带 auto 选项的字段（数据库自增），最多一个
	// CreateTime / UpdateTime 是带 autoCreateTime / autoUpdateTime 选项的字段，
	// 由框架在写入时填充（必须在写入前可 settable）。
	CreateTime *Field
	UpdateTime *Field
	// Relations 是按字段名索引的关联定义。
	Relations map[string]*Relation
	byName    map[string]*Field
	byLower   map[string]*Field
}

// Lookup 按列名查找字段：先精确匹配，再忽略大小写匹配。
func (m *Model) Lookup(col string) (*Field, bool) {
	if f, ok := m.byName[col]; ok {
		return f, true
	}
	f, ok := m.byLower[strings.ToLower(col)]
	return f, ok
}

var cache sync.Map // reflect.Type -> *Model

var (
	scannerType = reflect.TypeFor[sql.Scanner]()
	timeType    = reflect.TypeFor[time.Time]()
)

// IsScalar 判断 t 是否作为单列值扫描（而非逐字段映射）：
// 非结构体类型、time.Time、实现了 sql.Scanner 的类型。
func IsScalar(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return true
	}
	return t == timeType || reflect.PointerTo(t).Implements(scannerType)
}

// Of 返回结构体类型 t 的元数据（指针会被解引用）。
func Of(t reflect.Type) (*Model, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if m, ok := cache.Load(t); ok {
		return m.(*Model), nil
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("goxdb: %s is not a struct", t)
	}
	m, err := parse(t)
	if err != nil {
		return nil, err
	}
	actual, _ := cache.LoadOrStore(t, m)
	return actual.(*Model), nil
}

type candidate struct {
	f      *Field
	depth  int
	tagged bool
}

func parse(t reflect.Type) (*Model, error) {
	var cands []candidate
	var rels []*Relation
	if err := walk(t, nil, "", 0, &cands, &rels, map[reflect.Type]bool{}); err != nil {
		return nil, err
	}
	// 与 encoding/json 相同的冲突规则：层级最浅者胜出；
	// 同层级时显式打了标签名的胜出；仍有歧义则全部丢弃。
	groups := map[string][]candidate{}
	var order []string
	for _, c := range cands {
		if _, seen := groups[c.f.Column]; !seen {
			order = append(order, c.f.Column)
		}
		groups[c.f.Column] = append(groups[c.f.Column], c)
	}
	m := &Model{Type: t, Table: tableName(t), byName: map[string]*Field{}, byLower: map[string]*Field{}}
	if len(rels) > 0 {
		m.Relations = make(map[string]*Relation, len(rels))
		for _, r := range rels {
			m.Relations[r.GoName] = r
		}
	}
	for _, name := range order {
		f, ok := dominant(groups[name])
		if !ok {
			continue
		}
		m.Fields = append(m.Fields, f)
		if f.Has("pk") {
			m.PK = append(m.PK, f)
		}
		if f.Has("auto") {
			if m.Auto != nil {
				return nil, fmt.Errorf("goxdb: %s has more than one auto field (%s, %s)", t, m.Auto.GoName, f.GoName)
			}
			m.Auto = f
		}
		if f.Has("autoCreateTime") {
			if err := checkAutoTime(t, f, "autoCreateTime"); err != nil {
				return nil, err
			}
			if m.CreateTime != nil {
				return nil, fmt.Errorf("goxdb: %s has more than one autoCreateTime field (%s, %s)", t, m.CreateTime.GoName, f.GoName)
			}
			m.CreateTime = f
		}
		if f.Has("autoUpdateTime") {
			if err := checkAutoTime(t, f, "autoUpdateTime"); err != nil {
				return nil, err
			}
			if m.UpdateTime != nil {
				return nil, fmt.Errorf("goxdb: %s has more than one autoUpdateTime field (%s, %s)", t, m.UpdateTime.GoName, f.GoName)
			}
			m.UpdateTime = f
		}
		m.byName[f.Column] = f
		lower := strings.ToLower(f.Column)
		if _, dup := m.byLower[lower]; !dup {
			m.byLower[lower] = f
		}
	}
	return m, nil
}

func dominant(cs []candidate) (*Field, bool) {
	if len(cs) == 1 {
		return cs[0].f, true
	}
	if len(cs) == 1 {
		return cs[0].f, true
	}
	minDepth := cs[0].depth
	for _, c := range cs[1:] {
		minDepth = min(minDepth, c.depth)
	}
	var best []candidate
	for _, c := range cs {
		if c.depth == minDepth {
			best = append(best, c)
		}
	}
	if len(best) == 1 {
		return best[0].f, true
	}
	var tagged []candidate
	for _, c := range best {
		if c.tagged {
			tagged = append(tagged, c)
		}
	}
	if len(tagged) == 1 {
		return tagged[0].f, true
	}
	return nil, false
}

func walk(t reflect.Type, index []int, prefix string, depth int, out *[]candidate, relOut *[]*Relation, visiting map[reflect.Type]bool) error {
	if visiting[t] {
		return nil // 递归嵌入，停止
	}
	visiting[t] = true
	defer delete(visiting, t)

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag, hasTag := sf.Tag.Lookup(TagName)
		if tag == "-" {
			continue
		}
		name, opts := parseTag(tag)
		if rel, isRel := opts["rel"]; isRel {
			if !sf.IsExported() {
				continue
			}
			idx := append(append([]int(nil), index...), i)
			r, err := newRelation(sf, idx, rel, opts)
			if err != nil {
				return err
			}
			*relOut = append(*relOut, r)
			continue // 关联字段不是列
		}
		idx := append(append([]int(nil), index...), i)

		ft := sf.Type
		if sf.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && !IsScalar(et) {
				if !sf.IsExported() && ft.Kind() == reflect.Pointer {
					continue // 无法为未导出的嵌入指针分配内存
				}
				if err := walk(et, idx, prefix+sf.Name+".", depth+1, out, relOut, visiting); err != nil {
					return err
				}
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		explicit := hasTag && name != ""
		if name == "" {
			name = SnakeCase(sf.Name)
		}
		*out = append(*out, candidate{
			f: &Field{
				Column:  name,
				GoName:  prefix + sf.Name,
				Index:   idx,
				Type:    ft,
				Options: opts,
			},
			depth:  depth,
			tagged: explicit,
		})
	}
	return nil
}

// Tabler 由模型实现，用于自定义表名。
type Tabler interface{ TableName() string }

var tablerType = reflect.TypeFor[Tabler]()

func tableName(t reflect.Type) string {
	if t.Implements(tablerType) {
		return reflect.Zero(t).Interface().(Tabler).TableName()
	}
	if reflect.PointerTo(t).Implements(tablerType) {
		return reflect.New(t).Interface().(Tabler).TableName()
	}
	return Plural(SnakeCase(t.Name()))
}

// Plural 返回英文单词的简单复数形式：user -> users，category -> categories，box -> boxes。
func Plural(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"),
		strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	default:
		return s + "s"
	}
}

// parseTag 解析 `name,opt1,opt2:value`。
func parseTag(tag string) (string, map[string]string) {
	if tag == "" {
		return "", nil
	}
	parts := strings.Split(tag, ",")
	name := strings.TrimSpace(parts[0])
	optParts := parts[1:]
	if strings.Contains(name, ":") { // 首段即是选项，如 `xdb:"rel:has_many,fk:user_id"`
		name, optParts = "", parts
	}
	var opts map[string]string
	for _, p := range optParts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if opts == nil {
			opts = map[string]string{}
		}
		k, v, _ := strings.Cut(p, ":")
		opts[k] = v
	}
	return name, opts
}

// newRelation 由字段与标签构造关联定义。
// kind 来自 rel:has_many / rel:has_one / rel:belongs_to，fk 为外键列名（必填）。
func newRelation(sf reflect.StructField, idx []int, kind string, opts map[string]string) (*Relation, error) {
	fk := opts["fk"]
	if fk == "" {
		return nil, fmt.Errorf("goxdb: %s.%s has rel:%s but no fk: option", sf.Type, sf.Name, kind)
	}
	var k RelationKind
	switch RelationKind(kind) {
	case HasMany, HasOne, BelongsTo:
		k = RelationKind(kind)
	default:
		return nil, fmt.Errorf("goxdb: %s.%s has unknown relation kind %q (want has_many, has_one or belongs_to)", sf.Type, sf.Name, kind)
	}
	r := &Relation{GoName: sf.Name, Index: idx, Kind: k, FK: fk, Type: sf.Type, Target: sf.Type}
	switch {
	case k == HasMany:
		if sf.Type.Kind() != reflect.Slice {
			return nil, fmt.Errorf("goxdb: %s.%s is rel:has_many but is %s, want a slice", sf.Type, sf.Name, sf.Type)
		}
		r.Target = sf.Type.Elem()
	case k == HasOne, k == BelongsTo:
		if sf.Type.Kind() != reflect.Pointer {
			return nil, fmt.Errorf("goxdb: %s.%s is rel:%s but is %s, want a pointer", sf.Type, sf.Name, kind, sf.Type)
		}
		r.Target = sf.Type.Elem()
	}
	if r.Target.Kind() == reflect.Pointer {
		r.Target = r.Target.Elem()
	}
	if r.Target.Kind() != reflect.Struct {
		return nil, fmt.Errorf("goxdb: %s.%s relation target must be a struct, got %s", sf.Type, sf.Name, r.Target)
	}
	return r, nil
}

// checkAutoTime 校验自动时间戳字段可被写入：必须是 time.Time，
// 否则框架无法在写入前赋值。
func checkAutoTime(t reflect.Type, f *Field, opt string) error {
	ft := f.Type
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	if ft != timeType {
		return fmt.Errorf("goxdb: %s.%s has %s but is %s, want time.Time or *time.Time", t, f.GoName, opt, f.Type)
	}
	return nil
}

// SnakeCase 把 Go 标识符转为 snake_case，并保持缩写词完整：
// ID -> id，UserID -> user_id，HTTPServer -> http_server。
func SnakeCase(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := rs[i-1]
				nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
