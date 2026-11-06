package xdb_test

import (
	"strings"
	"sync"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/dialect/postgres"
)

// TestQueryColumnsReused 验证 Query[T] 复用同一份列清单，
// 同时确认某个查询追加/替换列不会污染其它查询。
func TestQueryColumnsReused(t *testing.T) {
	base := xdb.Query[benchUser](nil)
	before, _, err := base.SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}

	// 派生出多个查询：它们改自己的 WHERE / ORDER BY，但选择列表应保持一致。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := base.Where(xdb.CNum[int64](nil, "id").Eq(int64(i))).OrderBy(xdb.CNum[int64](nil, "id").Asc())
			if _, _, err := q.SQL(postgres.Dialect); err != nil {
				t.Error(err)
			}
			// Columns 替换选择列表，绝不能改到共享切片。
			only := base.Columns(xdb.CNum[int64](nil, "id"))
			got, _, err := only.SQL(postgres.Dialect)
			if err != nil {
				t.Error(err)
				return
			}
			if strings.Contains(got, `"name"`) {
				t.Errorf("Columns() leaked into another query: %s", got)
			}
		}(i)
	}
	wg.Wait()

	after, _, err := base.SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("base query changed:\n before: %s\n after:  %s", before, after)
	}
}

// TestQueryColumnsAliasedTable 验证带别名的表不复用缓存列清单
// （别名会改变列的限定符，复用会串味）。
func TestQueryColumnsAliasedTable(t *testing.T) {
	u := xdb.TableOf[benchUser]().As("u")
	q := xdb.Select[benchUser](nil, xdb.C[int64](u, "id")).From(u)
	got, _, err := q.SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"u"."id"`) {
		t.Fatalf("aliased column lost its qualifier: %s", got)
	}
}

// TestGroupExpressionReuse 验证 And/Or 组合不会把调用方的切片写进查询：
// 追加到调用方切片后，已构建的条件必须保持不变。
func TestGroupExpressionReuse(t *testing.T) {
	id := xdb.CNum[int64](nil, "id")
	age := xdb.CNum[int](nil, "age")
	conds := []xdb.Cond{id.Eq(1), age.Ge(18)}
	cond := xdb.And(conds...)

	q1, _, err := xdb.Query[benchUser](nil).Where(cond).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	// 往原切片追加：容量已被截断，append 必然重新分配，不会影响 cond。
	conds = append(conds, id.Eq(2))
	q2, _, err := xdb.Query[benchUser](nil).Where(cond).SQL(postgres.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	if q1 != q2 {
		t.Fatalf("condition was mutated through the caller's slice:\n q1: %s\n q2: %s", q1, q2)
	}
}
