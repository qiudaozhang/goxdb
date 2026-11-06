package pg_test

import (
	"strings"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/pg"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
)

type Doc struct {
	ID   int64  `xdb:"id"`
	Meta string `xdb:"meta"`
	Tags string `xdb:"tags"`
}

func docCols() (xdb.Table, xdb.StringCol, xdb.StringCol) {
	t := xdb.TableOf[Doc]()
	return t, xdb.CStr(t, "meta"), xdb.CStr(t, "tags")
}

// 方言扩展包只针对 PostgreSQL：这里直接断言 PG 方言的渲染结果。
func TestPGJSONB(t *testing.T) {
	_, meta, tags := docCols()
	_ = tags
	q := xdb.Query[Doc](nil).Where(
		pg.JSONB(meta).Text("status").Eq("active"),
		pg.JSONB(meta).Field("a").Field("b").Text("c").Eq("x"),
	)
	got, args, err := q.SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "docs"."id", "docs"."meta", "docs"."tags" FROM "docs" WHERE ("docs"."meta" ->> 'status' = $1 AND "docs"."meta" -> 'a' -> 'b' ->> 'c' = $2)`
	if got != want {
		t.Fatalf("sql:\n got %s\nwant %s", got, want)
	}
	if len(args) != 2 || args[0] != "active" || args[1] != "x" {
		t.Fatalf("args = %#v", args)
	}
}

// HasKey 用 jsonb_exists 避免与占位符 ? 冲突。
func TestPGJSONBHasKeyAndContains(t *testing.T) {
	_, meta, _ := docCols()
	got, _, err := xdb.Query[Doc](nil).Where(
		pg.JSONB(meta).HasKey("k"),
		pg.JSONB(meta).Contains(`{"a":1}`),
	).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "docs"."id", "docs"."meta", "docs"."tags" FROM "docs" WHERE (jsonb_exists("docs"."meta", 'k') AND "docs"."meta" @> '{"a":1}'::jsonb)`
	if got != want {
		t.Fatalf("sql:\n got %s\nwant %s", got, want)
	}
}

func TestPGArray(t *testing.T) {
	_, _, tags := docCols()
	got, args, err := xdb.Query[Doc](nil).Where(
		pg.Array(tags).Any(xdb.V("red")),
		pg.Array(tags).Contains(xdb.V("a"), xdb.V("b")),
		pg.Array(tags).Overlaps(xdb.V("c")),
		pg.Array(tags).Len().Gt(2),
	).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "docs"."id", "docs"."meta", "docs"."tags" FROM "docs" WHERE ($1 = ANY("docs"."tags") AND "docs"."tags" @> ARRAY[$2, $3] AND "docs"."tags" && ARRAY[$4] AND array_length("docs"."tags", 1) > $5)`
	if got != want {
		t.Fatalf("sql:\n got %s\nwant %s", got, want)
	}
	if len(args) != 5 {
		t.Fatalf("args = %#v", args)
	}
}

// 键名中的单引号必须被转义，否则会破坏 SQL 字面量。
func TestPGQuoteEscaping(t *testing.T) {
	_, meta, _ := docCols()
	got, _, err := xdb.Query[Doc](nil).Where(pg.JSONB(meta).Text("it's").Eq("v")).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, `->> 'it''s'`) {
		t.Fatalf("single quote must be doubled: %s", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// 空元素列表不能渲染成 ARRAY[]（PostgreSQL 无法推断类型），改用无类型的 '{}'。
func TestPGArrayEmpty(t *testing.T) {
	_, _, tags := docCols()
	got, args, err := xdb.Query[Doc](nil).Where(
		pg.Array(tags).Contains(),
		pg.Array(tags).Overlaps(),
	).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "docs"."id", "docs"."meta", "docs"."tags" FROM "docs" WHERE ("docs"."tags" @> '{}' AND "docs"."tags" && '{}')`
	if got != want || len(args) != 0 {
		t.Fatalf("sql:\n got %s %#v\nwant %s", got, args, want)
	}
}
