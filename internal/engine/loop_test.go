package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// scriptedProvider 模拟第 03 讲的升级版 Mock：tools 为空时是 Thinking，否则是 Action。
type scriptedProvider struct {
	actions  int
	thinks   int
	sawTools [][]schema.ToolDefinition
	lastMsgs []schema.Message
}

func (m *scriptedProvider) Generate(_ context.Context, msgs []schema.Message, tools []schema.ToolDefinition) (*schema.Message, error) {
	m.lastMsgs = msgs
	m.sawTools = append(m.sawTools, tools)
	if len(tools) == 0 {
		m.thinks++
		return &schema.Message{Role: schema.RoleAssistant, Content: "【推理中】先 ls 再定夺"}, nil
	}
	m.actions++
	if m.actions == 1 {
		return &schema.Message{Role: schema.RoleAssistant, Content: "执行计划",
			ToolCalls: []schema.ToolCall{{ID: "call_1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)}}}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "看到 main.go，完成"}, nil
}

type fakeRegistry struct{ calls []schema.ToolCall }

func (f *fakeRegistry) Register(tools.BaseTool) {}
func (f *fakeRegistry) IsReadOnly(string) bool  { return false }

func (f *fakeRegistry) GetAvailableTools() []schema.ToolDefinition {
	return []schema.ToolDefinition{{Name: "bash"}}
}
func (f *fakeRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	f.calls = append(f.calls, call)
	return schema.ToolResult{ToolCallID: call.ID, Output: "main.go\n"}
}

func TestTwoStageLoop(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		p, r := &scriptedProvider{}, &fakeRegistry{}
		if err := NewAgentEngine(p, r, t.TempDir(), thinking).Run(context.Background(), "检查文件", nil); err != nil {
			t.Fatal(err)
		}
		if p.actions != 2 || len(r.calls) != 1 {
			t.Fatalf("thinking=%v: actions=%d calls=%d", thinking, p.actions, len(r.calls))
		}
		wantThinks := 0
		if thinking {
			wantThinks = 2
		}
		if p.thinks != wantThinks {
			t.Fatalf("thinking=%v: thinks=%d want %d", thinking, p.thinks, wantThinks)
		}
		// 最后一次 Action 请求必须带着 tool 结果（ToolCallID 对齐）
		var found bool
		for _, m := range p.lastMsgs {
			if m.ToolCallID == "call_1" && m.Content == "main.go\n" {
				found = true
			}
		}
		if !found {
			t.Fatalf("observation 未按 ToolCallID 回灌上下文")
		}
	}
}

// slowTool 用于验证并发与保序。
type slowTool struct {
	name     string
	readOnly bool
}

func (s slowTool) Name() string                      { return s.name }
func (s slowTool) ReadOnly() bool                    { return s.readOnly }
func (s slowTool) Definition() schema.ToolDefinition { return schema.ToolDefinition{Name: s.name} }
func (s slowTool) Execute(_ context.Context, a json.RawMessage) (string, error) {
	time.Sleep(300 * time.Millisecond)
	return "out:" + string(a), nil
}

func TestExecuteToolsParallelAndOrdered(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(slowTool{"ro", true})
	r.Register(slowTool{"rw", false})
	e := NewAgentEngine(nil, r, t.TempDir(), false)
	calls := []schema.ToolCall{
		{ID: "a", Name: "ro", Arguments: []byte(`"1"`)},
		{ID: "b", Name: "ro", Arguments: []byte(`"2"`)},
		{ID: "c", Name: "ro", Arguments: []byte(`"3"`)},
	}
	start := time.Now()
	obs := e.executeTools(context.Background(), calls, NopReporter{})
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Fatalf("只读批次应并发执行，耗时 %v", d)
	}
	for i, o := range obs {
		if o.ToolCallID != calls[i].ID || o.Content != "out:"+string(calls[i].Arguments) {
			t.Fatalf("结果顺序错乱: %#v", obs)
		}
	}
	calls[1].Name = "rw"
	start = time.Now()
	e.executeTools(context.Background(), calls, NopReporter{})
	if d := time.Since(start); d < 850*time.Millisecond {
		t.Fatalf("含写操作的批次应串行，耗时 %v", d)
	}
}
