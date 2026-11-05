package schema

import (
	"reflect"
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

type User struct {
	Base
	*Audit
	Name    string `xdb:"name"`
	Secret  string `xdb:"-"`
	Orders  []int  `xdb:"rel:has_many,fk:user_id"`
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
