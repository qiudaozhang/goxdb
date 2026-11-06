// Package dialect 定义每种数据库方言需要实现的契约。
//
// 方言负责吸收不同数据库之间所有语法与行为差异（占位符、标识符引号、
// 词法规则、错误码等），使 goxdb 的其余部分保持与数据库无关。
package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Dialect 描述一种 SQL 数据库方言。
type Dialect interface {
	// Name 返回方言的规范名称，如 "mysql"、"postgres"。
	Name() string
	// DriverName 返回 sql.Open 使用的 database/sql 驱动名。
	DriverName() string
	// Placeholder 返回第 n 个（从 1 开始）绑定参数的占位符。
	Placeholder(n int) string
	// QuoteIdent 为单个标识符（表名/列名/别名）加引号。
	QuoteIdent(name string) string
	// AppendIdent 与 QuoteIdent 等价，但把结果追加到 dst 并返回。
	// 构建器走这条路径可以避免每个标识符都产生一个临时字符串。
	AppendIdent(dst []byte, name string) []byte
	// Syntax 返回安全扫描原始 SQL 文本所需的词法规则。
	Syntax() Syntax
	// Features 描述方言支持的 SQL 特性，构建器据此生成不同的语句。
	Features() Features
	// MaxParams 返回单条语句允许的最大绑定参数个数。
	MaxParams() int
	// CastType 把可移植的类型名映射为方言原生类型名，用于 CAST(x AS t)。
	// 例如 MySQL 不接受 "BIGINT"，需映射为 "SIGNED"。
	// 未列入映射的名字原样返回，以允许方言专属类型。
	CastType(logical string) string
	// ClassifyError 识别驱动错误。ok 为 true 表示这是服务端错误，
	// 即服务端已收到请求并明确拒绝（区别于网络/客户端故障）。
	ClassifyError(err error) (info ErrorInfo, ok bool)
}

// Syntax 描述扫描 SQL 文本查找占位符时需要关心的词法规则：
// 字符串字面量、带引号的标识符、注释中的内容都必须原样保留。
type Syntax struct {
	// BacktickIdent：`ident` 形式的标识符（MySQL）。
	BacktickIdent bool
	// DoubleQuoteString："..." 是字符串而非标识符（MySQL 默认）。
	DoubleQuoteString bool
	// BackslashEscape：'...' 中反斜杠作为转义符（MySQL 默认）。
	BackslashEscape bool
	// EscapeStringPrefix：E'...' 字符串支持反斜杠转义（PostgreSQL）。
	EscapeStringPrefix bool
	// DollarQuote：$tag$ ... $tag$ 美元引用字符串（PostgreSQL）。
	DollarQuote bool
	// HashComment：# 开始单行注释（MySQL）。
	HashComment bool
	// DashCommentNeedsSpace："--" 后必须跟空白才算注释（MySQL）。
	DashCommentNeedsSpace bool
}

// UpsertStyle 是 upsert 的语法风格。
type UpsertStyle uint8

const (
	// UpsertOnConflict：INSERT ... ON CONFLICT (...) DO UPDATE / DO NOTHING（PostgreSQL）。
	UpsertOnConflict UpsertStyle = iota + 1
	// UpsertOnDuplicateKey：INSERT ... AS new ON DUPLICATE KEY UPDATE（MySQL 8.0.19+）。
	UpsertOnDuplicateKey
)

// UpdateJoinStyle 是多表 UPDATE / DELETE 的语法风格。
type UpdateJoinStyle uint8

const (
	// UpdateJoinInline：UPDATE a JOIN b ON ... SET ...（MySQL）。
	UpdateJoinInline UpdateJoinStyle = iota + 1
	// UpdateJoinFrom：UPDATE a SET ... FROM b WHERE ...（PostgreSQL）。
	UpdateJoinFrom
)

// Features 描述方言支持的 SQL 特性。
type Features struct {
	// Returning：支持 INSERT/UPDATE/DELETE ... RETURNING。
	Returning bool
	// Upsert：upsert 语法风格。
	Upsert UpsertStyle
	// OffsetRequiresLimit：OFFSET 必须与 LIMIT 一起出现（MySQL）。
	OffsetRequiresLimit bool
	// ILike：原生支持 ILIKE；不支持时用 LOWER(x) LIKE LOWER(y) 模拟。
	ILike bool
	// UpdateJoinStyle：多表 UPDATE / DELETE 的语法风格（两者相同：MySQL 都是 JOIN 内联，
	// PostgreSQL 都是 FROM / USING 子句）。
	UpdateJoinStyle UpdateJoinStyle
	// DeadlockRollsBackTx：死锁时服务端回滚**整个**事务（MySQL InnoDB）。此后连接处于
	// 自动提交模式，后续语句会各自提交，因此框架必须把事务标记为中止。
	// PostgreSQL 只中止当前（子）事务，可以 ROLLBACK TO SAVEPOINT 恢复，不设置此项。
	DeadlockRollsBackTx bool
}

// ErrorKind 是与数据库无关的服务端错误分类。
type ErrorKind int

const (
	KindUnknown ErrorKind = iota
	KindUniqueViolation
	KindForeignKeyViolation
	KindNotNullViolation
	KindCheckViolation
	KindDeadlock
	KindSerialization
	KindLockTimeout
	// KindTxAborted 表示事务因先前语句失败而无法再提交
	// （如 PostgreSQL 的 "in failed sql transaction"）。
	KindTxAborted
)

var kindNames = [...]string{
	KindUnknown:             "unknown",
	KindUniqueViolation:     "unique violation",
	KindForeignKeyViolation: "foreign key violation",
	KindNotNullViolation:    "not null violation",
	KindCheckViolation:      "check violation",
	KindDeadlock:            "deadlock",
	KindSerialization:       "serialization failure",
	KindLockTimeout:         "lock timeout",
	KindTxAborted:           "transaction aborted",
}

func (k ErrorKind) String() string {
	if int(k) >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("ErrorKind(%d)", int(k))
}

// ErrorInfo 是归一化后的服务端错误描述。
type ErrorInfo struct {
	Kind       ErrorKind
	Code       string // 原生错误码：MySQL 错误号或 PostgreSQL SQLSTATE
	Constraint string // 服务端报告的约束/索引名
	Message    string // 服务端错误信息
}

var (
	regMu    sync.RWMutex
	registry = map[string]Dialect{}
)

// Register 以一个或多个名称注册方言，通常在方言包的 init 中调用。
func Register(d Dialect, names ...string) {
	regMu.Lock()
	defer regMu.Unlock()
	if len(names) == 0 {
		names = []string{d.Name()}
	}
	for _, n := range names {
		if _, dup := registry[n]; dup {
			panic("goxdb/dialect: Register called twice for " + n)
		}
		registry[n] = d
	}
}

// Lookup 按名称查找已注册的方言。
func Lookup(name string) (Dialect, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

// Names 返回所有已注册的方言名（已排序）。
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Column 是数据库中的一列，用于 DB-first 反向生成模型。
type Column struct {
	Name     string
	DataType string // 方言原生类型，如 "varchar(50)"、"int8"
	Nullable bool
	Default  string
	IsPK     bool
	AutoInc  bool
	Type     reflect.Type // 映射后的 Go 类型
}

// Table 是数据库中的一张表。
type Table struct {
	Name    string
	Columns []Column
	PK      []string
}

// Introspector 由支持 DB-first 的方言实现：列出表、读取列信息。
//
// 查询用 database/sql 执行（传入 *sql.DB），因此方言包需要自行编写
// information_schema 查询。返回值用于生成 Go 模型代码。
type Introspector interface {
	// ListTables 返回表名（已排除系统表与视图）。
	ListTables(ctx context.Context, db *sql.DB) ([]string, error)
	// DescribeTable 返回表的列信息。
	DescribeTable(ctx context.Context, db *sql.DB, table string) (*Table, error)
}
