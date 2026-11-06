package xdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
	"github.com/qiudaozhang/goxdb/internal/schema"
)

const (
	upsertOnDuplicateKey = dialect.UpsertOnDuplicateKey
	// mysqlRowAlias 是 MySQL upsert 中“待插入行”的别名：INSERT ... AS xdb_new ON DUPLICATE KEY UPDATE。
	mysqlRowAlias = "xdb_new"
)

var (
	// ErrConflictTarget：PostgreSQL 的 ON CONFLICT DO UPDATE 必须指定冲突列。
	ErrConflictTarget = errors.New("xdb: OnConflict(...).DoUpdate requires conflict target columns")
	// ErrForeignColumn：使用了不属于语句目标表的列。
	ErrForeignColumn = errors.New("xdb: column does not belong to the target table")
	// ErrUnknownField：列名在模型中找不到对应字段。
	ErrUnknownField = errors.New("xdb: column has no matching model field")
)

// InsertQ 是 INSERT 语句。与 Q 一样是不可变的。
type InsertQ[T any] struct {
	ex       Executor
	model    *schema.Model
	table    Table
	rows     []reflect.Value // 可寻址的结构体值，用于回填自增主键
	cols     []Column        // 显式指定的列；nil 表示默认（全部非自增列）
	omit     []Column
	batch    int
	conflict *conflictSpec
	err      error
}

type conflictSpec struct {
	target    []Column
	doNothing bool
	set       []Assignment
}

// Insert 插入一行或多行（传入指针）。成功后自增主键（带 auto 选项的字段）会被回填：
// PostgreSQL 使用 RETURNING，MySQL 使用 LastInsertId（多行时按 auto_increment_increment 推算）。
//
//	err := xdb.Insert(db, &u).Exec(ctx)
func Insert[T any](ex Executor, rows ...*T) *InsertQ[T] {
	q := newInsert[T](ex)
	for i, r := range rows {
		if r == nil {
			q.fail(fmt.Errorf("xdb: Insert row #%d is nil", i))
			continue
		}
		q.rows = append(q.rows, reflect.ValueOf(r).Elem())
	}
	return q
}

// InsertMany 插入切片中的全部元素，自增主键会回填到切片元素中。
func InsertMany[T any](ex Executor, rows []T) *InsertQ[T] {
	q := newInsert[T](ex)
	rv := reflect.ValueOf(rows)
	for i := range rows {
		q.rows = append(q.rows, rv.Index(i))
	}
	return q
}

func newInsert[T any](ex Executor) *InsertQ[T] {
	q := &InsertQ[T]{ex: ex}
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		q.err = fmt.Errorf("xdb: Insert requires a struct model, got %s", t)
		return q
	}
	m, err := schema.Of(t)
	if err != nil {
		q.err = err
		return q
	}
	q.model, q.table = m, Table{name: m.Table}
	return q
}

func (q *InsertQ[T]) fail(err error) {
	if q.err == nil {
		q.err = err
	}
}

func (q *InsertQ[T]) clone() *InsertQ[T] { c := *q; return &c }

// Columns 只插入指定的列。显式列出自增列时会插入其值，且不再回填。
func (q *InsertQ[T]) Columns(cols ...Column) *InsertQ[T] { c := q.clone(); c.cols = cols; return c }

// Omit 在默认列的基础上排除指定列（例如交给数据库默认值的列）。
func (q *InsertQ[T]) Omit(cols ...Column) *InsertQ[T] {
	c := q.clone()
	c.omit = append(slices.Clip(c.omit), cols...)
	return c
}

// Batch 设置每条 INSERT 语句最多包含的行数（还会受方言参数个数上限约束）。
//
// 注意：多条语句不会自动包在事务中（goxdb 不隐式开启事务），需要原子性时请在事务中执行。
func (q *InsertQ[T]) Batch(n int) *InsertQ[T] { c := q.clone(); c.batch = n; return c }

// OnConflict 开始 upsert 子句。target 为冲突判定的唯一列（PostgreSQL 的 ON CONFLICT (...)）；
// MySQL 的 ON DUPLICATE KEY 会对任何唯一键冲突生效，target 在 MySQL 上不起作用。
func (q *InsertQ[T]) OnConflict(target ...Column) *ConflictQ[T] {
	return &ConflictQ[T]{q: q, target: target}
}

// ConflictQ 用于指定冲突时的动作。
type ConflictQ[T any] struct {
	q      *InsertQ[T]
	target []Column
}

// DoNothing 冲突时忽略该行。此时不回填自增主键（被忽略的行没有新 ID）。
func (c *ConflictQ[T]) DoNothing() *InsertQ[T] {
	q := c.q.clone()
	q.conflict = &conflictSpec{target: c.target, doNothing: true}
	return q
}

// DoUpdate 冲突时用本次插入的值覆盖指定列。
func (c *ConflictQ[T]) DoUpdate(cols ...Column) *InsertQ[T] {
	set := make([]Assignment, len(cols))
	for i, col := range cols {
		set[i] = Assignment{col: col, val: excludedNode{name: col.colName()}}
	}
	return c.DoUpdateSet(set...)
}

// DoUpdateSet 冲突时执行自定义赋值，可用 xdb.Excluded 引用本次插入的值。
func (c *ConflictQ[T]) DoUpdateSet(set ...Assignment) *InsertQ[T] {
	q := c.q.clone()
	if len(set) == 0 {
		q.fail(errors.New("xdb: DoUpdate requires at least one column"))
	}
	q.conflict = &conflictSpec{target: c.target, set: set}
	return q
}

// resolveCols 计算实际插入的字段，并报告自增字段是否需要回填。
func (q *InsertQ[T]) resolveCols() ([]*schema.Field, bool, error) {
	if q.err != nil {
		return nil, false, q.err
	}
	lookup := func(c Column) (*schema.Field, error) {
		if err := checkColumn(q.table, c); err != nil {
			return nil, err
		}
		f, ok := q.model.Lookup(c.colName())
		if !ok {
			return nil, fmt.Errorf("%w: %s.%s", ErrUnknownField, q.model.Type, c.colName())
		}
		return f, nil
	}
	var fields []*schema.Field
	if q.cols != nil {
		for _, c := range q.cols {
			f, err := lookup(c)
			if err != nil {
				return nil, false, err
			}
			fields = append(fields, f)
		}
	} else {
		for _, f := range q.model.Fields {
			if f != q.model.Auto {
				fields = append(fields, f)
			}
		}
	}
	for _, c := range q.omit {
		f, err := lookup(c)
		if err != nil {
			return nil, false, err
		}
		fields = slices.DeleteFunc(fields, func(x *schema.Field) bool { return x == f })
	}
	if len(fields) == 0 {
		return nil, false, errors.New("xdb: INSERT has no columns")
	}
	backfill := q.model.Auto != nil && !slices.Contains(fields, q.model.Auto)
	return fields, backfill, nil
}

// stampTimes 在 INSERT 前填充 autoCreateTime / autoUpdateTime 字段。
// 仅在对应字段属于本次插入列、且当前值为零值时填充：显式赋值优先。
// 这需要可寻址的结构体，因此 Insert 要求传指针，InsertMany 在切片元素上就地修改。
func (q *InsertQ[T]) stampTimes(fields []*schema.Field, now time.Time) {
	for _, row := range q.rows {
		for _, f := range []*schema.Field{q.model.CreateTime, q.model.UpdateTime} {
			if f == nil || !slices.Contains(fields, f) {
				continue
			}
			setZeroTime(row, f, now, false)
		}
	}
}

// setZeroTime 在字段可设置时赋值：force 为 false 时只填零值（显式赋值优先），
// 为 true 时无条件覆盖。指针形式的字段会被分配。
func setZeroTime(row reflect.Value, f *schema.Field, now time.Time, force bool) {
	v, err := row.FieldByIndexErr(f.Index)
	if err != nil || !v.CanSet() {
		return
	}
	if f.Type.Kind() == reflect.Pointer {
		if v.IsNil() || force {
			p := reflect.New(f.Type.Elem())
			p.Elem().Set(reflect.ValueOf(now))
			v.Set(p)
		}
		return
	}
	if f.Type != reflect.TypeFor[time.Time]() {
		return
	}
	if force || v.Interface().(time.Time).IsZero() {
		v.Set(reflect.ValueOf(now))
	}
}

// now 返回当前时间；无 Executor（纯渲染场景）时退化为 time.Now。
func (q *InsertQ[T]) now() time.Time {
	if q.ex != nil {
		return q.ex.session(false).db.now()
	}
	return time.Now()
}

func (q *InsertQ[T]) chunkSize(d dialect.Dialect, ncols int) int {
	n := max(d.MaxParams()/ncols, 1)
	if q.batch > 0 {
		n = min(n, q.batch)
	}
	return n
}

// SQL 按指定方言渲染第一批语句，用于调试。
func (q *InsertQ[T]) SQL(d dialect.Dialect) (string, []any, error) {
	fields, backfill, err := q.resolveCols()
	if err != nil {
		return "", nil, err
	}
	if len(q.rows) == 0 {
		return "", nil, errors.New("xdb: INSERT has no rows")
	}
	n := min(len(q.rows), q.chunkSize(d, len(fields)))
	return q.render(d, fields, q.rows[:n], q.returning(d, backfill))
}

// ToSQL 按 Executor 的方言渲染第一批语句。
func (q *InsertQ[T]) ToSQL() (string, []any, error) {
	if q.ex == nil {
		return "", nil, ErrNoExecutor
	}
	return q.SQL(q.ex.Dialect())
}

// returning 返回需要 RETURNING 回填的字段（不需要时为 nil）。
func (q *InsertQ[T]) returning(d dialect.Dialect, backfill bool) *schema.Field {
	if !backfill || !d.Features().Returning || (q.conflict != nil && q.conflict.doNothing) {
		return nil
	}
	return q.model.Auto
}

func (q *InsertQ[T]) render(d dialect.Dialect, fields []*schema.Field, rows []reflect.Value, ret *schema.Field) (string, []any, error) {
	b := newBuilder(d)
	b.write("INSERT INTO ")
	q.table.buildFrom(b)
	b.write(" (")
	for i, f := range fields {
		if i > 0 {
			b.write(", ")
		}
		b.ident(f.Column)
	}
	b.write(") VALUES ")
	for r, row := range rows {
		if r > 0 {
			b.write(", ")
		}
		b.write("(")
		for i, f := range fields {
			if i > 0 {
				b.write(", ")
			}
			b.arg(fieldValue(row, f.Index))
		}
		b.write(")")
	}
	if q.conflict != nil {
		q.buildConflict(b, fields)
	}
	if ret != nil {
		b.write(" RETURNING ")
		b.ident(ret.Column)
	}
	return b.result()
}

func (q *InsertQ[T]) buildConflict(b *builder, fields []*schema.Field) {
	cf := q.conflict
	for _, c := range cf.target {
		if err := checkColumn(q.table, c); err != nil {
			b.fail(err)
		}
	}
	set := cf.set
	switch b.d.Features().Upsert {
	case dialect.UpsertOnConflict:
		b.write(" ON CONFLICT")
		if len(cf.target) > 0 {
			b.write(" (")
			for i, c := range cf.target {
				if i > 0 {
					b.write(", ")
				}
				b.ident(c.colName())
			}
			b.write(")")
		}
		if cf.doNothing {
			b.write(" DO NOTHING")
			return
		}
		if len(cf.target) == 0 {
			b.fail(ErrConflictTarget)
			return
		}
		b.write(" DO UPDATE SET ")
	case dialect.UpsertOnDuplicateKey:
		b.write(" AS ")
		b.ident(mysqlRowAlias)
		b.write(" ON DUPLICATE KEY UPDATE ")
		if cf.doNothing {
			// 无副作用的自赋值，等价于忽略冲突行；不使用 INSERT IGNORE，
			// 因为它还会吞掉截断、类型转换等其它错误。
			// 两侧都必须带表限定符：行别名存在时，裸列名会产生
			// "Column 'x' in field list is ambiguous"。
			b.ident(q.table.qualifier())
			b.write(".")
			b.ident(fields[0].Column)
			b.write(" = ")
			b.ident(q.table.qualifier())
			b.write(".")
			b.ident(fields[0].Column)
			return
		}
	default:
		b.fail(fmt.Errorf("xdb: dialect %s does not support upsert", b.d.Name()))
		return
	}
	buildAssignments(b, q.table, set)
}

func buildAssignments(b *builder, table Table, set []Assignment) {
	for i, a := range set {
		if err := checkColumn(table, a.col); err != nil {
			b.fail(err)
			return
		}
		if i > 0 {
			b.write(", ")
		}
		b.ident(a.col.colName())
		b.write(" = ")
		b.node(a.val)
	}
}

// checkColumn 校验列属于目标表（未限定表名的列视为属于目标表）。
func checkColumn(table Table, c Column) error {
	if c == nil {
		return errEmptyExpr
	}
	if src := c.colSource(); src != nil && src.qualifier() != table.qualifier() {
		return fmt.Errorf("%w: %s.%s (target %s)", ErrForeignColumn, src.qualifier(), c.colName(), table.qualifier())
	}
	return nil
}

// fieldValue 沿索引路径取字段值；途经 nil 嵌入指针时返回 nil（插入 NULL）。
func fieldValue(v reflect.Value, index []int) any {
	f, err := v.FieldByIndexErr(index)
	if err != nil {
		return nil
	}
	return f.Interface()
}

// Exec 执行插入，返回受影响行数。
func (q *InsertQ[T]) Exec(ctx context.Context) (int64, error) {
	if q.ex == nil {
		return 0, ErrNoExecutor
	}
	fields, backfill, err := q.resolveCols()
	if err != nil {
		return 0, err
	}
	if len(q.rows) == 0 {
		return 0, nil
	}
	// 模型回调：BeforeCreate 在写入前逐行调用；任一行失败则整批不执行。
	for _, row := range q.rows {
		if err := beforeCreate(ctx, row.Addr().Interface()); err != nil {
			return 0, wrapCallback("BeforeCreate", err)
		}
	}
	// 只在真正执行时填充时间戳：ToSQL/SQL 是只读调试接口，不应修改模型。
	q.stampTimes(fields, q.now())
	d := q.ex.Dialect()
	ret := q.returning(d, backfill)
	size := q.chunkSize(d, len(fields))
	var total int64
	for start := 0; start < len(q.rows); start += size {
		chunk := q.rows[start:min(start+size, len(q.rows))]
		sqlText, args, err := q.render(d, fields, chunk, ret)
		if err != nil {
			return total, err
		}
		if ret != nil {
			n, err := q.execReturning(ctx, sqlText, args, chunk, ret)
			total += n
			if err != nil {
				return total, err
			}
			continue
		}
		res, err := doExec(ctx, q.ex, sqlText, args)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if backfill && q.conflict == nil && !d.Features().Returning {
			if err := q.backfillLastID(ctx, res, chunk); err != nil {
				return total, err
			}
		}
	}
	// AfterCreate 在所有语句成功之后调用：此时自增主键已回填。
	for _, row := range q.rows {
		if err := afterCreate(ctx, row.Addr().Interface()); err != nil {
			return total, wrapCallback("AfterCreate", err)
		}
	}
	return total, nil
}

func (q *InsertQ[T]) execReturning(ctx context.Context, sqlText string, args []any, chunk []reflect.Value, ret *schema.Field) (int64, error) {
	rows, err := doQuery(ctx, q.ex, sqlText, args, false)
	if err != nil {
		return 0, err
	}
	var n int
	for rows.Next() {
		if n >= len(chunk) {
			_ = rows.Close()
			return int64(n), errors.New("xdb: RETURNING produced more rows than inserted")
		}
		// 主键可能位于 nil 的嵌入指针（*Base）里，先分配再取址。
		schema.AllocEmbedded(chunk[n], q.model.EmbedPtrs)
		dst := chunk[n].FieldByIndex(ret.Index)
		if err := rows.Scan(dst.Addr().Interface()); err != nil {
			_ = rows.Close()
			return int64(n), err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return int64(n), err
	}
	if err := rows.Close(); err != nil {
		return int64(n), err
	}
	if n != len(chunk) {
		return int64(n), fmt.Errorf("xdb: RETURNING produced %d rows for %d inserted", n, len(chunk))
	}
	return int64(n), nil
}

func (q *InsertQ[T]) backfillLastID(ctx context.Context, res interface{ LastInsertId() (int64, error) }, chunk []reflect.Value) error {
	last, err := res.LastInsertId()
	if err != nil {
		return err
	}
	step := int64(1)
	if len(chunk) > 1 {
		if step, err = q.ex.session(false).db.autoIncrementStep(ctx, q.ex); err != nil {
			return err
		}
	}
	for i, row := range chunk {
		schema.AllocEmbedded(row, q.model.EmbedPtrs)
		if err := setInt(row.FieldByIndex(q.model.Auto.Index), last+int64(i)*step); err != nil {
			return err
		}
	}
	return nil
}

func setInt(v reflect.Value, n int64) error {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	default:
		return fmt.Errorf("xdb: auto field must be an integer, got %s", v.Type())
	}
	return nil
}

// autoIncrementStep 读取并缓存 MySQL 的 auto_increment_increment（多主复制环境下可能不为 1）。
func (db *DB) autoIncrementStep(ctx context.Context, ex Executor) (int64, error) {
	db.autoIncMu.Lock()
	defer db.autoIncMu.Unlock()
	if db.autoIncStep > 0 {
		return db.autoIncStep, nil
	}
	step, err := One[int64](ctx, Raw(ex, "SELECT @@auto_increment_increment"))
	if err != nil {
		return 0, fmt.Errorf("xdb: read auto_increment_increment: %w", err)
	}
	db.autoIncStep = max(step, 1)
	return db.autoIncStep, nil
}
