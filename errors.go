package xdb

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/scan"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

// 查询结果相关错误。
var (
	// ErrNotFound：查询单行时没有任何结果。与 sql.ErrNoRows 是同一个值，
	// 因此 errors.Is(err, sql.ErrNoRows) 同样成立。
	ErrNotFound = sql.ErrNoRows
	// ErrMultipleRows：查询单行时返回了多于一行。
	ErrMultipleRows = errors.New("xdb: query returned more than one row")
	// ErrNoExecutor：语句没有绑定 Executor（例如作为子查询构造的语句被直接执行）。
	ErrNoExecutor = errors.New("xdb: statement has no executor")
	// ErrUnknownRelation：Preload 指定的关联字段不存在（或未带 rel 标签）。
	ErrUnknownRelation = errors.New("xdb: model has no such relation")
	// ErrInvalidDest：Scan 的目标不是非 nil 指针。
	ErrInvalidDest = errors.New("xdb: scan destination must be a non-nil pointer")

	ErrUnknownColumn   = scan.ErrUnknownColumn
	ErrDuplicateColumn = scan.ErrDuplicateColumn
	ErrColumnCount     = scan.ErrColumnCount
	ErrArgCount        = sqlbind.ErrArgCount
	ErrEmptySlice      = sqlbind.ErrEmptySlice
)

// 归一化后的数据库约束/并发错误，可用 errors.Is 判断。
var (
	ErrUniqueViolation     = errors.New("xdb: unique violation")
	ErrForeignKeyViolation = errors.New("xdb: foreign key violation")
	ErrNotNullViolation    = errors.New("xdb: not null violation")
	ErrCheckViolation      = errors.New("xdb: check violation")
	ErrDeadlock            = errors.New("xdb: deadlock")
	ErrSerialization       = errors.New("xdb: serialization failure")
	ErrLockTimeout         = errors.New("xdb: lock timeout")
	// ErrTxAborted：事务中先前的语句已失败，事务只能回滚（PostgreSQL 常见）。
	ErrTxAborted = errors.New("xdb: transaction aborted by a previous error")
)

// 事务相关错误。
var (
	// ErrTxDone：事务（或 savepoint）已结束。与 sql.ErrTxDone 是同一个值。
	ErrTxDone = sql.ErrTxDone
	// ErrCommitUnknown：COMMIT 发出后未能得到确定结果（如网络中断），
	// 事务可能已提交也可能未提交，调用方需要按幂等方式处理。
	ErrCommitUnknown = errors.New("xdb: commit outcome unknown")
	// ErrTxConcurrentUse：同一事务被并发使用，或上一个 Rows 尚未关闭就执行了新语句。
	ErrTxConcurrentUse = errors.New("xdb: transaction used concurrently or a previous Rows is still open")
	// ErrTxChildActive：事务存在尚未结束的嵌套事务（savepoint），此时只能使用内层 Tx。
	ErrTxChildActive = errors.New("xdb: transaction has an active nested transaction; use the inner Tx")
	// ErrNestedTxOptions：嵌套事务不能指定隔离级别、只读、重试等选项。
	ErrNestedTxOptions = errors.New("xdb: options are not allowed on nested transactions")
	// ErrRetryOnManualTx：手动事务无法自动重试。
	ErrRetryOnManualTx = errors.New("xdb: retry option is only supported by managed transactions")
	// ErrExecutorMisuse：在事务上下文中却使用了 *DB 执行语句（开启 WithStrictTx 时返回）。
	ErrExecutorMisuse = errors.New("xdb: statement executed on *DB inside a transaction context; use the Tx (or xdb.WithoutTx to opt out)")
)

var kindSentinel = map[dialect.ErrorKind]error{
	dialect.KindUniqueViolation:     ErrUniqueViolation,
	dialect.KindForeignKeyViolation: ErrForeignKeyViolation,
	dialect.KindNotNullViolation:    ErrNotNullViolation,
	dialect.KindCheckViolation:      ErrCheckViolation,
	dialect.KindDeadlock:            ErrDeadlock,
	dialect.KindSerialization:       ErrSerialization,
	dialect.KindLockTimeout:         ErrLockTimeout,
	dialect.KindTxAborted:           ErrTxAborted,
}

// DBError 是经过归一化的服务端错误。
//
//	var de *xdb.DBError
//	if errors.As(err, &de) { log.Println(de.Code, de.Constraint) }
//
// 同时可以直接 errors.Is(err, xdb.ErrUniqueViolation)。
type DBError struct {
	Kind       dialect.ErrorKind
	Code       string // 原生错误码
	Constraint string // 约束/索引名（服务端提供时）
	Message    string // 服务端错误信息
	Query      string // 触发错误的 SQL（不含参数值，避免泄露敏感数据）
	Err        error  // 原始驱动错误
}

func (e *DBError) Error() string {
	if e.Kind == dialect.KindUnknown {
		return "xdb: " + e.Err.Error()
	}
	return fmt.Sprintf("xdb: %s: %v", e.Kind, e.Err)
}

func (e *DBError) Unwrap() error { return e.Err }

// Is 让 errors.Is(err, ErrUniqueViolation) 等判断生效。
func (e *DBError) Is(target error) bool {
	s, ok := kindSentinel[e.Kind]
	return ok && s == target
}

// IsRetryable 判断错误是否适合整体重试事务（死锁、序列化失败）。
func IsRetryable(err error) bool {
	return errors.Is(err, ErrDeadlock) || errors.Is(err, ErrSerialization)
}

// wrapErr 用方言把驱动错误归一化为 *DBError；无法识别的错误原样返回。
func (db *DB) wrapErr(err error, query string) error {
	if err == nil {
		return nil
	}
	var de *DBError
	if errors.As(err, &de) {
		return err
	}
	info, ok := db.d.ClassifyError(err)
	if !ok {
		return err
	}
	return &DBError{
		Kind:       info.Kind,
		Code:       info.Code,
		Constraint: info.Constraint,
		Message:    info.Message,
		Query:      query,
		Err:        err,
	}
}
