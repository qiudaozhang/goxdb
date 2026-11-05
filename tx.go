package xdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/qiudaozhang/goxdb/dialect"
)

// 隔离级别，直接复用 database/sql 的定义。
const (
	LevelDefault    = sql.LevelDefault
	ReadUncommitted = sql.LevelReadUncommitted
	ReadCommitted   = sql.LevelReadCommitted
	RepeatableRead  = sql.LevelRepeatableRead
	Serializable    = sql.LevelSerializable
)

// savepoint 回滚使用独立超时，保证调用方 context 已取消时依然能回滚干净。
const cleanupTimeout = 5 * time.Second

type txState uint8

const (
	txActive     txState = iota
	txCommitting         // 正在提交，此时任何语句都会被拒绝
	txCommitted
	txRolledBack
	txReleased // savepoint 已 RELEASE，并入父事务
	txUnknown  // 提交结果未知
)

// Tx 是一个事务，或事务中的一个嵌套事务（savepoint）。
//
// 托管事务（DB.Tx / Tx.Tx）的闭包中拿不到 Commit/Rollback，提交与回滚完全由
// 框架根据闭包的返回值决定，从根本上杜绝“提前提交后继续写”等错误。
//
// Tx 不是并发安全的执行通道：同一时刻只能执行一条语句，结果集未关闭前也不能
// 执行下一条语句，违反时返回 ErrTxConcurrentUse 而不是让驱动产生不可预期的错误。
type Tx struct {
	db     *DB
	std    *sql.Tx
	root   *Tx
	parent *Tx
	name   string          // savepoint 名，根事务为空
	hctx   context.Context // 开启事务时的 context，供提交/回滚的 Hook 使用

	// 以下字段由 root.mu 保护。
	mu            sync.Mutex // 仅根事务使用
	state         txState
	child         *Tx
	busy          bool // 仅根事务使用：是否有语句正在执行
	spSeq         int  // 仅根事务使用：savepoint 序号
	afterCommit   []func()
	afterRollback []func()
}

// Dialect 返回方言。
func (t *Tx) Dialect() dialect.Dialect { return t.db.d }

// session 始终使用事务自己的连接（在主库上），readOnly 参数被忽略：
// 事务一旦开启就固定在主库，避免读到不一致的快照。
func (t *Tx) session(bool) session { return session{db: t.db, conn: t.std, tx: t} }

// ---------- 事务选项 ----------

// Backoff 返回第 attempt 次（从 1 开始）失败后的等待时长。
type Backoff func(attempt int) time.Duration

// ConstBackoff 固定等待时长。
func ConstBackoff(d time.Duration) Backoff { return func(int) time.Duration { return d } }

// ExpBackoff 指数退避（带 full jitter），等待时长不超过 max。
func ExpBackoff(base, max time.Duration) Backoff {
	return func(attempt int) time.Duration {
		d := base << min(attempt-1, 30)
		if d <= 0 || d > max {
			d = max
		}
		return time.Duration(randInt63n(int64(d) + 1))
	}
}

type retryPolicy struct {
	attempts int
	backoff  Backoff
	cond     func(error) bool
}

type txConfig struct {
	opts  sql.TxOptions
	retry *retryPolicy
	n     int // 选项个数，嵌套事务不允许任何选项
}

// TxOption 是事务选项。
type TxOption func(*txConfig)

// TxIsolation 设置隔离级别。
func TxIsolation(l sql.IsolationLevel) TxOption {
	return func(c *txConfig) { c.opts.Isolation = l }
}

// TxReadOnly 开启只读事务。
func TxReadOnly() TxOption { return func(c *txConfig) { c.opts.ReadOnly = true } }

// TxRetry 在死锁、序列化失败时整体重试事务，最多执行 attempts 次（含首次）。
// backoff 为 nil 时使用 ExpBackoff(10ms, 1s)。
//
// 由于整个闭包会被重新执行，闭包内不得产生外部副作用（发消息、写缓存等），
// 这类操作请放到 Tx.AfterCommit 中。
func TxRetry(attempts int, backoff Backoff) TxOption {
	return func(c *txConfig) {
		if backoff == nil {
			backoff = ExpBackoff(10*time.Millisecond, time.Second)
		}
		cond := IsRetryable
		if c.retry != nil && c.retry.cond != nil {
			cond = c.retry.cond
		}
		c.retry = &retryPolicy{attempts: max(attempts, 1), backoff: backoff, cond: cond}
	}
}

// TxRetryIf 自定义哪些错误触发重试（需配合 TxRetry 使用，顺序不限）。
func TxRetryIf(cond func(error) bool) TxOption {
	return func(c *txConfig) {
		if c.retry == nil {
			c.retry = &retryPolicy{}
		}
		c.retry.cond = cond
	}
}

func buildTxConfig(opts []TxOption) txConfig {
	var c txConfig
	for _, o := range opts {
		o(&c)
		c.n++
	}
	if c.retry != nil && c.retry.attempts == 0 { // 只设置了 TxRetryIf
		c.retry = nil
	}
	return c
}

// ---------- 托管事务 ----------

// TxFunc 是事务闭包。传入的 ctx 携带事务标记，应在闭包内向下传递，
// 以便 ExecutorFrom 取得事务、并检测误用 *DB 的情况。
type TxFunc func(ctx context.Context, tx *Tx) error

// Tx 在事务中执行 fn：
//
//   - fn 返回 nil 时提交；返回错误时回滚并原样返回该错误；
//   - fn panic 时先回滚再重新 panic；
//   - ctx 在 fn 返回时已取消，则回滚而不提交；
//   - 提交结果无法确定时返回包裹 ErrCommitUnknown 的错误；
//   - 配置 TxRetry 时，遇到可重试错误会整体重新执行 fn。
//
// 事务的生命周期不绑定 ctx 的取消（只有单条语句受 ctx 控制），
// 回滚与提交始终由本方法确定性地完成，不会出现驱动在后台异步回滚的竞态。
func (db *DB) Tx(ctx context.Context, fn TxFunc, opts ...TxOption) error {
	cfg := buildTxConfig(opts)
	for attempt := 1; ; attempt++ {
		err := db.runTx(ctx, fn, cfg)
		if err == nil || cfg.retry == nil || attempt >= cfg.retry.attempts || !cfg.retry.cond(err) {
			return err
		}
		timer := time.NewTimer(cfg.retry.backoff(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}

func (db *DB) runTx(ctx context.Context, fn TxFunc, cfg txConfig) (err error) {
	t, err := db.begin(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			if rerr := t.rollback(); rerr != nil {
				db.logger.Error("xdb: rollback after panic failed", "error", rerr)
			}
			panic(p)
		}
	}()
	if err := fn(contextWithTx(ctx, t), t); err != nil {
		return t.rollbackWith(err)
	}
	if err := ctx.Err(); err != nil {
		return t.rollbackWith(err)
	}
	return t.commit()
}

func (db *DB) begin(ctx context.Context, cfg txConfig, managed bool) (*Tx, error) {
	ev := &QueryEvent{Op: OpBegin, InTx: true, Start: time.Now(), Rows: -1}
	hctx := db.before(ctx, ev)
	bctx := hctx
	if managed {
		// 托管事务自己负责回滚，不让 database/sql 因 ctx 取消而在后台异步回滚，
		// 否则提交与回滚会产生竞态，提交结果也会变得不可判定。
		bctx = context.WithoutCancel(hctx)
	}
	std, err := db.std.BeginTx(bctx, &cfg.opts)
	err = db.wrapErr(err, "BEGIN")
	ev.Err = err
	db.after(hctx, ev)
	if err != nil {
		return nil, err
	}
	t := &Tx{db: db, std: std, hctx: hctx}
	t.root = t
	return t, nil
}

// Tx 开启嵌套事务（基于 SAVEPOINT）。fn 返回错误或 panic 时只回滚到该
// savepoint，外层事务可以选择继续；fn 成功时 RELEASE savepoint 并入外层。
// 嵌套事务不接受任何选项（隔离级别等只能在最外层指定）。
//
// 在嵌套事务执行期间，外层 Tx 不可使用（返回 ErrTxChildActive）。
func (t *Tx) Tx(ctx context.Context, fn TxFunc, opts ...TxOption) (err error) {
	if len(opts) > 0 {
		return ErrNestedTxOptions
	}
	child, err := t.savepoint(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			if rerr := child.rollbackSavepoint(ctx); rerr != nil {
				t.db.logger.Error("xdb: rollback to savepoint after panic failed", "error", rerr)
			}
			panic(p)
		}
	}()
	if err := fn(contextWithTx(ctx, child), child); err != nil {
		return child.rollbackSavepointWith(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return child.rollbackSavepointWith(ctx, err)
	}
	return child.release(ctx)
}

// AfterCommit 注册在最外层事务成功提交后执行的回调，按注册顺序执行。
// 在嵌套事务中注册的回调，若该嵌套事务被回滚则会被丢弃。
// 适合放置清缓存、发消息等外部副作用。对已结束的事务调用会 panic。
func (t *Tx) AfterCommit(fn func()) {
	t.root.mu.Lock()
	defer t.root.mu.Unlock()
	t.mustActiveLocked("AfterCommit")
	t.afterCommit = append(t.afterCommit, fn)
}

// AfterRollback 注册回滚后执行的回调：所在的嵌套事务回滚时，或最外层事务回滚时执行。
// 对已结束的事务调用会 panic。
func (t *Tx) AfterRollback(fn func()) {
	t.root.mu.Lock()
	defer t.root.mu.Unlock()
	t.mustActiveLocked("AfterRollback")
	t.afterRollback = append(t.afterRollback, fn)
}

func (t *Tx) mustActiveLocked(what string) {
	if t.state != txActive || t.root.state != txActive {
		panic("xdb: " + what + " called on a finished transaction")
	}
}

// ---------- 手动事务 ----------

// ManualTx 是手动控制的事务，用于托管事务无法覆盖的场景。推荐写法：
//
//	tx, err := db.Begin(ctx)
//	if err != nil { return err }
//	defer tx.RollbackUnlessCommitted()
//	...
//	return tx.Commit()
//
// 与 database/sql 一致，手动事务的生命周期绑定 ctx：ctx 取消后事务会被自动回滚。
// ManualTx 同样实现 Executor。
type ManualTx struct {
	t *Tx
}

// Begin 开启手动事务。不支持 TxRetry。
func (db *DB) Begin(ctx context.Context, opts ...TxOption) (*ManualTx, error) {
	cfg := buildTxConfig(opts)
	if cfg.retry != nil {
		return nil, ErrRetryOnManualTx
	}
	t, err := db.begin(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	return &ManualTx{t: t}, nil
}

// Dialect 返回方言。
func (m *ManualTx) Dialect() dialect.Dialect { return m.t.Dialect() }

func (m *ManualTx) session(readOnly bool) session { return m.t.session(readOnly) }

// Tx 开启嵌套事务，语义同 Tx.Tx。
func (m *ManualTx) Tx(ctx context.Context, fn TxFunc, opts ...TxOption) error {
	return m.t.Tx(ctx, fn, opts...)
}

// AfterCommit 同 Tx.AfterCommit。
func (m *ManualTx) AfterCommit(fn func()) { m.t.AfterCommit(fn) }

// AfterRollback 同 Tx.AfterRollback。
func (m *ManualTx) AfterRollback(fn func()) { m.t.AfterRollback(fn) }

// Context 返回携带本事务的 context，便于跨层传递（配合 ExecutorFrom）。
func (m *ManualTx) Context(ctx context.Context) context.Context { return contextWithTx(ctx, m.t) }

// Commit 提交事务。
func (m *ManualTx) Commit() error { return m.t.commit() }

// Rollback 回滚事务；事务已结束时返回 ErrTxDone。
func (m *ManualTx) Rollback() error {
	m.t.mu.Lock()
	active := m.t.state == txActive
	m.t.mu.Unlock()
	if !active {
		return ErrTxDone
	}
	return m.t.rollback()
}

// RollbackUnlessCommitted 在事务仍未结束时回滚，适合 defer。
func (m *ManualTx) RollbackUnlessCommitted() {
	if err := m.t.rollback(); err != nil {
		m.t.db.logger.Error("xdb: rollback failed", "error", err)
	}
}

// ---------- 内部实现 ----------

// acquire 校验事务可用并占用连接，返回释放函数。
func (t *Tx) acquire() (func(), error) {
	r := t.root
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := t.usableLocked(); err != nil {
		return nil, err
	}
	if r.busy {
		return nil, ErrTxConcurrentUse
	}
	r.busy = true
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.busy = false
			r.mu.Unlock()
		})
	}, nil
}

func (t *Tx) usableLocked() error {
	if t.state != txActive || t.root.state != txActive {
		return ErrTxDone
	}
	if t.child != nil {
		return ErrTxChildActive
	}
	return nil
}

// control 执行事务控制语句（SAVEPOINT 等），绕过占位符处理。
func (t *Tx) control(ctx context.Context, op Op, query string) error {
	ev := &QueryEvent{Op: op, Query: query, InTx: true, Start: time.Now(), Rows: -1}
	hctx := t.db.before(ctx, ev)
	_, err := t.std.ExecContext(hctx, query)
	err = t.db.wrapErr(err, query)
	ev.Err = err
	t.db.after(hctx, ev)
	return err
}

func (t *Tx) savepoint(ctx context.Context) (*Tx, error) {
	r := t.root
	r.mu.Lock()
	if err := t.usableLocked(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if r.busy {
		r.mu.Unlock()
		return nil, ErrTxConcurrentUse
	}
	r.spSeq++
	name := "xdb_sp_" + strconv.Itoa(r.spSeq)
	r.busy = true
	r.mu.Unlock()

	err := t.control(ctx, OpSavepoint, "SAVEPOINT "+name)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = false
	if err != nil {
		return nil, err
	}
	child := &Tx{db: t.db, std: t.std, root: r, parent: t, name: name, hctx: t.hctx}
	t.child = child
	return child, nil
}

// release 执行 RELEASE SAVEPOINT，并把回调并入父事务。
func (c *Tx) release(ctx context.Context) error {
	r := c.root
	r.mu.Lock()
	if err := c.usableLocked(); err != nil {
		r.mu.Unlock()
		return c.rollbackSavepointWith(ctx, err)
	}
	if r.busy {
		r.mu.Unlock()
		return c.rollbackSavepointWith(ctx, ErrTxConcurrentUse)
	}
	r.busy = true
	r.mu.Unlock()

	err := c.control(ctx, OpReleaseSavepoint, "RELEASE SAVEPOINT "+c.name)

	r.mu.Lock()
	r.busy = false
	if err == nil {
		c.state = txReleased
		p := c.parent
		p.child = nil
		p.afterCommit = append(p.afterCommit, c.afterCommit...)
		p.afterRollback = append(p.afterRollback, c.afterRollback...)
		c.afterCommit, c.afterRollback = nil, nil
	}
	r.mu.Unlock()
	if err != nil {
		return c.rollbackSavepointWith(ctx, err)
	}
	return nil
}

func (c *Tx) rollbackSavepointWith(ctx context.Context, cause error) error {
	if err := c.rollbackSavepoint(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("xdb: rollback to savepoint %s failed: %w", c.name, err))
	}
	return cause
}

// rollbackSavepoint 回滚到 savepoint。即便失败，也会把该嵌套事务标记为已回滚
// 并解除与父事务的关联；若回滚语句未成功，父事务通常也已不可用，最终会整体回滚。
func (c *Tx) rollbackSavepoint(ctx context.Context) error {
	r := c.root
	r.mu.Lock()
	if c.state != txActive {
		r.mu.Unlock()
		return nil
	}
	rootActive := r.state == txActive
	busy := r.busy
	if rootActive && !busy {
		r.busy = true
	}
	r.mu.Unlock()

	var err error
	switch {
	case !rootActive:
		// 根事务已结束，savepoint 随之失效，无需也无法回滚。
	case busy:
		err = ErrTxConcurrentUse
	default:
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		err = c.control(cctx, OpRollbackSavepoint, "ROLLBACK TO SAVEPOINT "+c.name)
		cancel()
	}

	r.mu.Lock()
	if rootActive && !busy {
		r.busy = false
	}
	callbacks := c.markRolledBackLocked()
	c.parent.child = nil
	r.mu.Unlock()

	runAll(callbacks)
	return err
}

// markRolledBackLocked 把 c 及其（理论上不应存在的）后代标记为已回滚，
// 返回需要执行的 AfterRollback 回调（由内向外）。
func (c *Tx) markRolledBackLocked() []func() {
	var cbs []func()
	if c.child != nil {
		cbs = c.child.markRolledBackLocked()
		c.child = nil
	}
	c.state = txRolledBack
	cbs = append(cbs, c.afterRollback...)
	c.afterCommit, c.afterRollback = nil, nil
	return cbs
}

func (t *Tx) commit() error {
	r := t.root
	r.mu.Lock()
	if r.state != txActive {
		r.mu.Unlock()
		return ErrTxDone
	}
	if r.child != nil {
		r.mu.Unlock()
		return t.rollbackWith(ErrTxChildActive)
	}
	r.state = txCommitting
	r.mu.Unlock()

	ev := &QueryEvent{Op: OpCommit, Query: "COMMIT", InTx: true, Start: time.Now(), Rows: -1}
	hctx := t.db.before(r.hctx, ev)
	err := r.std.Commit()

	var result error
	var callbacks []func()
	r.mu.Lock()
	switch {
	case err == nil:
		r.state = txCommitted
		callbacks = r.afterCommit
	case errors.Is(err, sql.ErrTxDone), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// database/sql 在发出 COMMIT 之前就已拒绝（事务已被回滚）。
		r.state = txRolledBack
		callbacks = r.afterRollback
		result = fmt.Errorf("xdb: commit failed, transaction was rolled back: %w", err)
	default:
		if _, ok := t.db.d.ClassifyError(err); ok {
			// 服务端明确拒绝了提交：事务未生效。
			r.state = txRolledBack
			callbacks = r.afterRollback
			result = t.db.wrapErr(err, "COMMIT")
		} else {
			r.state = txUnknown
			result = fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
	}
	r.afterCommit, r.afterRollback = nil, nil
	r.mu.Unlock()

	ev.Err = result
	t.db.after(hctx, ev)
	runAll(callbacks)
	return result
}

func (t *Tx) rollbackWith(cause error) error {
	if err := t.rollback(); err != nil {
		return errors.Join(cause, fmt.Errorf("xdb: rollback failed: %w", err))
	}
	return cause
}

// rollback 回滚根事务；事务已结束时为空操作。
func (t *Tx) rollback() error {
	r := t.root
	r.mu.Lock()
	if r.state != txActive {
		r.mu.Unlock()
		return nil
	}
	callbacks := r.markRolledBackLocked()
	r.mu.Unlock()

	ev := &QueryEvent{Op: OpRollback, Query: "ROLLBACK", InTx: true, Start: time.Now(), Rows: -1}
	hctx := t.db.before(r.hctx, ev)
	err := r.std.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		err = nil // 已被 database/sql 因 ctx 取消而回滚
	}
	err = t.db.wrapErr(err, "ROLLBACK")
	ev.Err = err
	t.db.after(hctx, ev)
	runAll(callbacks)
	return err
}

func runAll(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}

// ---------- context 传递 ----------

type txCtxKey struct{}

func contextWithTx(ctx context.Context, t *Tx) context.Context {
	return context.WithValue(ctx, txCtxKey{}, t)
}

// WithoutTx 返回不再携带事务的 context。用于在事务中有意通过 *DB
// 执行独立语句（例如写入不随事务回滚的审计日志）。
func WithoutTx(ctx context.Context) context.Context {
	return context.WithValue(ctx, txCtxKey{}, (*Tx)(nil))
}

// TxFromContext 返回 context 中当前可用的事务（属于任意 DB）。
func TxFromContext(ctx context.Context) (*Tx, bool) {
	t, _ := ctx.Value(txCtxKey{}).(*Tx)
	t = t.usableAncestor()
	return t, t != nil
}

// ExecutorFrom 返回 context 中属于 db 的当前事务；不存在时返回 db 本身。
// 适合分层架构中 repository 层的写法：
//
//	func (r *Repo) Get(ctx context.Context, id int64) (User, error) {
//		return xdb.One[User](ctx, xdb.Raw(xdb.ExecutorFrom(ctx, r.db), "..."))
//	}
func ExecutorFrom(ctx context.Context, db *DB) Executor {
	if t, ok := TxFromContext(ctx); ok && t.db == db {
		return t
	}
	return db
}

// usableAncestor 返回 t 所在事务链上当前可执行语句的那一层：
// 先向外找到第一个未结束的事务（例如内层嵌套事务已结束，ctx 仍被外层使用），
// 再向内沿着活动的嵌套事务找到最内层。整个事务已结束时返回 nil。
func (t *Tx) usableAncestor() *Tx {
	if t == nil {
		return nil
	}
	r := t.root
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != txActive {
		return nil
	}
	c := t
	for c.state != txActive {
		c = c.parent
	}
	for c.child != nil {
		c = c.child
	}
	return c
}

// checkMisuse 检测在事务 context 中直接使用 *DB 的情况。
func (db *DB) checkMisuse(ctx context.Context) error {
	t, ok := TxFromContext(ctx)
	if !ok || t.db != db {
		return nil
	}
	if db.strictTx {
		return ErrExecutorMisuse
	}
	db.logger.WarnContext(ctx, ErrExecutorMisuse.Error())
	return nil
}
