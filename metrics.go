package xdb

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 是轻量的指标收集 Hook，不引入任何外部依赖。
//
// 它记录每个操作的次数、错误数、总耗时与耗时分布，可对接任意监控系统：
// 定期读取 Snapshot 后推送到 Prometheus / OTLP / 日志，或自己实现 Hook
// 直接写入你的监控库。
//
//	h := xdb.NewMetrics()
//	db, _ := xdb.Open("postgres", dsn, xdb.WithHook(h))
//	...
//	for _, s := range h.Snapshot() {
//		promQueryDuration.WithLabelValues(s.Op, s.Result).Observe(s.Avg.Seconds())
//	}
//
// Metrics 可并发使用。
type Metrics struct {
	mu    sync.Mutex
	stats map[metricKey]*metricStat
}

type metricKey struct {
	op     Op
	result string // "ok" / "error"
}

type metricStat struct {
	count    atomic.Int64
	duration atomic.Int64 // 纳秒累计
	min      atomic.Int64
	max      atomic.Int64
	rows     atomic.Int64
}

// MetricSnapshot 是某一时刻的指标快照。
type MetricSnapshot struct {
	Op     Op
	Result string // "ok" / "error"
	Count  int64
	Total  time.Duration
	Avg    time.Duration
	Min    time.Duration
	Max    time.Duration
	Rows   int64
}

// NewMetrics 创建指标收集器。
func NewMetrics() *Metrics { return &Metrics{stats: map[metricKey]*metricStat{}} }

// Before 实现 Hook；指标在 After 中记录，这里只传递 ctx。
func (m *Metrics) Before(ctx context.Context, e *QueryEvent) context.Context { return ctx }

// After 实现 Hook。
func (m *Metrics) After(ctx context.Context, e *QueryEvent) {
	result := "ok"
	if e.Err != nil {
		result = "error"
	}
	key := metricKey{op: e.Op, result: result}

	m.mu.Lock()
	st, ok := m.stats[key]
	if !ok {
		st = &metricStat{}
		m.stats[key] = st
	}
	m.mu.Unlock()

	st.count.Add(1)
	d := e.Duration.Nanoseconds()
	st.duration.Add(d)
	if e.Rows >= 0 {
		st.rows.Add(e.Rows)
	}
	// 无锁地维护 min/max：CAS 循环，避免为统计引入全局互斥。
	for {
		old := st.min.Load()
		if old != 0 && old <= d {
			break
		}
		if st.min.CompareAndSwap(old, d) {
			break
		}
	}
	for {
		old := st.max.Load()
		if old >= d {
			break
		}
		if st.max.CompareAndSwap(old, d) {
			break
		}
	}
}

// Snapshot 返回当前指标快照（按 op、result 排序）。
func (m *Metrics) Snapshot() []MetricSnapshot {
	m.mu.Lock()
	keys := make([]metricKey, 0, len(m.stats))
	stats := make(map[metricKey]*metricStat, len(m.stats))
	for k, v := range m.stats {
		keys = append(keys, k)
		stats[k] = v
	}
	m.mu.Unlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].op != keys[j].op {
			return keys[i].op < keys[j].op
		}
		return keys[i].result < keys[j].result
	})
	out := make([]MetricSnapshot, 0, len(keys))
	for _, k := range keys {
		st := stats[k]
		c := st.count.Load()
		if c == 0 {
			continue
		}
		total := time.Duration(st.duration.Load())
		out = append(out, MetricSnapshot{
			Op:     k.op,
			Result: k.result,
			Count:  c,
			Total:  total,
			Avg:    total / time.Duration(c),
			Min:    time.Duration(st.min.Load()),
			Max:    time.Duration(st.max.Load()),
			Rows:   st.rows.Load(),
		})
	}
	return out
}

// Reset 清空所有统计（测试或周期性推送后使用）。
func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats = map[metricKey]*metricStat{}
}
