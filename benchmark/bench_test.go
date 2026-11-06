package benchmark

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 四个框架在同一进程、同一连接池、同一组表上运行，彼此只隔一次 bench 迭代，
// 因此相对差异反映框架本身的额外开销。

type env struct {
	b      backend
	std    *sql.DB
	impls  map[string]impl
	render map[string]func() (string, []any, error)
}

var envs []*env

const (
	poolSize    = 32
	benchID     = 7
	insertName  = "bench-insert"
	txUpdate    = "tx-renamed"
	plainUpdate = "renamed"
	// upsertName 在 users 上有唯一索引，先被插入、之后每次都是冲突更新。
	upsertName = "bench-upsert"
)

// nameSeq 生成唯一的 name：users.name 上有唯一索引，
// InsertOne / InsertBatch 若复用同一个名字会直接撞唯一键。
var nameSeq atomic.Int64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, nameSeq.Add(1))
}

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, b := range backends {
		std, err := sql.Open(b.driver, b.dsn())
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", b.name, err)
			continue
		}
		std.SetMaxOpenConns(poolSize)
		std.SetMaxIdleConns(poolSize)
		if err := std.PingContext(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v (run `docker compose up -d --wait`)\n", b.name, err)
			std.Close()
			continue
		}
		if err := createTable(ctx, std, b); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: create table: %v\n", b.name, err)
			std.Close()
			continue
		}
		e := &env{b: b, std: std}
		e.impls = map[string]impl{
			"xdb":  newXDB(std, b.name),
			"sqlx": newSQLX(std, b),
			"gorm": newGORM(std, b),
			"bun":  newBun(std, b),
			// xdb+gen：同样的构建器与 SQL，但结果类型由 goxdb gen 生成扫描器，
			// 读路径不经过反射。用于量化扫描器的实际收益。
			"xdb+gen": newXDBGen(std, b.name),
		}
		e.render = map[string]func() (string, []any, error){
			"xdb":  renderXDB(b.name),
			"gorm": renderGORM(std, b),
			"bun":  renderBun(std, b),
		}
		envs = append(envs, e)
	}

	code := m.Run()

	for _, e := range envs {
		dropTables(ctx, e.std)
		e.std.Close()
	}
	os.Exit(code)
}

var (
	readFrameworks  = []string{"xdb", "sqlx", "gorm", "bun"}
	allFrameworks   = []string{"xdb", "sqlx", "gorm", "bun"}
	orderFrameworks = []string{"xdb", "sqlx", "gorm", "bun"}
	// genFrameworks 只包含读路径有意义的实现；xdb+gen 与 xdb 共用构建器，
	// 因此写操作没有区别，只对比读。
	genFrameworks = []string{"xdb", "xdb+gen"}
)

// TestCorrectness 确认四个实现在同一套数据上行为一致（含返回值校验）；
// 任何框架跑错或「少干活」，基准数字都没有可比性。
func TestCorrectness(t *testing.T) {
	if len(envs) == 0 {
		t.Skip("no database reachable; run `docker compose up -d --wait`")
	}
	for _, e := range envs {
		e := e
		t.Run(e.b.name, func(t *testing.T) {
			ctx := context.Background()
			for _, name := range append(allFrameworks, "xdb+gen") {
				name, im := name, e.impls[name]
				t.Run(name, func(t *testing.T) {
					// 每个框架从同一份种子数据出发，否则行数断言会互相污染。
					cleanup(ctx, e)
					checkImpl(ctx, t, name, e, im)
					cleanup(ctx, e)
				})
			}
		})
	}
}

func checkImpl(ctx context.Context, t *testing.T, name string, e *env, im impl) {
	t.Helper()

	if err := im.SelectOne(ctx, benchID); err != nil {
		t.Fatalf("SelectOne: %v", err)
	}
	if err := im.SelectMany(ctx); err != nil {
		t.Fatalf("SelectMany: %v", err)
	}
	if err := im.SelectPage(ctx, 2, pageSize); err != nil {
		t.Fatalf("SelectPage: %v", err)
	}
	if err := im.Count(ctx); err != nil {
		t.Fatalf("Count: %v", err)
	}

	row := BenchUser{Name: uniqueName(insertName), Age: 30, CreatedAt: seedTime}
	if err := im.InsertOne(ctx, &row); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if row.ID == 0 {
		t.Fatalf("InsertOne did not backfill id")
	}

	batch := make([]BenchUser, 100)
	for i := range batch {
		batch[i] = BenchUser{Name: uniqueName("bench-batch"), Age: 20, CreatedAt: seedTime}
	}
	if err := im.InsertBatch(ctx, batch); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	var n int
	if err := e.std.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tableName+" WHERE name LIKE 'bench-batch-%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatalf("InsertBatch wrote %d rows, want 100", n)
	}

	if err := im.UpdateOne(ctx, benchID, plainUpdate); err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}

	// upsert：第一次插入，第二次冲突必须更新而不是报错。
	up := BenchUser{Name: upsertName, Age: 41, CreatedAt: seedTime}
	if err := im.Upsert(ctx, &up); err != nil {
		t.Fatalf("Upsert insert: %v", err)
	}
	up.Age = 42
	if err := im.Upsert(ctx, &up); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	var age int
	q := fmt.Sprintf("SELECT age FROM %s WHERE name = %s", tableName, e.b.placeholder(1))
	if err := e.std.QueryRowContext(ctx, q, upsertName).Scan(&age); err != nil {
		t.Fatal(err)
	}
	if age != 42 {
		t.Fatalf("Upsert did not update: age = %d, want 42", age)
	}
	var count int
	if err := e.std.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE name = %s", tableName, e.b.placeholder(1)), upsertName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Upsert created %d rows, want 1", count)
	}

	if err := im.Preload(ctx); err != nil {
		t.Fatalf("Preload: %v", err)
	}
	if err := im.JoinAggregate(ctx); err != nil {
		t.Fatalf("JoinAggregate: %v", err)
	}

	if err := im.Tx(ctx, benchID, txUpdate); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	var got string
	q = fmt.Sprintf("SELECT name FROM %s WHERE id = %s", tableName, e.b.placeholder(1))
	if err := e.std.QueryRowContext(ctx, q, benchID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != txUpdate {
		t.Fatalf("Tx did not commit: name = %q", got)
	}
}

func cleanup(ctx context.Context, e *env) {
	ph := e.b.placeholder
	if _, err := e.std.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET name = %s WHERE id = %s", tableName, ph(1), ph(2)), "user-7", benchID); err != nil {
		e.logf("reset name: %v", err)
	}
	like := fmt.Sprintf("DELETE FROM %s WHERE name LIKE %s", tableName, ph(1))
	if _, err := e.std.ExecContext(ctx, like, "bench-%"); err != nil {
		e.logf("delete bench rows: %v", err)
	}
	if err := resetIdentity(ctx, e.std, e.b); err != nil {
		e.logf("reset identity: %v", err)
	}
	if err := analyzeTables(ctx, e.std, e.b); err != nil {
		e.logf("analyze: %v", err)
	}
}

func (e *env) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func forEachEnv(b *testing.B, fn func(b *testing.B, e *env)) {
	if len(envs) == 0 {
		b.Skip("no database reachable; run `docker compose up -d --wait`")
	}
	for _, e := range envs {
		e := e
		b.Run(e.b.name, func(b *testing.B) { fn(b, e) })
	}
}

func forEachImpl(b *testing.B, e *env, names []string, fn func(b *testing.B, im impl)) {
	for _, name := range names {
		name, im := name, e.impls[name]
		b.Run(name, func(b *testing.B) { fn(b, im) })
	}
}

// ---------- 读 ----------

func BenchmarkSelectOne(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, readFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.SelectOne(ctx, benchID); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

func BenchmarkSelectMany(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, readFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.SelectMany(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

func BenchmarkSelectPage(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, readFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.SelectPage(ctx, 2, pageSize); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

func BenchmarkCount(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, readFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.Count(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

// BenchmarkPreload 是关联预加载：50 个用户 + 250 条订单，两次查询。
// 这是 ORM 之间拉开差距的主要场景（框架要做主键收集与内存回填）。
func BenchmarkPreload(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, orderFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.Preload(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

// BenchmarkJoinAggregate 是 JOIN + GROUP BY + 聚合函数 + CAST：
// 检验构建器生成的 SQL 是否被正确下推（而不是在内存里算）。
func BenchmarkJoinAggregate(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, orderFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.JoinAggregate(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

// ---------- 写 ----------

func BenchmarkInsertOne(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				// 每轮都是零主键 + 唯一名字：
				// 零主键避免 GORM 把有主键的行改判为 UPDATE，
				// 唯一名字避免撞 users.name 的唯一索引。
				row := BenchUser{Name: uniqueName(insertName), Age: 30, CreatedAt: seedTime}
				if err := im.InsertOne(ctx, &row); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

// BenchmarkInsertBatch 是单条多值 INSERT 写 100 行，测参数编码与批量拼接。
func BenchmarkInsertBatch(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			rows := make([]BenchUser, 100)
			b.ReportAllocs()
			for b.Loop() {
				for i := range rows {
					rows[i] = BenchUser{Name: uniqueName("bench-batch"), Age: 20, CreatedAt: seedTime}
				}
				if err := im.InsertBatch(ctx, rows); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

func BenchmarkUpdateOne(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.UpdateOne(ctx, benchID, plainUpdate); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

// BenchmarkUpsert 测冲突路径：name 唯一索引命中后走 DO UPDATE，不新增行。
func BenchmarkUpsert(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		ctx := context.Background()
		seedUp := BenchUser{Name: upsertName, Age: 41, CreatedAt: seedTime}
		for _, name := range allFrameworks {
			if err := e.impls[name].Upsert(ctx, &seedUp); err != nil {
				b.Fatalf("%s seed upsert: %v", name, err)
			}
		}
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			tmpl := BenchUser{Name: upsertName, Age: 42, CreatedAt: seedTime}
			b.ReportAllocs()
			for b.Loop() {
				row := tmpl
				if err := im.Upsert(ctx, &row); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

func BenchmarkTx(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if err := im.Tx(ctx, benchID, txUpdate); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

// ---------- 并发 ----------

// BenchmarkMixedParallel 模拟真实混合负载：N 个 goroutine 并发做
// 「读 1 行 + 更新自己的那行」，检验框架在锁竞争与分配压力下的表现。
// 每个 worker 用独立的主键范围，避免行锁互相等待污染结果。
func BenchmarkMixedParallel(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		forEachImpl(b, e, allFrameworks, func(b *testing.B, im impl) {
			ctx := context.Background()
			var next int64 = 100 // 与 benchID 错开
			var mu sync.Mutex
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				mu.Lock()
				next++
				id := next
				mu.Unlock()
				if id > seedUsers {
					id = id%seedUsers + 1
				}
				// name 上有唯一索引，因此每个 worker 用自己的 id 生成名字，
				// 否则并发更新会把多行改成同一个值而触发唯一键冲突。
				name := fmt.Sprintf("par-%d", id)
				for pb.Next() {
					if err := im.SelectOne(ctx, id); err != nil {
						b.Error(err)
						return
					}
					if err := im.UpdateOne(ctx, id, name); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			cleanup(ctx, e)
		})
	})
}

// ---------- 纯渲染 ----------

// BenchmarkRender 只测 SQL 生成与参数编码，不碰数据库（纯 CPU 开销）。
// sqlx 没有构建器，只能手拼 SQL 字符串，因此不参与此项对比。
func BenchmarkRender(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		for _, name := range []string{"xdb", "gorm", "bun"} {
			name, fn := name, e.render[name]
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, _, err := fn(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}

// BenchmarkGenScan 专门对比「反射扫描」与「go xdb gen 生成的零反射扫描」。
// 两者使用完全相同的构建器、SQL、连接池与结果集，差异只来自扫描实现。
func BenchmarkGenScan(b *testing.B) {
	forEachEnv(b, func(b *testing.B, e *env) {
		for _, tc := range []struct {
			name string
			fn   func(ctx context.Context) error
		}{
			{"100rows/reflect", e.impls["xdb"].SelectMany},
			{"100rows/generated", e.impls["xdb+gen"].SelectMany},
			{"1row/reflect", func(ctx context.Context) error { return e.impls["xdb"].SelectOne(ctx, benchID) }},
			{"1row/generated", func(ctx context.Context) error { return e.impls["xdb+gen"].SelectOne(ctx, benchID) }},
		} {
			tc := tc
			b.Run(tc.name, func(b *testing.B) {
				ctx := context.Background()
				b.ReportAllocs()
				for b.Loop() {
					if err := tc.fn(ctx); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}
