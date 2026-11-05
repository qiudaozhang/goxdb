# goxdb

优雅、易用、事务严谨的 Go 通用数据库框架，一等支持 MySQL 8+ 与 PostgreSQL 14+。

> 当前进度：**M0 地基**已完成（执行层、扫描、Hook、错误归一化、事务）。
> 类型化查询构建器（M1）与代码生成（M2）开发中，整体设计见 [docs/DESIGN.md](docs/DESIGN.md)。

## 安装

```bash
go get github.com/qiudaozhang/goxdb
```

## 快速开始

```go
import (
	xdb "github.com/qiudaozhang/goxdb"
	_ "github.com/qiudaozhang/goxdb/dialect/postgres" // 或 dialect/mysql（DSN 需带 parseTime=true）
)

type User struct {
	ID    int64   `xdb:"id"`
	Name  string  `xdb:"name"`
	Email *string `xdb:"email"` // 可空列用指针或 sql.Null[T]
}

db, err := xdb.Open("postgres", dsn,
	xdb.WithPool(xdb.Pool{MaxOpen: 50}),
	xdb.WithHook(xdb.LogSlow(slog.Default(), 200*time.Millisecond)),
)

// 占位符统一写 ?，自动适配方言；切片自动展开
users, err := xdb.All[User](ctx, xdb.Raw(db, "SELECT * FROM users WHERE id IN (?)", ids))
u, err := xdb.One[User](ctx, xdb.Raw(db, "SELECT * FROM users WHERE id = ?", 1)) // 无结果 → xdb.ErrNotFound
n, err := xdb.One[int64](ctx, xdb.Raw(db, "SELECT COUNT(*) FROM users"))

for u, err := range xdb.Iter[User](ctx, xdb.Raw(db, "SELECT * FROM users")) { ... }
```

## 事务

```go
err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
	if _, err := xdb.Raw(tx, "UPDATE accounts SET balance = balance - ? WHERE id = ?", 10, 1).Exec(ctx); err != nil {
		return err // 返回错误 → 回滚
	}
	tx.AfterCommit(func() { cache.Del("account:1") }) // 仅在真正提交后执行

	// 嵌套事务 = SAVEPOINT，失败只回滚内层
	_ = tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error { ... })
	return nil // → 提交
}, xdb.TxIsolation(xdb.Serializable), xdb.TxRetry(3, nil))
```

- panic 自动回滚并重新抛出；ctx 取消不会提交
- 死锁 / 序列化失败自动重试（需显式开启）
- 提交结果不确定时返回 `xdb.ErrCommitUnknown`
- 事务被并发使用、结果集未关闭又执行语句、在事务中误用 `db` 均可被检测

## 错误处理

```go
if errors.Is(err, xdb.ErrUniqueViolation) { ... } // MySQL 1062 / PG 23505 统一
var de *xdb.DBError
if errors.As(err, &de) { log.Println(de.Code, de.Constraint) }
```

## 测试

```bash
go test -race ./...
```

真实数据库集成测试：

```bash
docker compose up -d --wait
go test -tags integration ./tests/integration/...
```

连接串可用 `XDB_MYSQL_DSN` / `XDB_PG_DSN` 覆盖。

