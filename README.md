# goxdb

优雅、易用、事务严谨的 Go 通用数据库框架，一等支持 MySQL 8+ 与 PostgreSQL 14+。

> 当前进度：**M0 地基**与 **M1 类型化查询构建器**已完成（免代码生成的列描述符、CRUD、条件组合、JOIN、
> 子查询、CTE、Upsert、行锁、批量插入）。代码生成与关联预加载（M2）开发中，整体设计见 [docs/DESIGN.md](docs/DESIGN.md)。

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

## 查询构建器

列描述符可以不依赖代码生成，用 `xdb.T` + `xdb.C[...]` 手写（M2 的 `goxdb gen` 会为你生成这些代码）：

```go
type customerTable struct {
	xdb.Table
	ID   xdb.NumberCol[int64]
	Name xdb.StringCol
	Age  xdb.NumberCol[int]
}

func newCustomerTable(t xdb.Table) customerTable {
	return customerTable{t, xdb.CNum[int64](t, "id"), xdb.CStr(t, "name"), xdb.CNum[int](t, "age")}
}

var U = newCustomerTable(xdb.TableOf[User]())
```

```go
// 查询：构建器不可变，可放心复用基础查询
base := xdb.Query[User](db).Where(U.Age.Ge(18))
adults, err := base.OrderBy(U.ID.Desc()).Limit(20).All(ctx)
active, err := base.Where(U.Name.Contains("tom")).All(ctx)

u, err := xdb.Query[User](db).Where(U.ID.Eq(1)).One(ctx) // 无结果 → xdb.ErrNotFound
n, err := xdb.Query[User](db).Where(U.Age.Ge(18)).Count(ctx)
ok, err := xdb.Query[User](db).Where(U.Email.Eq(e)).Exists(ctx)
ids, err := xdb.Pluck(ctx, xdb.Query[User](db).Where(...), U.ID)

// 动态条件：nil 条件自动忽略
xdb.Query[User](db).Where(
	xdb.If(req.Name != "", U.Name.Contains(req.Name)),
	xdb.Or(U.Age.Lt(18), xdb.And(U.Age.Gt(60), U.Name.HasPrefix("vip_"))),
	xdb.Not(U.Email.IsNull()),
)

// 写入：显式 Set，绝不做零值推断
u := &User{Name: "tom", Age: 20}
err := xdb.Insert(db, u).Exec(ctx)                       // 自增主键回填（PG RETURNING / MySQL LastInsertId）
err = xdb.InsertMany(db, users).Batch(500).Exec(ctx)     // 自动按方言参数上限分批
err = xdb.Insert(db, u).OnConflict(U.Email).DoUpdate(U.Name, U.Age).Exec(ctx)

affected, err := xdb.Update[User](db).
	Set(U.Name.To("jerry"), U.Age.Incr(1)).
	Where(U.ID.Eq(1)).
	Exec(ctx)
err = xdb.UpdateModel(db, u, U.Name, U.Age).Where(U.Version.Eq(v)).Exec(ctx) // 乐观锁
_, err = xdb.Delete[User](db).Where(U.ID.Eq(1)).Exec(ctx)
```

`Update` / `Delete` 漏写 `Where` 会返回 `xdb.ErrMissingWhere`，确需全表操作请显式调用 `.AllRows()`。

### JOIN、聚合与 CAST

```go
o := O.As("o")
total := xdb.Cast[int64]("BIGINT", xdb.Sum(o.Amount)) // SUM(bigint) 在 MySQL 是 DECIMAL、PG 是 NUMERIC
stats, err := xdb.Select[UserStat](db,
	U.ID.As("user_id"), U.Name,
	xdb.Coalesce(total, 0).As("total"),
	xdb.CountOf(o.ID).As("cnt"),
).From(U).
	LeftJoin(o, o.UserID.EqCol(U.ID), o.State.Eq("paid")).
	GroupBy(U.ID, U.Name).
	Having(xdb.Coalesce(total, 0).Ge(100)).
	OrderBy(xdb.C[int64](nil, "total").Desc()).
	All(ctx)
```

### 子查询与 CTE

```go
paid := xdb.Select[int64](nil, O.UserID).From(O).Where(O.State.Eq("paid"))
users, err := xdb.Query[User](db).Where(U.ID.InSub(paid)).All(ctx)

cnt := xdb.Scalar(xdb.Select[int64](nil, xdb.Count()).From(O).Where(O.UserID.EqCol(U.ID)))
rows, err := xdb.Select[UserStat](db, U.ID, cnt.As("order_cnt")).From(U).All(ctx)

// 递归 CTE
tree := xdb.CTE("tree", baseSelect.UnionAll(recursiveSelect)).Columns("id", "parent_id")
rows, err := xdb.Select[Node](db).WithRecursive(tree).From(tree).All(ctx)
```

### 行锁

```go
db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
	job, err := xdb.Query[Job](tx).Where(J.State.Eq("new")).OrderBy(J.ID.Asc()).
		Limit(1).ForUpdate().SkipLocked().One(ctx)
	return err
})
```

`ForUpdate` / `ForShare` 在非事务 `Executor` 上使用返回 `xdb.ErrLockOutsideTx`；`NoWait` / `SkipLocked` 须配合锁定子句。
调试时用 `sql, args, err := q.ToSQL()`。

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

构建器金标准测试对 **MySQL 与 PostgreSQL 两个方言**分别断言生成的 SQL 与参数（每个用例都必须给出两个方言的期望值）。

真实数据库集成测试：

```bash
docker compose up -d --wait
go test -tags integration ./tests/integration/...
```

连接串可用 `XDB_MYSQL_DSN` / `XDB_PG_DSN` 覆盖。

