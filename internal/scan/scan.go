// Package scan 把结果集的行映射到 Go 值。
package scan

import (
	"errors"
	"fmt"
	"reflect"

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
// Plan 不可变，可在同一结果集的每一行复用。
type Plan struct {
	typ    reflect.Type // 目标类型（可以是指针类型）
	mode   mode
	cols   []string
	fields []*schema.Field // 每列对应的字段，nil 表示丢弃（宽松模式）
}

// NewPlan 为目标类型 t 和结果列构建扫描计划。
// 严格模式下每一列都必须映射到某个字段。
func NewPlan(t reflect.Type, cols []string, strict bool) (*Plan, error) {
	p := &Plan{typ: t, cols: cols}
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

// ScanRow 把当前行扫描到 dst，dst 必须是计划目标类型的可设置值。
func (p *Plan) ScanRow(rows Rows, dst reflect.Value) error {
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
	ptrs := make([]any, len(p.fields))
	for i, f := range p.fields {
		if f == nil {
			ptrs[i] = new(any)
			continue
		}
		ptrs[i] = fieldByIndexAlloc(sv, f.Index).Addr().Interface()
	}
	return rows.Scan(ptrs...)
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
