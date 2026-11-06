// Package scan 把结果集的行映射到 Go 值。
package scan

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/qiudaozhang/goxdb/internal/schema"
)

var (
	// ErrUnknownColumn：结果列找不到对应字段（严格模式）。
	ErrUnknownColumn = errors.New("goxdb: result column has no matching field")
	// ErrDuplicateColumn：多个结果列映射到同一个字段。
	ErrDuplicateColumn = errors.New("goxdb: multiple result columns map to the same field")
	// ErrColumnCount：标量目标要求结果恰好一列。
	ErrColumnCount = errors.New("goxdb: scalar target requires exactly one column")
)

// Rows 是扫描器用到的 *sql.Rows 方法子集。
type Rows interface {
	Columns() ([]string, error)
	Scan(dest ...any) error
}

type mode uint8

const (
	modeScalar mode = iota
	modeStruct
	modeMap
)

var mapType = reflect.TypeFor[map[string]any]()

// Plan 是预先计算好的“结果列 → Go 类型”映射。
//
// Plan 在构造后完全不可变，因此可以跨 goroutine、跨结果集共享；
// 每行扫描需要的可变缓冲放在 RowScanner 里。
type Plan struct {
	typ    reflect.Type // 目标类型（可以是指针类型）
	mode   mode
	cols   []string
	strict bool
	fields []*schema.Field // 每列对应的字段，nil 表示丢弃（宽松模式）
}

// planSet 是同一目标类型下的全部扫描计划。
// 每个类型通常只有一两种投影（整模型 / 自定义 DTO），因此线性比较切片
// 比构造字符串键更快，且完全不分配。
type planSet struct {
	mu    sync.RWMutex
	plans []*Plan
}

var planCache sync.Map // reflect.Type -> *planSet

// NewPlan 为目标类型 t 和结果列构建扫描计划；同一组合会被缓存并复用。
// 严格模式下每一列都必须映射到某个字段。
//
// 返回的 *Plan 不可变，可以在多个 RowScanner 之间共享。
func NewPlan(t reflect.Type, cols []string, strict bool) (*Plan, error) {
	v, _ := planCache.LoadOrStore(t, &planSet{})
	set := v.(*planSet)

	set.mu.RLock()
	for _, p := range set.plans {
		if p.strict == strict && equalCols(p.cols, cols) {
			set.mu.RUnlock()
			return p, nil
		}
	}
	set.mu.RUnlock()

	p, err := buildPlan(t, cols, strict)
	if err != nil {
		// 失败结果不缓存：列名错误通常是代码 bug，重复构造的代价可以接受，
		// 而缓存错误会在语义上把一次瞬时失败变成永久失败。
		return nil, err
	}
	// 复制列名：cols 由驱动提供，生命周期与内容都不受我们控制。
	p.cols = append([]string(nil), cols...)

	set.mu.Lock()
	defer set.mu.Unlock()
	// 双重检查：并发构建时保留先写入的那份，避免同一投影出现两个 Plan。
	for _, existing := range set.plans {
		if existing.strict == strict && equalCols(existing.cols, p.cols) {
			return existing, nil
		}
	}
	set.plans = append(set.plans, p)
	return p, nil
}

func equalCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func buildPlan(t reflect.Type, cols []string, strict bool) (*Plan, error) {
	p := &Plan{typ: t, strict: strict}
	base := t
	for base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	switch {
	case base == mapType:
		p.mode = modeMap
		return p, nil
	case schema.IsScalar(base):
		if len(cols) != 1 {
			return nil, fmt.Errorf("%w: target %s, got %d columns %v", ErrColumnCount, t, len(cols), cols)
		}
		p.mode = modeScalar
		return p, nil
	}
	if t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Pointer {
		return nil, fmt.Errorf("goxdb: unsupported scan target %s", t)
	}
	m, err := schema.Of(base)
	if err != nil {
		return nil, err
	}
	p.mode = modeStruct
	p.fields = make([]*schema.Field, len(cols))
	used := make(map[*schema.Field]string, len(cols))
	for i, c := range cols {
		f, ok := m.Lookup(c)
		if !ok {
			if strict {
				return nil, fmt.Errorf("%w: column %q, type %s", ErrUnknownColumn, c, base)
			}
			continue
		}
		if prev, dup := used[f]; dup {
			if strict {
				return nil, fmt.Errorf("%w: columns %q and %q -> %s.%s", ErrDuplicateColumn, prev, c, base, f.GoName)
			}
		}
		used[f] = c
		p.fields[i] = f
	}
	return p, nil
}

// Columns 返回计划对应的结果列。
func (p *Plan) Columns() []string { return p.cols }

// RowScanner 在 Plan 之上复用行扫描的临时缓冲。
// 它**不是**并发安全的：每个结果集应有自己的 RowScanner。
type RowScanner struct {
	plan *Plan
	ptrs []any // 复用给 rows.Scan 的地址切片
	// blank 是宽松模式下丢弃列的去处。必须是每扫描器独占的：
	// Plan 被全局共享，写在 Plan 上会引发并发竞态。
	blank any
}

// Scanner 返回一个可复用的行扫描器。
//
// 返回值而非指针：调用方通常把它作为结果集游标的一个字段持有，
// 按值返回可以省掉一次堆分配。
func (p *Plan) Scanner() RowScanner {
	rs := RowScanner{plan: p, blank: new(any)}
	if p.mode == modeStruct {
		rs.ptrs = make([]any, len(p.fields))
	}
	return rs
}

// ScanRow 把当前行扫描到 dst，dst 必须是计划目标类型的可设置值。
func (rs *RowScanner) ScanRow(rows Rows, dst reflect.Value) error {
	p := rs.plan
	switch p.mode {
	case modeScalar:
		return rows.Scan(dst.Addr().Interface())
	case modeMap:
		vals := make([]any, len(p.cols))
		ptrs := make([]any, len(p.cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		m := make(map[string]any, len(p.cols))
		for i, c := range p.cols {
			m[c] = vals[i]
		}
		v := reflect.ValueOf(m)
		if dst.Kind() == reflect.Pointer {
			pm := reflect.New(mapType)
			pm.Elem().Set(v)
			v = pm
		}
		dst.Set(v)
		return nil
	}

	sv := dst
	if sv.Kind() == reflect.Pointer {
		if sv.IsNil() {
			sv.Set(reflect.New(sv.Type().Elem()))
		}
		sv = sv.Elem()
	}
	for i, f := range p.fields {
		if f == nil {
			rs.ptrs[i] = rs.blank
			continue
		}
		rs.ptrs[i] = fieldByIndexAlloc(sv, f.Index).Addr().Interface()
	}
	return rows.Scan(rs.ptrs...)
}

// ScanRow 扫描一行；内部创建一个一次性的 RowScanner。
// 逐行调用时优先使用 Plan.Scanner() 复用缓冲。
func (p *Plan) ScanRow(rows Rows, dst reflect.Value) error {
	rs := p.Scanner()
	return rs.ScanRow(rows, dst)
}

// fieldByIndexAlloc 沿索引路径取字段，途中为 nil 的嵌入指针分配内存。
func fieldByIndexAlloc(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}
