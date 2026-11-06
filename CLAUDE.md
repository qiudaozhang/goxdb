# goxdb — Agent 指南

类型化、零魔法的 Go SQL 构建器 / 轻 ORM（包名 `xdb`），支持 MySQL 与 PostgreSQL。
设计细节见 [docs/DESIGN.md](docs/DESIGN.md)，用法见 [README.md](README.md)。

## 约定

- 代码注释写**中文**；错误字符串保持**英文**，前缀 `xdb:`（根包）或 `goxdb:`（internal / dialect）。
- 所有执行路径必须经过 `executor.go` 的 `doExec` / `doQuery`（Hook、错误归一化、事务占用检查都在那里）。
- 构建器（`Q` / `InsertQ` / `UpdateQ` / `DeleteQ`）是**不可变**的：方法先 `clone()`，切片追加前 `slices.Clip`。
- 构建期错误存进 `q.err`，渲染时返回；不要在构建方法里 panic。
- 新增渲染行为时同时补两种方言的 golden 测试（见 `builder_test.go` 的 `golden` 辅助函数）。

## 常用命令

```bash
go vet ./... && go test ./...                         # 单元测试（无需数据库，使用 internal/fakedb）
go test -race ./...
docker compose up -d --wait && go test -tags integration ./tests/integration/...   # 集成测试
cd benchmark && go test -bench . -benchmem            # 独立 module，与 sqlx/GORM/bun 对比
```

## 目录速览

| 路径 | 职责 |
|---|---|
| `db.go` `executor.go` `hook.go` `errors.go` | 句柄、执行管线、Hook、错误归一化 |
| `tx.go` | 托管/手动事务、savepoint、AfterCommit、ctx 传递 |
| `expr.go` `column.go` `func.go` `table.go` `window.go` `builder.go` | 表达式树与 SQL 渲染 |
| `query.go` `insert.go` `update.go` `insertselect.go` `preload.go` | 各语句构建器 |
| `raw.go` `scanhook.go` | Raw SQL、All/One/Iter、扫描入口（含生成代码的零反射扫描钩子） |
| `callback.go` `metrics.go` | 模型回调、指标 Hook |
| `internal/schema` | 结构体标签 → 模型元数据（缓存） |
| `internal/scan` | 结果列 → Go 值的扫描计划 |
| `internal/sqlbind` | `?` 占位符改写、切片展开（有 fuzz 测试） |
| `internal/codegen` | `goxdb gen`：model-first 生成描述符与扫描器 |
| `internal/reverse` + `dialect/*/introspect.go` | DB-first：从库表反向生成模型 |
| `dialect/{mysql,postgres}` | 方言实现；`dialect/{my,pg}` 是方言专属扩展（JSON、数组、全文检索） |
| `cmd/goxdb` | CLI 入口 |
| `internal/fakedb` | 测试用假驱动，记录收到的 SQL |

## 文件稳定性标记（避免每次全量扫描）

**基线提交：`0af4eb9`**（2026-11-05 全量审查 + 修复后复审）。

使用规则：
1. 🟢 文件**默认不必通读**，除非任务直接修改它或调试指向它。看接口用 `grep -n "^func \|^type "` 即可。
2. 若 `git log 0af4eb9.. -- <file>` 非空，或 `git diff 0af4eb9 -- <file>` 有改动，
   该文件的标记**失效**，按 🟡 对待；重新审查后更新本表与基线。
3. 🟡 文件改动前先读「注意事项」，避免重复踩坑或重复排查。

### 🟢 稳定

`rand.go` `errors.go` `executor.go` `hook.go` `callback.go` `column.go` `table.go` `expr.go` `builder.go`
`db.go` `tx.go` `metrics.go` `insertselect.go` `window.go` `func.go` `query.go` `raw.go` `insert.go` `scanhook.go`
`internal/sqlbind/*` `internal/fakedb/*` `internal/schema/*` `internal/scan/*`
`dialect/dialect.go` `dialect/mysql/mysql.go` `dialect/postgres/postgres.go` `dialect/my/*` `dialect/pg/*`
`cmd/goxdb/main.go`

### 🟡 注意事项（改动相关区域前先看）

| 文件 | 注意事项 |
|---|---|
| `internal/codegen/codegen.go` | 嵌入字段展开**必须与 `schema.walk` 同规则**（同名列层级最浅者胜、同层歧义全丢、跳过未导出嵌入指针），否则生成扫描器与运行时列集不一致；`codegen` 的 `TestEmbeddedFields` 与 `schema` 的 `TestEmbeddedConflictsMatchCodegen` 用同一组模型互相对照，改一边要同步另一边。生成器只收带 `xdb` 标签的字段，而运行时收全部导出字段——无标签字段的模型会回退到反射扫描（安全但慢） |
| `preload.go` | 关联键经 `relKey` 归一化（解引用指针、展开 `driver.Valuer`）；子表软删除过滤跟随主查询的 `Unscoped()` |
| `update.go` | 软删除 DELETE 渲染为 `UPDATE ... WHERE <col> IS NULL AND ...`，重复删除影响 0 行；带 Join 时与 `UpdateQ.Join` 共用 `buildUpdateTarget` / `buildUpdateJoinFrom` |
| `insert.go` / `internal/scan` / `scanhook.go` | 写入嵌入字段前用 `schema.AllocEmbedded(v, m.EmbedPtrs)` 分配 nil 嵌入指针；新增写字段的路径要照做 |

### ⚪ 未深入审查

`internal/reverse/*`、`dialect/*/introspect.go`、
`benchmark/`、`tests/integration/`。需要改动时按常规审查。
