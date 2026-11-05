package xdb_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

func exec(ctx context.Context, ex xdb.Executor, q string) error {
	_, err := xdb.Raw(ex, q).Exec(ctx)
	return err
}

func logIs(t *testing.T, f *fakedb.DB, want ...string) {
	t.Helper()
	if got := strings.Join(f.Log(), " | "); got != strings.Join(want, " | ") {
		t.Fatalf("statements\n got: %s\nwant: %s", got, strings.Join(want, " | "))
	}
}

func TestTxCommit(t *testing.T) {
	db, f := newDB(t, nil)
	var committed bool
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		tx.AfterCommit(func() { committed = true })
		tx.AfterRollback(func() { t.Fatal("must not run") })
		return exec(ctx, tx, "INSERT 1")
	}, xdb.TxIsolation(xdb.Serializable))
	if err != nil || !committed {
		t.Fatalf("err=%v committed=%v", err, committed)
	}
	logIs(t, f, "BEGIN ISOLATION Serializable", "INSERT 1", "COMMIT")
}

func TestTxRollbackOnError(t *testing.T) {
	db, f := newDB(t, nil)
	boom := errors.New("boom")
	var rolledBack bool
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		tx.AfterCommit(func() { t.Fatal("must not run") })
		tx.AfterRollback(func() { rolledBack = true })
		_ = exec(ctx, tx, "INSERT 1")
		return boom
	})
	if err != boom || !rolledBack {
		t.Fatalf("err=%v rolledBack=%v", err, rolledBack)
	}
	logIs(t, f, "BEGIN", "INSERT 1", "ROLLBACK")
}

func TestTxRollbackErrorJoined(t *testing.T) {
	rbErr := io.ErrUnexpectedEOF
	db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if q == "ROLLBACK" {
			return nil, rbErr
		}
		return nil, nil
	})
	boom := errors.New("boom")
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error { return boom })
	if !errors.Is(err, boom) || !errors.Is(err, rbErr) {
		t.Fatalf("both errors must be preserved, got %v", err)
	}
}

func TestTxPanic(t *testing.T) {
	db, f := newDB(t, nil)
	defer func() {
		if p := recover(); p != "kaboom" {
			t.Fatalf("panic must propagate unchanged, got %v", p)
		}
		logIs(t, f, "BEGIN", "ROLLBACK")
	}()
	_ = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error { panic("kaboom") })
}

func TestTxContextCanceledNoCommit(t *testing.T) {
	db, f := newDB(t, nil)
	cctx, cancel := context.WithCancel(ctx)
	err := db.Tx(cctx, func(ctx context.Context, tx *xdb.Tx) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	logIs(t, f, "BEGIN", "ROLLBACK")
}

func TestTxNested(t *testing.T) {
	db, f := newDB(t, nil)
	var calls []string
	boom := errors.New("inner failed")
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		tx.AfterCommit(func() { calls = append(calls, "outer") })
		if err := exec(ctx, tx, "A"); err != nil {
			return err
		}
		// 失败的嵌套事务：只回滚到 savepoint，回调被丢弃
		err := tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
			sp.AfterCommit(func() { calls = append(calls, "discarded") })
			sp.AfterRollback(func() { calls = append(calls, "sp1-rolledback") })
			// 嵌套事务进行中，外层不可用
			if err := exec(ctx, tx, "X"); !errors.Is(err, xdb.ErrTxChildActive) {
				t.Fatalf("want ErrTxChildActive, got %v", err)
			}
			_ = exec(ctx, sp, "B")
			return boom
		})
		if err != boom {
			t.Fatalf("inner error must be returned unchanged, got %v", err)
		}
		// 成功的嵌套事务：并入外层，回调在最终提交后执行
		return tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
			sp.AfterCommit(func() { calls = append(calls, "sp2") })
			return exec(ctx, sp, "C")
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	logIs(t, f, "BEGIN", "A",
		"SAVEPOINT xdb_sp_1", "B", "ROLLBACK TO SAVEPOINT xdb_sp_1",
		"SAVEPOINT xdb_sp_2", "C", "RELEASE SAVEPOINT xdb_sp_2",
		"COMMIT")
	if got := strings.Join(calls, ","); got != "sp1-rolledback,outer,sp2" {
		t.Fatalf("callbacks: %s", got)
	}
}

func TestTxNestedOptionsRejected(t *testing.T) {
	db, _ := newDB(t, nil)
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		return tx.Tx(ctx, func(context.Context, *xdb.Tx) error { return nil }, xdb.TxReadOnly())
	})
	if !errors.Is(err, xdb.ErrNestedTxOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestTxUseAfterDone(t *testing.T) {
	db, _ := newDB(t, nil)
	var leaked *xdb.Tx
	_ = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error { leaked = tx; return nil })
	if err := exec(ctx, leaked, "X"); !errors.Is(err, xdb.ErrTxDone) {
		t.Fatalf("want ErrTxDone, got %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("AfterCommit on finished tx must panic")
		}
	}()
	leaked.AfterCommit(func() {})
}

func TestTxRetry(t *testing.T) {
	var attempts atomic.Int32
	db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
		if q == "UPDATE" && attempts.Add(1) < 3 {
			return nil, &pgconn.PgError{Code: "40001"}
		}
		return nil, nil
	})
	var commits int
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		tx.AfterCommit(func() { commits++ })
		return exec(ctx, tx, "UPDATE")
	}, xdb.TxRetry(3, xdb.ConstBackoff(0)))
	if err != nil || attempts.Load() != 3 || commits != 1 {
		t.Fatalf("err=%v attempts=%d commits=%d", err, attempts.Load(), commits)
	}

	// 非可重试错误不重试；超过次数返回最后一次错误
	attempts.Store(-100)
	err = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		return exec(ctx, tx, "UPDATE")
	}, xdb.TxRetry(2, xdb.ConstBackoff(0)))
	if !errors.Is(err, xdb.ErrSerialization) || attempts.Load() != -98 {
		t.Fatalf("err=%v attempts=%d", err, attempts.Load())
	}
}

func TestCommitOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantIs   error
		rollback bool // AfterRollback 是否应执行
	}{
		{"unknown", io.ErrUnexpectedEOF, xdb.ErrCommitUnknown, false},
		{"server rejected", &pgconn.PgError{Code: "40001"}, xdb.ErrSerialization, true},
		{"aborted tx", pgx.ErrTxCommitRollback, xdb.ErrTxAborted, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, _ := newDB(t, func(q string, _ []any) (*fakedb.Result, error) {
				if q == "COMMIT" {
					return nil, c.err
				}
				return nil, nil
			})
			var rb bool
			err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
				tx.AfterRollback(func() { rb = true })
				tx.AfterCommit(func() { t.Fatal("must not run") })
				return nil
			})
			if !errors.Is(err, c.wantIs) || rb != c.rollback {
				t.Fatalf("err=%v rollbackCallback=%v", err, rb)
			}
		})
	}
}

func TestExecutorMisuse(t *testing.T) {
	db, _ := newDB(t, nil, xdb.WithStrictTx())
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		if err := exec(ctx, db, "X"); !errors.Is(err, xdb.ErrExecutorMisuse) {
			t.Fatalf("want ErrExecutorMisuse, got %v", err)
		}
		if err := exec(xdb.WithoutTx(ctx), db, "AUDIT"); err != nil {
			t.Fatalf("WithoutTx must bypass check: %v", err)
		}
		if got := xdb.ExecutorFrom(ctx, db); got != tx {
			t.Fatal("ExecutorFrom must return the tx")
		}
		return tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
			if got := xdb.ExecutorFrom(ctx, db); got != sp {
				t.Fatal("ExecutorFrom must return innermost tx")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := xdb.ExecutorFrom(ctx, db); got != db {
		t.Fatal("outside tx ExecutorFrom must return db")
	}
}

func TestManualTx(t *testing.T) {
	db, f := newDB(t, nil)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec(ctx, tx, "A"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error { return exec(ctx, sp, "B") }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx.RollbackUnlessCommitted() // 已提交，空操作
	if err := tx.Rollback(); !errors.Is(err, xdb.ErrTxDone) {
		t.Fatalf("want ErrTxDone, got %v", err)
	}
	logIs(t, f, "BEGIN", "A", "SAVEPOINT xdb_sp_1", "B", "RELEASE SAVEPOINT xdb_sp_1", "COMMIT")

	if _, err := db.Begin(ctx, xdb.TxRetry(3, nil)); !errors.Is(err, xdb.ErrRetryOnManualTx) {
		t.Fatalf("got %v", err)
	}
}
