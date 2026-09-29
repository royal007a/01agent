package envlock

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// ModelGuard 装饰 Provider，在每次响应上核对服务端回报的实际模型。
// Expected 为空时以本进程首次观测到的版本为准，仍能发现长驻服务运行途中的静默滚动。
// strict 下版本不符直接返回错误，避免在不同的模型上悄悄继续跑。
type ModelGuard struct {
	Inner    provider.LLMProvider
	Expected string
	Strict   bool
	Logf     func(string, ...any) // 为空时用 log.Printf

	mu       sync.Mutex
	observed string
	warned   map[string]bool
	drifts   atomic.Int64
}

// Generate 实现 provider.LLMProvider。
func (g *ModelGuard) Generate(ctx context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	resp, err := g.Inner.Generate(ctx, msgs, tools)
	if err != nil || resp == nil || resp.Model == "" {
		return resp, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.observed == "" {
		g.observed = resp.Model
	}
	want := g.Expected
	if want == "" {
		want = g.observed
	}
	if resp.Model == want {
		return resp, nil
	}
	g.drifts.Add(1)
	if g.warned == nil {
		g.warned = map[string]bool{}
	}
	if !g.warned[resp.Model] { // 同一漂移只报一次，避免刷屏
		g.warned[resp.Model] = true
		logf := g.Logf
		if logf == nil {
			logf = log.Printf
		}
		logf("⚠️ 环境漂移 [model] 服务端回报 %q，锁定/首次观测为 %q", resp.Model, want)
	}
	if g.Strict {
		return nil, fmt.Errorf("envlock: 模型已漂移（期望 %q，实际 %q），strict 模式拒绝继续", want, resp.Model)
	}
	return resp, nil
}

// Observed 本进程首次观测到的实际模型版本。
func (g *ModelGuard) Observed() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observed
}

// Drifts 观测到的模型漂移次数。
func (g *ModelGuard) Drifts() int64 { return g.drifts.Load() }

// Probe 发一条极短请求，拿到服务端回报的实际模型版本（claw env lock -probe 用）。
func Probe(ctx context.Context, p provider.LLMProvider) (string, error) {
	resp, err := p.Generate(ctx, []schema.Message{{Role: schema.RoleUser, Content: "ping，只回复 pong"}}, nil)
	if err != nil {
		return "", err
	}
	if resp.Model == "" {
		return "", fmt.Errorf("envlock: 服务端响应未回报模型版本")
	}
	return resp.Model, nil
}

// ModelOf 从 Provider 配置提取需要锁定的模型身份（不含 API Key）。
func ModelOf(c provider.Config) Model {
	return Model{Provider: c.Kind, BaseURL: c.BaseURL, Requested: c.Model}
}
