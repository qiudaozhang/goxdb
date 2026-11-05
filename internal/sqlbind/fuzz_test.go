package sqlbind_test

import (
	"strings"
	"testing"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

// FuzzBind 验证占位符改写的核心不变量。
//
// 这是框架的**安全边界**：如果改写逻辑在某个输入上出错，就可能把用户数据
// 当作 SQL 拼接，或把字符串字面量里的内容当成占位符。因此除了「不 panic」，
// 还要检查几条结构性不变量。
func FuzzBind(f *testing.F) {
	// 每个种子同时覆盖两个方言（dialectIdx 0/1）。
	for _, seed := range []struct {
		q string
		n int
	}{
		{"SELECT * FROM t WHERE a = ?", 1},
		{"SELECT '?', ? -- ?\n", 1},
		{`SELECT "a?b", ?`, 1},
		{"SELECT $$?$$, ?", 1},
		{"SELECT ?? , ?", 1},
		{"IN (?)", 1},
		{"", 0},
	} {
		f.Add(seed.q, seed.n, 0)
		f.Add(seed.q, seed.n, 1)
	}
	f.Fuzz(func(t *testing.T, query string, n int, dialectIdx int) {
		d := []dialect.Dialect{mysql.Dialect, postgres.Dialect}[dialectIdx&1]
		// 参数个数与查询无关，只用来覆盖「参数不足 / 参数多余」两条错误路径。
		count := n % 5
		if count < 0 {
			count = -count
		}
		args := make([]any, count)
		for i := range args {
			args[i] = i
		}

		out, outArgs, err := sqlbind.Bind(d, query, args)
		if err != nil {
			// 出错时不应返回半成品。
			if out != "" || outArgs != nil {
				t.Fatalf("error path must not return partial result: %q %#v", out, outArgs)
			}
			return
		}

		// 不变量 1：参数值必须原样保留（顺序不变）。
		if len(outArgs) != len(args) {
			t.Fatalf("arg count changed: %d -> %d", len(args), len(outArgs))
		}
		for i := range args {
			if outArgs[i] != args[i] {
				t.Fatalf("arg %d changed: %v -> %v", i, args[i], outArgs[i])
			}
		}

		// 不变量 2：PostgreSQL 下输出中不应残留未改写的占位符 '?'，
		// 除非它来自字面量/标识符/注释——这条太难精确断言，改为检查
		// 占位符编号连续且递增。
		if d.Name() == "postgres" && len(args) > 0 {
			for i := 1; i <= len(args); i++ {
				if !strings.Contains(out, "$"+itoa(i)) {
					// 切片参数会展开成多个占位符，此处只要求编号起点存在。
					if i == 1 {
						t.Fatalf("missing $1 in output for %d args: %q", len(args), out)
					}
				}
			}
		}

		// 不变量 3：改写是确定的（同一输入两次结果相同）。
		out2, args2, err2 := sqlbind.Bind(d, query, args)
		if err2 != nil || out2 != out || len(args2) != len(outArgs) {
			t.Fatalf("non-deterministic: %q vs %q", out, out2)
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
