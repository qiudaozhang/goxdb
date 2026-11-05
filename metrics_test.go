package xdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	xdb "github.com/qiudaozhang/goxdb"
	"github.com/qiudaozhang/goxdb/internal/fakedb"
)

func TestMetricsCountsAndErrors(t *testing.T) {
	m := xdb.NewMetrics()
	fail := false
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(3)}}}, nil
	}, xdb.WithHook(m))

	// 成功
	if _, err := xdb.Raw(db, "SELECT 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := xdb.Raw(db, "SELECT 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := xdb.Raw(db, "SELECT 1").Exec(ctx); err == nil {
		t.Fatal("expected error")
	}

	var ok, bad *xdb.MetricSnapshot
	for i := range m.Snapshot() {
		s := m.Snapshot()[i]
		switch s.Result {
		case "ok":
			ok = &s
		case "error":
			bad = &s
		}
	}
	if ok == nil || ok.Count != 2 {
		t.Fatalf("ok stats = %+v", ok)
	}
	if bad == nil || bad.Count != 1 {
		t.Fatalf("error stats = %+v", bad)
	}
}

func TestMetricsDurationsAndRows(t *testing.T) {
	m := xdb.NewMetrics()
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		time.Sleep(2 * time.Millisecond)
		return &fakedb.Result{Columns: []string{"count"}, Rows: [][]any{{int64(7)}}}, nil
	}, xdb.WithHook(m))

	for i := 0; i < 3; i++ {
		if _, err := xdb.Raw(db, "SELECT 1").Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	snaps := m.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.Count != 3 {
		t.Fatalf("count = %d", s.Count)
	}
	if s.Min <= 0 || s.Max <= 0 || s.Total < s.Max {
		t.Fatalf("bad durations: %+v", s)
	}
	if s.Avg <= 0 {
		t.Fatalf("avg = %v", s.Avg)
	}
}

func TestMetricsReset(t *testing.T) {
	m := xdb.NewMetrics()
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		return &fakedb.Result{Affected: 1}, nil
	}, xdb.WithHook(m))
	if _, err := xdb.Raw(db, "UPDATE t SET a = 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshot()) == 0 {
		t.Fatal("expected stats")
	}
	m.Reset()
	if len(m.Snapshot()) != 0 {
		t.Fatal("reset must clear stats")
	}
}

// Metrics 必须可并发使用（Hook 在多个 goroutine 中被调用）。
func TestMetricsConcurrent(t *testing.T) {
	m := xdb.NewMetrics()
	db, _ := newDB(t, func(q string, args []any) (*fakedb.Result, error) {
		return &fakedb.Result{Affected: 1}, nil
	}, xdb.WithHook(m))

	const n = 50
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			_, _ = xdb.Raw(db, "UPDATE t SET a = 1").Exec(context.Background())
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	if s := m.Snapshot(); len(s) != 1 || s[0].Count != n {
		t.Fatalf("stats = %+v", s)
	}
}
