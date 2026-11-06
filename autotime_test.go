package xdb_test

import (
	"strings"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

type Article struct {
	ID        int64      `xdb:"id,pk,auto"`
	Title     string     `xdb:"title"`
	CreatedAt time.Time  `xdb:"created_at,autoCreateTime"`
	UpdatedAt time.Time  `xdb:"updated_at,autoUpdateTime"`
	DeletedAt *time.Time `xdb:"deleted_at"`
}

type articleTable struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	Title     xdb.StringCol
	UpdatedAt xdb.TimeCol
}

var A = articleTable{
	xdb.TableOf[Article](),
	xdb.CNum[int64](xdb.TableOf[Article](), "id"),
	xdb.CStr(xdb.TableOf[Article](), "title"),
	xdb.CTime(xdb.TableOf[Article](), "updated_at"),
}

var fixedNow = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func clockedDB(t *testing.T) (*xdb.DB, *[]string, *[][]any) {
	t.Helper()
	var stmts []string
	var args [][]any
	db, _ := newDB(t, func(q string, a []any) (*fakedb.Result, error) {
		stmts = append(stmts, q)
		args = append(args, a)
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}, nil
	}, xdb.WithClock(func() time.Time { return fixedNow }))
	return db, &stmts, &args
}

func TestAutoTimeInsert(t *testing.T) {
	db, stmts, _ := clockedDB(t)
	a := &Article{Title: "hello"}
	if _, err := xdb.Insert(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.CreatedAt.Equal(fixedNow) || !a.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("model not stamped: %+v", a)
	}
	if a.DeletedAt != nil {
		t.Fatalf("non-auto column must stay nil: %v", a.DeletedAt)
	}
	if got, want := (*stmts)[0], `INSERT INTO "articles" ("title", "created_at", "updated_at", "deleted_at") VALUES ($1, $2, $3, $4) RETURNING "id"`; got != want {
		t.Fatalf("sql:\n got %s\nwant %s", got, want)
	}
}

func TestAutoTimeInsertKeepsExplicitValue(t *testing.T) {
	db, _, _ := clockedDB(t)
	explicit := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	a := &Article{Title: "x", CreatedAt: explicit}
	if _, err := xdb.Insert(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.CreatedAt.Equal(explicit) {
		t.Fatalf("explicit value overwritten: %v", a.CreatedAt)
	}
}

func TestAutoTimeUpdate(t *testing.T) {
	db, stmts, _ := clockedDB(t)
	a := &Article{ID: 3, Title: "x"}
	if _, err := xdb.UpdateModel(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("update not stamped: %v", a.UpdatedAt)
	}
	sql := (*stmts)[0]
	if strings.Count(sql, `"updated_at" = $`) != 1 {
		t.Fatalf("updated_at must appear exactly once: %s", sql)
	}
	if !strings.Contains(sql, `WHERE "articles"."id" = $`) {
		t.Fatalf("missing pk where: %s", sql)
	}
}

// UpdateModel 的 updated_at 是加载时的快照，总被刷新为当前时间；
// 需要写入特定时间时用 Update[T]().Set() 显式指定。
func TestAutoTimeUpdateModelAlwaysRefreshes(t *testing.T) {
	db, _, args := clockedDB(t)
	a := &Article{ID: 3, Title: "x", UpdatedAt: time.Date(2019, 5, 5, 0, 0, 0, 0, time.UTC)}
	if _, err := xdb.UpdateModel(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.UpdatedAt.Equal(fixedNow) {
		t.Fatalf("UpdateModel must refresh updated_at: %v", a.UpdatedAt)
	}
	if got, ok := (*args)[0][1].(time.Time); !ok || got != fixedNow {
		t.Fatalf("bound updated_at: %#v", got)
	}
}

// 显式 Set 的值优先，且不会被追加重复列。
func TestAutoTimeUpdateSetWins(t *testing.T) {
	db, stmts, args := clockedDB(t)
	mine := time.Date(2019, 5, 5, 0, 0, 0, 0, time.UTC)
	if _, err := xdb.Update[Article](db).Set(A.Title.To("x"), A.UpdatedAt.To(mine)).
		Where(A.ID.Eq(3)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	sql := (*stmts)[0]
	if strings.Count(sql, `"updated_at"`) != 1 {
		t.Fatalf("updated_at must appear once: %s", sql)
	}
	if got, ok := (*args)[0][1].(time.Time); !ok || got != mine {
		t.Fatalf("bound updated_at: %#v", got)
	}
}

// DDL/ToSQL 是只读调试接口，不应修改模型。
func TestAutoTimeToSQLDoesNotStamp(t *testing.T) {
	a := &Article{Title: "x"}
	if _, _, err := xdb.Insert(nil, a).ToSQL(); err == nil {
		t.Fatal("expected ErrNoExecutor")
	}
	if !a.CreatedAt.IsZero() {
		t.Fatalf("ToSQL must not stamp: %v", a.CreatedAt)
	}
}

// 数据库最多存微秒：Linux 上 time.Now 带纳秒与单调时钟，不截断时模型里的值与读回的值不相等。
// 自动填充的时间（插入与 UpdateModel，含 WithClock 注入的时钟）都截断到微秒。
func TestAutoTimeTruncatedToMicroseconds(t *testing.T) {
	nano := time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.UTC)
	want := time.Date(2026, 2, 3, 4, 5, 6, 123456000, time.UTC)
	var args [][]any
	db, _ := newDB(t, func(q string, a []any) (*fakedb.Result, error) {
		args = append(args, a)
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}, nil
	}, xdb.WithClock(func() time.Time { return nano }))

	a := &Article{Title: "hello"}
	if _, err := xdb.Insert(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if a.CreatedAt != want || a.UpdatedAt != want {
		t.Fatalf("insert: model not truncated: %v / %v", a.CreatedAt, a.UpdatedAt)
	}
	if got := args[0][1].(time.Time); got != want {
		t.Fatalf("insert: bound %v, want %v", got, want)
	}

	if _, err := xdb.UpdateModel(db, a).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if a.UpdatedAt != want {
		t.Fatalf("update: model not truncated: %v", a.UpdatedAt)
	}

	// 默认时钟（time.Now）也去掉单调时钟读数与亚微秒部分
	db2, _ := newDB(t, func(q string, a []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}, nil
	})
	b := &Article{Title: "x"}
	if _, err := xdb.Insert(db2, b).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if b.CreatedAt.Nanosecond()%1000 != 0 || b.CreatedAt != b.CreatedAt.Round(0) {
		t.Fatalf("default clock not truncated: %v", b.CreatedAt)
	}
}
