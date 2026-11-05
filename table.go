package xdb

import (
	"reflect"

	"github.com/qiudaozhang/goxdb/internal/schema"
)

// Source 是可以出现在 FROM / JOIN 中的数据源：表、派生表、CTE。
type Source interface {
	// qualifier 返回用于限定列名的名字（别名优先）。
	qualifier() string
	buildFrom(b *builder)
	// sourceModel 返回数据源对应的模型元数据；派生表与手写表名为 nil。
	// 通过内嵌 Table 的自定义表结构（代码生成产物）会自动转发此方法。
	sourceModel() *schema.Model
}

// Table 是一张表（可带别名）。值类型，As 返回新值，不影响原表。
type Table struct {
	name  string
	alias string
	model *schema.Model // TableOf 构造时记录，用于推导软删除等模型级行为
}

// T 按表名构造表。
func T(name string) Table { return Table{name: name} }

// TableOf 返回模型 M 对应的表：M 实现 TableName() 时用其返回值，
// 否则为类型名的 snake_case 复数（User -> users）。M 不是结构体时 panic。
func TableOf[M any]() Table {
	m, err := schema.Of(reflect.TypeFor[M]())
	if err != nil {
		panic(err)
	}
	return Table{name: m.Table, model: m}
}

// As 返回带别名的表，用于自连接或简化书写。
func (t Table) As(alias string) Table { t.alias = alias; return t }

// Name 返回表名。
func (t Table) Name() string { return t.name }

// Alias 返回别名，未设置时为空。
func (t Table) Alias() string { return t.alias }

// Star 返回该表的全部列：表名.*。
func (t Table) Star() Node { return starNode{t.qualifier()} }

func (t Table) sourceModel() *schema.Model { return t.model }

func (t Table) qualifier() string {
	if t.alias != "" {
		return t.alias
	}
	return t.name
}

func (t Table) buildFrom(b *builder) {
	b.ident(t.name)
	if t.alias != "" {
		b.write(" AS ")
		b.ident(t.alias)
	}
}

type starNode struct{ qual string }

func (n starNode) build(b *builder) {
	if n.qual != "" {
		b.ident(n.qual)
		b.write(".")
	}
	b.write("*")
}

// Star 表示 SELECT *。
func Star() Node { return starNode{} }

// DerivedTable 是 FROM 中的子查询：(SELECT ...) AS alias。
type DerivedTable struct {
	q     Node
	alias string
}

// Derived 构造派生表。列可通过 xdb.C[T](derived, "col") 引用。
func Derived(alias string, q Node) DerivedTable { return DerivedTable{q: q, alias: alias} }

func (d DerivedTable) qualifier() string { return d.alias }

func (d DerivedTable) sourceModel() *schema.Model { return nil }

func (d DerivedTable) buildFrom(b *builder) {
	b.node(d.q)
	b.write(" AS ")
	b.ident(d.alias)
}

// Star 返回派生表的全部列。
func (d DerivedTable) Star() Node { return starNode{d.alias} }

// CTETable 是 WITH 子句中定义的公用表表达式，可作为数据源使用。
type CTETable struct {
	name string
	cols []string
	q    Node
}

// CTE 定义公用表表达式 name AS (q)。在 Q.With / Q.WithRecursive 中声明后，
// 即可在 FROM / JOIN / 子查询中把它当作表使用。
// 递归 CTE 的查询体中用 xdb.T(name) 引用自身。
func CTE(name string, q Node) CTETable { return CTETable{name: name, q: q} }

// Columns 指定 CTE 的列名列表：name (c1, c2) AS (...)。
func (c CTETable) Columns(cols ...string) CTETable { c.cols = cols; return c }

func (c CTETable) qualifier() string          { return c.name }
func (c CTETable) sourceModel() *schema.Model { return nil }
func (c CTETable) buildFrom(b *builder)       { b.ident(c.name) }
func (c CTETable) Star() Node                 { return starNode{c.name} }
func (c CTETable) buildDef(b *builder) {
	b.ident(c.name)
	if len(c.cols) > 0 {
		b.write(" (")
		for i, col := range c.cols {
			if i > 0 {
				b.write(", ")
			}
			b.ident(col)
		}
		b.write(")")
	}
	b.write(" AS ")
	b.node(c.q)
}
