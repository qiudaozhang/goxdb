package schema

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"ID": "id", "UserID": "user_id", "HTTPServer": "http_server",
		"CreatedAt": "created_at", "Name": "name", "OAuth2Token": "o_auth2_token", "A1B": "a1_b",
	} {
		if got := SnakeCase(in); got != want {
			t.Errorf("SnakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

type Base struct {
	ID        int64 `xdb:"id,pk,auto"`
	CreatedAt time.Time
}

type Audit struct {
	CreatedAt time.Time `xdb:"created_at"` // 与 Base.CreatedAt 同层，显式标签名胜出
	Operator  string
}

type Order struct {
	ID     int64 `xdb:"id,pk,auto"`
	UserID int64 `xdb:"user_id"`
}

type Profile struct {
	ID     int64  `xdb:"id,pk,auto"`
	UserID int64  `xdb:"user_id"`
	Bio    string `xdb:"bio"`
}

type Team struct {
	ID   int64  `xdb:"id,pk,auto"`
	Name string `xdb:"name"`
}

type User struct {
	Base
	*Audit
	Name    string   `xdb:"name"`
	Secret  string   `xdb:"-"`
	Orders  []Order  `xdb:"rel:has_many,fk:user_id"`
	Profile *Profile `xdb:"rel:has_one,fk:user_id"`
	Team    *Team    `xdb:"rel:belongs_to,fk:team_id"`
	private int
}

func TestModel(t *testing.T) {
	m, err := Of(reflect.TypeFor[*User]())
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, f := range m.Fields {
		cols = append(cols, f.Column)
	}
	want := []string{"id", "created_at", "operator", "name"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("columns = %v, want %v", cols, want)
	}
	f, _ := m.Lookup("created_at")
	if !reflect.DeepEqual(f.Index, []int{1, 0}) {
		t.Fatalf("created_at should come from Audit (tagged), index %v", f.Index)
	}
	id, _ := m.Lookup("ID")
	if !id.Has("pk") || !id.Has("auto") {
		t.Fatal("id options lost")
	}
	if op, _ := m.Lookup("operator"); !reflect.DeepEqual(op.Index, []int{1, 1}) {
		t.Fatalf("operator index %v", op.Index)
	}
	for _, c := range []struct {
		name string
		kind RelationKind
		fk   string
	}{
		{"Orders", HasMany, "user_id"},
		{"Profile", HasOne, "user_id"},
		{"Team", BelongsTo, "team_id"},
	} {
		r, ok := m.Relations[c.name]
		if !ok {
			t.Fatalf("relation %s not parsed", c.name)
		}
		if r.Kind != c.kind || r.FK != c.fk {
			t.Fatalf("relation %s = %v/%s, want %v/%s", c.name, r.Kind, r.FK, c.kind, c.fk)
		}
	}
}

type A struct{ X int }
type B struct{ X int }
type Ambiguous struct {
	A
	B
	Y int
}

func TestAmbiguousDropped(t *testing.T) {
	m, err := Of(reflect.TypeFor[Ambiguous]())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup("x"); ok {
		t.Fatal("ambiguous field x should be dropped")
	}
}

func TestIsScalar(t *testing.T) {
	if !IsScalar(reflect.TypeFor[time.Time]()) || !IsScalar(reflect.TypeFor[*int]()) || IsScalar(reflect.TypeFor[User]()) {
		t.Fatal("IsScalar wrong")
	}
}

type OrderItem struct{ ID int64 }
type Category struct{ ID int64 }
type custom struct{ ID int64 }

func (*custom) TableName() string { return "t_custom" }

func TestTableName(t *testing.T) {
	for typ, want := range map[reflect.Type]string{
		reflect.TypeFor[OrderItem](): "order_items",
		reflect.TypeFor[Category]():  "categories",
		reflect.TypeFor[custom]():    "t_custom",
		reflect.TypeFor[User]():      "users",
	} {
		m, err := Of(typ)
		if err != nil || m.Table != want {
			t.Errorf("%s: table %q, want %q (%v)", typ, m.Table, want, err)
		}
	}
	m, _ := Of(reflect.TypeFor[User]())
	if len(m.PK) != 1 || m.Auto == nil || m.Auto.Column != "id" {
		t.Fatal("pk/auto not detected")
	}
}

// 与 internal/codegen 的 TestEmbeddedFields 使用同一组模型：两边选出的列必须一致，
// 否则生成的扫描器与运行时列集对不上。
type ebBase struct {
	ID        int64     `xdb:"id,pk,auto"`
	CreatedAt time.Time `xdb:"created_at"`
	Note      string    `xdb:"base_note"`
	Name      string    `xdb:"base_name"`
}
type EbAudit struct {
	CreatedAt time.Time `xdb:"created_at"`
	By        string    `xdb:"by"`
}
type ebTagged struct {
	By string `xdb:"by"`
}
type ebUser struct {
	ebBase
	*EbAudit
	ebTagged
	Name string `xdb:"name"`
}

func TestEmbeddedConflictsMatchCodegen(t *testing.T) {
	m, err := Of(reflect.TypeFor[ebUser]())
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, f := range m.Fields {
		cols = append(cols, f.Column)
	}
	if got, want := strings.Join(cols, ","), "id,base_note,base_name,name"; got != want {
		t.Fatalf("columns = %s, want %s", got, want)
	}
	// *EbAudit 的列全部因冲突被丢弃，不应出现在 EmbedPtrs 中。
	if len(m.EmbedPtrs) != 0 {
		t.Fatalf("EmbedPtrs = %v, want none", m.EmbedPtrs)
	}
}

type EbInner struct {
	X int `xdb:"x"`
}
type EbMid struct {
	*EbInner
	Y int `xdb:"y"`
}
type ebOuter struct {
	*EbMid
	Z int `xdb:"z"`
}

// 多层嵌入指针：路径外层在前，AllocEmbedded 能逐层分配。
func TestEmbedPtrsNested(t *testing.T) {
	m, err := Of(reflect.TypeFor[ebOuter]())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(m.EmbedPtrs); got != "[[0] [0 0]]" {
		t.Fatalf("EmbedPtrs = %s", got)
	}
	var o ebOuter
	AllocEmbedded(reflect.ValueOf(&o).Elem(), m.EmbedPtrs)
	if o.EbMid == nil || o.EbInner == nil {
		t.Fatal("embedded pointers not allocated")
	}
	o.X = 1 // 不应 panic
}
