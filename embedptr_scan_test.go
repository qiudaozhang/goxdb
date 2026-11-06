package xdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// 嵌入指针的模型 + 生成的扫描器：验证框架在调用 ScanDest 前
// 已经把 nil 的嵌入指针分配好，否则 &m.Base.ID 会 panic。
//
// 嵌入类型必须导出：反射无法为未导出的嵌入指针分配内存，
// schema 会跳过它们（生成器同样跳过）。
type EmbedBase struct {
	ID        int64     `xdb:"id"`
	CreatedAt time.Time `xdb:"created_at"`
}

type embedUser struct {
	*EmbedBase
	Name string `xdb:"name"`
}

type embedUserScanner struct{ dest [3]any }

func (s *embedUserScanner) ScanDest(dst any) ([]any, error) {
	m, ok := dst.(*embedUser)
	if !ok {
		return nil, errors.New("wrong type")
	}
	s.dest[0] = &m.EmbedBase.ID
	s.dest[1] = &m.EmbedBase.CreatedAt
	s.dest[2] = &m.Name
	return s.dest[:], nil
}

func (u *embedUser) NewRowScanner(columns []string) (xdb.ScanRows, error) {
	if len(columns) != 3 {
		return nil, nil
	}
	return &embedUserScanner{}, nil
}

func TestEmbeddedPointerScanNoPanic(t *testing.T) {
	cols := []string{"id", "created_at", "name"}
	f := fakedb.New(func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{
			Columns: cols,
			Rows:    [][]any{{int64(42), time.Unix(0, 0).UTC(), "tom"}},
		}, nil
	})
	db := xdb.New(f.Open(), postgres.Dialect)
	defer db.Close()

	got, err := xdb.All[embedUser](context.Background(), xdb.Raw(db, "SELECT * FROM users"))
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0].EmbedBase == nil {
		t.Fatal("BUG: embedded pointer was not allocated before ScanDest")
	}
	if got[0].EmbedBase.ID != 42 || got[0].Name != "tom" {
		t.Fatalf("values: ID=%d Name=%q", got[0].EmbedBase.ID, got[0].Name)
	}
}

type EmbedPK struct {
	ID int64 `xdb:"id,pk,auto"`
}

type embedPKUser struct {
	*EmbedPK
	Name string `xdb:"name"`
}

// 自增主键位于 nil 的嵌入指针中：回填前要先分配，不能 panic。
func TestInsertBackfillThroughNilEmbeddedPointer(t *testing.T) {
	db, f := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(9)}}}, nil
	})
	u := embedPKUser{Name: "tom"}
	if _, err := xdb.Insert(db, &u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u.EmbedPK == nil || u.ID != 9 {
		t.Fatalf("backfill failed: %+v", u.EmbedPK)
	}
	if got := f.Log(); len(got) != 1 || got[0] != `INSERT INTO "embed_pk_users" ("name") VALUES ($1) RETURNING "id"` {
		t.Fatalf("sql = %q", got)
	}
}
