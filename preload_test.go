package xdb_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

type plAuthor struct {
	ID   int64  `xdb:"id,pk"`
	Name string `xdb:"name"`
}

func (plAuthor) TableName() string { return "authors" }

// 可空外键：*int64 与 sql.NullInt64 两种写法。
type plBook struct {
	ID       int64     `xdb:"id,pk"`
	AuthorID *int64    `xdb:"author_id"`
	Author   *plAuthor `xdb:"rel:belongs_to,fk:author_id"`
}

func (plBook) TableName() string { return "books" }

type plNullBook struct {
	ID       int64         `xdb:"id,pk"`
	AuthorID sql.NullInt64 `xdb:"author_id"`
	Author   *plAuthor     `xdb:"rel:belongs_to,fk:author_id"`
}

func (plNullBook) TableName() string { return "books" }

func preloadDB(t *testing.T) (*xdb.DB, *fakedb.DB) {
	return newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if strings.Contains(q, `FROM "authors"`) {
			return &fakedb.Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(7), "ann"}}}, nil
		}
		return &fakedb.Result{Columns: []string{"id", "author_id"}, Rows: [][]any{
			{int64(1), int64(7)}, {int64(2), int64(7)}, {int64(3), nil},
		}}, nil
	})
}

// 外键为指针时按解引用后的值匹配，并对相同的键去重；NULL 外键不参与。
func TestPreloadPointerFK(t *testing.T) {
	db, f := preloadDB(t)
	books, err := xdb.Query[plBook](db).Preload("Author").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPreloaded(t, f, len(books), func(i int) *plAuthor { return books[i].Author })
}

func TestPreloadValuerFK(t *testing.T) {
	db, f := preloadDB(t)
	books, err := xdb.Query[plNullBook](db).Preload("Author").All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkPreloaded(t, f, len(books), func(i int) *plAuthor { return books[i].Author })
}

func checkPreloaded(t *testing.T, f *fakedb.DB, n int, author func(int) *plAuthor) {
	t.Helper()
	if n != 3 {
		t.Fatalf("got %d books", n)
	}
	for i := range 2 {
		if a := author(i); a == nil || a.Name != "ann" {
			t.Errorf("book #%d author = %v, want ann", i, a)
		}
	}
	if a := author(2); a != nil {
		t.Errorf("book with NULL fk got author %v", a)
	}
	log := f.Log()
	if len(log) != 2 || !strings.HasSuffix(log[1], `WHERE "authors"."id" IN ($1)`) {
		t.Errorf("related query should dedupe keys: %q", log)
	}
}

type plComment struct {
	ID        int64      `xdb:"id,pk"`
	PostID    int64      `xdb:"post_id"`
	DeletedAt *time.Time `xdb:"deleted_at,softDelete"`
}

func (plComment) TableName() string { return "comments" }

type plPost struct {
	ID       int64       `xdb:"id,pk"`
	Comments []plComment `xdb:"rel:has_many,fk:post_id"`
}

func (plPost) TableName() string { return "plposts" }

// 关联模型带软删除列时，预加载只取未删除的行；主查询 Unscoped 时不过滤。
func TestPreloadSoftDeletedChildren(t *testing.T) {
	db, f := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if strings.Contains(q, `FROM "comments"`) {
			return &fakedb.Result{Columns: []string{"id", "post_id", "deleted_at"}}, nil
		}
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}, nil
	})
	if _, err := xdb.Query[plPost](db).Preload("Comments").All(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Query[plPost](db).Unscoped().Preload("Comments").All(ctx); err != nil {
		t.Fatal(err)
	}
	log := f.Log()
	want := []string{
		`SELECT "id", "post_id", "deleted_at" FROM "comments" WHERE ("comments"."post_id" IN ($1) AND "comments"."deleted_at" IS NULL)`,
		`SELECT "id", "post_id", "deleted_at" FROM "comments" WHERE "comments"."post_id" IN ($1)`,
	}
	if len(log) != 4 || log[1] != want[0] || log[3] != want[1] {
		t.Fatalf("got %q", log)
	}
}
