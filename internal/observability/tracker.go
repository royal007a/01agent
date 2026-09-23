// Package observability 可观测性：在 Harness 层无侵入地拦截并记录 Token 消耗、成本与耗时（第 18 讲）。
package observability

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// Price 单位：人民币元 / 百万 Token。
// 课程里的表注释写"美元"、打印却用"¥"，这里统一为人民币。
type Price struct {
	InputPrice  float64
	OutputPrice float64
}

// PricingModel 常见模型的大致标价（仅供演示，以厂商官网为准；可用 CLAW_PRICE_IN/CLAW_PRICE_OUT 覆盖）。
var PricingModel = map[string]Price{
	"glm-4.5-air":          {InputPrice: 0.8, OutputPrice: 2},
	"doubao-seed-evolving": {InputPrice: 0.8, OutputPrice: 8},
}

// PriceFor 查询模型价格，环境变量优先。
func PriceFor(model string) (Price, bool) {
	in, e1 := strconv.ParseFloat(os.Getenv("CLAW_PRICE_IN"), 64)
	out, e2 := strconv.ParseFloat(os.Getenv("CLAW_PRICE_OUT"), 64)
	if e1 == nil && e2 == nil {
		return Price{in, out}, true
	}
	p, ok := PricingModel[model]
	return p, ok
}

// Cost 计算一次调用的花费（元）。
func (p Price) Cost(u schema.Usage) float64 {
	return (float64(u.PromptTokens)*p.InputPrice + float64(u.CompletionTokens)*p.OutputPrice) / 1e6
}

// Metrics 是一份不含锁的指标快照。
type Metrics struct {
	LLMCalls         int
	LLMErrors        int
	LLMLatency       time.Duration
	PromptTokens     int
	CompletionTokens int
	Cost             float64
	ToolCalls        int
	ToolErrors       int
	ToolLatency      time.Duration
}

// Stats 进程级累计指标（所有会话之和），并发安全。
type Stats struct {
	mu sync.Mutex
	Metrics
}

// Snapshot 返回一份拷贝。
func (s *Stats) Snapshot() Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Metrics
}

// CostTracker 是包装了真实 LLMProvider 的装饰器：Main Loop 完全不知道自己被监控了。
type CostTracker struct {
	next    provider.LLMProvider
	model   string
	session *ctxpkg.Session // 兜底会话；优先使用 ctx 中当前运行的会话
	Stats   *Stats
}

// NewCostTracker 接收一个现有的 Provider，返回一个被监控的 Provider。
// session 可以为 nil：服务端场景下每次调用从 ctx 中取当前会话记账（课程版本把会话写死在构造时，
// 多会话服务端会把所有人的账单都记到同一个会话上）。
func NewCostTracker(next provider.LLMProvider, model string, session *ctxpkg.Session) *CostTracker {
	return &CostTracker{next: next, model: model, session: session, Stats: &Stats{}}
}

func (t *CostTracker) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	start := time.Now()
	resp, err := t.next.Generate(ctx, msgs, availableTools)
	latency := time.Since(start)

	t.Stats.mu.Lock()
	t.Stats.LLMCalls++
	t.Stats.LLMLatency += latency
	if err != nil {
		t.Stats.LLMErrors++
	}
	t.Stats.mu.Unlock()
	if err != nil {
		log.Printf("[Tracker] ❌ API 调用失败，耗时: %v", latency.Round(time.Millisecond))
		return resp, err
	}
	if resp.Usage == nil {
		log.Printf("[Tracker] ⚠️ API 调用完成，但未返回 Usage 数据 | 耗时: %v", latency.Round(time.Millisecond))
		return resp, nil
	}
	var cost float64
	if price, ok := PriceFor(t.model); ok {
		cost = price.Cost(*resp.Usage)
	}
	t.Stats.mu.Lock()
	t.Stats.PromptTokens += resp.Usage.PromptTokens
	t.Stats.CompletionTokens += resp.Usage.CompletionTokens
	t.Stats.Cost += cost
	t.Stats.mu.Unlock()

	phase := "Action"
	if len(availableTools) == 0 {
		phase = "Thinking"
	}
	log.Printf("[Tracker] 📊 %s 调用完成 | 耗时: %v | 输入: %d tk | 输出: %d tk | 花费: ¥%.6f",
		phase, latency.Round(time.Millisecond), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, cost)

	sess, _ := runctx.Session(ctx).(*ctxpkg.Session)
	if sess == nil {
		sess = t.session
	}
	if sess != nil {
		sess.RecordUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens, cost)
		_, _, total := sess.UsageSnapshot()
		log.Printf("[Tracker] 💰 当前会话 (%s) 累计花费: ¥%.6f", sess.ID, total)
	}
	return resp, nil
}

// TimedRegistry 装饰 Registry，记录工具在本地物理执行的真实耗时（第 18 讲思考题）。
// Middleware 只能在执行"前"拦截，测不到执行时长，所以这里用装饰器包住 Execute。
type TimedRegistry struct {
	tools.Registry
	Stats *Stats
}

func NewTimedRegistry(r tools.Registry, stats *Stats) *TimedRegistry {
	return &TimedRegistry{Registry: r, Stats: stats}
}

func (t *TimedRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	start := time.Now()
	res := t.Registry.Execute(ctx, call)
	d := time.Since(start)
	t.Stats.mu.Lock()
	t.Stats.ToolCalls++
	t.Stats.ToolLatency += d
	if res.IsError {
		t.Stats.ToolErrors++
	}
	t.Stats.mu.Unlock()
	log.Printf("[Tracker] ⏱️ 工具 %s 本地执行耗时: %v", call.Name, d.Round(time.Millisecond))
	return res
}
