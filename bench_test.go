package xdb_test

import (
	"context"
	"database/sql"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// 这些基准把框架自身开销（元数据查找、SQL 渲染、反射扫描）与数据库驱动隔离开：
// 数据源是内存假驱动，因此数值反映的是框架成本，而不是网络或服务端成本。

type benchUser struct {
	ID    int64   `xdb:"id"`
	Name  string  `xdb:"name"`
	Email *string `xdb:"email"`
	Age   int     `xdb:"age"`
}

func benchRows(n int) *fakedb.Result {
	res := &fakedb.Result{Columns: []string{"id", "name", "email", "age"}}
	for i := 0; i < n; i++ {
		res.Rows = append(res.Rows, []any{int64(i), "user", nil, int64(20)})
	}
	return res
}

func benchDB(b *testing.B, rows int) *xdb.DB {
	b.Helper()
	f := fakedb.New(func(string, []any) (*fakedb.Result, error) { return benchRows(rows), nil })
	return xdb.New(f.Open(), postgres.Dialect)
}

// BenchmarkRenderer 衡量纯 SQL 构建（无执行、无扫描）。
func BenchmarkRenderer(b *testing.B) {
	u := xdb.TableOf[benchUser]()
	id := xdb.CNum[int64](u, "id")
	age := xdb.CNum[int](u, "age")
	name := xdb.CStr(u, "name")
	q := xdb.Query[benchUser](nil).Where(age.Ge(18), name.HasPrefix("t")).OrderBy(id.Desc()).Limit(20)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := q.SQL(postgres.Dialect); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRenderInsert 衡量结构体 → INSERT 参数的反射读取。
func BenchmarkRenderInsert(b *testing.B) {
	u := &benchUser{Name: "tom", Age: 30}
	q := xdb.Insert(nil, u)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := q.SQL(postgres.Dialect); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanStruct 衡量一行结构体扫描（含列名 → 字段路径映射）。
func BenchmarkScanStruct(b *testing.B) {
	db := benchDB(b, 1)
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := xdb.One[benchUser](context.Background(), xdb.Raw(db, "SELECT id, name, email, age FROM users")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanStruct90 衡量 90 行扫描（摊薄每次查询的固定开销，更接近真实批量读）。
func BenchmarkScanStruct90(b *testing.B) {
	db := benchDB(b, 90)
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rows, err := xdb.All[benchUser](context.Background(), xdb.Raw(db, "SELECT id, name, email, age FROM users"))
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 90 {
			b.Fatal(len(rows))
		}
	}
}

// BenchmarkScanScalar 衡量标量目标（无字段映射，直接 Scan 到 int64）。
func BenchmarkScanScalar(b *testing.B) {
	f := fakedb.New(func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(42)}}}, nil
	})
	db := xdb.New(f.Open(), postgres.Dialect)
	defer db.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := xdb.One[int64](context.Background(), xdb.Raw(db, "SELECT COUNT(*) FROM users")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSchemaLookup 衡量元数据缓存命中后的查找成本（应是纯 map 读取）。
func BenchmarkSchemaLookup(b *testing.B) {
	u := xdb.TableOf[benchUser]()
	_ = u
	b.ReportAllocs()
	for b.Loop() {
		_ = xdb.TableOf[benchUser]()
	}
}

// 用于确认基准基线：database/sql 直接执行一行查询（不含框架）。
func BenchmarkStdlibBaseline(b *testing.B) {
	f := fakedb.New(func(string, []any) (*fakedb.Result, error) { return benchRows(1), nil })
	std := f.Open()
	defer std.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var (
			id   int64
			name string
			age  int
			mail sql.NullString
		)
		if err := std.QueryRow("SELECT id, name, email, age FROM users").Scan(&id, &name, &mail, &age); err != nil {
			b.Fatal(err)
		}
	}
}
