package xdb_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// router 记录每条语句实际走了主库还是副本。
type router struct {
	mu   sync.Mutex
	seen []string // "primary:EXEC" / "replica:QUERY" 等
}

func (r *router) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, s)
}

func (r *router) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *router) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = nil
}

// canned 按语句形状返回合适的结果，使同一份假驱动能服务读、写、计数。
func canned(q string) *fakedb.Result {
	switch {
	case strings.HasPrefix(q, "SELECT COUNT(*)"):
		return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(1)}}}
	case strings.HasPrefix(q, "SELECT EXISTS"):
		return &fakedb.Result{Columns: []string{"exists"}, Rows: [][]any{{true}}}
	case strings.HasPrefix(q, "SELECT"):
		return &fakedb.Result{Columns: []string{"id", "name", "email", "age"}, Rows: [][]any{{int64(1), "x", nil, int64(20)}}}
	case strings.HasPrefix(q, "INSERT"):
		return &fakedb.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}}
	}
	return &fakedb.Result{Affected: 1}
}

func replicaDB(t *testing.T, r *router) *xdb.DB {
	t.Helper()
	primary := fakedb.New(func(q string, args []any) (*fakedb.Result, error) {
		r.add("primary")
		return canned(q), nil
	})
	replica := fakedb.New(func(q string, args []any) (*fakedb.Result, error) {
		r.add("replica")
		return canned(q), nil
	})
	db := xdb.New(primary.Open(), postgres.Dialect, xdb.WithReplica(replica.Open()))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 只读查询走副本。
func TestReplicaReadsGoToReplica(t *testing.T) {
	r := &router{}
	db := replicaDB(t, r)

	if _, err := xdb.Query[Customer](db).All(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Query[Customer](db).Count(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.One[Customer](ctx, xdb.Raw(db, "SELECT * FROM customers")); err != nil {
		t.Fatal(err)
	}

	got := r.got()
	if len(got) == 0 {
		t.Fatal("no statements executed")
	}
	for _, s := range got {
		if s != "replica" {
			t.Fatalf("read must use replica, got %v", got)
		}
	}
}

// 写操作走主库。
func TestReplicaWritesGoToPrimary(t *testing.T) {
	r := &router{}
	db := replicaDB(t, r)

	c := &Customer{Name: "y"}
	if _, err := xdb.Insert(db, c).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Update[Customer](db).Set(U.Name.To("z")).Where(U.ID.Eq(1)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Delete[Customer](db).Where(U.ID.Eq(1)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	for _, s := range r.got() {
		if s != "primary" {
			t.Fatalf("write must use primary, got %v", r.got())
		}
	}
}

// 事务固定在主库：事务内的读也不走副本。
func TestReplicaTxStaysOnPrimary(t *testing.T) {
	r := &router{}
	db := replicaDB(t, r)

	if err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		if _, err := xdb.Query[Customer](tx).All(ctx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got := r.got()
	if len(got) == 0 {
		t.Fatal("no statements")
	}
	for _, s := range got {
		if s != "primary" {
			t.Fatalf("transaction must stay on primary, got %v", got)
		}
	}
}

// Primary(db) 强制只读查询也走主库。
func TestPrimaryForcesMainPool(t *testing.T) {
	r := &router{}
	db := replicaDB(t, r)

	if _, err := xdb.Query[Customer](xdb.Primary(db)).All(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); len(got) != 1 || got[0] != "primary" {
		t.Fatalf("Primary() must force main pool, got %v", got)
	}
}

// 未配置副本时，读也走主库。
func TestNoReplicaUsesPrimary(t *testing.T) {
	r := &router{}
	f := fakedb.New(func(q string, args []any) (*fakedb.Result, error) {
		r.add("primary")
		return canned(q), nil
	})
	db := xdb.New(f.Open(), postgres.Dialect)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := xdb.Query[Customer](db).All(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); len(got) != 1 || got[0] != "primary" {
		t.Fatalf("without replica everything uses primary, got %v", got)
	}
}

// 把同一个池同时当作主库和副本时不做区分（避免无意义的多余连接）。
func TestSamePoolAsReplicaIsIgnored(t *testing.T) {
	f := fakedb.New(func(q string, args []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"id", "name", "email", "age"}, Rows: [][]any{}}, nil
	})
	pool := f.Open()
	db := xdb.New(pool, postgres.Dialect, xdb.WithReplica(pool))
	t.Cleanup(func() { _ = db.Close() })
	if db.Replica() != nil {
		t.Fatal("same pool must be treated as no replica")
	}
}
