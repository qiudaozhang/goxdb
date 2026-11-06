# goxdb 设计方案（草案 v0.1）

> 目标：一个基于 Go 的通用数据库框架。首批一等支持 MySQL 8+ / PostgreSQL 14+。
> 关键词：**优雅、易用、类型安全、事务严谨、无魔法**。

---

## 1. 设计原则

| 原则 | 含义 |
|---|---|
| **SQL 优先，而非对象优先** | API 形状贴近 SQL 本身，看代码就能推出生成的 SQL；不发明一套新的查询心智模型。 |
| **编译期尽量发现错误** | 列名、列类型通过代码生成的类型化描述符提供，`U.Age.Eq("abc")` 直接编译失败。 |
| **显式优于隐式** | 不做 GORM 式的“零值跳过”、不自动建表、不隐式开启事务、不静默吞错。 |
| **单一执行入口** | `*DB`、`*Tx` 都实现同一个 `Executor` 接口，同一段查询代码在事务内外无差别使用。 |
| **方言差异由框架吸收** | 占位符、引号、RETURNING、Upsert、UPDATE JOIN、锁语法等差异全部在 dialect 层处理。 |
| **可渐进采用** | 不用代码生成也能用（字符串列 + 泛型）；随时可退回 Raw SQL，并仍享受扫描、Hook、事务能力。 |
| **零反射热路径** | 结构体元数据首次解析后缓存；扫描使用预计算的字段索引路径。 |

---

## 2. 整体分层

```
┌────────────────────────────────────────────────────────────┐
│  用户代码                                                   │
├────────────────────────────────────────────────────────────┤
│  xdb (根包，面向用户的全部 API)                             │
│   ├─ Query/Select/Insert/Update/Delete 构建器（泛型）       │
│   ├─ 表达式系统 Expr[T] / Cond / 聚合 / 子查询 / CTE        │
│   ├─ 关联预加载 Preload（避免 N+1）                         │
│   └─ DB / Tx / Executor / Hook / 错误归一化                 │
├────────────────────────────────────────────────────────────┤
│  internal/schema   结构体元数据解析与缓存                    │
│  internal/scan     结果集 → struct / DTO / 标量 / map        │
│  internal/builder  SQL AST → 字符串 + 参数                   │
├────────────────────────────────────────────────────────────┤
│  dialect/mysql   dialect/postgres   (dialect/sqlite 预留)    │
├────────────────────────────────────────────────────────────┤
│  database/sql  (go-sql-driver/mysql, pgx/stdlib)            │
│  ※ 通过内部 conn 抽象，后期可接入 pgx 原生连接池            │
└────────────────────────────────────────────────────────────┘

cmd/goxdb   代码生成器：model struct → 类型化表/列描述符
```

建议目录：

```
goxdb/
├── go.mod                 module github.com/qiudaozhang/goxdb   (Go ≥ 1.25)
├── xdb.go  db.go  tx.go  executor.go  hook.go  errors.go
├── query.go  insert.go  update.go  delete.go  raw.go
├── expr.go  column.go  cond.go  func.go  subquery.go  cte.go
├── preload.go
├── dialect/{dialect.go, mysql/, postgres/}
├── internal/{schema, scan, builder}
├── cmd/goxdb/             代码生成
└── tests/{golden, integration}
```

---

## 3. 模型定义与代码生成

### 3.1 模型就是普通 struct

```go
package model

type User struct {
    ID        int64     `xdb:"id,pk,auto"`
    Name      string    `xdb:"name"`
    Email     string    `xdb:"email,unique"`
    Age       int       `xdb:"age"`
    Status    int8      `xdb:"status"`
    CreatedAt time.Time `xdb:"created_at,autoCreateTime"`
    UpdatedAt time.Time `xdb:"updated_at,autoUpdateTime"`

    Orders []*Order `xdb:"rel:has_many,fk:user_id"` // 关联，非列
}

func (User) TableName() string { return "users" }

type Order struct {
    ID     int64           `xdb:"id,pk,auto"`
    UserID int64           `xdb:"user_id"`
    Amount decimal.Decimal `xdb:"amount"`
    State  string          `xdb:"state"`

    User *User `xdb:"rel:belongs_to,fk:user_id"`
}
```

- 支持嵌入结构体、`sql.Scanner` / `driver.Valuer`、`xdb.Null[T]`、指针表示可空。
- 不需要实现任何接口，`TableName()` 可选（默认 snake_case 复数）。

### 3.2 代码生成类型化描述符

`go generate` 调用 `goxdb gen ./model`，生成 `model/xdb_gen.go`：

```go
var U = struct {
    xdb.Table[User]
    ID        xdb.NumberCol[int64]
    Name      xdb.StringCol
    Email     xdb.StringCol
    Age       xdb.NumberCol[int]
    Status    xdb.NumberCol[int8]
    CreatedAt xdb.TimeCol
    Orders    xdb.HasMany[User, Order]
}{ /* ... */ }

var O = struct { /* 同上 */ }{}
```

列类型分族，让每种列只暴露合理的方法：

| 类型 | 额外方法 |
|---|---|
| `Col[T]`（通用） | `Eq Ne In NotIn IsNull IsNotNull Asc Desc As` 以及 `EqExpr` 等列/表达式比较 |
| `NumberCol[T]` | `Gt Ge Lt Le Between` + `Add Sub Mul Incr` |
| `StringCol` | `Like ILike HasPrefix Contains` |
| `TimeCol` | `Gt Lt Between Before After` |

**不用代码生成的退路**（原型、脚本、动态表）：

```go
users := xdb.T("users")
age   := xdb.C[int](users, "age")
xdb.Query[User](db).Where(age.Gt(18)).All(ctx)
```

---

## 4. 核心 API 预览

### 4.1 连接

```go
db, err := xdb.Open("postgres", dsn,
    xdb.WithPool(xdb.Pool{MaxOpen: 50, MaxIdle: 10, MaxLifetime: time.Hour}),
    xdb.WithHook(xdb.LogSlow(slog.Default(), 200*time.Millisecond)),
)
defer db.Close()
```

`*xdb.DB` 和 `*xdb.Tx` 都实现：

```go
type Executor interface {
    Dialect() dialect.Dialect
    // 内部方法：exec/query，未导出，防止外部误实现
}
```

所有构建器以 **`Executor` 作为第一个参数**，因此“事务内外同一套代码”是天然的。

> Go 方法不能有类型参数，所以泛型入口是包级函数：`xdb.Query[User](db)`。

### 4.2 单表 CRUD

```go
// 插入：自动回填自增主键（PG 用 RETURNING，MySQL 用 LastInsertId）
u := &User{Name: "tom", Email: "t@x.com"}
err := xdb.Insert(db, u).Exec(ctx)

// 批量插入：自动按方言参数上限分批（PG 65535 / MySQL max_allowed_packet）
err = xdb.InsertMany(db, users).Batch(500).Exec(ctx)

// Upsert：统一 API，方言翻译为 ON CONFLICT / ON DUPLICATE KEY UPDATE
err = xdb.Insert(db, u).OnConflict(U.Email).DoUpdate(U.Name, U.Age).Exec(ctx)

// 查询
list, err := xdb.Query[User](db).
    Where(U.Age.Ge(18), U.Name.Like("t%")).   // 多个条件默认 AND
    OrderBy(U.ID.Desc()).
    Limit(20).Offset(40).
    All(ctx)                                  // []User

one, err := xdb.Query[User](db).Where(U.ID.Eq(1)).One(ctx)   // 无记录 → xdb.ErrNotFound
n,   err := xdb.Query[User](db).Where(U.Status.Eq(1)).Count(ctx)
ok,  err := xdb.Query[User](db).Where(U.Email.Eq(e)).Exists(ctx)
ids, err := xdb.Pluck(xdb.Query[User](db).Where(...), U.ID)   // []int64

// 流式遍历（Go 1.23 iter）
for u, err := range xdb.Query[User](db).Iter(ctx) { ... }

// 更新：显式 Set，绝不做零值推断
affected, err := xdb.Update[User](db).
    Set(U.Name.To("jerry"), U.Age.Incr(1)).
    Where(U.ID.Eq(1)).
    Exec(ctx)

// 按主键更新整个模型 / 指定列
err = xdb.UpdateModel(db, u).Exec(ctx)                         // 全部非主键列
err = xdb.UpdateModel(db, u).Columns(U.Name, U.Age).Exec(ctx)  // 仅指定列

// 删除
_, err = xdb.Delete[User](db).Where(U.ID.Eq(1)).Exec(ctx)
```

**安全护栏**：`Update` / `Delete` 无 `Where` 时返回 `ErrMissingWhere`，必须显式 `.AllRows()` 才能全表操作。

### 4.3 条件组合

```go
Where(
    U.Status.Eq(1),
    xdb.Or(
        U.Age.Lt(18),
        xdb.And(U.Age.Gt(60), U.Name.HasPrefix("vip_")),
    ),
    xdb.Not(U.Email.IsNull()),
)

// 动态条件：nil Cond 自动忽略，避免 if 嵌套
Where(
    xdb.If(req.Name != "", U.Name.Contains(req.Name)),
    xdb.If(req.MinAge > 0, U.Age.Ge(req.MinAge)),
)
```

### 4.4 多表 JOIN 与自定义投影

```go
type UserStat struct {
    UserID int64           `xdb:"user_id"`
    Name   string          `xdb:"name"`
    Total  decimal.Decimal `xdb:"total"`
    Cnt    int64           `xdb:"cnt"`
}

o := O.As("o") // 表别名，返回带别名的列集合（自连接同理）

stats, err := xdb.Select[UserStat](db,
        U.ID.As("user_id"),
        U.Name,
        xdb.Cast[int64]("BIGINT", xdb.Sum(o.Amount)).As("total"),
        xdb.CountOf(o.ID).As("cnt"),
    ).
    From(U).
    LeftJoin(o, o.UserID.EqCol(U.ID), o.State.Eq("paid")).
    Where(U.Status.Eq(1)).
    GroupBy(U.ID, U.Name).
    Having(xdb.Cast[int64]("BIGINT", xdb.Sum(o.Amount)).Gt(100)).
    OrderBy(xdb.C[int64](nil, "total").Desc()).
    All(ctx)
```

- `Select[T]` 按列别名映射到 DTO 的 `xdb` tag；列与字段对不上时 **报错而非静默忽略**（可用 `.Lenient()` 放宽）。
- `EqCol` 要求两边类型一致：`o.UserID.EqCol(U.Name)` 编译失败。
- 排序输出别名用无表限定的列：`xdb.C[T](nil, "total").Desc()`（表限定的列在 `ORDER BY` 中引用的是表列，不是别名）。
- **聚合结果的类型**：`SUM(bigint)` 在 MySQL 是 `DECIMAL`、PostgreSQL 是 `NUMERIC`，直接扫入 `int64` 会失败，需 `xdb.Cast[int64]("BIGINT", ...)` 显式转换。`COUNT` 返回 `BIGINT`，可直接扫入 `int64`。
- `COUNT` 与 `GROUP BY` 组合时 `Count()`（查询方法）会自动包装成子查询计数，保证与 `All` 的行数一致。
- 也支持把一行扫描到嵌套结构：`struct{ User `xdb:"u.*"`; Order `xdb:"o.*"` }`（M2）。

### 4.5 子查询 / CTE / 派生表

```go
// IN 子查询（类型化：子查询选出 int64，才能用于 int64 列）
paid := xdb.Select[int64](nil, O.UserID).From(O).Where(O.State.Eq("paid"))
users, _ := xdb.Query[User](db).Where(U.ID.InSub(paid)).All(ctx)

// EXISTS / 关联子查询（投影用字面量；PostgreSQL 无法推断裸 $1 的类型）
xdb.Query[User](db).Where(xdb.Exists(
    xdb.Select[int](nil, xdb.SQL[int]("1")).From(O).Where(O.UserID.EqCol(U.ID)),
))

// 标量子查询作为列
cnt := xdb.Scalar(xdb.Select[int64](nil, xdb.Count()).From(O).Where(O.UserID.EqCol(U.ID)))
xdb.Select[UserWithCnt](db, U.ID, U.Name, cnt.As("order_cnt")).From(U)

// 派生表
t := xdb.Derived("t", xdb.Select[UserStat](nil, ...).From(...).GroupBy(...))
xdb.Select[UserStat](db, t.Star()).From(t).Where(xdb.C[int64](t, "cnt").Gt(10))

// CTE（含 WITH RECURSIVE）
top := xdb.CTE("top_users", xdb.Select[int64](nil, O.UserID).From(O).GroupBy(O.UserID).Having(...))
xdb.Query[User](db).With(top).Where(U.ID.InSub(top.Select())).All(ctx)
```

> 子查询构建时 Executor 传 `nil`，它只作为表达式嵌入，不能单独执行。

`UNION` / `UNION ALL` 的 `ORDER BY` 作用于整个组合结果，只能引用输出列名，且被合并的查询自身不能带 `ORDER BY / LIMIT / OFFSET / 锁 / CTE`（违规时返回错误而非生成非法 SQL）。

### 4.6 关联预加载（解决 N+1）

```go
users, err := xdb.Query[User](db).
    Where(U.Status.Eq(1)).
    Preload(U.Orders, func(q *xdb.Q[Order]) { q.Where(O.State.Eq("paid")).OrderBy(O.ID.Desc()) }).
    All(ctx)
```

实现：主查询 → 收集外键 → `WHERE user_id IN (...)`（超长自动分批）→ 内存回填。支持 has_one / has_many / belongs_to / many_to_many(through)。**不做隐式懒加载**。

### 4.7 多表写操作

```go
// UPDATE ... JOIN（MySQL）/ UPDATE ... FROM（PG），统一写法
xdb.Update[Order](db).
    Join(U, U.ID.EqCol(O.UserID)).
    Set(O.State.To("frozen")).
    Where(U.Status.Eq(0)).
    Exec(ctx)

// DELETE ... USING（PG）/ DELETE t FROM t JOIN（MySQL）
xdb.Delete[Order](db).Join(U, U.ID.EqCol(O.UserID)).Where(U.Status.Eq(0)).Exec(ctx)

// INSERT ... SELECT
xdb.InsertSelect[Archive](db, xdb.Select[Archive](nil, ...).From(O).Where(...)).Exec(ctx)
```

### 4.8 Raw SQL（任何时候都能退回）

```go
var rows []UserStat
err := xdb.Raw(db, `SELECT ... FROM users u WHERE u.id IN (?) AND u.age > ?`, ids, 18).Scan(ctx, &rows)
```

`?` 自动按方言改写为 `$n`，切片自动展开，仍走 Hook、错误归一化与事务。

### 4.9 调试

```go
sql, args, err := xdb.Query[User](db).Where(...).ToSQL()
```

---

## 5. 事务设计（重点）

### 5.1 托管事务（推荐唯一常用方式）

```go
err := db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
    if err := xdb.Insert(tx, &order).Exec(ctx); err != nil {
        return err
    }
    _, err := xdb.Update[User](tx).Set(U.Balance.Sub(order.Amount)).
        Where(U.ID.Eq(order.UserID), U.Balance.Ge(order.Amount)).
        Exec(ctx)
    return err
},
    xdb.TxIsolation(xdb.Serializable),
    xdb.TxRetry(3, xdb.ExpBackoff(10*time.Millisecond, time.Second)),
)
```

闭包收到的 `ctx` 带有事务标记，应向下传递（供 `ExecutorFrom` 与误用检测使用）。

语义保证（M0 已实现并测试）：

1. `fn` 返回 `nil` → Commit；返回 error → Rollback，并返回原 error（rollback 自身失败用 `errors.Join` 一并返回，不吞掉任何一个）。
2. `fn` panic → 先 Rollback，再原样 re-panic（不把 panic 转成 error 掩盖 bug）。
3. 闭包内 **拿不到 `Commit/Rollback`**，杜绝“提前提交后又继续写”的错误。
4. `fn` 返回时 `ctx` 已取消 → 回滚而不提交。
5. 事务生命周期**不绑定** ctx 的取消（只有单条语句受 ctx 控制），避免 database/sql 在后台异步回滚与我们的提交产生竞态；回滚/提交始终由框架确定性地完成。
6. Commit 结果分三类：成功；服务端明确拒绝（如 PG 已中止事务的 COMMIT → `ErrTxAborted`）→ 视为已回滚；网络等无法判定的错误 → `ErrCommitUnknown`，提示调用方幂等处理。

### 5.2 嵌套事务 = Savepoint

```go
db.Tx(ctx, func(ctx context.Context, tx *xdb.Tx) error {
    createOrder(ctx, tx)

    // 内层失败只回滚到 savepoint，外层可选择继续
    err := tx.Tx(ctx, func(ctx context.Context, sp *xdb.Tx) error {
        return grantCoupon(ctx, sp)
    })
    if err != nil { log.Warn("coupon skipped", err) }
    return nil
})
```

- 自动生成 `SAVEPOINT xdb_sp_N` / `RELEASE SAVEPOINT` / `ROLLBACK TO SAVEPOINT`。
- 内层不能指定任何选项（传入返回 `ErrNestedTxOptions`，而不是静默忽略）。
- 内层进行中，外层 Tx 不可使用（返回 `ErrTxChildActive`），防止语句落入错误的 savepoint。

### 5.3 自动重试

- 默认仅在死锁与序列化失败时触发：PG `40001`、`40P01`；MySQL `1213`。可用 `TxRetryIf` 自定义（例如把 `ErrLockTimeout` 也纳入）。
- 整个 `fn` 重新执行，因此 `fn` 内 **不得有外部副作用**；副作用用 5.4 的 `AfterCommit`。
- 嵌套 savepoint 内不重试（只有最外层能重试）；手动事务不支持重试（`ErrRetryOnManualTx`）。

### 5.4 提交后回调

```go
tx.AfterCommit(func() { cache.Del(key); mq.Publish(evt) })
tx.AfterRollback(func() { ... })
```

`AfterCommit` 只在**最外层真正提交成功后**执行；savepoint 回滚时，其内部注册的 `AfterCommit` 被丢弃、`AfterRollback` 立即执行。对已结束的事务注册回调会 panic（编程错误）。

### 5.5 状态机与误用检测

```
Active ──commit ok──────────► Committed
   │  ├─commit 被服务端拒绝──► RolledBack
   │  └─commit 结果未知──────► Unknown
   └──rollback─────────────► RolledBack
（savepoint：Active ──release──► Released / ──rollback to──► RolledBack）
```

- 非 Active 状态下任何操作返回 `ErrTxDone`。
- 同一事务同一时刻只允许一条语句；并发使用、或结果集未关闭就执行下一条语句，返回 `ErrTxConcurrentUse`（而不是让驱动报出 “busy buffer” 一类难以理解的错误）。
- **用错 Executor 检测**：事务闭包的 ctx 带有标记，若用它在 `db` 上执行语句，默认记录告警；开启 `xdb.WithStrictTx()` 后返回 `ErrExecutorMisuse`（建议开发/测试环境开启）。确实需要独立于事务执行时，用 `xdb.WithoutTx(ctx)`。

### 5.6 手动事务（高级场景）

```go
tx, err := db.Begin(ctx, xdb.TxReadOnly())
if err != nil { return err }
defer tx.RollbackUnlessCommitted()   // 已提交则为 no-op
...
return tx.Commit()
```

`*xdb.ManualTx` 同样实现 `Executor`；与 database/sql 一致，其生命周期绑定 ctx。

### 5.7 行锁

```go
xdb.Query[Account](tx).Where(A.ID.Eq(1)).ForUpdate().SkipLocked().One(ctx)
// ForUpdate / ForShare(PG: FOR SHARE, MySQL8: FOR SHARE) / NoWait / SkipLocked
```

在非事务 Executor 上调用 `ForUpdate` 返回 `ErrLockOutsideTx`。

### 5.8 跨服务层传递事务（可选）

主推显式传 `Executor`。为兼容分层架构，提供：

```go
// 托管事务的 ctx 自动携带事务；手动事务用 ctx = mtx.Context(ctx)
exec := xdb.ExecutorFrom(ctx, db) // 有 tx 用（最内层的）tx，否则用 db
```

---

## 6. 方言层

```go
type Dialect interface {
    Name() string
    Placeholder(buf *Buffer, idx int)          // ? / $1
    QuoteIdent(buf *Buffer, name string)       // `x` / "x"
    SupportsReturning() bool
    BuildUpsert(...)                           // ON CONFLICT / ON DUPLICATE KEY
    BuildUpdateJoin(...) BuildDeleteJoin(...)
    BuildLock(...)
    MaxParams() int
    ClassifyError(err error) error             // 错误归一化
}
```

| 能力 | MySQL 8 | PostgreSQL |
|---|---|---|
| 占位符 / 引号 | `?` / `` ` `` | `$n` / `"` |
| 自增回填 | LastInsertId（批量按连续 ID 推算，需 innodb_autoinc_lock_mode 说明） | RETURNING |
| Upsert | ON DUPLICATE KEY UPDATE（VALUES()/别名语法） | ON CONFLICT (...) DO UPDATE SET ... EXCLUDED |
| ILIKE | 降级为 `LOWER(x) LIKE LOWER(?)` | 原生 |
| UPDATE 多表 | `UPDATE a JOIN b ...` | `UPDATE a ... FROM b` |
| 方言专属 | JSON_EXTRACT 等放 `dialect/mysql` | 数组、JSONB `->>`、`ANY()` 放 `dialect/postgres` |

方言专属功能放在子包，用户显式 import，**不污染通用 API**，也让“使用了不可移植特性”在代码里一眼可见。

### 错误归一化

```go
errors.Is(err, xdb.ErrNotFound)
errors.Is(err, xdb.ErrUniqueViolation)   // MySQL 1062 / PG 23505
errors.Is(err, xdb.ErrForeignKey)        // 1452,1451 / 23503
errors.Is(err, xdb.ErrDeadlock)
errors.Is(err, xdb.ErrSerialization)
var de *xdb.DBError; errors.As(err, &de) // 原始驱动错误、SQL、约束名
```

---

## 7. 扫描与元数据

- `internal/schema`：`sync.Map[reflect.Type]*Model` 缓存，记录列名、字段索引路径、主键、自动时间戳、关联。
- 扫描时按结果集列名构建 “列 → 字段路径” 映射，逐行只做 `Scan` 指针分配。
- 支持目标：`*T`、`*[]T`、`*[]*T`、标量、`map[string]any`、`iter.Seq2[T, error]`。
- 基准目标：扫描开销接近 sqlx，显著低于 GORM。

### 7.1 运行时机制（当前实现）

诚实地说：**M0/M1/M2 目前的实现完全基于反射，没有代码生成**。具体分工是：

| 环节 | 机制 | 何时发生 |
|---|---|---|
| 结构体 → 模型元数据（表名、列名、字段索引路径、主键、自增、自动时间戳） | `reflect` + `sync.Map` 缓存 | **每个类型仅一次**，之后是纯 map 查表 |
| INSERT / UPDATE 取值 | `reflect.Value.FieldByIndex` | 每次执行，逐字段 |
| 结果集 → 结构体 | 预构建“列 → 索引路径”数组 + `reflect` 取地址 | 每个结果集一次建计划，逐行只做指针取值与 `database/sql` 的 `Scan` |
| SQL 渲染（条件、JOIN、方言差异） | 泛型 + 接口，**无反射** | 每次构建 |

也就是说热路径上唯一的反射是「按已算好的索引路径取字段地址」与「填入参数值」，
类型解析、列名匹配、方言选择这些相对昂贵的部分都已经被缓存或编译期确定。

实测（Apple M4，假驱动，隔离掉网络与服务端）：

```
BenchmarkRenderer       2401 ns/op   31 allocs   // SQL 构建
BenchmarkRenderInsert   1347 ns/op   23 allocs   // 结构体 → INSERT 参数
BenchmarkScanStruct     4158 ns/op   21 allocs   // 单行结构体扫描
BenchmarkScanStruct90  37250 ns/op  300 allocs   // 90 行扫描（≈414 ns/行）
BenchmarkScanScalar      982 ns/op   20 allocs   // 标量扫描
BenchmarkStdlibBaseline  688 ns/op   14 allocs   // 基线：database/sql 直接 QueryRow+Scan
```

对照基线可以看出：单行扫描中框架自身约占 3.5 µs（其中大部分是每次查询固定开销 —
新建 builder、渲染 SQL、建扫描计划），而每行增量约 0.4 µs。

### 7.2 为什么现在不做零反射

反射的成本远小于一次数据库往返（毫秒级），在绝大多数业务场景不可观测。
过早引入代码生成会带来三类实际代价：生成代码需要维护影子类型、
模型变更后忘记重新生成会产生难以理解的错误、`go generate` 拖慢构建。

**M2 的 `goxdb gen` 的真正目的不是提速，而是类型安全**：让 `U.Age.Eq("abc")` 编译失败。
若将来基准显示反射成为瓶颈，可以在保留 API 形状的前提下，
让生成代码接管「取值 / 扫描」两个热路径（方案：生成的表描述符附带预编译的读写函数，
框架按类型断言优先使用，缺失时回退反射）。这条路径不需要破坏现有 API。

---

## 8. 可观测性与扩展

```go
type Hook interface {
    Before(ctx context.Context, e *QueryEvent) context.Context
    After(ctx context.Context, e *QueryEvent)   // 含耗时、行数、错误
}
```

**内置（零外部依赖）**：

- `LogQueries` / `LogSlow`：基于 slog 的结构化日志与慢查询，默认不记录参数值。
- `NewMetrics()`：按「操作类型 × 成功/失败」聚合次数、总耗时、均值、最小/最大耗时与行数。
  `Snapshot()` 返回快照，由调用方推送到自己的监控系统（Prometheus / OTLP / 日志均可）。

**OpenTelemetry 不内建，由用户通过 Hook 接入。** 这是有意为之：

- OTel 的 SDK + OTLP exporter 会带来约 28 条 go.mod 条目、90 个模块（含 gRPC、protobuf、genproto），
  而 goxdb 核心目前只有 2 个直接依赖。为所有用户承担这份依赖不划算。
- 用户自己写一个 `HookFuncs`（约 30 行）就能得到完整 tracing，并且能自行控制语义约定、
  属性名与采样策略——比框架内建更灵活。

```go
xdb.WithHook(xdb.HookFuncs{
    BeforeFunc: func(ctx context.Context, e *xdb.QueryEvent) context.Context {
        ctx, span := tracer.Start(ctx, "db."+e.Op.String())
        return context.WithValue(ctx, key{}, span)
    },
    AfterFunc: func(ctx context.Context, e *xdb.QueryEvent) {
        span := ctx.Value(key{}).(trace.Span)
        span.SetAttributes(attribute.String("db.statement", e.Query))
        if e.Err != nil { span.RecordError(e.Err) }
        span.End()
    },
})
```

模型级回调（`BeforeCreate` / `AfterFind` 等）作为**可选接口**提供，仅对模型级 API 生效，批量 `Update[T]` / `Delete[T]` 不触发——在文档中明确，避免“有时触发有时不触发”的困惑。

---

## 9. 测试策略

1. **SQL 金标准测试**：每个构建器用例对两个方言分别断言生成的 SQL 与参数（不需要数据库，跑得飞快）。
2. **集成测试**：`docker compose up -d --wait` 启动 MySQL 8.4 与 PostgreSQL 17。
   旧版本用 `docker compose --profile legacy up -d --wait` 启动 MySQL 8.0 / PostgreSQL 14，
   再以 `XDB_MYSQL_DSN` / `XDB_PG_DSN` 指向对应端口运行。
   已在 **MySQL 8.0 / 8.4 与 PostgreSQL 14 / 17 四个组合**上全部跑通。
3. **事务专项**：并发转账一致性、savepoint 回滚、panic 回滚、ctx 取消回滚、死锁/序列化失败重试、连接泄漏检测（测试结束断言 `db.Stats().InUse == 0`）。
4. **Fuzz**：`FuzzBind`（占位符改写，安全边界）+ `FuzzBuilder`（构建器渲染）。
   断言的不只是「不 panic」，还包括参数原样保留、占位符编号连续、改写确定性。
   已跑过数百万次执行无失败。
5. **Benchmark**：`bench_test.go` 覆盖渲染、结构体取值、单行/批量/标量扫描、元数据查找，
   以 `database/sql` 直连为基线。与 sqlx / GORM / bun 的横向对比尚未做。

**CI 现状**：暂无 CI。仓库为私有，Actions 按分钟计费；完整的矩阵（Go 版本 × 4 个数据库版本 + fuzz）
单次约消耗 22 分钟额度，开发期不划算。本地用下列命令做等价验证：

```bash
gofmt -l . && go vet ./... && go vet -tags integration ./... \
  && go test -race -count=1 ./... \
  && docker compose up -d --wait && go test -tags integration -count=1 ./tests/integration/... \
  && go test -run '^$' -fuzz FuzzBind -fuzztime 30s ./internal/sqlbind/ \
  && go test -run '^$' -fuzz FuzzBuilder -fuzztime 30s .
```

仓库转为公开（Actions 免费）或有协作者后再启用 CI。

---

## 10. 路线图

| 阶段 | 内容 | 产出 |
|---|---|---|
| **M0 地基** | DB / Executor / Tx（托管、savepoint、重试、AfterCommit、状态机）、Dialect 接口与两个实现、Raw、扫描、Hook、错误归一化 | 可以替代 sqlx 使用 |
| **M1 构建器** | 类型化列（无代码生成版）、Query/Select/Insert/Update/Delete、条件组合、JOIN、GroupBy/Having、子查询、CTE、Upsert、行锁、批量插入、ToSQL | 核心价值成型 |
| **M2 代码生成与关联** | `goxdb gen`、类型化描述符、Preload、多表 UPDATE/DELETE、INSERT…SELECT、自动时间戳 | 达到“非常易用” |
| **M3 增强** | 窗口函数、UNION、软删除、读写分离（事务固定主库）、方言扩展包（JSON/数组）、pgx 原生连接、DB-first 反向生成模型 | 生产完善 |

迁移（migration）**不进核心**，推荐配合 atlas / goose，后期可出独立子模块。

---

## 11. 已确认的决策

1. 类型安全：代码生成类型化列 + 无生成的字符串退路。
2. Executor 在入口绑定：`xdb.Query[User](db).All(ctx)`。
3. ORM 能力：关联预加载、模型回调进入 M2；软删除放 M3 作为可选插件。
4. 模块路径 `github.com/qiudaozhang/goxdb`，包名 `xdb`。
5. 最低 Go 版本 1.25（pgx v5 依赖要求）。

## 12. 阶段状态

### M0 地基（已完成）

DB / Executor / Raw / All·One·Iter·Scan / 结构体扫描 / Hook / 错误归一化 / 托管事务、savepoint、重试、回调、状态机、误用检测 / 手动事务 / MySQL 与 PostgreSQL 方言。

### M1 构建器（已完成）

- **列**：`Col[T]` / `NumberCol[T]` / `StringCol` / `TimeCol`，`xdb.T` + `xdb.C[...]` 的免生成退路可用。
- **查询**：`Query[T]` / `Select[T]`，`Where / Or / And / Not / If`，`Join / LeftJoin / RightJoin / CrossJoin`，`GroupBy / Having`，`Distinct`，`OrderBy / Limit / Offset / Page`，`Union / UnionAll`，`With / WithRecursive`。
- **表达式**：`In / NotIn / InSub / NotInSub / Between / IsNull / Like / ILike / HasPrefix·HasSuffix·Contains`，聚合 `Count / CountOf / CountDistinct / Sum / Avg / Min / Max`，`Coalesce / Lower / Upper / Add / Subtract / Multiply / Divide / Func / Cast / SQL`，`Exists / NotExists / Scalar`，`Derived` / `CTE`。
- **写入**：`Insert / InsertMany`（批量分批、自增回填、`Batch` / `Columns` / `Omit`），upsert `OnConflict(...).DoUpdate / DoUpdateSet / DoNothing`（含 `Excluded`），`Update / UpdateModel / Delete / DeleteModel`（`ErrMissingWhere` 护栏、`AllRows` 显式放行）。
- **锁与调试**：`ForUpdate / ForShare / NoWait / SkipLocked`（非事务内使用报 `ErrLockOutsideTx`），`ToSQL()`。
- 所有构建器不可变：每个方法返回新对象，可放心复用基础查询。

### M3（进行中）

- **窗口函数**：`RowNumber / Rank / DenseRank / PercentRank / CumeDist / Ntile / Lag / Lead / FirstValue / LastValue / NthValue`，以及窗口版聚合 `SumOver / CountOver / AvgOver / MinOver / MaxOver`。构造函数返回导出类型 `WindowFunc[T]`；可能产出 NULL 的 `Lag / Lead / NthValue` 结果类型为 `*T`，整数参数在构建期校验。窗口用 `xdb.Win().PartitionBy(...).OrderBy(...).Frame(...)` 描述；框架语法在 MySQL 8 与 PostgreSQL 之间一致，故不作方言翻译。窗口函数只能出现在投影与 ORDER BY 中，过滤需用派生表包一层（测试里覆盖了「每个客户金额最大的订单」这一典型写法）。
- **软删除**：字段加 `xdb:"deleted_at,softDelete"` 后，`Query / Select / 子查询`自动附加 `<col> IS NULL`，`Delete` 渲染为 `UPDATE ... SET <col> = CURRENT_TIMESTAMP WHERE <col> IS NULL AND ...`（带 Join 时为多表 UPDATE；重复删除不覆盖首次删除时间）。Preload 的关联表、UNION 各分支同样过滤。`Unscoped()` 用于绕过过滤（查询）或真正物理删除（删除）。软删除列属于**源表**，因此从 `from` 的模型推导而非结果类型——`xdb.Select[int64](nil, P.ID).From(P)` 也能正确过滤；手写的 `xdb.T("posts")` 没有模型信息，不会自动过滤（行为显式）。
- **方言扩展包**：`dialect/pg`（JSONB 路径 `->` / `->>`、`@>`、`jsonb_exists`、数组 `ANY / @> / && / array_length`）与 `dialect/my`（`JSON_EXTRACT` / `JSON_UNQUOTE` / `JSON_CONTAINS_PATH` / `JSON_CONTAINS`、`MATCH ... AGAINST`）。放在子包里，用户显式 import，**不污染通用 API**——用了方言专属特性在 import 上一眼可见。
  - MySQL 的 `??` 占位符转义对 `?` 操作符不友好，因此 `HasKey` 改用等价的 `jsonb_exists()` 函数。
- **`xdb.Lit[T]`**：把一个原样输出的 SQL 片段包装为表达式（不做参数绑定）。方言扩展包用它内联固定语法；因为它绕过参数绑定，文档中明确要求只用于代码常量，禁止传入用户输入。
- **读写分离**：`xdb.WithReplica(replicaPool)`。`Executor.session(readOnly bool)` 区分意图：只读查询（All / One / Iter / Count / Exists / Pluck）走副本，写操作与事务始终走主库。`INSERT ... RETURNING` 虽然返回结果集，但按写操作处理（`doQuery(..., readOnly=false)`）——这是个容易写错的地方，测试专门覆盖。`Raw` 语句只有首个关键字是 `SELECT` 时才走副本（`WITH` 因可含 DML 一律走主库）。`xdb.Primary(db)` 提供强一致读的逃生通道；同一连接池同时作为主库与副本时自动忽略副本配置。
- **DB-first 反向生成**：`goxdb -db postgres -dsn "$DSN" -dir ./model` 读 information_schema，生成 Go 模型结构体。方言通过 `dialect.Introspector`（`ListTables` / `DescribeTable`）提供 schema 读取，MySQL 与 PostgreSQL 各有一份实现。
  - 类型映射：`dialect.Column.Type` 存映射后的 Go 类型；可空列生成指针，保持「可空 = 可为 nil」。
  - 命名：复数表名单数化（`user_profiles` → `UserProfile`），`id` 字段固定为 `ID`；`status`/`analysis` 等以 `us`/`is` 结尾的词不做单数化（避免 `statu`）。
  - 结构体名冲突自动加序号；`created_at` 等时间列自动补 `autoCreateTime`，但只在类型确实是时间时生效。
  - 指定不存在的表会报错，而不是静默生成空文件。
- 待完成：pgx 原生连接（`database/sql` 之上的原生接口，属性能优化项，不影响功能）。

### 版本覆盖发现的问题

跑 MySQL 8.0 / PostgreSQL 14 矩阵时发现一个真实缺陷：手动事务在 **ctx 取消后 Commit 失败**时，
`commit()` 只把状态改成 `RolledBack` 而没有调用 `std.Rollback()`，`database/sql` 底层的连接
迟迟无法归还连接池，在 MySQL 8.0 上表现为偶发 `db.Stats().InUse > 0`。修复为在这些分支显式
回滚（对已结束的事务是幂等的）。这说明单版本验证会漏掉与版本相关的时序差异。

### M2（已完成）

- **代码生成 `goxdb gen`**：`go run github.com/qiudaozhang/goxdb/cmd/goxdb -dir ./model` 扫描带 `xdb` 标签的结构体，生成类型化的表与列描述符（`var U = struct{ xdb.Table; ID xdb.NumberCol[int64]; ... }`）。列类型由 Go 字段类型决定，`U.Age.Eq("abc")` 编译失败。`TableName()` 的值优先于 snake_case 复数推导；没有 `xdb` 标签的结构体（DTO）不生成。描述符变量名取类型名的大写字母（`User` → `U`），与类型名冲突时回退为 `<Type>Table`。
- **关联预加载 `Preload`**：`Query[T](db).Preload("Orders", conds...)`，支持 has_many / has_one / belongs_to。实现为主查询 → 收集键 → `WHERE fk IN (...)` 分批查询 → 内存回填，超长自动分批。未 Preload 的关联保持零值（不做隐式懒加载）；关联名不存在返回 `ErrUnknownRelation`。
- **模型级回调**：可选接口 `Creator / Created / Updater / Updated / Finder`（`BeforeCreate`、`AfterCreate`、`BeforeUpdate`、`AfterUpdate`、`AfterFind`）。只在「操作具体模型实例」的 API 上触发行级回调——`Insert / InsertMany / UpdateModel / 查询` 触发，批量 `Update[T] / Delete[T]` 不触发（无法知道会命中哪些行）。回调错误可用 `errors.Is(err, xdb.ErrCallback)` 识别，`BeforeCreate` 失败会阻止写入。
- **自动时间戳**：`xdb:"created_at,autoCreateTime"` / `xdb:"updated_at,autoUpdateTime"`。插入时填零值字段（显式赋值优先），`UpdateModel` 时把 `autoUpdateTime` 刷成当前时间并回写模型，`UpdateModel` 默认不再更新 `autoCreateTime` 列。时钟可用 `xdb.WithClock` 注入，便于测试与可预测写入。
- **多表写操作**：`Update[T]().Join(...)` / `Delete[T]().Join(...)`，由 `Features.UpdateJoinStyle` 翻译：MySQL 为 `UPDATE a JOIN b ON ... SET` / `DELETE a FROM a JOIN b ON ...`，PostgreSQL 为 `UPDATE a SET ... FROM b WHERE` / `DELETE FROM a USING b WHERE`（连接条件下沉到 WHERE）。
- **`INSERT ... SELECT`**：`xdb.InsertSelect[T](db, query).Columns(...)`；列按**列名**映射到目标模型（不像其他写入那样要求列属于目标表，因为归档表往往没有描述符），不填充自增与时间戳。
- **基准测试**（`bench_test.go`）：纯渲染、结构体→INSERT 参数、单行/90 行扫描、标量扫描、元数据查找，并以 `database/sql` 直连作为基线，用于持续盯住反射开销。

### 测试

- 金标准测试（`builder_test.go`）：**每个用例对 MySQL 与 PostgreSQL 两个方言都断言生成的 SQL 与参数**，缺一即失败（`golden` 在两个方言期望值均为空时直接报错，防止漏断言）。
- 集成测试（`tests/integration/builder_test.go`）：在真实 MySQL 8.4 / PostgreSQL 17 上验证插入回填、聚合与 JOIN、子查询与标量子查询、递归 CTE、upsert（含 `DoNothing` 的 MySQL 行别名歧义回归）、更新/删除护栏、`SKIP LOCKED` / `NOWAIT` 行锁、`CAST` 跨方言类型映射。
- 命令：`go test -race ./...`；`docker compose up -d --wait && go test -tags integration ./tests/integration/...`。

### M1 期间发现并修复的方言问题

1. **MySQL upsert `DoNothing` 生成歧义列**：`AS new ON DUPLICATE KEY UPDATE name = name` 在存在行别名时 MySQL 报 `Error 1052: Column 'name' in field list is ambiguous`。修复为两侧都带表限定符：`` `customers`.`name` = `customers`.`name` ``。
2. **`CAST` 的类型名不可移植**：MySQL 的 `CAST` 不接受 `BIGINT / INTEGER / BOOLEAN / VARCHAR / TIMESTAMP`，需映射为 `SIGNED / CHAR / DATETIME`。新增 `dialect.Dialect.CastType`，由方言吸收该差异；`xdb.Cast[int64]("BIGINT", x)` 在两个方言下都正确。
3. **聚合结果类型**：`SUM(bigint)` 在 MySQL 返回 `DECIMAL`、在 PostgreSQL 返回 `NUMERIC`，直接扫入 `int64` 会失败。这是 SQL 语义而非框架缺陷，需用 `xdb.Cast[int64]("BIGINT", ...)` 显式转换——文档中已注明。

