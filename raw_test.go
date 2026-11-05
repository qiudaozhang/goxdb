package xdb_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

type User struct {
	ID        int64     `xdb:"id"`
	Name      string    `xdb:"name"`
	Email     *string   `xdb:"email"`
	CreatedAt time.Time `xdb:"created_at"`
}

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func usersResult() *fakedb.Result {
	return &fakedb.Result{
		Columns: []string{"id", "name", "email", "created_at"},
		Rows: [][]any{
			{int64(1), "tom", "t@x.com", now},
			{int64(2), "jerry", nil, now},
		},
	}
}

func newDB(t *testing.T, h fakedb.Handler, opts ...xdb.Option) (*xdb.DB, *fakedb.DB) {
	t.Helper()
	f := fakedb.New(h)
	db := xdb.New(f.Open(), postgres.Dialect, opts...)
	t.Cleanup(func() {
		if n := db.Stats().InUse; n != 0 {
			t.Errorf("connection leak: %d in use", n)
		}
		db.Close()
	})
	return db, f
}

var ctx = context.Background()

func TestAllStruct(t *testing.T) {
	var gotQuery string
	var gotArgs []any
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		gotQuery, gotArgs = q, args
		return usersResult(), nil
	})
	users, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT * FROM users WHERE id IN (?) AND name <> ?", []int64{1, 2}, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "SELECT * FROM users WHERE id IN ($1, $2) AND name <> $3" || len(gotArgs) != 3 {
		t.Fatalf("query %q args %v", gotQuery, gotArgs)
	}
	if len(users) != 2 || users[0].Name != "tom" || *users[0].Email != "t@x.com" || users[1].Email != nil || !users[1].CreatedAt.Equal(now) {
		t.Fatalf("unexpected %+v", users)
	}

	ptrs, err := xdb.All[*User](ctx, xdb.Raw(db, "SELECT"))
	if err != nil || len(ptrs) != 2 || ptrs[1].ID != 2 {
		t.Fatalf("%v %v", ptrs, err)
	}
}

func TestAllEmptyIsNonNil(t *testing.T) {
	db, _ := newDB(t, func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"id"}}, nil
	})
	got, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT"))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestStrictAndLenient(t *testing.T) {
	db, _ := newDB(t, func(string, []any) (*fakedb.Result, error) {
		return &fakedb.Result{Columns: []string{"id", "nope"}, Rows: [][]any{{int64(1), "x"}}}, nil
	})
	if _, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT")); !errors.Is(err, xdb.ErrUnknownColumn) {
		t.Fatalf("want ErrUnknownColumn, got %v", err)
	}
	got, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT").Lenient())
	if err != nil || got[0].ID != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestOne(t *testing.T) {
	rows := 0
	db, _ := newDB(t, func(string, []any) (*fakedb.Result, error) {
		r := usersResult()
		r.Rows = r.Rows[:rows]
		return r, nil
	})
	if _, err := xdb.One[User](ctx, xdb.Raw(db, "SELECT")); !errors.Is(err, xdb.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	rows = 2
	if _, err := xdb.One[User](ctx, xdb.Raw(db, "SELECT")); !errors.Is(err, xdb.ErrMultipleRows) {
		t.Fatalf("want ErrMultipleRows, got %v", err)
	}
	rows = 1
	u, err := xdb.One[User](ctx, xdb.Raw(db, "SELECT"))
	if err != nil || u.Name != "tom" {
		t.Fatalf("%v %v", u, err)
	}
}

func TestScalarMapAndScan(t *testing.T) {
	db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if strings.Contains(q, "count") {
			return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(42)}}}, nil
		}
		return usersResult(), nil
	})
	n, err := xdb.One[int64](ctx, xdb.Raw(db, "SELECT count(*)"))
	if err != nil || n != 42 {
		t.Fatalf("%v %v", n, err)
	}
	if _, err := xdb.One[int64](ctx, xdb.Raw(db, "SELECT *")); !errors.Is(err, xdb.ErrColumnCount) {
		t.Fatalf("want ErrColumnCount, got %v", err)
	}
	maps, err := xdb.All[map[string]any](ctx, xdb.Raw(db, "SELECT *"))
	if err != nil || maps[0]["name"] != "tom" {
		t.Fatalf("%v %v", maps, err)
	}

	var us []User
	if err := xdb.Raw(db, "SELECT *").Scan(ctx, &us); err != nil || len(us) != 2 {
		t.Fatalf("%v %v", us, err)
	}
	var cnt int
	if err := xdb.Raw(db, "SELECT count(*)").Scan(ctx, &cnt); err != nil || cnt != 42 {
		t.Fatalf("%v %v", cnt, err)
	}
	if err := xdb.Raw(db, "SELECT").Scan(ctx, us); !errors.Is(err, xdb.ErrInvalidDest) {
		t.Fatalf("want ErrInvalidDest, got %v", err)
	}
}

func TestIterBreakReleases(t *testing.T) {
	db, _ := newDB(t, func(string, []any) (*fakedb.Result, error) { return usersResult(), nil })
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		for u, err := range xdb.Iter[User](ctx, xdb.Raw(tx, "SELECT")) {
			if err != nil {
				return err
			}
			// 迭代期间在同一事务上执行语句必须被拒绝
			if _, err := xdb.Raw(tx, "UPDATE").Exec(ctx); !errors.Is(err, xdb.ErrTxConcurrentUse) {
				t.Fatalf("want ErrTxConcurrentUse, got %v", err)
			}
			_ = u
			break
		}
		// break 之后结果集已关闭，可以继续使用事务
		_, err := xdb.Raw(tx, "UPDATE").Exec(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestErrorNormalization(t *testing.T) {
	db, _ := newDB(t, func(string, []any) (*fakedb.Result, error) {
		return nil, &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key", Message: "duplicate key"}
	})
	_, err := xdb.Raw(db, "INSERT INTO users(email) VALUES (?)", "a").Exec(ctx)
	if !errors.Is(err, xdb.ErrUniqueViolation) {
		t.Fatalf("want ErrUniqueViolation, got %v", err)
	}
	var de *xdb.DBError
	if !errors.As(err, &de) || de.Constraint != "users_email_key" || de.Code != "23505" || !strings.Contains(de.Query, "$1") {
		t.Fatalf("bad DBError %+v", de)
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		t.Fatal("original driver error should be reachable")
	}
}

func TestHooks(t *testing.T) {
	var events []string
	h := xdb.HookFuncs{
		BeforeFunc: func(ctx context.Context, e *xdb.QueryEvent) context.Context {
			events = append(events, "before:"+e.Op.String())
			return ctx
		},
		AfterFunc: func(ctx context.Context, e *xdb.QueryEvent) {
			events = append(events, "after:"+e.Op.String()+":"+itoa(e.Rows))
		},
	}
	db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if strings.HasPrefix(q, "SELECT") {
			return usersResult(), nil
		}
		return &fakedb.Result{Affected: 3}, nil
	}, xdb.WithHook(h))
	if _, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT")); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Raw(db, "UPDATE").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	want := "before:query after:query:2 before:exec after:exec:3"
	if got := strings.Join(events, " "); got != want {
		t.Fatalf("events\n got %s\nwant %s", got, want)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
