package keylock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNoLeakAndMutualExclusion(t *testing.T) {
	tb := New()
	for i := 0; i < 1000; i++ {
		u, ok := tb.TryLock(fmt.Sprint(i))
		if !ok {
			t.Fatal("空闲 key 应能拿到锁")
		}
		u()
	}
	if tb.Len() != 0 {
		t.Fatalf("条目未回收: %d", tb.Len())
	}
	var mu sync.Mutex
	inside, max := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := tb.Lock("k")
			mu.Lock()
			inside++
			if inside > max {
				max = inside
			}
			mu.Unlock()
			mu.Lock()
			inside--
			mu.Unlock()
			u()
		}()
	}
	wg.Wait()
	if max != 1 || tb.Len() != 0 {
		t.Fatalf("max=%d len=%d", max, tb.Len())
	}
	u := tb.Lock("x")
	if _, ok := tb.TryLock("x"); ok {
		t.Fatal("已被持有时 TryLock 应失败")
	}
	u()
	if tb.Len() != 0 {
		t.Fatal("失败的 TryLock 不应留下条目")
	}
}

func TestLockContextCancelDoesNotLeak(t *testing.T) {
	tb := New()
	unlock := tb.Lock("ws")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := tb.LockContext(ctx, "ws"); err == nil {
		t.Fatal("锁被占用时 LockContext 应随 ctx 超时返回错误")
	}
	unlock()
	if tb.Len() != 0 {
		t.Fatalf("取消的等待者不应留下条目, len=%d", tb.Len())
	}
	u2, err := tb.LockContext(context.Background(), "ws")
	if err != nil {
		t.Fatal(err)
	}
	u2()
}
