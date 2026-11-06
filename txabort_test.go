package xdb_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// deadlockDB 返回一个在执行 "DEADLOCK" 时报死锁的假库；fail 控制本次是否报错。
func deadlockDB(t *testing.T, d dialect.Dialect, fail func() bool) (*xdb.DB, *fakedb.DB) {
	t.Helper()
	f := fakedb.New(func(q string, _ []any) (*fakedb.Result, error) {
		if q == "DEADLOCK" && (fail == nil || fail()) {
			if d.Name() == "mysql" {
				return nil, &mysqldrv.MySQLError{Number: 1213, Message: "Deadlock found"}
			}
			return nil, &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
		}
		return nil, nil
	})
	db := xdb.New(f.Open(), d)
	t.Cleanup(func() {
		if n := db.Stats().InUse; n != 0 {
			t.Errorf("connection leak: %d in use", n)
		}
		db.Close()
	})
	return db, f
}

func logHas(f *fakedb.DB, q string) bool {
	for _, l := range f.Log() {
		if l == q {
			return true
		}
	}
	return false
}

// MySQL 死锁回滚整个事务：吞掉错误继续执行时，后续语句必须被拒绝（否则会在自动提交
// 模式下各自生效），最终不能 COMMIT，并返回可重试的错误。
func TestMySQLDeadlockAbortsTx(t *testing.T) {
	db, f := deadlockDB(t, mysql.Dialect, nil)
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		tx.AfterCommit(func() { t.Error("AfterCommit must not run") })
		_ = exec(ctx, tx, "DEADLOCK") // 错误被吞掉
		if err := exec(ctx, tx, "INSERT"); !errors.Is(err, xdb.ErrTxAborted) || !errors.Is(err, xdb.ErrDeadlock) {
			t.Errorf("statement after deadlock: %v", err)
		}
		return nil
	})
	if !errors.Is(err, xdb.ErrTxAborted) || !xdb.IsRetryable(err) {
		t.Fatalf("Tx err = %v", err)
	}
	if logHas(f, "INSERT") || logHas(f, "COMMIT") || !logHas(f, "ROLLBACK") {
		t.Fatalf("log: %s", strings.Join(f.Log(), " | "))
	}
}

// savepoint 里的死锁：服务端连 savepoint 一起回滚了，不能再发 ROLLBACK TO SAVEPOINT，
// 外层即使选择继续，也不能提交。
func TestMySQLDeadlockInSavepoint(t *testing.T) {
	db, f := deadlockDB(t, mysql.Dialect, nil)
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		inner := tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
			return exec(ctx, sp, "DEADLOCK")
		})
		if !errors.Is(inner, xdb.ErrDeadlock) {
			t.Errorf("inner err = %v", inner)
		}
		return nil // 把内层失败当作可选步骤
	})
	if !errors.Is(err, xdb.ErrTxAborted) {
		t.Fatalf("Tx err = %v", err)
	}
	for _, l := range f.Log() {
		if strings.HasPrefix(l, "ROLLBACK TO SAVEPOINT") || l == "COMMIT" {
			t.Fatalf("unexpected %q in log: %s", l, strings.Join(f.Log(), " | "))
		}
	}
}

// 配合 TxRetry：被吞掉的死锁也会触发整体重试。
func TestMySQLSwallowedDeadlockRetries(t *testing.T) {
	calls := 0
	db, f := deadlockDB(t, mysql.Dialect, func() bool { calls++; return calls == 1 })
	attempts := 0
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		attempts++
		_ = exec(ctx, tx, "DEADLOCK")
		return exec(ctx, tx, "INSERT")
	}, xdb.TxRetry(3, xdb.ConstBackoff(0)))
	if err != nil || attempts != 2 || !logHas(f, "COMMIT") {
		t.Fatalf("err=%v attempts=%d log=%s", err, attempts, strings.Join(f.Log(), " | "))
	}
}

// PostgreSQL 的死锁只中止当前（子）事务：savepoint 回滚后外层可以继续并提交。
func TestPGDeadlockInSavepointRecovers(t *testing.T) {
	db, f := deadlockDB(t, postgres.Dialect, nil)
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		_ = tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
			return exec(ctx, sp, "DEADLOCK")
		})
		return exec(ctx, tx, "INSERT")
	})
	if err != nil || !logHas(f, "ROLLBACK TO SAVEPOINT xdb_sp_1") || !logHas(f, "COMMIT") {
		t.Fatalf("err=%v log=%s", err, strings.Join(f.Log(), " | "))
	}
}

// PostgreSQL：吞掉死锁后下一条语句只报 25P02（事务已中止），框架附上先前的死锁原因，
// 使 TxRetry 仍能识别并整体重试。
func TestPGSwallowedDeadlockRetries(t *testing.T) {
	attempt := 0
	f := fakedb.New(func(q string, _ []any) (*fakedb.Result, error) {
		switch {
		case q == "DEADLOCK" && attempt == 1:
			return nil, &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
		case q == "INSERT" && attempt == 1:
			return nil, &pgconn.PgError{Code: "25P02", Message: "current transaction is aborted"}
		}
		return nil, nil
	})
	db := xdb.New(f.Open(), postgres.Dialect)
	defer db.Close()
	err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
		attempt++
		_ = exec(ctx, tx, "DEADLOCK")
		err := exec(ctx, tx, "INSERT")
		if attempt == 1 && (!errors.Is(err, xdb.ErrTxAborted) || !errors.Is(err, xdb.ErrDeadlock)) {
			t.Errorf("aborted error should carry the deadlock cause: %v", err)
		}
		return err
	}, xdb.TxRetry(3, xdb.ConstBackoff(0)))
	if err != nil || attempt != 2 {
		t.Fatalf("err=%v attempts=%d", err, attempt)
	}
}
