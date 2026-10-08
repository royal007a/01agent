// Package keylock 提供按 key 串行的互斥锁表：引用计数归零即删除条目，
// 不会因为大量不同的会话 ID 而无限增长（sync.Map + 永久 Mutex 的做法会）。
package keylock

import (
	"context"
	"sync"
)

// entry 用容量为 1 的通道实现互斥，便于支持带 ctx 取消的等待。
type entry struct {
	ch   chan struct{}
	refs int
}

type Table struct {
	mu sync.Mutex
	m  map[string]*entry
}

func New() *Table { return &Table{m: make(map[string]*entry)} }

func (t *Table) acquire(key string) *entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[key]
	if e == nil {
		e = &entry{ch: make(chan struct{}, 1)}
		t.m[key] = e
	}
	e.refs++
	return e
}

func (t *Table) release(key string, e *entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(t.m, key)
	}
}

// Lock 阻塞直到拿到 key 的锁，返回解锁函数。
func (t *Table) Lock(key string) (unlock func()) {
	e := t.acquire(key)
	e.ch <- struct{}{}
	return func() { <-e.ch; t.release(key, e) }
}

// LockContext 阻塞直到拿到 key 的锁或 ctx 结束；ctx 结束时不占锁、不泄漏条目。
func (t *Table) LockContext(ctx context.Context, key string) (unlock func(), err error) {
	e := t.acquire(key)
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; t.release(key, e) }, nil
	case <-ctx.Done():
		t.release(key, e)
		return nil, ctx.Err()
	}
}

// TryLock 非阻塞尝试；失败返回 ok=false。
func (t *Table) TryLock(key string) (unlock func(), ok bool) {
	e := t.acquire(key)
	select {
	case e.ch <- struct{}{}:
		return func() { <-e.ch; t.release(key, e) }, true
	default:
		t.release(key, e)
		return nil, false
	}
}

// Len 当前条目数（测试用）。
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
