package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
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

func (f *fakeRegistry) Register(tools.BaseTool)  {}
func (f *fakeRegistry) Use(tools.MiddlewareFunc) {}
func (f *fakeRegistry) IsReadOnly(string) bool   { return false }

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
		sess := ctxpkg.NewSession("t", t.TempDir())
		if err := NewAgentEngine(p, r, thinking, false).Chat(context.Background(), sess, "检查文件", nil); err != nil {
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
	e := NewAgentEngine(nil, r, false, false)
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

func TestThinkingMergedIntoSingleAssistant(t *testing.T) {
	p, r := &scriptedProvider{}, &fakeRegistry{}
	sess := ctxpkg.NewSession("t", t.TempDir())
	if err := NewAgentEngine(p, r, true, false).Chat(context.Background(), sess, "检查文件", nil); err != nil {
		t.Fatal(err)
	}
	h := sess.History()
	// user, assistant(思考+行动+toolcall), tool, assistant(思考+总结)
	if len(h) != 4 {
		t.Fatalf("session 历史条数错误: %d %#v", len(h), h)
	}
	for i := 1; i < len(h); i++ {
		if h[i].Role == schema.RoleAssistant && h[i-1].Role == schema.RoleAssistant {
			t.Fatal("不得出现连续两条 assistant")
		}
	}
	if !strings.Contains(h[1].Content, "【推理中】") || len(h[1].ToolCalls) != 1 {
		t.Fatalf("思考应与行动合并: %#v", h[1])
	}
	for _, m := range h {
		if strings.Contains(m.Content, "[系统:") {
			t.Fatal("临时 nudge 不应写入 Session")
		}
	}
}

// subProvider：主 Agent 派出两个子智能体（并行），子智能体用 bash 查找后汇报。
type subProvider struct{}

func (subProvider) Generate(_ context.Context, msgs []schema.Message, defs []schema.ToolDefinition) (*schema.Message, error) {
	last := msgs[len(msgs)-1]
	isSub := strings.Contains(msgs[0].Content, "Explorer Subagent")
	switch {
	case isSub && !last.IsToolResult():
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "s1", Name: "bash", Arguments: []byte(`{"command":"cat legacy/config.txt"}`)}}}, nil
	case isSub:
		return &schema.Message{Role: schema.RoleAssistant, Content: "找到: " + strings.TrimSpace(last.Content)}, nil
	case !last.IsToolResult():
		for _, d := range defs {
			if d.Name == "spawn_subagent" {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{
					{ID: "m1", Name: "spawn_subagent", Arguments: []byte(`{"task_prompt":"找密码 A"}`)},
					{ID: "m2", Name: "spawn_subagent", Arguments: []byte(`{"task_prompt":"找密码 B"}`)},
				}}, nil
			}
		}
		return nil, fmt.Errorf("主 Agent 看不到 spawn_subagent")
	default:
		return &schema.Message{Role: schema.RoleAssistant, Content: "主 Agent 收到: " + last.Content}, nil
	}
}

func TestSubagentIsolationAndReadOnly(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, "legacy"), 0o755)
	os.WriteFile(filepath.Join(wd, "legacy/config.txt"), []byte("核心密码是: pw_42"), 0o644)
	main := tools.NewRegistry()
	ro := tools.NewRegistry()
	ro.Register(tools.NewBashTool(wd))
	ro.Use(tools.ReadOnlyGuard())
	eng := NewAgentEngine(subProvider{}, main, false, false)
	main.Register(tools.NewSubagentTool(eng, ro))

	sess := ctxpkg.NewSession("t", wd)
	if err := eng.Chat(context.Background(), sess, "找密码", nil); err != nil {
		t.Fatal(err)
	}
	h := sess.History()
	// 主会话只有：user, assistant(2 spawn), 2 个报告, assistant(总结) —— 子智能体的 bash 轨迹不污染主干
	if len(h) != 5 {
		t.Fatalf("主上下文被污染: %d 条", len(h))
	}
	for _, m := range h[2:4] {
		if !strings.Contains(m.Content, "【子智能体探索报告】") || !strings.Contains(m.Content, "pw_42") {
			t.Fatalf("报告错误: %s", m.Content)
		}
	}
	// 只读守卫
	res := ro.Execute(context.Background(), schema.ToolCall{ID: "w", Name: "bash", Arguments: []byte(`{"command":"echo hacked > legacy/config.txt"}`)})
	if !res.IsError || !strings.Contains(res.Output, "只读") {
		t.Fatalf("子智能体写操作应被拦截: %#v", res)
	}
	for _, okCmd := range []string{"find . -name '*.txt' 2>/dev/null", "grep -rn 密码 . | head -5", "ls -la 2>&1"} {
		b, _ := json.Marshal(map[string]string{"command": okCmd})
		if r := ro.Execute(context.Background(), schema.ToolCall{ID: "r", Name: "bash", Arguments: b}); r.IsError {
			t.Fatalf("只读命令 %q 被误拦: %s", okCmd, r.Output)
		}
	}
}
