package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/keylock"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
	"github.com/weberzhao/go-tiny-claw/internal/trace"
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

type slowProvider struct{ delay time.Duration }

func (p slowProvider) Generate(ctx context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	select {
	case <-time.After(p.delay):
		return &schema.Message{Role: schema.RoleAssistant, Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunDurationBudgetIsHardAndTyped(t *testing.T) {
	e := NewAgentEngine(slowProvider{100 * time.Millisecond}, &fakeRegistry{}, false, false)
	e.MaxDuration = 10 * time.Millisecond
	err := e.Chat(context.Background(), ctxpkg.NewSession("budget", t.TempDir()), "run", nil)
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) || exceeded.Budget != "duration" {
		t.Fatalf("want typed duration budget error, got %v", err)
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

// stubbornProvider 永远原样重试同一个失败的调用，直到看到熔断消息。
type stubbornProvider struct{ calls int }

func (p *stubbornProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	last := msgs[len(msgs)-1]
	if strings.Contains(last.Content, "Harness 熔断") {
		return &schema.Message{Role: schema.RoleAssistant, Content: "放弃，需要人工帮助"}, nil
	}
	p.calls++
	return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprint("c", p.calls), Name: "read_file", Arguments: []byte(`{"path":"nope.txt"}`)}}}, nil
}

func TestDoomLoopBreaker(t *testing.T) {
	wd := t.TempDir()
	r := tools.NewRegistry()
	r.Register(tools.NewReadFileTool(wd))
	p := &stubbornProvider{}
	sess := ctxpkg.NewSession("t", wd)
	if err := NewAgentEngine(p, r, false, false).Chat(context.Background(), sess, "读 nope.txt", nil); err != nil {
		t.Fatal(err)
	}
	var reminders, executed int
	for _, m := range sess.History() {
		if !m.IsToolResult() && strings.Contains(m.Content, "SYSTEM REMINDER") {
			reminders++
		}
		if m.IsToolResult() && !strings.Contains(m.Content, "Harness 熔断") {
			executed++
		}
	}
	if reminders != 1 || executed != 3 || p.calls != 4 {
		t.Fatalf("reminders=%d executed=%d calls=%d", reminders, executed, p.calls)
	}
	if !strings.Contains(sess.History()[len(sess.History())-1].Content, "需要人工帮助") {
		t.Fatal("熔断后模型应收手")
	}
}

// 第 19 讲：Trace 树结构、逐轮结束、子智能体子树与导出文件。
func TestTraceTree(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, "legacy"), 0o755)
	os.WriteFile(filepath.Join(wd, "legacy/config.txt"), []byte("核心密码是: pw_42"), 0o644)
	main := tools.NewRegistry()
	ro := tools.NewRegistry()
	ro.Register(tools.NewBashTool(wd))
	eng := NewAgentEngine(subProvider{}, main, false, false)
	main.Register(tools.NewSubagentTool(eng, ro))
	var root *trace.Span
	eng.OnRunFinished = func(r *trace.Span) { root = r }
	if err := eng.Chat(context.Background(), ctxpkg.NewSession("trace-t", wd), "找密码", nil); err != nil {
		t.Fatal(err)
	}
	if root == nil || root.Name != "Agent.Run" || root.Get("turns") != 2 {
		t.Fatalf("根 Span 异常: %+v", root)
	}
	if root.Count("Turn-1") != 1 || root.Count("Subagent.Run") != 2 || root.Count("LLM.Action") != 2+4 {
		t.Fatalf("树结构: turns=%d sub=%d llm=%d", root.Count("Turn-1"), root.Count("Subagent.Run"), root.Count("LLM.Action"))
	}
	var turn1End, turn2Start time.Time
	root.Walk(func(sp *trace.Span, depth int) {
		if sp.Name == "Turn-1" {
			turn1End = sp.EndTime
		}
		if sp.Name == "Turn-2" {
			turn2Start = sp.StartTime
		}
	})
	if turn1End.IsZero() || turn1End.After(turn2Start) {
		t.Fatal("Turn Span 应在下一轮开始前结束")
	}
	files, _ := filepath.Glob(filepath.Join(wd, ".claw/traces/trace_trace-t_*"))
	if len(files) != 2 {
		t.Fatalf("应导出决策树与 perfetto 两个文件: %v", files)
	}
}

// lazyProvider：第一次只"宣布要做"，被提醒后才真正调用工具。
type lazyProvider struct{ calls int }

func (l *lazyProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	l.calls++
	last := msgs[len(msgs)-1]
	switch {
	case last.IsToolResult():
		return &schema.Message{Role: schema.RoleAssistant, Content: "已完成，结果: " + last.Content}, nil
	case strings.Contains(last.Content, "[系统提醒]"):
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "x", Name: "bash", Arguments: []byte(`{"command":"echo hi"}`)}}}, nil
	default:
		return &schema.Message{Role: schema.RoleAssistant, Content: "好的，首先我将检查 error.log 文件。"}, nil
	}
}

func TestEarlyExitNudge(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(tools.NewBashTool(t.TempDir()))
	p := &lazyProvider{}
	sess := ctxpkg.NewSession("t", t.TempDir())
	if err := NewAgentEngine(p, r, false, false).Chat(context.Background(), sess, "查日志", nil); err != nil {
		t.Fatal(err)
	}
	h := sess.History()
	if !strings.Contains(h[len(h)-1].Content, "已完成，结果: hi") {
		t.Fatalf("防早退后应真正执行工具: %#v", h[len(h)-1])
	}
	// 永远只说不做的模型：最多提醒 EarlyExitNudges 次后正常结束，不会死循环
	stub := &stubbornTalker{}
	eng := NewAgentEngine(stub, r, false, false)
	if err := eng.Chat(context.Background(), ctxpkg.NewSession("t2", t.TempDir()), "x", nil); err != nil || stub.n != 1+eng.EarlyExitNudges {
		t.Fatalf("n=%d err=%v", stub.n, err)
	}
	// 正常的最终答复不应被误判
	if announcesUnfinishedAction(func() *ctxpkg.Session {
		s := ctxpkg.NewSession("x", "")
		s.Append(schema.Message{Role: schema.RoleAssistant, Content: "修复完成：已将 locat 改为 location，nginx -t 通过。"})
		return s
	}()) {
		t.Fatal("最终答复被误判为早退")
	}
}

type stubbornTalker struct{ n int }

func (s *stubbornTalker) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (*schema.Message, error) {
	s.n++
	return &schema.Message{Role: schema.RoleAssistant, Content: "让我先看看。"}, nil
}

// todoProvider：第一次直接宣布完成；看到 TODO 未完成提醒后把 TODO.md 勾完再结束。
type todoProvider struct {
	wd    string
	calls int
}

func (p *todoProvider) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	p.calls++
	last := msgs[len(msgs)-1]
	if strings.Contains(last.Content, "TODO.md 中仍有") {
		_ = os.WriteFile(filepath.Join(p.wd, "TODO.md"), []byte("- [x] 写代码\n- [x] 写测试\n"), 0o644)
		return &schema.Message{Role: schema.RoleAssistant, Content: "都做完了。"}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "任务完成。"}, nil
}

// 第 02 讲作者答疑"完成度校验"：Plan Mode 下 TODO.md 还有 - [ ] 时模型想退出，Harness 拦截并提醒。
func TestPlanModeBlocksExitWithUncheckedTodos(t *testing.T) {
	wd := t.TempDir()
	_ = os.WriteFile(filepath.Join(wd, "TODO.md"), []byte("- [x] 写代码\n- [ ] 写测试\n"), 0o644)
	p := &todoProvider{wd: wd}
	if err := NewAgentEngine(p, &fakeRegistry{}, false, true).Chat(context.Background(), ctxpkg.NewSession("t", wd), "做完它", nil); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatalf("未完成的 TODO 应拦截一次退出，模型调用次数=%d", p.calls)
	}
	// 永远不勾 TODO 的模型：有限次提醒后正常结束，不死循环
	_ = os.WriteFile(filepath.Join(wd, "TODO.md"), []byte("- [ ] 永远做不完\n"), 0o644)
	stub := &stubbornTalker{}
	if err := NewAgentEngine(stub, &fakeRegistry{}, false, true).Chat(context.Background(), ctxpkg.NewSession("t2", wd), "做", nil); err != nil {
		t.Fatal(err)
	}
	if stub.n > 1+2+2 { // 首轮 + 最多 2 次防早退 + 最多 2 次 TODO 提醒
		t.Fatalf("TODO 提醒必须有上限, calls=%d", stub.n)
	}
	// 非 Plan Mode 不读 TODO.md
	p2 := &todoProvider{wd: wd}
	_ = NewAgentEngine(p2, &fakeRegistry{}, false, false).Chat(context.Background(), ctxpkg.NewSession("t3", wd), "做", nil)
	if p2.calls != 1 {
		t.Fatalf("非 Plan Mode 不应做 TODO 完成度校验, calls=%d", p2.calls)
	}
}

// gateProvider：记录并发进入 Generate 的数量，第一次调用阻塞直到 release 关闭。
type gateProvider struct {
	mu      sync.Mutex
	active  int
	max     int
	started chan struct{}
	release chan struct{}
}

func (g *gateProvider) Generate(ctx context.Context, _ []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	g.mu.Lock()
	g.active++
	if g.active > g.max {
		g.max = g.active
	}
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.active--; g.mu.Unlock() }()
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "完成。"}, nil
}

// 第 09 讲思考题：同一工作区同一时间只允许一个 Agent 任务执行（跨会话），等待可被取消。
func TestWorkspaceLockSerializesRunsAcrossSessions(t *testing.T) {
	wd := t.TempDir()
	g := &gateProvider{started: make(chan struct{}, 4), release: make(chan struct{})}
	e := NewAgentEngine(g, &fakeRegistry{}, false, false)
	e.WorkspaceLocks = keylock.New()
	done := make(chan error, 2)
	go func() { done <- e.Chat(context.Background(), ctxpkg.NewSession("a", wd), "任务A", nil) }()
	<-g.started
	go func() { done <- e.Chat(context.Background(), ctxpkg.NewSession("b", wd), "任务B", nil) }()
	select {
	case <-g.started:
		t.Fatal("同一工作区的第二个任务不应在第一个结束前开始")
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.Chat(ctx, ctxpkg.NewSession("c", wd), "任务C", nil); err == nil {
		t.Fatal("等待工作区锁时 ctx 超时应返回错误")
	}
	close(g.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if g.max != 1 {
		t.Fatalf("同一工作区的任务发生了并发, max=%d", g.max)
	}
}

// nestRegistry：所有工具只读；spawn_subagent 在执行中再并发派发 5 个叶子工具（模拟子 Agent 的并行批次）。
type nestRegistry struct {
	e           *AgentEngine
	mu          sync.Mutex
	active, max int
}

func (n *nestRegistry) Register(tools.BaseTool)  {}
func (n *nestRegistry) Use(tools.MiddlewareFunc) {}
func (n *nestRegistry) IsReadOnly(string) bool   { return true }
func (n *nestRegistry) GetAvailableTools() []schema.ToolDefinition {
	return []schema.ToolDefinition{{Name: "leaf"}, {Name: tools.SubagentToolName}}
}
func (n *nestRegistry) Execute(ctx context.Context, call schema.ToolCall) schema.ToolResult {
	if call.Name == tools.SubagentToolName {
		leaves := make([]schema.ToolCall, 5)
		for i := range leaves {
			leaves[i] = schema.ToolCall{ID: fmt.Sprint(i), Name: "leaf"}
		}
		n.e.executeToolsWith(ctx, n, leaves, NopReporter{})
		return schema.ToolResult{ToolCallID: call.ID, Output: "sub done"}
	}
	n.mu.Lock()
	n.active++
	if n.active > n.max {
		n.max = n.active
	}
	n.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	n.mu.Lock()
	n.active--
	n.mu.Unlock()
	return schema.ToolResult{ToolCallID: call.ID, Output: "ok"}
}

// 第 08 讲思考题：并发上限是全局的——5 个并行子 Agent 各自再并发 5 个工具，叶子工具总并发仍 ≤ MaxParallelTools，且不死锁。
func TestGlobalToolConcurrencyCapWithNestedSubagents(t *testing.T) {
	e := NewAgentEngine(&scriptedProvider{}, &fakeRegistry{}, false, false)
	n := &nestRegistry{e: e}
	calls := make([]schema.ToolCall, 5)
	for i := range calls {
		calls[i] = schema.ToolCall{ID: fmt.Sprint("s", i), Name: tools.SubagentToolName}
	}
	done := make(chan struct{})
	go func() { e.executeToolsWith(context.Background(), n, calls, NopReporter{}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("嵌套子 Agent 的并发执行发生死锁")
	}
	if n.max > MaxParallelTools {
		t.Fatalf("叶子工具总并发 %d 超过全局上限 %d", n.max, MaxParallelTools)
	}
}
