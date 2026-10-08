package observability

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

type fakeLLM struct{ fail bool }

func (f fakeLLM) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (*schema.Message, error) {
	time.Sleep(20 * time.Millisecond)
	if f.fail {
		return nil, errors.New("boom")
	}
	return &schema.Message{Role: schema.RoleAssistant, Usage: &schema.Usage{PromptTokens: 1000, CompletionTokens: 500}}, nil
}

func TestCostTracker(t *testing.T) {
	t.Setenv("CLAW_PRICE_IN", "2")
	t.Setenv("CLAW_PRICE_OUT", "8")
	fallback := ctxpkg.NewSession("fallback", "")
	a := ctxpkg.NewSession("A", "")
	tr := NewCostTracker(fakeLLM{}, "any", fallback)
	// ctx 中带会话 → 记到 A；否则记到兜底会话
	tr.Generate(runctx.WithSession(context.Background(), a), nil, nil)
	tr.Generate(runctx.WithSession(context.Background(), a), nil, nil)
	tr.Generate(context.Background(), nil, nil)
	p, c, cost := a.UsageSnapshot()
	want := (1000*2 + 500*8) / 1e6 * 2
	if p != 2000 || c != 1000 || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("A: %d %d %f want %f", p, c, cost, want)
	}
	if fp, _, _ := fallback.UsageSnapshot(); fp != 1000 {
		t.Fatal("兜底会话记账错误")
	}
	s := tr.Stats.Snapshot()
	if s.LLMCalls != 3 || s.PromptTokens != 3000 || s.LLMLatency < 60*time.Millisecond {
		t.Fatalf("stats: %+v", s)
	}
	bad := NewCostTracker(fakeLLM{fail: true}, "any", fallback)
	if _, err := bad.Generate(context.Background(), nil, nil); err == nil || bad.Stats.Snapshot().LLMErrors != 1 {
		t.Fatal("错误应透传并计数")
	}
}

type sleepyToolAdapter struct{}

func (sleepyToolAdapter) Name() string { return "sleepy" }
func (sleepyToolAdapter) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "sleepy"}
}
func (sleepyToolAdapter) Execute(context.Context, json.RawMessage) (string, error) {
	time.Sleep(50 * time.Millisecond)
	return "", errors.New("x")
}

func TestTimedRegistry(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(sleepyToolAdapter{})
	st := &Stats{}
	tr := NewTimedRegistry(r, st)
	tr.Execute(context.Background(), schema.ToolCall{Name: "sleepy"})
	s := st.Snapshot()
	if s.ToolCalls != 1 || s.ToolErrors != 1 || s.ToolLatency < 50*time.Millisecond {
		t.Fatalf("%+v", s)
	}
	if len(tr.GetAvailableTools()) != 1 {
		t.Fatal("装饰器应透传其余方法")
	}
}

type quickTool struct{}

func (quickTool) Name() string { return "quick" }
func (quickTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "quick"}
}
func (quickTool) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

// 第 18 讲思考题：只统计工具在本地"物理执行"的耗时，人工审批/写锁等待不能算进去。
func TestTimedRegistryExcludesApprovalWait(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(quickTool{})
	r.Use(func(context.Context, schema.ToolCall) (bool, string) {
		time.Sleep(120 * time.Millisecond) // 模拟等待人工审批
		return true, ""
	})
	st := &Stats{}
	NewTimedRegistry(r, st).Execute(context.Background(), schema.ToolCall{Name: "quick", Arguments: []byte(`{}`)})
	if got := st.Snapshot().ToolLatency; got >= 100*time.Millisecond {
		t.Fatalf("审批等待被计入了工具执行耗时: %v", got)
	}
}
