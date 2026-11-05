package sqlbind_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/qiudaozhang/goxdb/dialect/mysql"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
	"github.com/qiudaozhang/goxdb/internal/sqlbind"
)

type valuer struct{ v []int }

func (valuer) Value() (any, error) { return "x", nil }

func TestBindPostgres(t *testing.T) {
	d := postgres.Dialect
	cases := []struct {
		name  string
		in    string
		args  []any
		out   string
		oargs []any
	}{
		{"simple", "SELECT * FROM t WHERE a = ? AND b = ?", []any{1, "x"}, "SELECT * FROM t WHERE a = $1 AND b = $2", []any{1, "x"}},
		{"slice", "a IN (?) AND b = ?", []any{[]int{1, 2, 3}, 9}, "a IN ($1, $2, $3) AND b = $4", []any{1, 2, 3, 9}},
		{"bytes not expanded", "a = ?", []any{[]byte("ab")}, "a = $1", []any{[]byte("ab")}},
		{"valuer not expanded", "a = ?", []any{valuer{}}, "a = $1", []any{valuer{}}},
		{"asis", "a = ANY(?)", []any{sqlbind.AsIs{V: []int{1}}}, "a = ANY($1)", []any{[]int{1}}},
		{"string literal", "SELECT '?', ? FROM t", []any{1}, "SELECT '?', $1 FROM t", []any{1}},
		{"doubled quote", "SELECT 'it''s ?', ?", []any{1}, "SELECT 'it''s ?', $1", []any{1}},
		{"quoted ident", `SELECT "a?b", ?`, []any{1}, `SELECT "a?b", $1`, []any{1}},
		{"line comment", "SELECT ? -- what?\n, ?", []any{1, 2}, "SELECT $1 -- what?\n, $2", []any{1, 2}},
		{"block comment", "SELECT /* ? /* nested ? */ ? */ ?", []any{1}, "SELECT /* ? /* nested ? */ ? */ $1", []any{1}},
		{"dollar quote", "SELECT $fn$ ? $fn$, $$?$$, ?", []any{1}, "SELECT $fn$ ? $fn$, $$?$$, $1", []any{1}},
		{"positional untouched", "SELECT $1", nil, "SELECT $1", nil},
		{"escape string", `SELECT E'\'?', ?`, []any{1}, `SELECT E'\'?', $1`, []any{1}},
		{"jsonb ?? operator", "SELECT data ?? 'k' FROM t WHERE id = ?", []any{1}, "SELECT data ? 'k' FROM t WHERE id = $1", []any{1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, args, err := sqlbind.Bind(d, c.in, c.args)
			if err != nil {
				t.Fatal(err)
			}
			if out != c.out {
				t.Fatalf("query:\n got %q\nwant %q", out, c.out)
			}
			if len(args) != len(c.oargs) || (len(args) > 0 && !reflect.DeepEqual(args, c.oargs)) {
				t.Fatalf("args: got %#v want %#v", args, c.oargs)
			}
		})
	}
}

func TestBindMySQL(t *testing.T) {
	d := mysql.Dialect
	cases := []struct{ in, out string }{
		{"SELECT `a?`, ?", "SELECT `a?`, ?"},
		{`SELECT "x\"?", ?`, `SELECT "x\"?", ?`},
		{`SELECT 'x\'?', ?`, `SELECT 'x\'?', ?`},
		{"SELECT ? # c?\n", "SELECT ? # c?\n"},
		{"SELECT 1--?", "SELECT 1--?"}, // MySQL: "--" 后无空白不是注释
	}
	for _, c := range cases {
		out, _, err := sqlbind.Bind(d, c.in, []any{1})
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if out != c.out {
			t.Fatalf("got %q want %q", out, c.out)
		}
	}
	out, args, err := sqlbind.Bind(d, "a IN (?)", []any{[]string{"x", "y"}})
	if err != nil || out != "a IN (?, ?)" || len(args) != 2 {
		t.Fatalf("got %q %v %v", out, args, err)
	}
}

func TestBindErrors(t *testing.T) {
	d := postgres.Dialect
	if _, _, err := sqlbind.Bind(d, "a = ? AND b = ?", []any{1}); !errors.Is(err, sqlbind.ErrArgCount) {
		t.Fatalf("want ErrArgCount, got %v", err)
	}
	if _, _, err := sqlbind.Bind(d, "a = ?", []any{1, 2}); !errors.Is(err, sqlbind.ErrArgCount) {
		t.Fatalf("want ErrArgCount, got %v", err)
	}
	if _, _, err := sqlbind.Bind(d, "a IN (?)", []any{[]int{}}); !errors.Is(err, sqlbind.ErrEmptySlice) {
		t.Fatalf("want ErrEmptySlice, got %v", err)
	}
}
