package xdb

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/schema"
)

// Preload 声明需要在主查询之后批量加载的关联，避免 N+1。
//
// 参数是模型上的关联字段名（`xdb:"rel:..."` 标签），额外的 conds 会作为
// 子查询的 WHERE 条件，用于过滤关联数据：
//
//	users, err := xdb.Query[User](db).
//		Where(U.Status.Eq(1)).
//		Preload("Orders", xdb.C[string](xdb.T("orders"), "state").Eq("paid")).
//		All(ctx)
//
// 实现：主查询 → 收集主键（或外键）→ `WHERE fk IN (...)` 分批查询 → 内存回填。
// 关联不会被隐式懒加载：没有 Preload 的关联字段保持零值。
func (q *Q[T]) Preload(name string, conds ...Cond) *Q[T] {
	c := q.clone()
	c.preloads = append(slices.Clip(c.preloads), preloadSpec{name: name, conds: conds})
	return c
}

type preloadSpec struct {
	name  string
	conds []Cond
}

// runPreloads 为 rows（必须是可寻址的切片）加载所有声明的关联。
func (q *Q[T]) runPreloads(ctx context.Context, rows reflect.Value) error {
	if len(q.preloads) == 0 || rows.Len() == 0 {
		return nil
	}
	m, err := schema.Of(reflect.TypeFor[T]())
	if err != nil {
		return err
	}
	for _, spec := range q.preloads {
		rel, ok := m.Relations[spec.name]
		if !ok {
			return fmt.Errorf("%w: %s has no relation %q", ErrUnknownRelation, m.Type, spec.name)
		}
		if err := q.preloadOne(ctx, m, rel, spec.conds, rows); err != nil {
			return fmt.Errorf("xdb: preload %s.%s: %w", m.Type, spec.name, err)
		}
	}
	return nil
}

func (q *Q[T]) preloadOne(ctx context.Context, m *schema.Model, rel *schema.Relation, conds []Cond, rows reflect.Value) error {
	if q.ex == nil {
		return ErrNoExecutor
	}
	child, err := schema.Of(rel.Target)
	if err != nil {
		return err
	}
	// 关联两端参与匹配的列：has_many/has_one 用子表外键对主表主键；
	// belongs_to 用子表主键对主表外键。
	var parentCol, childCol string
	switch rel.Kind {
	case schema.HasMany, schema.HasOne:
		if len(m.PK) != 1 {
			return fmt.Errorf("%w: %s needs exactly one pk to preload", ErrNoPrimaryKey, m.Type)
		}
		parentCol, childCol = m.PK[0].Column, rel.FK
		if _, ok := child.Lookup(childCol); !ok {
			return fmt.Errorf("%w: %s has no column %q", ErrUnknownField, child.Type, childCol)
		}
	case schema.BelongsTo:
		if len(child.PK) != 1 {
			return fmt.Errorf("%w: %s needs exactly one pk to preload", ErrNoPrimaryKey, child.Type)
		}
		if f, ok := m.Lookup(rel.FK); !ok {
			return fmt.Errorf("%w: %s has no column %q", ErrUnknownField, m.Type, rel.FK)
		} else {
			parentCol = f.Column
		}
		childCol = child.PK[0].Column
	}

	parentField, ok := m.Lookup(parentCol)
	if !ok {
		return fmt.Errorf("%w: %s has no column %q", ErrUnknownField, m.Type, parentCol)
	}
	keys := make([]any, 0, rows.Len())
	seen := make(map[string]bool, rows.Len())
	for i := 0; i < rows.Len(); i++ {
		k := fieldValue(rows.Index(i), parentField.Index)
		if k == nil {
			continue
		}
		if s := fmt.Sprint(k); seen[s] {
			continue
		} else {
			seen[s] = true
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}

	// 分批查询子表，避免超出方言参数上限。
	limit := max(q.ex.Dialect().MaxParams()-len(conds)-8, 1)
	byKey := make(map[string][]reflect.Value, len(keys))
	for start := 0; start < len(keys); start += limit {
		chunk := keys[start:min(start+limit, len(keys))]
		vals, err := q.fetchRelated(ctx, child, childCol, chunk, conds)
		if err != nil {
			return err
		}
		cfield, _ := child.Lookup(childCol)
		for _, v := range vals {
			k := fmt.Sprint(fieldValue(v, cfield.Index))
			byKey[k] = append(byKey[k], v)
		}
	}

	return assignRelated(rows, m, rel, parentField, byKey)
}

// fetchRelated 执行子表查询并返回可寻址的目标类型值列表。
func (q *Q[T]) fetchRelated(ctx context.Context, child *schema.Model, col string, keys []any, conds []Cond) ([]reflect.Value, error) {
	d := q.ex.Dialect()
	b := newBuilder(d)
	b.write("SELECT ")
	for i, f := range child.Fields {
		if i > 0 {
			b.write(", ")
		}
		b.ident(f.Column)
	}
	b.write(" FROM ")
	Table{name: child.Table}.buildFrom(b)
	b.write(" WHERE ")
	keyCol := C[any](Table{name: child.Table}, col)
	where := And(append([]Cond{keyCol.In(keys...)}, conds...)...)
	where.build(b)
	q2, args, err := b.result()
	if err != nil {
		return nil, err
	}

	cursor, err := openCursor(ctx, stmt{ex: q.ex, render: func(dialect.Dialect) (string, []any, error) { return q2, args, nil }}, child.Type)
	if err != nil {
		return nil, err
	}
	var out []reflect.Value
	for cursor.rows.Next() {
		v := reflect.New(child.Type).Elem()
		if err := cursor.scan(v); err != nil {
			return nil, cursor.close(err)
		}
		out = append(out, v)
	}
	if err := cursor.close(nil); err != nil {
		return nil, err
	}
	return out, nil
}

// assignRelated 把按 key 归组的子行写回主行的关联字段。
func assignRelated(rows reflect.Value, m *schema.Model, rel *schema.Relation, parentField *schema.Field, byKey map[string][]reflect.Value) error {
	_ = m
	for i := 0; i < rows.Len(); i++ {
		row := rows.Index(i)
		dst := row.FieldByIndex(rel.Index)
		k := fmt.Sprint(fieldValue(row, parentField.Index))
		matches := byKey[k]

		switch rel.Kind {
		case schema.HasMany:
			elem := rel.Type.Elem()
			out := reflect.MakeSlice(rel.Type, 0, len(matches))
			for _, v := range matches {
				if elem.Kind() == reflect.Pointer {
					p := reflect.New(elem.Elem())
					p.Elem().Set(v)
					out = reflect.Append(out, p)
				} else {
					out = reflect.Append(out, v)
				}
			}
			dst.Set(out)
		case schema.HasOne, schema.BelongsTo:
			if len(matches) == 0 {
				dst.SetZero()
				continue
			}
			p := reflect.New(rel.Type.Elem())
			p.Elem().Set(matches[0])
			dst.Set(p)
		}
	}
	return nil
}
