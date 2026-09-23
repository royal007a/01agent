// Package keylock 提供按 key 串行的互斥锁表：引用计数归零即删除条目，
// 不会因为大量不同的会话 ID 而无限增长（sync.Map + 永久 Mutex 的做法会）。
package keylock

import "sync"

type entry struct {
	mu   sync.Mutex
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
		e = &entry{}
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
	e.mu.Lock()
	return func() { e.mu.Unlock(); t.release(key, e) }
}

// TryLock 非阻塞尝试；失败返回 ok=false。
func (t *Table) TryLock(key string) (unlock func(), ok bool) {
	e := t.acquire(key)
	if !e.mu.TryLock() {
		t.release(key, e)
		return nil, false
	}
	return func() { e.mu.Unlock(); t.release(key, e) }, true
}

// Len 当前条目数（测试用）。
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
