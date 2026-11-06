package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
)

// Introspect 实现 dialect.Introspector，用于 DB-first 反向生成模型。
var Introspect dialect.Introspector = introspector{}

type introspector struct{}

// introspection 查询只读取用户 schema（public），排除视图与系统表。
const listTablesSQL = `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = current_schema()
  AND table_type = 'BASE TABLE'
ORDER BY table_name`

const describeTableSQL = `
SELECT
	c.column_name,
	-- 统一成带精度的可读类型名，便于生成 Go 类型与注释
	CASE
		WHEN c.data_type = 'character varying' AND c.character_maximum_length IS NOT NULL
			THEN 'varchar(' || c.character_maximum_length || ')'
		WHEN c.data_type = 'numeric' AND c.numeric_precision IS NOT NULL
			THEN 'numeric(' || c.numeric_precision || ',' || COALESCE(c.numeric_scale, 0) || ')'
		ELSE c.data_type
	END AS data_type,
	c.is_nullable = 'YES' AS nullable,
	COALESCE(c.column_default, '') AS column_default,
	COALESCE(pk.is_pk, false) AS is_pk,
	(COALESCE(c.is_identity = 'YES', false)
	 OR COALESCE(c.column_default LIKE 'nextval(%', false)) AS is_identity
FROM information_schema.columns c
LEFT JOIN (
	SELECT kcu.column_name, true AS is_pk
	FROM information_schema.table_constraints tc
	JOIN information_schema.key_column_usage kcu
	  ON tc.constraint_name = kcu.constraint_name
	 AND tc.table_schema = kcu.table_schema
	WHERE tc.table_schema = current_schema()
	  AND tc.table_name = $1
	  AND tc.constraint_type = 'PRIMARY KEY'
) pk ON pk.column_name = c.column_name
WHERE c.table_schema = current_schema()
  AND c.table_name = $1
ORDER BY c.ordinal_position`

func (introspector) ListTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, listTablesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (introspector) DescribeTable(ctx context.Context, db *sql.DB, table string) (*dialect.Table, error) {
	rows, err := db.QueryContext(ctx, describeTableSQL, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t := &dialect.Table{Name: table}
	for rows.Next() {
		var (
			c        dialect.Column
			dataType string
			isIdent  bool
		)
		if err := rows.Scan(&c.Name, &dataType, &c.Nullable, &c.Default, &c.IsPK, &isIdent); err != nil {
			return nil, err
		}
		c.DataType = dataType
		c.AutoInc = isIdent
		c.Type = GoType(dataType, c.Nullable)
		if c.IsPK {
			t.PK = append(t.PK, c.Name)
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.Columns) == 0 {
		return nil, fmt.Errorf("postgres: table %q has no columns (or does not exist)", table)
	}
	return t, nil
}

// GoType 把 PostgreSQL 类型映射为最合适的 Go 类型。
//
// nullable 为 true 时返回指针类型：这保持了「数据库可空 = Go 可为 nil」的直觉，
// 避免把 NULL 静默扫成零值。
func GoType(pgType string, nullable bool) reflect.Type {
	base := pgType
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = base[:i]
	}
	var t reflect.Type
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "bigint", "int8", "bigserial":
		t = reflect.TypeFor[int64]()
	case "integer", "int", "int4", "serial":
		t = reflect.TypeFor[int32]()
	case "smallint", "int2", "smallserial":
		t = reflect.TypeFor[int16]()
	case "boolean", "bool":
		t = reflect.TypeFor[bool]()
	case "real", "float4":
		t = reflect.TypeFor[float32]()
	case "double precision", "float8":
		t = reflect.TypeFor[float64]()
	case "numeric", "decimal":
		// 精度敏感的金额应使用 decimal 库；这里给出 float64 并保留原始类型注释。
		t = reflect.TypeFor[float64]()
	case "money":
		// money 的文本形式带货币符号（"$1.00"），扫不进浮点数。
		t = reflect.TypeFor[string]()
	case "text", "varchar", "character varying", "char", "character", "citext", "uuid", "inet", "cidr", "macaddr":
		t = reflect.TypeFor[string]()
	case "bytea":
		t = reflect.TypeFor[[]byte]()
	case "date", "timestamp", "timestamptz", "timestamp with time zone",
		"timestamp without time zone":
		t = reflect.TypeFor[time.Time]()
	case "time", "timetz", "time without time zone", "time with time zone", "interval":
		// 一天中的时刻 / 时间间隔没有日期部分，驱动不会把它们转成 time.Time。
		t = reflect.TypeFor[string]()
	case "json", "jsonb":
		// 用 []byte 承载原始 JSON，调用方可自定义 scanner。
		t = reflect.TypeFor[[]byte]()
	case "array", "user-defined", "enum":
		t = reflect.TypeFor[string]()
	default:
		if strings.HasSuffix(base, "[]") {
			t = reflect.TypeFor[[]string]()
		} else {
			t = reflect.TypeFor[string]()
		}
	}
	if nullable {
		return reflect.PointerTo(t)
	}
	return t
}
