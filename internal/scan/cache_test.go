package scan

import (
	"errors"
	"reflect"
	"testing"

	"github.com/qiudaozhang/goxdb/internal/schema"
)

func reflectTypeOf[T any]() reflect.Type       { return reflect.TypeFor[T]() }
func reflectValueOf[T any](p *T) reflect.Value { return reflect.ValueOf(p) }

// assign 把 v 写入数据库扫描目标；模拟 database/sql 的转换子集。
func assign(dest any, v any) error {
	if v == nil {
		return nil
	}
	switch d := dest.(type) {
	case *int64:
		*d = v.(int64)
	case *string:
		*d = v.(string)
	case *any:
		*d = v
	}
	return nil
}

type scannable struct {
	ID   int64   `xdb:"id"`
	Name string  `xdb:"name"`
	Note *string `xdb:"note"`
}

// fakeRows 记录收到的 Scan 目标，并按预设值填充。
type fakeRows struct {
	cols []string
	vals []any
	// dests 是最近一次 Scan 收到的目标地址。
	dests []any
	err   error
	// scanErr 让 Scan 返回错误，用于验证错误路径。
	scanErr error
}

func (r *fakeRows) Columns() ([]string, error) { return r.cols, nil }

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	r.dests = dest
	for i, d := range dest {
		if i >= len(r.vals) {
			break
		}
		if err := assign(d, r.vals[i]); err != nil {
			return err
		}
	}
	return nil
}

func TestPlanIsShared(t *testing.T) {
	cols := []string{"id", "name", "note"}
	a, err := NewPlan(reflectTypeOf[scannable](), cols, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPlan(reflectTypeOf[scannable](), cols, true)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("identical column sets must reuse the same cached Plan")
	}
}

func TestPlanCacheKeySeparatesColumns(t *testing.T) {
	a, err := NewPlan(reflectTypeOf[scannable](), []string{"id", "name"}, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPlan(reflectTypeOf[scannable](), []string{"id", "name", "note"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("different column sets must not share a Plan")
	}
	// 分隔符必须防止 ["a","b"] 与 ["a\x00b"] 撞键。
	c, err := NewPlan(reflectTypeOf[scannable](), []string{"id\x00name"}, false)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewPlan(reflectTypeOf[scannable](), []string{"id", "name"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if c == d {
		t.Fatal("column joining must not collide across different splits")
	}
}

func TestPlanErrorsAreNotCached(t *testing.T) {
	cols := []string{"definitely_missing"}
	if _, err := NewPlan(reflectTypeOf[scannable](), cols, true); !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("want ErrUnknownColumn, got %v", err)
	}
	if _, err := NewPlan(reflectTypeOf[scannable](), cols, true); !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("cached error must be re-derived, got %v", err)
	}
	// 宽松模式允许未知列，且不应被上面的失败影响。
	if _, err := NewPlan(reflectTypeOf[scannable](), cols, false); err != nil {
		t.Fatalf("lenient plan: %v", err)
	}
}

func TestScannerBufferIsReused(t *testing.T) {
	cols := []string{"id", "name", "note"}
	p, err := NewPlan(reflectTypeOf[scannable](), cols, true)
	if err != nil {
		t.Fatal(err)
	}
	rs := p.Scanner()
	rows := &fakeRows{cols: cols, vals: []any{int64(7), "tom", nil}}
	var got scannable
	if err := rs.ScanRow(rows, reflectValueOf(&got).Elem()); err != nil {
		t.Fatal(err)
	}
	first := rows.dests
	if got.ID != 7 || got.Name != "tom" || got.Note != nil {
		t.Fatalf("scan mismatch: %+v", got)
	}
	// 第二行必须复用同一份目标切片（否则批量扫描会每行分配一次）。
	var got2 scannable
	if err := rs.ScanRow(rows, reflectValueOf(&got2).Elem()); err != nil {
		t.Fatal(err)
	}
	if &first[0] != &rows.dests[0] {
		t.Fatal("RowScanner must reuse its dest buffer across rows")
	}
}

// TestScannerNotSharedAcrossPlans 验证 Plan 上不存可变状态：
// 两个 Scanner 同时使用同一个 Plan 时，丢弃列的落点必须互不干扰。
func TestScannerNotSharedAcrossPlans(t *testing.T) {
	p, err := NewPlan(reflectTypeOf[scannable](), []string{"id", "unknown_a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	rowsA := &fakeRows{cols: p.Columns(), vals: []any{int64(1), "a"}}
	rowsB := &fakeRows{cols: p.Columns(), vals: []any{int64(2), "b"}}
	var a, b scannable
	sa, sb := p.Scanner(), p.Scanner()
	if err := sa.ScanRow(rowsA, reflectValueOf(&a).Elem()); err != nil {
		t.Fatal(err)
	}
	if err := sb.ScanRow(rowsB, reflectValueOf(&b).Elem()); err != nil {
		t.Fatal(err)
	}
	// 两个扫描器丢弃列的落点必须是不同地址，否则并发使用会数据竞争。
	if rowsA.dests[1] == rowsB.dests[1] {
		t.Fatal("RowScanner must not share the discarded-column target")
	}
}

// TestSchemaOfIsShared 确认目标类型的元数据也来自缓存（避免每次查询重解析）。
func TestSchemaOfIsShared(t *testing.T) {
	a, err := schema.Of(reflectTypeOf[scannable]())
	if err != nil {
		t.Fatal(err)
	}
	b, err := schema.Of(reflectTypeOf[scannable]())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("schema metadata must be cached")
	}
}
