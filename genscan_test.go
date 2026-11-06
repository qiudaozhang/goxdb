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

// genUser 模拟 `goxdb gen` 的产物：手写等价的 NewRowScanner 实现。
// 真实项目里这段代码由生成器输出，这里手写是为了让测试留在主包内。
type genUser struct {
	ID        int64     `xdb:"id"`
	Name      string    `xdb:"name"`
	Email     *string   `xdb:"email"`
	Age       int       `xdb:"age"`
	CreatedAt time.Time `xdb:"created_at"`
}

// scannerHits 记录零反射路径被调用的次数，用来断言它确实生效。
var scannerHits int

type genUserScanner struct{ dest [5]any }

func (s *genUserScanner) ScanDest(dst any) ([]any, error) {
	m, ok := dst.(*genUser)
	if !ok {
		return nil, errors.New("wrong type")
	}
	s.dest[0] = &m.ID
	s.dest[1] = &m.Name
	s.dest[2] = &m.Email
	s.dest[3] = &m.Age
	s.dest[4] = &m.CreatedAt
	return s.dest[:], nil
}

func (u *genUser) NewRowScanner(columns []string) (xdb.ScanRows, error) {
	scannerHits++
	if len(columns) != 5 {
		return nil, nil
	}
	want := []string{"id", "name", "email", "age", "created_at"}
	for i, c := range want {
		if columns[i] != c {
			return nil, nil
		}
	}
	return &genUserScanner{}, nil
}

func genDB(cols []string, rows [][]any) *xdb.DB {
	f := fakedb.New(func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: cols, Rows: rows}, nil
	})
	return xdb.New(f.Open(), postgres.Dialect)
}

var genCols = []string{"id", "name", "email", "age", "created_at"}

func genRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{int64(i + 1), "user", nil, int64(20), time.Unix(0, 0).UTC()}
	}
	return rows
}

// TestGeneratedScannerIsUsed 验证实现了 RowScanner 的类型确实走零反射路径。
func TestGeneratedScannerIsUsed(t *testing.T) {
	db := genDB(genCols, genRows(3))
	defer db.Close()

	scannerHits = 0
	got, err := xdb.All[genUser](context.Background(), xdb.Raw(db, "SELECT * FROM users"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].ID != 3 || got[0].Name != "user" {
		t.Fatalf("scan mismatch: %+v", got)
	}
	if scannerHits != 1 {
		t.Fatalf("NewRowScanner called %d times, want exactly 1 per query", scannerHits)
	}
}

// TestGeneratedScannerOneAndIter 覆盖 One / Iter 两条路径。
func TestGeneratedScannerOneAndIter(t *testing.T) {
	db := genDB(genCols, genRows(4))
	defer db.Close()

	scannerHits = 0
	single := genDB(genCols, genRows(1))
	defer single.Close()
	one, err := xdb.One[genUser](context.Background(), xdb.Raw(single, "SELECT * FROM users WHERE id = ?", 1))
	if err != nil || one.ID != 1 {
		t.Fatalf("One: %+v %v", one, err)
	}
	var n int
	for _, err := range xdb.Iter[genUser](context.Background(), xdb.Raw(db, "SELECT * FROM users")) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 4 {
		t.Fatalf("Iter saw %d rows, want 4", n)
	}
	if scannerHits != 2 {
		t.Fatalf("NewRowScanner called %d times, want 2 (One + Iter)", scannerHits)
	}
}

// TestGeneratedScannerFallsBack 验证列不匹配时回退到反射路径，
// 并且严格模式仍然给出准确的错误信息。
func TestGeneratedScannerFallsBack(t *testing.T) {
	// 投影只有两列：生成器会返回 nil，框架必须回退。
	db := genDB([]string{"id", "name"}, [][]any{{int64(9), "zoe"}})
	defer db.Close()

	scannerHits = 0
	got, err := xdb.All[genUser](context.Background(), xdb.Raw(db, "SELECT id, name FROM users"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 9 || got[0].Name != "zoe" {
		t.Fatalf("fallback scan mismatch: %+v", got)
	}
	if scannerHits != 1 {
		t.Fatalf("hook must be offered once, got %d", scannerHits)
	}
}

// TestGeneratedScannerStrictError 验证回退后仍能报出未知列。
func TestGeneratedScannerStrictError(t *testing.T) {
	db := genDB([]string{"id", "bogus"}, [][]any{{int64(1), "x"}})
	defer db.Close()

	_, err := xdb.All[genUser](context.Background(), xdb.Raw(db, "SELECT id, bogus FROM users"))
	if !errors.Is(err, xdb.ErrUnknownColumn) {
		t.Fatalf("want ErrUnknownColumn, got %v", err)
	}
}

// TestGeneratedScannerOnPointerTarget 验证 *T 目标同样走钩子。
func TestGeneratedScannerOnPointerTarget(t *testing.T) {
	db := genDB(genCols, genRows(2))
	defer db.Close()

	scannerHits = 0
	got, err := xdb.All[*genUser](context.Background(), xdb.Raw(db, "SELECT * FROM users"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] == nil || got[1].ID != 2 {
		t.Fatalf("pointer target mismatch: %+v", got)
	}
	if scannerHits != 1 {
		t.Fatalf("pointer target should still use the hook, got %d calls", scannerHits)
	}
}
