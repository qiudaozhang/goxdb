package xdb_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// 软删除模型：deleted_at 带 softDelete 标签。
type Post struct {
	ID        int64      `xdb:"id,pk,auto"`
	Title     string     `xdb:"title"`
	DeletedAt *time.Time `xdb:"deleted_at,softDelete"`
}

type postTable struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	Title     xdb.StringCol
	DeletedAt xdb.Col[*time.Time]
}

func newPostTable(t xdb.Table) postTable {
	return postTable{
		t,
		xdb.CNum[int64](t, "id"),
		xdb.CStr(t, "title"),
		xdb.C[*time.Time](t, "deleted_at"),
	}
}

var P = newPostTable(xdb.TableOf[Post]())

// 查询默认只返回未删除的行。
func TestSoftDeleteQuery(t *testing.T) {
	golden(t, xdb.Query[Post](nil),
		"SELECT `posts`.`id`, `posts`.`title`, `posts`.`deleted_at` FROM `posts` WHERE `posts`.`deleted_at` IS NULL",
		`SELECT "posts"."id", "posts"."title", "posts"."deleted_at" FROM "posts" WHERE "posts"."deleted_at" IS NULL`)

	// 用户条件与软删除条件以 AND 组合
	golden(t, xdb.Query[Post](nil).Where(P.Title.Eq("x")),
		"SELECT `posts`.`id`, `posts`.`title`, `posts`.`deleted_at` FROM `posts` WHERE (`posts`.`deleted_at` IS NULL AND `posts`.`title` = ?)",
		`SELECT "posts"."id", "posts"."title", "posts"."deleted_at" FROM "posts" WHERE ("posts"."deleted_at" IS NULL AND "posts"."title" = $1)`,
		"x")

	// Unscoped 关闭过滤
	golden(t, xdb.Query[Post](nil).Unscoped(),
		"SELECT `posts`.`id`, `posts`.`title`, `posts`.`deleted_at` FROM `posts`",
		`SELECT "posts"."id", "posts"."title", "posts"."deleted_at" FROM "posts"`)
}

// Delete 在软删除模型上渲染为 UPDATE，而非物理删除；已删除的行不再被重复标记。
func TestSoftDeleteDelete(t *testing.T) {
	golden(t, xdb.Delete[Post](nil).Where(P.ID.Eq(1)),
		"UPDATE `posts` SET `deleted_at` = CURRENT_TIMESTAMP WHERE (`posts`.`deleted_at` IS NULL AND `posts`.`id` = ?)",
		`UPDATE "posts" SET "deleted_at" = CURRENT_TIMESTAMP WHERE ("posts"."deleted_at" IS NULL AND "posts"."id" = $1)`,
		int64(1))

	// Unscoped 才会真正物理删除
	golden(t, xdb.Delete[Post](nil).Unscoped().Where(P.ID.Eq(1)),
		"DELETE FROM `posts` WHERE `posts`.`id` = ?",
		`DELETE FROM "posts" WHERE "posts"."id" = $1`, int64(1))

	// WHERE 护栏仍然生效
	if err := renderErr(xdb.Delete[Post](nil)); err == nil {
		t.Fatal("soft delete without WHERE must still fail")
	}
}

// 计数 / 存在性判断也要带上软删除过滤。
func TestSoftDeleteSubquery(t *testing.T) {
	// 子查询（InSub）同样继承软删除条件
	sub := xdb.Select[int64](nil, P.ID).From(P)
	golden(t, xdb.Select[Post](nil, P.ID).From(P).Where(P.ID.InSub(sub)),
		"SELECT `posts`.`id` FROM `posts` WHERE (`posts`.`deleted_at` IS NULL AND `posts`.`id` IN (SELECT `posts`.`id` FROM `posts` WHERE `posts`.`deleted_at` IS NULL))",
		`SELECT "posts"."id" FROM "posts" WHERE ("posts"."deleted_at" IS NULL AND "posts"."id" IN (SELECT "posts"."id" FROM "posts" WHERE "posts"."deleted_at" IS NULL))`)
}

// 没有 softDelete 标签的模型不应受影响。
func TestSoftDeleteNotAppliedWithoutTag(t *testing.T) {
	golden(t, xdb.Query[Customer](nil).Columns(U.ID),
		"SELECT `customers`.`id` FROM `customers`",
		`SELECT "customers"."id" FROM "customers"`)
}

// UNION 右侧的查询同样要带软删除过滤；右侧 Unscoped 时不带。
func TestSoftDeleteUnion(t *testing.T) {
	golden(t, xdb.Select[int64](nil, P.ID).From(P).Union(xdb.Select[int64](nil, P.ID).From(P)),
		"SELECT `posts`.`id` FROM `posts` WHERE `posts`.`deleted_at` IS NULL UNION SELECT `posts`.`id` FROM `posts` WHERE `posts`.`deleted_at` IS NULL",
		`SELECT "posts"."id" FROM "posts" WHERE "posts"."deleted_at" IS NULL UNION SELECT "posts"."id" FROM "posts" WHERE "posts"."deleted_at" IS NULL`)

	golden(t, xdb.Select[int64](nil, P.ID).From(P).Union(xdb.Select[int64](nil, P.ID).From(P).Unscoped()),
		"SELECT `posts`.`id` FROM `posts` WHERE `posts`.`deleted_at` IS NULL UNION SELECT `posts`.`id` FROM `posts`",
		`SELECT "posts"."id" FROM "posts" WHERE "posts"."deleted_at" IS NULL UNION SELECT "posts"."id" FROM "posts"`)
}

// Count / Pluck 内部会重建查询，必须保留 Unscoped。
func TestSoftDeleteUnscopedCountPluck(t *testing.T) {
	db, f := newDB(t, func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"c"}, Rows: [][]any{{int64(1)}}}, nil
	})
	if _, err := xdb.Query[Post](db).Unscoped().Count(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Pluck(ctx, xdb.Query[Post](db).Unscoped(), P.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`SELECT COUNT(*) FROM "posts"`,
		`SELECT "posts"."id" FROM "posts"`,
	}
	if got := f.Log(); !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// 未 Unscoped 时仍然过滤
	f.Reset()
	if _, err := xdb.Query[Post](db).Count(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.Log(); len(got) != 1 || !strings.Contains(got[0], `"deleted_at" IS NULL`) {
		t.Fatalf("Count without Unscoped must filter: %q", got)
	}
}

// 软删除模型的 Delete().Join(...) 渲染为多表 UPDATE，不能丢掉连接。
func TestSoftDeleteDeleteJoin(t *testing.T) {
	golden(t, xdb.Delete[Post](nil).Join(U, U.ID.EqCol(P.ID)).Where(U.Age.Gt(18)),
		"UPDATE `posts` JOIN `customers` ON `customers`.`id` = `posts`.`id` SET `posts`.`deleted_at` = CURRENT_TIMESTAMP WHERE (`posts`.`deleted_at` IS NULL AND `customers`.`age` > ?)",
		`UPDATE "posts" SET "deleted_at" = CURRENT_TIMESTAMP FROM "customers" WHERE ("customers"."id" = "posts"."id" AND "posts"."deleted_at" IS NULL AND "customers"."age" > $1)`,
		18)
}
