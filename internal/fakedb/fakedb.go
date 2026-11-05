// Package fakedb 是仅用于测试的 database/sql 假驱动：记录收到的全部语句，
// 并通过 Handler 脚本化返回结果或错误。
package fakedb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Result 是 Handler 的返回值。Columns 非空时视为查询结果。
type Result struct {
	Columns  []string
	Rows     [][]any
	Affected int64
	LastID   int64
}

// Handler 处理一条语句。事务控制语句以 "BEGIN"、"COMMIT"、"ROLLBACK" 传入。
type Handler func(query string, args []any) (*Result, error)

// DB 是一个假数据库，可通过 Open 得到 *sql.DB。
type DB struct {
	mu      sync.Mutex
	log     []string
	Handler Handler
}

// New 创建假数据库；handler 为 nil 时所有语句都成功且无结果。
func New(h Handler) *DB { return &DB{Handler: h} }

// Open 返回连接到该假数据库的 *sql.DB。
func (f *DB) Open() *sql.DB { return sql.OpenDB(connector{f}) }

// Log 返回已执行语句的副本。
func (f *DB) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// Reset 清空日志。
func (f *DB) Reset() {
	f.mu.Lock()
	f.log = nil
	f.mu.Unlock()
}

func (f *DB) handle(q string, args []driver.NamedValue) (*Result, error) {
	f.mu.Lock()
	f.log = append(f.log, q)
	h := f.Handler
	f.mu.Unlock()
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	if h == nil {
		return &Result{}, nil
	}
	res, err := h(q, vals)
	if res == nil {
		res = &Result{}
	}
	return res, err
}

type connector struct{ f *DB }

func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{f: c.f}, nil }
func (c connector) Driver() driver.Driver                        { return drv{} }

type drv struct{}

func (drv) Open(string) (driver.Conn, error) { return nil, errors.New("fakedb: use connector") }

type conn struct{ f *DB }

func (c *conn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakedb: prepare not supported")
}
func (c *conn) Close() error              { return nil }
func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *conn) BeginTx(_ context.Context, o driver.TxOptions) (driver.Tx, error) {
	q := "BEGIN"
	if o.Isolation != 0 {
		q += fmt.Sprintf(" ISOLATION %s", sql.IsolationLevel(o.Isolation))
	}
	if o.ReadOnly {
		q += " READ ONLY"
	}
	if _, err := c.f.handle(q, nil); err != nil {
		return nil, err
	}
	return tx{c.f}, nil
}

// CheckNamedValue 接受任意参数类型，便于测试透传。
func (c *conn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := c.f.handle(q, args)
	if err != nil {
		return nil, err
	}
	return result{res}, nil
}

func (c *conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := c.f.handle(q, args)
	if err != nil {
		return nil, err
	}
	return &rows{res: res}, nil
}

type tx struct{ f *DB }

func (t tx) Commit() error {
	_, err := t.f.handle("COMMIT", nil)
	return err
}

func (t tx) Rollback() error {
	_, err := t.f.handle("ROLLBACK", nil)
	return err
}

type result struct{ r *Result }

func (r result) LastInsertId() (int64, error) { return r.r.LastID, nil }
func (r result) RowsAffected() (int64, error) { return r.r.Affected, nil }

type rows struct {
	res *Result
	i   int
}

func (r *rows) Columns() []string { return r.res.Columns }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.i >= len(r.res.Rows) {
		return io.EOF
	}
	row := r.res.Rows[r.i]
	r.i++
	for i := range dest {
		dest[i] = row[i]
	}
	return nil
}
