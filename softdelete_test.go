package xdb_test

import (
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
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

// Delete 在软删除模型上渲染为 UPDATE，而非物理删除。
func TestSoftDeleteDelete(t *testing.T) {
	golden(t, xdb.Delete[Post](nil).Where(P.ID.Eq(1)),
		"UPDATE `posts` SET `deleted_at` = CURRENT_TIMESTAMP WHERE `posts`.`id` = ?",
		`UPDATE "posts" SET "deleted_at" = CURRENT_TIMESTAMP WHERE "posts"."id" = $1`,
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
