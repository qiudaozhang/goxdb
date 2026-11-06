# goxdb

优雅、易用、事务严谨的 Go 通用数据库框架，一等支持 MySQL 8+ 与 PostgreSQL 14+。

> 当前进度：**M0** 地基、**M1** 查询构建器、**M2**（代码生成、关联预加载、模型回调、自动时间戳、
> 多表写、INSERT…SELECT）、**M3**（窗口函数、软删除、方言扩展包、读写分离、DB-first 反向生成）已完成。
> 设计与实现机制见 [docs/DESIGN.md](docs/DESIGN.md)，版本变化见 [CHANGELOG.md](CHANGELOG.md)。

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

### 多表更新 / 删除

统一写法，方言自动翻译（MySQL 用 JOIN 内联，PostgreSQL 用 FROM / USING）：

```go
xdb.Update[Order](db).
	Join(User, User.ID.EqCol(Order.UserID)).
	Set(Order.State.To("frozen")).
	Where(User.Status.Eq(0)).
	Exec(ctx)

xdb.Delete[Order](db).Join(User, User.ID.EqCol(Order.UserID)).Where(User.Status.Eq(0)).Exec(ctx)

xdb.InsertSelect[ArchivedOrder](db,
	xdb.Select[ArchivedOrder](nil, Order.UserID, Order.Amount, Order.State).
		From(Order).Where(Order.State.Eq("old")),
).Columns(Order.UserID, Order.Amount, Order.State).Exec(ctx)
```

### 代码生成

在模型包内执行，生成类型化的表与列描述符：

```bash
go run github.com/qiudaozhang/goxdb/cmd/goxdb -dir ./model
# 或写在文件里：//go:generate go run github.com/qiudaozhang/goxdb/cmd/goxdb
```

反过来也支持 **DB-first**：从已有数据库反向生成模型结构体。

```bash
goxdb -db postgres -dsn "$DSN" -dir ./model -reverse-out models.go
goxdb -db mysql -dsn "$DSN" -dir ./model -tables users,orders -prefix t_
```

生成的代码包含 `xdb` 标签、`TableName()`，可空列用指针，`created_at` 自动补时间戳标签。

给定：

```go
type User struct {
	ID        int64     `xdb:"id,pk,auto"`
	Name      string    `xdb:"name"`
	Age       int       `xdb:"age"`
	CreatedAt time.Time `xdb:"created_at"`
}
```

生成 `xdb_gen.go`：

```go
var U = struct {
	xdb.Table
	ID        xdb.NumberCol[int64]
	Name      xdb.StringCol
	Age       xdb.NumberCol[int]
	CreatedAt xdb.TimeCol
}{}

// 零反射扫描器：把列顺序与字段地址固定在编译期。
func (u *User) NewRowScanner(columns []string) (xdb.ScanRows, error) { ... }
```

列类型由 Go 字段类型决定，因此 `U.Age.Eq("abc")` **编译失败**。

嵌入结构体（值或导出的指针，如 `Base` / `*Base`）会被展开，描述符沿用 Go 的字段提升：
`Base.ID` 生成为 `U.ID`；与模型自身的字段重名时改为 `U.BaseID`。同名列的取舍与运行时一致——
层级最浅者胜出，同层重复则都不作为列。嵌入的 `*Base` 为 nil 时，扫描与主键回填会自动分配。
与运行时一样，非结构体类型、实现 `sql.Scanner` 的结构体（含 `time.Time`、`sql.NullString`）以及
标签写了列名的嵌入字段算作**一列**而不展开。列类型引用其它包（如 `sql.NullString`、`json.RawMessage`）时，
生成文件会带上对应的 import。

生成器只解析语法树，看不到**其它包里结构体**的字段：嵌入这样的结构体时，生成器会打印警告，
已知列照常生成描述符，但该模型不生成 `NewRowScanner`（扫描走反射，结果正确）。
需要零反射扫描时，把这些字段写在本包的结构体里再嵌入。

#### 零反射扫描

生成器还会为每个模型生成 `NewRowScanner`，实现 `xdb.RowScanner`。
框架检测到该方法后，逐行扫描不再经过反射——直接把结果列写进 `&u.Field`：

```go
users, err := xdb.All[User](ctx, xdb.Query[User](db).Where(U.Age.Ge(18)))
// User 有生成代码时自动走零反射路径，否则回退到反射，API 完全一致
```

- 列名与顺序不匹配（例如自定义投影）时**自动回退**到反射路径，
  因此 `Select[DTO]`、`Pluck`、部分列查询都不受影响
- 未生成代码的类型同样走反射，无需一次性迁移
- 实测（fake 驱动，90 行扫描）：`19200ns → 15600ns`，**约 19% 提速**，
  每行省去 `reflect.Value.Field` 取址与接口装箱

在真实数据库上这部分收益会被网络往返淹没（单行查询约 110μs，
扫描只占其中约 0.6μs），批量读与高 QPS 场景才明显。
对比数据见 [benchmark/README.md](benchmark/README.md)。

### 关联预加载

```go
type User struct {
	ID     int64   `xdb:"id,pk,auto"`
	Orders []Order `xdb:"rel:has_many,fk:user_id"`
}

type Order struct {
	ID     int64  `xdb:"id,pk,auto"`
	UserID int64  `xdb:"user_id"`
	State  string `xdb:"state"`
	User   *User  `xdb:"rel:belongs_to,fk:user_id"`
}

// 两次查询完成，不会 N+1
users, err := xdb.Query[User](db).Preload("Orders").All(ctx)

// 带过滤条件的关联
users, err = xdb.Query[User](db).Preload("Orders", O.State.Eq("paid")).All(ctx)

orders, err := xdb.Query[Order](db).Preload("User").All(ctx) // belongs_to
```

未 Preload 的关联保持零值——**不做隐式懒加载**。

### 模型回调

实现可选接口即可自动生效：

```go
func (u *User) BeforeCreate(ctx context.Context) error { return u.Validate() }
func (u *User) AfterFind(ctx context.Context) error   { return u.Decrypt() }
```

| API | BeforeCreate | AfterCreate | BeforeUpdate | AfterUpdate | AfterFind |
|---|---|---|---|---|---|
| `Insert` / `InsertMany` | ✓ | ✓ | | | |
| `UpdateModel` | | | ✓ | ✓ | |
| `Query` / `Select` | | | | | ✓ |
| `Update[T]` / `Delete[T]`（批量） | | | | | |

批量按条件的写操作不触发行级回调。回调失败可用 `errors.Is(err, xdb.ErrCallback)` 判断。

### 自动时间戳

```go
type User struct {
	ID        int64     `xdb:"id,pk,auto"`
	Name      string    `xdb:"name"`
	CreatedAt time.Time `xdb:"created_at,autoCreateTime"`
	UpdatedAt time.Time `xdb:"updated_at,autoUpdateTime"`
}
```

插入时只填充零值字段（显式赋值优先）；`UpdateModel` 会把 `updated_at` 刷新为当前时间并回写模型，
且不会修改 `created_at`。需要写入特定时间就用 `Update[T]().Set(U.UpdatedAt.To(t))`。
时钟可用 `xdb.WithClock(fn)` 注入，便于测试。

### 窗口函数

```go
w := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.Amount.Desc())

// 排名
rows, err := xdb.Select[RankedOrder](db,
	O.ID, O.Amount,
	xdb.RowNumber().Over(w).As("rn"),
).From(O).All(ctx)

// 累计求和需要显式框架
runW := xdb.Win().PartitionBy(O.CustomerID).OrderBy(O.ID.Asc()).
	Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
running, err := xdb.Select[RankedOrder](db,
	xdb.SumOver[int64](O.Amount).Over(runW).As("running"),
).From(O).All(ctx)

// 过滤窗口结果必须用派生表（SQL 求值顺序所限）
inner := xdb.Select[RankedOrder](nil, O.CustomerID, O.Amount,
	xdb.RowNumber().Over(w).As("rn")).From(O)
d := xdb.Derived("r", inner)
top, err := xdb.Select[RankedOrder](db, d.Star()).From(d).Where(xdb.C[int64](d, "rn").Eq(1)).All(ctx)
```

可用：`RowNumber / Rank / DenseRank / PercentRank / CumeDist / Ntile / Lag / Lead /
FirstValue / LastValue / NthValue`，窗口版聚合 `SumOver / CountOver / AvgOver / MinOver / MaxOver`。
构造函数返回 `xdb.WindowFunc[T]`，可以存进变量或作参数传递。`Lag / Lead / NthValue` 的结果类型是 `*T`：
越过分区边界的行、或默认框架下还看不到第 n 行时值为 NULL。`Ntile(n)`、`NthValue(x, n)` 要求 `n >= 1`，
`Lag / Lead` 的 offset 不能为负（0 表示省略，用数据库默认的 1），非法值在渲染时返回错误。

### 方言专属能力

通用 API 只覆盖可移植的 SQL。方言专属能力放在子包里，**显式 import = 明确表示这段代码不可移植**。

```go
import (
	"github.com/qiudaozhang/goxdb/dialect/pg" // PostgreSQL：JSONB / 数组
	"github.com/qiudaozhang/goxdb/dialect/my" // MySQL：JSON / 全文检索
)
```

```go
// PostgreSQL
xdb.Query[Doc](db).Where(
	pg.JSONB(D.Meta).Text("status").Eq("active"),
	pg.JSONB(D.Meta).Field("a").Field("b").Text("c").Eq("x"),
	pg.JSONB(D.Meta).HasKey("k"),
	pg.JSONB(D.Meta).Contains(`{"a":1}`),
	pg.Array(D.Tags).Any(xdb.V("red")),
	pg.Array(D.Tags).Contains(xdb.V("a"), xdb.V("b")),
)

// MySQL
xdb.Query[Doc](db).Where(
	my.JSON(D.Meta).Text("status").Eq("active"),
	my.JSON(D.Meta).Field("a").Index(0).Text("name").Eq("x"),
	my.Match(D.Body, "hello", "IN BOOLEAN MODE"),
)
```

键名与 JSON 片段作为常量内联进 SQL（与生成列 / 函数索引上的表达式逐字一致），已按方言转义：
MySQL 中非标识符键名写成 `$."first-name"`，含反斜杠的常量改为绑定参数（不受 `NO_BACKSLASH_ESCAPES` 影响）。
它们应来自代码常量，不要拼接用户输入。`pg.Array(...).Contains()` / `Overlaps()` 不带元素时分别恒真 / 恒假。

### 软删除

```go
type Post struct {
	ID        int64      `xdb:"id,pk,auto"`
	Title     string     `xdb:"title"`
	DeletedAt *time.Time `xdb:"deleted_at,softDelete"`
}

xdb.Query[Post](db).All(ctx)                  // 自动附加 deleted_at IS NULL
xdb.Query[Post](db).Unscoped().All(ctx)       // 也能看到已删除的行

xdb.Delete[Post](db).Where(P.ID.Eq(1)).Exec(ctx)          // → UPDATE ... SET deleted_at = CURRENT_TIMESTAMP WHERE deleted_at IS NULL AND ...
xdb.Delete[Post](db).Unscoped().Where(P.ID.Eq(1)).Exec(ctx) // → 真正物理删除
```

软删除过滤对子查询、UNION 各分支、`Count` / `Pluck` 与 Preload 的关联表同样生效（`Unscoped()` 一并关闭）。
它由**源表**推导，所以 `xdb.Select[int64](nil, P.ID).From(P)` 也会过滤；
手写的 `xdb.T("posts")` 不含模型信息，不会自动过滤。已删除的行不会被再次标记，重复删除影响 0 行。

### 读写分离

```go
db, err := xdb.Open("postgres", primaryDSN,
	xdb.WithReplica(replicaPool), // *sql.DB，可为 nil
)

xdb.Query[User](db).All(ctx)            // → 副本
xdb.Query[User](db).Count(ctx)          // → 副本
xdb.Insert(db, &u).Exec(ctx)            // → 主库
db.Tx(ctx, func(...) error { ... })     // → 主库（事务固定在主库）

// 强一致读：显式走主库
xdb.Query[User](xdb.Primary(db)).Where(U.ID.Eq(u.ID)).One(ctx)
```

`INSERT ... RETURNING` 虽返回结果集，但按写操作路由到主库。主从延迟意味着刚写入的数据可能读不到，
需要强一致时用事务或 `xdb.Primary(db)`。

### 可观测性

```go
db, _ := xdb.Open("postgres", dsn,
	xdb.WithHook(xdb.LogSlow(slog.Default(), 200*time.Millisecond)),
	xdb.WithHook(metrics), // 见下
)
```

内置（零外部依赖）：

- `LogQueries` / `LogSlow`：slog 结构化日志与慢查询，默认不记录参数值
- `NewMetrics()`：按「操作 × 成功/失败」聚合次数、耗时均值/最小/最大与行数

```go
m := xdb.NewMetrics()
for _, s := range m.Snapshot() {
	log.Printf("%s %s: n=%d avg=%v max=%v rows=%d", s.Op, s.Result, s.Count, s.Avg, s.Max, s.Rows)
}
```

**OpenTelemetry 不内建**，用 `HookFuncs` 自己接（约 30 行）即可——框架不替所有用户承担
OTel SDK + OTLP exporter 的依赖（约 28 条 go.mod 条目 / 90 个模块）。

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

旧版本兼容性验证（MySQL 8.0 / PostgreSQL 14）：

```bash
docker compose --profile legacy up -d --wait
XDB_MYSQL_DSN='root:xdb@tcp(127.0.0.1:23306)/xdb?parseTime=true' \
XDB_PG_DSN='postgres://postgres:xdb@127.0.0.1:25432/xdb?sslmode=disable' \
  go test -tags integration ./tests/integration/...
```

已验证矩阵：MySQL 8.0 / 8.4 × PostgreSQL 14 / 17。

连接串可用 `XDB_MYSQL_DSN` / `XDB_PG_DSN` 覆盖。

Fuzz（占位符改写是安全边界，构建器渲染是注入防线）：

```bash
go test -run '^$' -fuzz FuzzBind -fuzztime 30s ./internal/sqlbind/
go test -run '^$' -fuzz FuzzBuilder -fuzztime 30s .
```

基准分两套：

```bash
# 框架自身开销：假驱动，隔离网络与服务端
go test -run XXX -bench . ./...

# 端到端：与 sqlx / GORM / bun 在同一连接池、同一张表上对比
docker compose up -d --wait
cd benchmark && go test -run XXX -bench . -benchtime 1s -count 3 ./...
```

对比结果、口径说明与解读见 [benchmark/README.md](benchmark/README.md)。

覆盖 13 个场景（单行/批量读、分页、计数、预加载、JOIN 聚合、单条/批量写、
Upsert、事务、并发读写、纯渲染），MySQL 与 PostgreSQL 各一轮。一句话结论：

- **真实往返主导一切**：PostgreSQL 上 xdb 与最优实现普遍相差 5% 以内
- **纯 SQL 生成**：xdb 0.60μs，与 bun 同档，比 GORM（1.20μs）快一倍
- **分配**：单行读 55 次（优化前 80）；批量读 459 次，低于 sqlx 的 635 与 GORM 的 972
- **JOIN 聚合**：此前测得慢 16–25%，查明是 MySQL 统计信息过期造成的假象；基准灌数后显式 `ANALYZE`，修正后与 sqlx / bun 同档
- **MySQL 上 bun 快 1.5–2 倍**来自驱动参数插值，加 `interpolateParams=true` 后四者持平
- **PG 上 bun 插入慢 2 倍**来自它的 `RETURNING *` 设计
- **`goxdb gen` 零反射扫描**：隔离测量下扫描本身快 19%，真实库里批量读约 4%

## 许可证

[MIT](LICENSE)
