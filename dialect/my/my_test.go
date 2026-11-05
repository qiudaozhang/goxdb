package my_test

import (
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

func TestMyPathEscaping(t *testing.T) {
	tb := xdb.TableOf[Doc]()
	meta := xdb.CStr(tb, "meta")
	got, _, err := xdb.Query[Doc](nil).Where(my.JSON(meta).Text(`a"b`).Eq("v")).SQL(mysql.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `$.a\"b`) {
		t.Fatalf("path must escape double quote: %s", got)
	}
}
