package mysql

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

const listTablesSQL = `
SELECT table_name
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_type = 'BASE TABLE'
ORDER BY table_name`

const describeTableSQL = `
SELECT
	column_name,
	column_type,
	is_nullable = 'YES' AS nullable,
	COALESCE(column_default, '') AS column_default,
	column_key = 'PRI' AS is_pk,
	extra LIKE '%auto_increment%' AS is_identity
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name = ?
ORDER BY ordinal_position`

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
			c       dialect.Column
			colType string
			isIdent bool
		)
		if err := rows.Scan(&c.Name, &colType, &c.Nullable, &c.Default, &c.IsPK, &isIdent); err != nil {
			return nil, err
		}
		c.DataType = colType
		c.AutoInc = isIdent
		c.Type = GoType(colType, c.Nullable)
		if c.IsPK {
			t.PK = append(t.PK, c.Name)
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(t.Columns) == 0 {
		return nil, fmt.Errorf("mysql: table %q has no columns (or does not exist)", table)
	}
	return t, nil
}

// GoType 把 MySQL 类型映射为最合适的 Go 类型。
//
// nullable 为 true 时返回指针类型；unsigned 整数映射到对应的无符号 Go 类型。
func GoType(mysqlType string, nullable bool) reflect.Type {
	base := strings.ToLower(mysqlType)
	if i := strings.IndexAny(base, "( "); i >= 0 {
		base = base[:i]
	}
	unsigned := strings.Contains(strings.ToLower(mysqlType), "unsigned")

	var t reflect.Type
	switch base {
	case "tinyint":
		if unsigned {
			t = reflect.TypeFor[uint8]()
		} else {
			t = reflect.TypeFor[int8]()
		}
	case "smallint":
		if unsigned {
			t = reflect.TypeFor[uint16]()
		} else {
			t = reflect.TypeFor[int16]()
		}
	case "mediumint", "int", "integer":
		if unsigned {
			t = reflect.TypeFor[uint32]()
		} else {
			t = reflect.TypeFor[int32]()
		}
	case "bigint":
		if unsigned {
			t = reflect.TypeFor[uint64]()
		} else {
			t = reflect.TypeFor[int64]()
		}
	case "float":
		t = reflect.TypeFor[float32]()
	case "double", "real", "decimal", "numeric":
		t = reflect.TypeFor[float64]()
	case "bool", "boolean":
		t = reflect.TypeFor[bool]()
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext",
		"enum", "set", "json", "uuid":
		t = reflect.TypeFor[string]()
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob",
		"bit", "geometry", "point", "linestring", "polygon":
		t = reflect.TypeFor[[]byte]()
	case "date", "datetime", "timestamp", "time", "year":
		t = reflect.TypeFor[time.Time]()
	default:
		t = reflect.TypeFor[string]()
	}
	if nullable {
		return reflect.PointerTo(t)
	}
	return t
}
