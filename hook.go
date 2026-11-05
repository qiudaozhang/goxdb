package xdb

import (
	"context"
	"log/slog"
	"time"
)

// Op 是被观测操作的类型。
type Op uint8

const (
	OpExec Op = iota
	OpQuery
	OpBegin
	OpCommit
	OpRollback
	OpSavepoint
	OpReleaseSavepoint
	OpRollbackSavepoint
)

var opNames = [...]string{"exec", "query", "begin", "commit", "rollback", "savepoint", "release_savepoint", "rollback_to_savepoint"}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return "unknown"
}

// QueryEvent 描述一次数据库操作，供 Hook 使用。
type QueryEvent struct {
	Op    Op
	Query string // 最终发送给数据库的 SQL（已完成占位符改写）
	Args  []any  // 绑定参数（已完成切片展开）
	InTx  bool   // 是否在事务中执行
	Start time.Time
	// 以下字段仅在 After 中有效。
	Duration time.Duration
	// Rows：Exec 为受影响行数，Query 为已读取行数；未知时为 -1。
	Rows int64
	Err  error
}

// Hook 是查询拦截器。Before 可以返回派生的 context（如开启 tracing span），
// 该 context 会传给对应的 After。多个 Hook 的 Before 按注册顺序调用，After 按逆序调用。
//
// 对于 Query，After 在结果集关闭（读取完毕或调用 Close）时触发，
// 因此 Duration 覆盖了完整的读取过程。
type Hook interface {
	Before(ctx context.Context, e *QueryEvent) context.Context
	After(ctx context.Context, e *QueryEvent)
}

// HookFuncs 用函数快速构造 Hook，任一字段可以为 nil。
type HookFuncs struct {
	BeforeFunc func(ctx context.Context, e *QueryEvent) context.Context
	AfterFunc  func(ctx context.Context, e *QueryEvent)
}

func (h HookFuncs) Before(ctx context.Context, e *QueryEvent) context.Context {
	if h.BeforeFunc != nil {
		return h.BeforeFunc(ctx, e)
	}
	return ctx
}

func (h HookFuncs) After(ctx context.Context, e *QueryEvent) {
	if h.AfterFunc != nil {
		h.AfterFunc(ctx, e)
	}
}

// LogQueries 返回一个记录所有操作的 Hook。出于安全考虑默认不记录参数值，
// withArgs 为 true 时才记录。
func LogQueries(l *slog.Logger, level slog.Level, withArgs bool) Hook {
	return HookFuncs{AfterFunc: func(ctx context.Context, e *QueryEvent) {
		lvl := level
		if e.Err != nil {
			lvl = slog.LevelError
		}
		attrs := eventAttrs(e, withArgs)
		l.LogAttrs(ctx, lvl, "xdb", attrs...)
	}}
}

// LogSlow 返回一个只记录耗时超过 threshold 的操作的 Hook（Warn 级别，不含参数值）。
func LogSlow(l *slog.Logger, threshold time.Duration) Hook {
	return HookFuncs{AfterFunc: func(ctx context.Context, e *QueryEvent) {
		if e.Duration >= threshold {
			l.LogAttrs(ctx, slog.LevelWarn, "xdb slow query", eventAttrs(e, false)...)
		}
	}}
}

func eventAttrs(e *QueryEvent, withArgs bool) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("op", e.Op.String()),
		slog.Duration("duration", e.Duration),
		slog.Bool("in_tx", e.InTx),
	}
	if e.Query != "" {
		attrs = append(attrs, slog.String("sql", e.Query))
	}
	if e.Rows >= 0 {
		attrs = append(attrs, slog.Int64("rows", e.Rows))
	}
	if withArgs && len(e.Args) > 0 {
		attrs = append(attrs, slog.Any("args", e.Args))
	}
	if e.Err != nil {
		attrs = append(attrs, slog.String("error", e.Err.Error()))
	}
	return attrs
}

func (db *DB) before(ctx context.Context, e *QueryEvent) context.Context {
	for _, h := range db.hooks {
		ctx = h.Before(ctx, e)
	}
	return ctx
}

func (db *DB) after(ctx context.Context, e *QueryEvent) {
	e.Duration = time.Since(e.Start)
	for i := len(db.hooks) - 1; i >= 0; i-- {
		db.hooks[i].After(ctx, e)
	}
}
