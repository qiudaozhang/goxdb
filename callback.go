package xdb

import (
	"context"
	"errors"
)

// 模型级回调是**可选接口**：让模型自己实现其中的方法即自动生效，无需注册。
//
// 明确的作用范围（避免“有时触发有时不触发”的困惑）：
//
//	API                       BeforeCreate  AfterCreate  BeforeUpdate  AfterUpdate  AfterFind
//	Insert / InsertMany       ✓             ✓                                        ✓
//	Insert.Select             ✗             ✗                                        ✗
//	UpdateModel               ✗                                         ✓            ✓
//	Update[T] / Delete[T]     ✗             ✗             ✗            ✗            ✗
//	Query / Select (All/One)  ✗             ✗                                        ✓
//
// 即：只在「操作对象是具体模型实例」的 API 上触发；批量按条件的
// Update[T] / Delete[T] 不触发行级回调——框架无法知道会命中哪些行。
//
// 回调在事务中执行时，传入的 ctx 就是事务的 ctx，因此回调内的
// 数据库操作会自动加入同一事务。

// Creator 在 Insert / InsertMany 之前调用。
type Creator interface {
	BeforeCreate(ctx context.Context) error
}

// Created 在 Insert / InsertMany 成功之后调用。
type Created interface {
	AfterCreate(ctx context.Context) error
}

// Updater 在 UpdateModel 执行之前调用。
type Updater interface {
	BeforeUpdate(ctx context.Context) error
}

// Updated 在 UpdateModel 成功之后调用。
type Updated interface {
	AfterUpdate(ctx context.Context) error
}

// Finder 在 Query / Select 扫描完结果之后调用（每行一次）。
type Finder interface {
	AfterFind(ctx context.Context) error
}

// beforeCreate 对模型调用 BeforeCreate（若实现）。
func beforeCreate(ctx context.Context, v any) error {
	if h, ok := v.(Creator); ok {
		return h.BeforeCreate(ctx)
	}
	return nil
}

// afterCreate 对模型调用 AfterCreate（若实现）。
func afterCreate(ctx context.Context, v any) error {
	if h, ok := v.(Created); ok {
		return h.AfterCreate(ctx)
	}
	return nil
}

func beforeUpdate(ctx context.Context, v any) error {
	if h, ok := v.(Updater); ok {
		return h.BeforeUpdate(ctx)
	}
	return nil
}

func afterUpdate(ctx context.Context, v any) error {
	if h, ok := v.(Updated); ok {
		return h.AfterUpdate(ctx)
	}
	return nil
}

func afterFind(ctx context.Context, v any) error {
	if h, ok := v.(Finder); ok {
		return h.AfterFind(ctx)
	}
	return nil
}

// ErrCallback 用于区分「模型回调返回的错误」与「SQL 执行失败」：
//
//	if _, err := xdb.Insert(db, u).Exec(ctx); errors.Is(err, xdb.ErrCallback) { ... }
var ErrCallback = errors.New("xdb: model callback failed")

func wrapCallback(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &callbackError{stage: stage, err: err}
}

type callbackError struct {
	stage string
	err   error
}

func (e *callbackError) Error() string { return "xdb: " + e.stage + ": " + e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }
func (e *callbackError) Is(target error) bool {
	return target == ErrCallback
}
