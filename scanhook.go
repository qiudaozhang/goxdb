package xdb

import (
	"reflect"

	"github.com/qiudaozhang/goxdb/internal/scan"
)

// RowScanner 是可选的零反射扫描钩子，由模型类型自己实现。
//
// 默认扫描路径用反射把结果列映射到结构体字段：每行、每列都要经过
// reflect.Value 取址。对读热点（尤其批量读）来说这是纯开销。
//
// `goxdb gen` 会为带 xdb 标签的模型生成这个方法，把列顺序与字段地址在编译期
// 固定下来，逐行扫描退化为直接写入 &u.Field。未生成该方法的类型自动走反射路径。
//
//	func (u *User) NewRowScanner(columns []string) (xdb.ScanRows, error)
//
// 约定：
//   - columns 是驱动返回的结果列名，顺序与 (*sql.Rows).Scan 一致；
//   - 返回 (nil, nil) 或 (nil, err) 都会让框架回退到反射路径，
//     因此列不匹配（例如自定义投影）仍由严格模式给出准确的错误信息；
//   - 返回的 ScanRows 只应被单个结果集使用，且不要在相邻行之间保留目标地址。
type RowScanner interface {
	NewRowScanner(columns []string) (ScanRows, error)
}

// ScanRows 为当前行准备 (*sql.Rows).Scan 的目标地址。
type ScanRows interface {
	// ScanDest 返回当前行的扫描目标，dst 是指向模型值的指针。
	// 返回切片的长度必须等于结果列数。
	ScanDest(dst any) ([]any, error)
}

// scanHook 是行扫描的来源：要么是生成的零反射扫描器，要么是反射计划。
type scanHook interface {
	// bind 用结果列完成一次性绑定；返回 false 表示需要回退到反射路径。
	bind(cols []string) bool
	ScanRow(rows scan.Rows, dst reflect.Value) error
}

// newScanHook 为类型 t 选择扫描实现。
// 类型实现了 RowScanner 时返回钩子，调用方在首次见列时绑定；
// 绑定失败会回退到反射，因此这里只做「有没有」的静态判断。
func newScanHook(t reflect.Type) (scanHook, bool) {
	rt := reflect.TypeFor[RowScanner]()
	ptr := t
	if t.Kind() != reflect.Pointer {
		ptr = reflect.PointerTo(t)
	}
	if !ptr.Implements(rt) {
		return nil, false
	}
	return &hookScanner{typ: ptr}, true
}

// compile-time 断言。
var _ scanHook = (*hookScanner)(nil)

// hookScanner 把生成的扫描器适配到内部游标协议。
// 每个结果集一个实例，绑定的扫描器随之复用。
type hookScanner struct {
	typ     reflect.Type // 实现了 RowScanner 的指针类型
	scanner ScanRows
}

// bind 在首次扫描前用结果列构造模型专属扫描器。
// 返回 ok=false 表示需要回退到反射路径。
func (h *hookScanner) bind(cols []string) (ok bool) {
	v := reflect.New(h.typ.Elem())
	rs, ok2 := v.Interface().(RowScanner)
	if !ok2 {
		return false
	}
	sc, err := rs.NewRowScanner(cols)
	if err != nil || sc == nil {
		return false
	}
	h.scanner = sc
	return true
}

func (h *hookScanner) ScanRow(rows scan.Rows, dst reflect.Value) error {
	// 与反射路径保持同一套语义：目标可能是指针（All[*T]），
	// 此时先分配再解引用，让生成代码始终只处理 *T。
	sv := dst
	if sv.Kind() == reflect.Pointer {
		if sv.IsNil() {
			sv.Set(reflect.New(sv.Type().Elem()))
		}
		sv = sv.Elem()
	}
	ptrs, err := h.scanner.ScanDest(sv.Addr().Interface())
	if err != nil {
		return err
	}
	return rows.Scan(ptrs...)
}
