//go:build live

package e2e

import (
	"context"
	"sync"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func ctxpkgUser(c string) schema.Message { return schema.Message{Role: schema.RoleUser, Content: c} }
func ctxpkgAssistant(c string) schema.Message {
	return schema.Message{Role: schema.RoleAssistant, Content: c}
}

// contextWithTurnBudget 在第 n 次工具调用后取消 ctx，用来模拟"进程被 Kill"。
func contextWithTurnBudget(parent context.Context, rec *recReporter, n int) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	var once sync.Once
	rec.onCall = func(count int) {
		if count >= n {
			once.Do(cancel)
		}
	}
	return ctx, cancel
}
