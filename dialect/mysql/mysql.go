// Package mysql 提供 goxdb 的 MySQL 方言（MySQL 8.0.19+，upsert 使用行别名语法）。
//
// 导入本包即以 "mysql" 注册方言，并注册
// github.com/go-sql-driver/mysql 驱动：
//
//	import _ "github.com/qiudaozhang/goxdb/dialect/mysql"
//
// DSN 注意：需要 parseTime=true 才能把 DATETIME 扫描为 time.Time。
package mysql

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	mysqldrv "github.com/go-sql-driver/mysql"

	"github.com/qiudaozhang/goxdb/dialect"
)

// Dialect 是 MySQL 方言，无状态，可并发使用。
var Dialect dialect.Dialect = mysqlDialect{}

func init() { dialect.Register(Dialect, "mysql") }

type mysqlDialect struct{}

func (mysqlDialect) Name() string           { return "mysql" }
func (mysqlDialect) DriverName() string     { return "mysql" }
func (mysqlDialect) Placeholder(int) string { return "?" }
func (mysqlDialect) MaxParams() int         { return 65535 }

// castTypes 把可移植类型名映射为 MySQL 在 CAST 中接受的类型名。
// MySQL 的 CAST 目标类型集合很小，不接受 BIGINT / INTEGER / BOOLEAN / VARCHAR / TIMESTAMP。
var castTypes = map[string]string{
	"BIGINT":    "SIGNED",
	"INTEGER":   "SIGNED",
	"INT":       "SIGNED",
	"SMALLINT":  "SIGNED",
	"BOOLEAN":   "SIGNED",
	"BOOL":      "SIGNED",
	"TEXT":      "CHAR",
	"VARCHAR":   "CHAR",
	"TIMESTAMP": "DATETIME",
}

func (mysqlDialect) CastType(logical string) string {
	if v, ok := castTypes[strings.ToUpper(logical)]; ok {
		return v
	}
	return logical
}
func (mysqlDialect) QuoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func (mysqlDialect) AppendIdent(dst []byte, s string) []byte {
	dst = append(dst, '`')
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			dst = append(dst, '`')
		}
		dst = append(dst, s[i])
	}
	return append(dst, '`')
}

func (mysqlDialect) Features() dialect.Features {
	return dialect.Features{
		Upsert:              dialect.UpsertOnDuplicateKey,
		OffsetRequiresLimit: true,
		UpdateJoinStyle:     dialect.UpdateJoinInline,
		DeadlockRollsBackTx: true,
	}
}

func (mysqlDialect) Syntax() dialect.Syntax {
	return dialect.Syntax{
		BacktickIdent:         true,
		DoubleQuoteString:     true,
		BackslashEscape:       true,
		HashComment:           true,
		DashCommentNeedsSpace: true,
	}
}

// 需要归一化的 MySQL 服务端错误号。
const (
	erDupEntry            = 1062
	erDupEntryWithKeyName = 1586
	erNoReferencedRow     = 1216
	erRowIsReferenced     = 1217
	erRowIsReferenced2    = 1451
	erNoReferencedRow2    = 1452
	erBadNullError        = 1048
	erCheckConstraintFail = 3819
	erLockDeadlock        = 1213
	erLockWaitTimeout     = 1205
	erLockNowait          = 3572
)

var (
	dupKeyRe = regexp.MustCompile(`for key '([^']+)'`)
	fkNameRe = regexp.MustCompile("CONSTRAINT `([^`]+)`")
	checkRe  = regexp.MustCompile(`Check constraint '([^']+)'`)
)

func (mysqlDialect) ClassifyError(err error) (dialect.ErrorInfo, bool) {
	var me *mysqldrv.MySQLError
	if !errors.As(err, &me) {
		return dialect.ErrorInfo{}, false
	}
	info := dialect.ErrorInfo{Code: strconv.Itoa(int(me.Number)), Message: me.Message}
	switch me.Number {
	case erDupEntry, erDupEntryWithKeyName:
		info.Kind = dialect.KindUniqueViolation
		info.Constraint = submatch(dupKeyRe, me.Message)
		// MySQL 8 报告的是 "表.索引"，只保留索引名。
		if i := strings.LastIndexByte(info.Constraint, '.'); i >= 0 {
			info.Constraint = info.Constraint[i+1:]
		}
	case erNoReferencedRow, erRowIsReferenced, erRowIsReferenced2, erNoReferencedRow2:
		info.Kind = dialect.KindForeignKeyViolation
		info.Constraint = submatch(fkNameRe, me.Message)
	case erBadNullError:
		info.Kind = dialect.KindNotNullViolation
	case erCheckConstraintFail:
		info.Kind = dialect.KindCheckViolation
		info.Constraint = submatch(checkRe, me.Message)
	case erLockDeadlock:
		info.Kind = dialect.KindDeadlock
	case erLockWaitTimeout, erLockNowait:
		info.Kind = dialect.KindLockTimeout
	}
	return info, true
}

func submatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}
