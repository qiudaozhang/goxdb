# 更新日志

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。
`v0.x` 阶段的次版本号升级可能包含不兼容变更，届时会在「不兼容变更」一节列出迁移方法。

## [v0.1.0] - 2026-11-05

首个发布版本，以 MIT 许可证发布。一等支持 MySQL 8+ 与 PostgreSQL 14+，需要 Go 1.25+。

### 功能

- **执行层**：`DB` 句柄、Hook、错误归一化（唯一键冲突、死锁、序列化失败等可用 `errors.Is` 判断）、
  `Metrics` 指标 Hook。
- **事务**：托管事务 `db.Tx`（可用 `TxRetry` 在死锁 / 序列化失败时整体重试）、手动事务 `db.Begin`、
  savepoint、`AfterCommit`、通过 `context` 传递事务；提交结果未知时返回 `ErrCommitUnknown`。
- **类型化查询构建器**：`Query[T]` / `Select[T]`，JOIN、子查询、派生表、CTE（含递归）、UNION、
  `GROUP BY` / `HAVING`、行锁、`If` 动态条件；所有构建器不可变，可安全复用。
- **写操作**：`Insert` / `InsertMany`（按方言参数上限自动分批、自增主键回填）、upsert
  （`OnConflict().DoNothing / DoUpdate / DoUpdateSet`）、`Update` / `UpdateModel`、`Delete` / `DeleteModel`、
  多表 UPDATE / DELETE、`INSERT … SELECT`；不带 `WHERE` 的 UPDATE / DELETE 需显式 `AllRows()`。
- **模型**：结构体标签映射、嵌入结构体（含嵌入指针）、自动时间戳、软删除（`Unscoped()` 取消过滤）、
  模型回调（`BeforeCreate` / `AfterCreate` / `BeforeUpdate` / `AfterUpdate` / `AfterFind`）。
- **关联预加载**：`Preload`（`has_one` / `has_many` / `belongs_to`，支持嵌套与附加条件）。
- **窗口函数**：`RowNumber`、`Rank`、`Lag`、`Lead`、`NthValue`、`SumOver` 等，窗口用 `xdb.Win()` 描述。
- **Raw SQL**：`?` 占位符按方言改写、切片参数自动展开、`All` / `One` / `Iter` 扫描。
- **读写分离**：读走从库，写与事务固定主库；Raw 仅在首个关键字为 `SELECT` 时走从库。
- **方言扩展包**：`dialect/pg`（JSONB、数组）、`dialect/my`（JSON、全文检索）。
- **代码生成**（`cmd/goxdb`）：
  - model-first：生成类型化表 / 列描述符（`U.Age.Eq("abc")` 编译失败）与零反射扫描器；
  - DB-first：从现有库表反向生成模型（`-db` / `-dsn`）。
- 与 sqlx / GORM / bun 的端到端基准见 [benchmark/](benchmark/)。

### 发布前的加固（相对此前未打 tag 的提交）

如果你在用 `main` 上的早期提交，升级到 v0.1.0 时请注意以下变化。

#### 不兼容变更

- `NthValue` 的结果类型由 `T` 改为 `*T`：默认窗口框架下分区前 n-1 行的值为 NULL，原类型扫描会失败。
  迁移：把接收字段改为指针，例如 `Second int64` → `Second *int64`。
- 窗口函数构造器的返回类型改为导出的 `WindowFunc[T]`（原为未导出类型）。只调用 `.Over(...)` 的代码不受影响。
- `goxdb` CLI 遇到位置参数时报错退出。原先 `goxdb gen -dir ./model` 中的 `gen` 会让后面的 `-dir`
  被静默忽略，并生成到当前目录；正确写法是 `goxdb -dir ./model`。

#### 修复

- 批量 upsert：满额批次加上 `DoUpdateSet` 绑定的参数会超过 65535 的占位符上限，两种数据库都会拒绝执行。
- 预加载：按附加条件实际绑定的参数个数分批（一个 `In(...)` 就可能绑定上百个值）。
- 预加载：外键为指针或 `driver.Valuer`（如 `sql.NullInt64`）时能正确匹配；子表软删除行默认被过滤，
  并跟随主查询的 `Unscoped()`。
- 软删除：重复删除不再覆盖首次删除时间；`Count` / `Pluck` 保留 `Unscoped()`；UNION 右侧也会过滤；
  带 `Join` 的软删除不再丢失连接条件。
- 读写分离：带 `RETURNING` 等写操作的 Raw SQL 不再被路由到从库。
- 嵌入指针为 nil 时，插入回填主键不再 panic。
- 窗口函数：`Ntile(n)` / `NthValue(x, n)` 要求 `n >= 1`，`Lag` / `Lead` 的 offset 不能为负，
  非法值在渲染时返回错误，而不是生成数据库才报错的 SQL。
- `dialect/my`：非标识符键名（如 `first-name`）写成 `$."first-name"`；含反斜杠的常量改为绑定参数，
  不受 `NO_BACKSLASH_ESCAPES` 影响。
- `dialect/pg`：`Array().Contains()` / `Overlaps()` 不带元素时渲染 `'{}'`，原先的 `ARRAY[]` 无法推断类型。
- 代码生成：
  - 列类型引用其它包（如 `sql.NullString`、`json.RawMessage`）时生成文件带上 import，原先无法编译；
  - 列集与运行时规则一致（无标签导出字段、`xdb:"-"`、嵌入冲突消解、作为单列的嵌入），
    否则生成的扫描器会静默回退到反射；
  - 嵌入其它包的结构体时打印警告且不生成扫描器；
  - 描述符变量名冲突时回退为 `<Type>Table`。
- 反向生成：`updated_at` 带上 `autoUpdateTime`；修正非法或冲突的字段名；`TIME` / `YEAR` / `money` /
  `timetz` / `interval` 映射为驱动真正能扫描进去的类型。
- 手动事务 `Commit` 失败后会回滚并归还连接。

[v0.1.0]: https://github.com/qiudaozhang/goxdb/releases/tag/v0.1.0
