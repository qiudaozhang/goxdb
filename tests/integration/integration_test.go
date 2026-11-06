//go:build integration

// 集成测试：针对真实的 MySQL 与 PostgreSQL 运行。
//
//	docker compose up -d --wait
//	go test -tags integration ./tests/integration/...
//
// 可通过 XDB_MYSQL_DSN / XDB_PG_DSN 覆盖连接串。
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	_ "github.com/qiudaozhang/goxdb/dialect/mysql"
	_ "github.com/qiudaozhang/goxdb/dialect/postgres"
)

type Account struct {
	ID      int64  `xdb:"id"`
	Name    string `xdb:"name"`
	Balance int64  `xdb:"balance"`
	Note    *string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// unreachable 在数据库不可达时跳过测试；设置 XDB_REQUIRE_DB=1（CI 中）时改为失败，
// 避免服务没起来却显示全部通过。
func unreachable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("XDB_REQUIRE_DB") != "" {
		t.Fatalf("database not reachable: %v", err)
	}
	t.Skipf("database not reachable (%v); run docker compose up -d --wait", err)
}

var targets = []struct{ dialect, dsn string }{
	{"mysql", env("XDB_MYSQL_DSN", "root:xdb@tcp(127.0.0.1:13306)/xdb?parseTime=true")},
	{"postgres", env("XDB_PG_DSN", "postgres://postgres:xdb@127.0.0.1:15432/xdb?sslmode=disable")},
}

func forEachDB(t *testing.T, fn func(t *testing.T, db *xdb.DB)) {
	for _, tg := range targets {
		t.Run(tg.dialect, func(t *testing.T) {
			db, err := xdb.Open(tg.dialect, tg.dsn, xdb.WithStrictTx(), xdb.WithPool(xdb.Pool{MaxOpen: 10}))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.Ping(ctx); err != nil {
				unreachable(t, err)
			}
			mustExec(t, db, "DROP TABLE IF EXISTS accounts")
			mustExec(t, db, `CREATE TABLE accounts (
				id BIGINT PRIMARY KEY,
				name VARCHAR(50) NOT NULL,
				balance BIGINT NOT NULL,
				note VARCHAR(100),
				CONSTRAINT accounts_name_key UNIQUE (name),
				CONSTRAINT accounts_balance_check CHECK (balance >= 0)
			)`)
			mustExec(t, db, "INSERT INTO accounts (id, name, balance) VALUES (?, ?, ?), (?, ?, ?)", 1, "alice", 100, 2, "bob", 100)
			fn(t, db)
			// ctx 取消后 database/sql 在后台 goroutine（Tx.awaitDone）中回滚并归还连接，
			// Commit 不等它完成就返回，因此给一点宽限期；真正的泄漏一秒后仍不会归零。
			for deadline := time.Now().Add(time.Second); db.Stats().InUse != 0 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			if n := db.Stats().InUse; n != 0 {
				t.Errorf("connection leak: %d connections in use", n)
			}
			db.Close()
		})
	}
}

func mustExec(t *testing.T, ex xdb.Executor, q string, args ...any) {
	t.Helper()
	if _, err := xdb.Raw(ex, q, args...).Exec(context.Background()); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func balance(t *testing.T, ex xdb.Executor, id int64) int64 {
	t.Helper()
	b, err := xdb.One[int64](context.Background(), xdb.Raw(ex, "SELECT balance FROM accounts WHERE id = ?", id))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func count(t *testing.T, ex xdb.Executor) int64 {
	t.Helper()
	n, err := xdb.One[int64](context.Background(), xdb.Raw(ex, "SELECT COUNT(*) FROM accounts"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var ctx = context.Background()

func TestScan(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		accs, err := xdb.All[Account](ctx, xdb.Raw(db, "SELECT * FROM accounts WHERE id IN (?) ORDER BY id", []int64{1, 2}))
		if err != nil || len(accs) != 2 || accs[1].Name != "bob" || accs[0].Note != nil {
			t.Fatalf("%+v %v", accs, err)
		}
		if _, err := xdb.One[Account](ctx, xdb.Raw(db, "SELECT * FROM accounts WHERE id = ?", 99)); !errors.Is(err, xdb.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		n := 0
		for a, err := range xdb.Iter[*Account](ctx, xdb.Raw(db, "SELECT id, name, balance FROM accounts")) {
			if err != nil || a.Balance != 100 {
				t.Fatal(a, err)
			}
			n++
		}
		if n != 2 {
			t.Fatal(n)
		}
		// 文本中的 '?' 不应被当作占位符
		s, err := xdb.One[string](ctx, xdb.Raw(db, "SELECT CONCAT('what?', name) FROM accounts WHERE id = ?", 1))
		if err != nil || s != "what?alice" {
			t.Fatal(s, err)
		}
	})
}

func TestErrorNormalization(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		_, err := xdb.Raw(db, "INSERT INTO accounts (id, name, balance) VALUES (?, ?, ?)", 3, "alice", 1).Exec(ctx)
		var de *xdb.DBError
		if !errors.Is(err, xdb.ErrUniqueViolation) || !errors.As(err, &de) || de.Constraint != "accounts_name_key" {
			t.Fatalf("unique: %v (%+v)", err, de)
		}
		_, err = xdb.Raw(db, "UPDATE accounts SET balance = -1 WHERE id = 1").Exec(ctx)
		if !errors.Is(err, xdb.ErrCheckViolation) {
			t.Fatalf("check: %v", err)
		}
		_, err = xdb.Raw(db, "INSERT INTO accounts (id, name, balance) VALUES (?, ?, ?)", 3, nil, 1).Exec(ctx)
		if !errors.Is(err, xdb.ErrNotNullViolation) {
			t.Fatalf("not null: %v", err)
		}
	})
}

func TestTxCommitRollback(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
			mustExec(t, tx, "UPDATE accounts SET balance = balance - 30 WHERE id = 1")
			mustExec(t, tx, "UPDATE accounts SET balance = balance + 30 WHERE id = 2")
			return nil
		})
		if err != nil || balance(t, db, 1) != 70 || balance(t, db, 2) != 130 {
			t.Fatal(err)
		}

		boom := errors.New("boom")
		err = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
			mustExec(t, tx, "UPDATE accounts SET balance = 0 WHERE id = 1")
			return boom
		})
		if err != boom || balance(t, db, 1) != 70 {
			t.Fatal("rollback failed", err)
		}
	})
}

func TestSavepointRecovery(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
			mustExec(t, tx, "INSERT INTO accounts (id, name, balance) VALUES (3, 'carol', 0)")
			// 内层语句失败；回滚到 savepoint 后外层事务仍然健康（PostgreSQL 尤其依赖这一点）
			err := tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
				mustExec(t, sp, "INSERT INTO accounts (id, name, balance) VALUES (4, 'dave', 0)")
				_, err := xdb.Raw(sp, "INSERT INTO accounts (id, name, balance) VALUES (5, 'alice', 0)").Exec(ctx)
				return err
			})
			if !errors.Is(err, xdb.ErrUniqueViolation) {
				t.Fatalf("inner: %v", err)
			}
			mustExec(t, tx, "INSERT INTO accounts (id, name, balance) VALUES (6, 'erin', 0)")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := count(t, db); n != 4 { // 1,2,3,6；4 随 savepoint 回滚
			t.Fatalf("count = %d", n)
		}
	})
}

func TestIgnoredErrorInTx(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
			mustExec(t, tx, "INSERT INTO accounts (id, name, balance) VALUES (3, 'carol', 0)")
			_, _ = xdb.Raw(tx, "INSERT INTO accounts (id, name, balance) VALUES (4, 'alice', 0)").Exec(ctx) // 错误被吞掉
			return nil
		})
		switch db.Dialect().Name() {
		case "postgres":
			// PostgreSQL：事务已中止，COMMIT 实际执行了 ROLLBACK，必须报错而不是假装成功
			if !errors.Is(err, xdb.ErrTxAborted) || count(t, db) != 2 {
				t.Fatalf("want ErrTxAborted and nothing committed, got %v", err)
			}
		case "mysql":
			// MySQL：失败语句不影响事务，其余写入正常提交
			if err != nil || count(t, db) != 3 {
				t.Fatalf("err=%v", err)
			}
		}
	})
}

func TestDeadlockRetry(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		var retries atomic.Int32
		readyA, readyB := make(chan struct{}), make(chan struct{})
		transfer := func(first, second int64, mine, other chan struct{}) error {
			attempt := 0
			return db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
				attempt++
				if attempt > 1 {
					retries.Add(1)
				}
				if _, err := xdb.One[int64](ctx, xdb.Raw(tx, "SELECT balance FROM accounts WHERE id = ? FOR UPDATE", first)); err != nil {
					return err
				}
				if attempt == 1 { // 仅首次制造交叉加锁
					close(mine)
					<-other
				}
				if _, err := xdb.Raw(tx, "UPDATE accounts SET balance = balance - 10 WHERE id = ?", second).Exec(ctx); err != nil {
					return err
				}
				_, err := xdb.Raw(tx, "UPDATE accounts SET balance = balance + 10 WHERE id = ?", first).Exec(ctx)
				return err
			}, xdb.TxRetry(5, xdb.ExpBackoff(20*time.Millisecond, 200*time.Millisecond)))
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); errs[0] = transfer(1, 2, readyA, readyB) }()
		go func() { defer wg.Done(); errs[1] = transfer(2, 1, readyB, readyA) }()
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v", errs)
		}
		if retries.Load() < 1 {
			t.Fatal("expected a deadlock to be detected and retried")
		}
		if balance(t, db, 1) != 100 || balance(t, db, 2) != 100 {
			t.Fatal("balances must be unchanged after symmetric transfers")
		}
	})
}

func TestConcurrentTransfers(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		var wg sync.WaitGroup
		var failed atomic.Int32
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				from, to := int64(1+i%2), int64(2-i%2)
				err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
					// 按 id 顺序加锁，避免死锁
					_, err := xdb.All[int64](ctx, xdb.Raw(tx, "SELECT id FROM accounts WHERE id IN (1, 2) ORDER BY id FOR UPDATE"))
					if err != nil {
						return err
					}
					if _, err := xdb.Raw(tx, "UPDATE accounts SET balance = balance - 1 WHERE id = ?", from).Exec(ctx); err != nil {
						return err
					}
					_, err = xdb.Raw(tx, "UPDATE accounts SET balance = balance + 1 WHERE id = ?", to).Exec(ctx)
					return err
				}, xdb.TxRetry(5, nil))
				if err != nil {
					failed.Add(1)
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if failed.Load() == 0 && balance(t, db, 1)+balance(t, db, 2) != 200 {
			t.Fatal("money was created or destroyed")
		}
	})
}

func TestSerializableRetry(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		if db.Dialect().Name() != "postgres" {
			t.Skip("serialization failures are PostgreSQL specific")
		}
		var retries atomic.Int32
		start := make(chan struct{})
		var wg sync.WaitGroup
		var reads sync.WaitGroup
		reads.Add(2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				attempt := 0
				errs[i] = db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
					attempt++
					if attempt > 1 {
						retries.Add(1)
					}
					sum, err := xdb.One[int64](ctx, xdb.Raw(tx, "SELECT SUM(balance) FROM accounts"))
					if err != nil {
						return err
					}
					if attempt == 1 {
						reads.Done()
						<-start
					}
					_, err = xdb.Raw(tx, "INSERT INTO accounts (id, name, balance) VALUES (?, ?, ?)", 10+int64(i)*10+int64(attempt), "x"+string(rune('a'+i))+string(rune('0'+attempt)), sum%7).Exec(ctx)
					return err
				}, xdb.TxIsolation(xdb.Serializable), xdb.TxRetry(5, xdb.ConstBackoff(10*time.Millisecond)))
			}(i)
		}
		reads.Wait()
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v", errs)
		}
		if retries.Load() < 1 {
			t.Fatal("expected a serialization failure to be retried")
		}
	})
}

func TestManualTxContextCancel(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		cctx, cancel := context.WithCancel(ctx)
		tx, err := db.Begin(cctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.RollbackUnlessCommitted()
		mustExec(t, tx, "UPDATE accounts SET balance = 0 WHERE id = 1")
		cancel()
		if err := tx.Commit(); err == nil {
			t.Fatal("commit after cancel must fail")
		}
		if balance(t, db, 1) != 100 {
			t.Fatal("canceled tx must not commit")
		}
	})
}

// 事务中吞掉死锁错误后继续执行：MySQL 已回滚整个事务、连接回到自动提交模式，
// 后续写入必须被拒绝，Tx 返回 ErrTxAborted 而不是报告成功；配合 TxRetry 时整体重试。
func TestSwallowedDeadlock(t *testing.T) {
	forEachDB(t, func(t *testing.T, db *xdb.DB) {
		for _, retry := range []bool{false, true} {
			mustExec(t, db, "UPDATE accounts SET note = NULL")
			mustExec(t, db, "DELETE FROM accounts WHERE id > 2")
			vLocked, hReady := make(chan struct{}), make(chan struct{})
			hDone := make(chan error, 1)
			go func() { // H：更重的事务（写入更多行），让 InnoDB 选 V 作为牺牲者
				<-vLocked
				h, err := db.Begin(ctx)
				if err != nil {
					hDone <- err
					return
				}
				defer h.RollbackUnlessCommitted()
				if _, err := xdb.Raw(h, "UPDATE accounts SET note = 'h' WHERE id = 2").Exec(ctx); err != nil {
					hDone <- err
					return
				}
				for i := 10; i < 40; i++ {
					if _, err := xdb.Raw(h, "INSERT INTO accounts (id, name, balance) VALUES (?, ?, 0)", i, fmt.Sprintf("h%d", i)).Exec(ctx); err != nil {
						hDone <- err
						return
					}
				}
				close(hReady)
				time.Sleep(300 * time.Millisecond) // 等 V 阻塞在 id=2 上
				_, err = xdb.Raw(h, "UPDATE accounts SET note = 'h' WHERE id = 1").Exec(ctx)
				hDone <- err // 之后回滚，不留数据
			}()
			attempts := 0
			var opts []xdb.TxOption
			if retry {
				opts = append(opts, xdb.TxRetry(3, xdb.ConstBackoff(50*time.Millisecond)))
			}
			err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
				attempts++
				if _, err := xdb.Raw(tx, "UPDATE accounts SET note = 'v' WHERE id = 1").Exec(ctx); err != nil {
					return err
				}
				if attempts == 1 {
					close(vLocked)
					<-hReady
					_, _ = xdb.Raw(tx, "UPDATE accounts SET note = 'v' WHERE id = 2").Exec(ctx) // 死锁，错误被吞掉
				}
				_, err := xdb.Raw(tx, "INSERT INTO accounts (id, name, balance) VALUES (3, 'carol', 0)").Exec(ctx)
				return err
			}, opts...)
			if herr := <-hDone; herr != nil {
				t.Fatalf("H: %v", herr)
			}
			note, _ := xdb.One[*string](ctx, xdb.Raw(db, "SELECT note FROM accounts WHERE id = 1"))
			carol := count(t, db) - 2
			if !retry {
				if !errors.Is(err, xdb.ErrTxAborted) || carol != 0 || note != nil {
					t.Fatalf("no retry: err=%v carol=%d note=%v", err, carol, note)
				}
				continue
			}
			if err != nil || attempts != 2 || carol != 1 || note == nil || *note != "v" {
				t.Fatalf("retry: err=%v attempts=%d carol=%d note=%v", err, attempts, carol, note)
			}
		}
	})
}
