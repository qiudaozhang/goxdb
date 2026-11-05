package xdb_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

// recorder 记录回调调用顺序，测试模型内嵌它。
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// eventHooks 让扫描出来的行也能触达 recorder：回调从包级钩子读取它。
var eventHooks *recorder

type Event struct {
	ID   int64  `xdb:"id,pk,auto"`
	Name string `xdb:"name"`

	rec       *recorder
	failStage string
}

func (e *Event) BeforeCreate(ctx context.Context) error {
	e.rec.add("BeforeCreate")
	if e.failStage == "BeforeCreate" {
		return errors.New("boom")
	}
	return nil
}

func (e *Event) AfterCreate(ctx context.Context) error {
	e.rec.add("AfterCreate")
	if e.failStage == "AfterCreate" {
		return errors.New("boom")
	}
	return nil
}

func (e *Event) BeforeUpdate(ctx context.Context) error {
	e.rec.add("BeforeUpdate")
	return nil
}

func (e *Event) AfterUpdate(ctx context.Context) error {
	e.rec.add("AfterUpdate")
	return nil
}

func (e *Event) AfterFind(ctx context.Context) error {
	// 扫描会创建新值，e.rec 为 nil；用包级钩子记录。
	r := e.rec
	if r == nil {
		r = eventHooks
	}
	if r == nil {
		return nil
	}
	r.add("AfterFind")
	return nil
}

// 没有实现任何回调的普通模型，验证接口判断不误触发。
type Plain struct {
	ID   int64  `xdb:"id,pk,auto"`
	Name string `xdb:"name"`
}

func eventDB(t *testing.T) *xdb.DB {
	t.Helper()
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		if strings.HasPrefix(q, "INSERT") {
			// RETURNING 的行数必须与插入行数一致
			res := &fakedb.Result{Columns: []string{"id"}}
			for i := range args {
				res.Rows = append(res.Rows, []any{int64(7 + i)})
			}
			return res, nil
		}
		return &fakedb.Result{Columns: []string{"id", "name"}, Rows: [][]any{{int64(1), "x"}}}, nil
	})
	return db
}

func TestCallbackInsert(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	e := &Event{Name: "x", rec: rec}
	if _, err := xdb.Insert(db, e).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 2 || got[0] != "BeforeCreate" || got[1] != "AfterCreate" {
		t.Fatalf("calls = %v", got)
	}
	if e.ID == 0 {
		t.Fatal("AfterCreate should run after the id is backfilled")
	}
}

func TestCallbackInsertMany(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	rows := []Event{{Name: "a", rec: rec}, {Name: "b", rec: rec}}
	if _, err := xdb.InsertMany(db, rows).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	// 每行各一次 Before + After
	got := rec.got()
	if len(got) != 4 {
		t.Fatalf("calls = %v", got)
	}
	if got[0] != "BeforeCreate" || got[1] != "BeforeCreate" || got[2] != "AfterCreate" || got[3] != "AfterCreate" {
		t.Fatalf("calls = %v", got)
	}
}

func TestCallbackBeforeCreateAborts(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	e := &Event{Name: "x", rec: rec, failStage: "BeforeCreate"}
	if _, err := xdb.Insert(db, e).Exec(ctx); err == nil {
		t.Fatal("BeforeCreate error must abort the insert")
	} else if !errors.Is(err, xdb.ErrCallback) || !strings.Contains(err.Error(), "BeforeCreate") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped BeforeCreate error, got %v", err)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "BeforeCreate" {
		t.Fatalf("AfterCreate must not run: %v", got)
	}
}

func TestCallbackUpdateModel(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	e := &Event{ID: 3, Name: "x", rec: rec}
	if _, err := xdb.UpdateModel(db, e).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 2 || got[0] != "BeforeUpdate" || got[1] != "AfterUpdate" {
		t.Fatalf("calls = %v", got)
	}
}

// 批量 Update[T] 不触发行级回调。
func TestCallbackNotFiredOnBulkUpdate(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	e := &Event{Name: "x", rec: rec}
	_, _ = xdb.Update[Event](db).Set(xdb.CStr(xdb.T("events"), "name").To("y")).
		Where(xdb.CNum[int64](xdb.T("events"), "id").Eq(1)).Exec(ctx)
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("bulk update must not fire callbacks, got %v", got)
	}
	_ = e
}

func TestCallbackAfterFind(t *testing.T) {
	db := eventDB(t)
	rec := &recorder{}
	eventHooks = rec
	t.Cleanup(func() { eventHooks = nil })

	if _, err := xdb.One[Event](ctx, xdb.Raw(db, "SELECT id, name FROM events")); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "AfterFind" {
		t.Fatalf("One: calls = %v", got)
	}

	rec.reset()
	if _, err := xdb.All[Event](ctx, xdb.Raw(db, "SELECT id, name FROM events")); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "AfterFind" {
		t.Fatalf("All: calls = %v", got)
	}

	// Iter 也要触发
	rec.reset()
	for range xdb.Iter[Event](ctx, xdb.Raw(db, "SELECT id, name FROM events")) {
	}
	if got := rec.got(); len(got) != 1 || got[0] != "AfterFind" {
		t.Fatalf("Iter: calls = %v", got)
	}
}

// AfterFind 返回错误时必须中止并传播。
func TestCallbackAfterFindError(t *testing.T) {
	db := eventDB(t)
	eventHooks = &recorder{}
	t.Cleanup(func() { eventHooks = nil })
	if _, err := xdb.One[failingFind](ctx, xdb.Raw(db, "SELECT id FROM events")); err == nil {
		t.Fatal("AfterFind error must propagate")
	}
}

type failingFind struct {
	ID int64 `xdb:"id"`
}

func (f *failingFind) AfterFind(ctx context.Context) error { return errors.New("boom") }

func TestCallbackNoInterfaceNoop(t *testing.T) {
	db := eventDB(t)
	p := &Plain{Name: "x"}
	if _, err := xdb.Insert(db, p).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.UpdateModel(db, p).Exec(ctx); err != nil {
		t.Fatal(err)
	}
}

var _ = context.Background
