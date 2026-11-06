package my_test

import (
	"reflect"
	"strings"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/my"
	"github.com/qiudaozhang/goxdb/dialect/mysql"
)

type Doc struct {
	ID   int64  `xdb:"id"`
	Meta string `xdb:"meta"`
	Body string `xdb:"body"`
}

func TestMyJSON(t *testing.T) {
	tb := xdb.TableOf[Doc]()
	meta := xdb.CStr(tb, "meta")
	got, args, err := xdb.Query[Doc](nil).Where(
		my.JSON(meta).Text("status").Eq("active"),
		my.JSON(meta).Field("a").Index(0).Text("name").Eq("x"),
		my.JSON(meta).HasKey("k"),
		my.JSON(meta).Contains(`{"a":1}`),
	).SQL(mysql.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT `docs`.`id`, `docs`.`meta`, `docs`.`body` FROM `docs` WHERE (" +
		"JSON_UNQUOTE(JSON_EXTRACT(`docs`.`meta`, '$.status')) = ?" +
		" AND JSON_UNQUOTE(JSON_EXTRACT(`docs`.`meta`, '$.a[0].name')) = ?" +
		" AND JSON_CONTAINS_PATH(`docs`.`meta`, 'one', '$.k')" +
		" AND JSON_CONTAINS(`docs`.`meta`, '{\"a\":1}', '$'))"
	if got != want {
		t.Fatalf("sql:\n got %s\nwant %s", got, want)
	}
	if len(args) != 2 {
		t.Fatalf("args = %#v", args)
	}
}

func TestMyMatch(t *testing.T) {
	tb := xdb.TableOf[Doc]()
	body := xdb.CStr(tb, "body")
	got, args, err := xdb.Query[Doc](nil).Where(
		my.Match(body, "hello", "IN BOOLEAN MODE"),
	).SQL(mysql.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "MATCH(`docs`.`body`) AGAINST (? IN BOOLEAN MODE)") {
		t.Fatalf("sql: %s", got)
	}
	if len(args) != 1 || args[0] != "hello" {
		t.Fatalf("args = %#v", args)
	}
}

// 键名：标识符原样；其它加双引号（内部 " 与 \ 转义）；含反斜杠的常量改为绑定参数，
// 以免受 NO_BACKSLASH_ESCAPES 影响。真实 MySQL 上的行为见集成测试 TestBuilderDialectPackages。
func TestMyPathEscaping(t *testing.T) {
	tb := xdb.TableOf[Doc]()
	meta := xdb.CStr(tb, "meta")
	for _, c := range []struct {
		key, frag string
		args      []any
	}{
		{"status", "JSON_EXTRACT(`docs`.`meta`, '$.status')", []any{"v"}},
		{"first-name", "JSON_EXTRACT(`docs`.`meta`, '$.\"first-name\"')", []any{"v"}},
		{"it's", "JSON_EXTRACT(`docs`.`meta`, '$.\"it''s\"')", []any{"v"}},
		{"9lives", "JSON_EXTRACT(`docs`.`meta`, '$.\"9lives\"')", []any{"v"}},
		{`a"b`, "JSON_EXTRACT(`docs`.`meta`, ?)", []any{`$."a\"b"`, "v"}},
		{`a\b`, "JSON_EXTRACT(`docs`.`meta`, ?)", []any{`$."a\\b"`, "v"}},
	} {
		got, args, err := xdb.Query[Doc](nil).Where(my.JSON(meta).Text(c.key).Eq("v")).SQL(mysql.Dialect)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, c.frag) || !reflect.DeepEqual(args, c.args) {
			t.Errorf("key %q:\n got: %s %#v\nwant: ...%s... %#v", c.key, got, args, c.frag, c.args)
		}
	}

	got, args, err := xdb.Query[Doc](nil).Where(my.JSON(meta).Field("a").Contains(`{"p":"C:\\d"}`)).SQL(mysql.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "JSON_CONTAINS(`docs`.`meta`, ?, '$.a')") || !reflect.DeepEqual(args, []any{`{"p":"C:\\d"}`}) {
		t.Errorf("contains: %s %#v", got, args)
	}
}
