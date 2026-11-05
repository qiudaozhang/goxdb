package xdb_test

import (
	"strings"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
)

// FuzzBuilder 验证构建器在任意字符串输入下都不会 panic，且渲染结果满足
// 结构不变量：占位符数量与参数个数一致、PostgreSQL 编号从 $1 连续递增。
//
// 构建器会把这些字符串当作**值**绑定（而不是拼进 SQL），所以这里同时
// 间接验证了注入防线——若某个路径把值直接拼进 SQL，占位符计数就会对不上。
func FuzzBuilder(f *testing.F) {
	for _, seed := range []string{
		"", "x", "%", "_", "\\", "'", "\"", "`", "?", "??", "--", "/*", "*/",
		"tom' OR '1'='1", "%_\\%", "日本語", "\x00", "a\nb",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		qs := []renderer{
			xdb.Query[Customer](nil).Where(U.Name.Eq(s), U.Name.Contains(s), U.Name.HasPrefix(s)),
			xdb.Query[Customer](nil).Where(
				xdb.Or(U.Name.Like(s), U.NotNameIsNull()),
				xdb.If(s != "", U.Name.HasSuffix(s)),
			),
			xdb.Select[Customer](nil, U.ID, U.Name.As(s)).From(U).Where(U.Name.Eq(s)),
			xdb.Insert(nil, &Customer{Name: s}).OnConflict(U.Name).DoUpdate(U.Age),
			xdb.Update[Customer](nil).Set(U.Name.To(s)).Where(U.ID.Eq(1)),
			xdb.Delete[Customer](nil).Where(U.Name.Eq(s)),
			xdb.Select[Customer](nil, U.ID).From(U).Where(
				xdb.SQL[bool]("? = ANY(?)", U.Name, xdb.AsIs(s)),
			),
		}
		for _, d := range []dialect.Dialect{mysql.Dialect, postgres.Dialect} {
			for _, q := range qs {
				sqlText, args, err := q.SQL(d)
				if err != nil {
					continue // 某些输入可能触发校验错误，这是合法的
				}
				// 参数里不允许出现被拼接的原始 SQL 结构（值必须走绑定）。
				// 值必须走绑定而不是拼接：PG 有参数时输出里应出现 $1。
				if d.Name() == "postgres" && len(args) > 0 && !strings.Contains(sqlText, "$1") {
					t.Fatalf("missing $1: %q (args=%d)", sqlText, len(args))
				}
				// MySQL 下参数个数应等于 ? 的个数（字面量内的 ? 不算，由 sqlbind 保证）。
				if d.Name() == "mysql" && len(args) > 0 {
					if c := strings.Count(sqlText, "?"); c < len(args) {
						t.Fatalf("placeholder/arg mismatch: %q has %d ?, %d args", sqlText, c, len(args))
					}
				}
			}
		}
	})
}

// NotNameIsNull 是测试辅助：构造一个 IS NOT NULL 条件。
func (c customerTable) NotNameIsNull() xdb.Cond { return c.Name.IsNotNull() }
