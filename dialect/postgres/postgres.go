// Package postgres 提供 goxdb 的 PostgreSQL 方言（PostgreSQL 14+）。
//
// 导入本包即以 "postgres"、"postgresql"、"pgx" 注册方言，
// 底层使用 github.com/jackc/pgx/v5/stdlib 驱动：
//
//	import _ "github.com/qiudaozhang/goxdb/dialect/postgres"
package postgres

import (
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" database/sql 驱动

	"github.com/qiudaozhang/goxdb/dialect"
)

// Dialect 是 PostgreSQL 方言，无状态，可并发使用。
var Dialect dialect.Dialect = pgDialect{}

func init() { dialect.Register(Dialect, "postgres", "postgresql", "pgx") }

type pgDialect struct{}

func (pgDialect) Name() string             { return "postgres" }
func (pgDialect) DriverName() string       { return "pgx" }
func (pgDialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }
func (pgDialect) MaxParams() int           { return 65535 }

// CastType 原样返回：PostgreSQL 的 CAST 接受标准类型名（BIGINT / INTEGER / TEXT / BOOLEAN ...）。
func (pgDialect) CastType(logical string) string { return logical }
func (pgDialect) QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (pgDialect) Features() dialect.Features {
	return dialect.Features{
		Returning:       true,
		Upsert:          dialect.UpsertOnConflict,
		ILike:           true,
		UpdateJoinStyle: dialect.UpdateJoinFrom,
	}
}

func (pgDialect) Syntax() dialect.Syntax {
	return dialect.Syntax{
		EscapeStringPrefix: true,
		DollarQuote:        true,
	}
}

func (pgDialect) ClassifyError(err error) (dialect.ErrorInfo, bool) {
	// PostgreSQL 中对已中止事务执行 COMMIT 会静默变成 ROLLBACK，
	// pgx 将其报告为 ErrTxCommitRollback，这是确定的失败。
	if errors.Is(err, pgx.ErrTxCommitRollback) {
		return dialect.ErrorInfo{Kind: dialect.KindTxAborted, Message: err.Error()}, true
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return dialect.ErrorInfo{}, false
	}
	info := dialect.ErrorInfo{Code: pe.Code, Constraint: pe.ConstraintName, Message: pe.Message}
	switch pe.Code {
	case "23505":
		info.Kind = dialect.KindUniqueViolation
	case "23503":
		info.Kind = dialect.KindForeignKeyViolation
	case "23502":
		info.Kind = dialect.KindNotNullViolation
	case "23514":
		info.Kind = dialect.KindCheckViolation
	case "40P01":
		info.Kind = dialect.KindDeadlock
	case "40001":
		info.Kind = dialect.KindSerialization
	case "55P03":
		info.Kind = dialect.KindLockTimeout
	case "25P02":
		info.Kind = dialect.KindTxAborted
	}
	return info, true
}
