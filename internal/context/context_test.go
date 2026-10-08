package context

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func TestComposer(t *testing.T) {
	wd := t.TempDir()
	os.WriteFile(filepath.Join(wd, "AGENTS.md"), []byte("所有错误必须中文"), 0o644)
	os.MkdirAll(filepath.Join(wd, ".claw/skills/git-workflow"), 0o755)
	os.WriteFile(filepath.Join(wd, ".claw/skills/git-workflow/SKILL.md"), []byte("---\r\nname: git-workflow\r\ndescription: \"提交代码时使用\"\r\n---\r\n# SOP\r\n1. git status\r\n"), 0o644)

	off := NewPromptComposer(wd, false).Build()
	if off.Role != schema.RoleSystem || !strings.Contains(off.Content, "所有错误必须中文") {
		t.Fatal("AGENTS.md 未注入")
	}
	if !strings.Contains(off.Content, "技能名称: git-workflow") || !strings.Contains(off.Content, "**触发条件**: 提交代码时使用") || !strings.Contains(off.Content, "1. git status") {
		t.Fatalf("Skill 解析/注入失败:\n%s", off.Content)
	}
	if strings.Contains(off.Content, "Plan Mode: ON") {
		t.Fatal("Plan Mode 关闭时不应注入")
	}
	if on := NewPromptComposer(wd, true).Build(); !strings.Contains(on.Content, "Plan Mode: ON") || !strings.Contains(on.Content, "TODO.md") {
		t.Fatal("Plan Mode 开启时应注入状态外部化规范")
	}
	if bare := NewPromptComposer(t.TempDir(), false).Build(); strings.Contains(bare.Content, "AGENTS.md)") || strings.Contains(bare.Content, "技能名称") {
		t.Fatal("空工作区不应出现 AGENTS/Skills 段落")
	}
}

func TestWorkingMemoryDropsOrphans(t *testing.T) {
	s := NewSession("s", "")
	s.Append(
		schema.Message{Role: schema.RoleUser, Content: "q"},
		schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "1"}, {ID: "2"}}},
		schema.Message{Role: schema.RoleUser, ToolCallID: "1", Content: "r1"},
		schema.Message{Role: schema.RoleUser, ToolCallID: "2", Content: "r2"},
		schema.Message{Role: schema.RoleAssistant, Content: "done"},
	)
	wm := s.GetWorkingMemory(3)
	if len(wm) != 1 || wm[0].Content != "done" {
		t.Fatalf("截断后首部孤儿工具结果应被丢弃: %#v", wm)
	}
	if len(s.GetWorkingMemory(0)) != 5 || len(s.GetWorkingMemory(10)) != 5 {
		t.Fatal("全量返回错误")
	}
	wm[0].Content = "mutated"
	if s.History()[4].Content != "done" {
		t.Fatal("必须返回拷贝")
	}
}

func TestSessionIsolationAndPersistence(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(dir)
	a, b := m.GetOrCreate("chat/A", "/w"), m.GetOrCreate("chat_B", "/w")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); a.Append(schema.Message{Role: schema.RoleUser, Content: "a"}) }()
		go func() { defer wg.Done(); b.Append(schema.Message{Role: schema.RoleUser, Content: "b"}) }()
	}
	wg.Wait()
	if a.Len() != 50 || b.Len() != 50 || m.GetOrCreate("chat/A", "") != a {
		t.Fatal("会话隔离失败")
	}
	// 模拟进程重启：新 Manager 从磁盘恢复
	m2 := NewSessionManager(dir)
	a2 := m2.GetOrCreate("chat/A", "/w")
	if a2.Len() != 50 || a2.History()[0].Content != "a" {
		t.Fatalf("持久化恢复失败: %d", a2.Len())
	}
	m2.Reset("chat/A")
	if a2.Len() != 0 || NewSessionManager(dir).GetOrCreate("chat/A", "").Len() != 0 {
		t.Fatal("Reset 未清空")
	}
}

func TestResetRemovesUnloadedPersistedSession(t *testing.T) {
	dir := t.TempDir()
	first := NewSessionManager(dir)
	s := first.GetOrCreate("after-restart", t.TempDir())
	s.Append(schema.Message{Role: schema.RoleUser, Content: "persisted"})
	// Simulate a process restart: a fresh manager has no in-memory session.
	second := NewSessionManager(dir)
	second.Reset("after-restart")
	reloaded := second.GetOrCreate("after-restart", t.TempDir())
	if got := reloaded.Len(); got != 0 {
		t.Fatalf("reset must remove an unloaded persisted session, got %d messages", got)
	}
}

func TestCompactor(t *testing.T) {
	big := strings.Repeat("这是一段极其冗长的日志\n", 800)
	msgs := []schema.Message{
		{Role: schema.RoleSystem, Content: strings.Repeat("S", 3000)},
		{Role: schema.RoleUser, Content: "开始"},
		{Role: schema.RoleAssistant, Content: strings.Repeat("思考", 200), ToolCalls: []schema.ToolCall{{ID: "1", Name: "read_file", Arguments: []byte(`{"path":"old.log"}`)}}},
		{Role: schema.RoleUser, ToolCallID: "1", Content: big},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "2", Name: "read_file", Arguments: []byte(`{"path":"new.log"}`)}}},
		{Role: schema.RoleUser, ToolCallID: "2", Content: big},
	}
	c := NewCompactor(3000, 2)
	out := c.Compact(msgs)
	if out[0].Content != msgs[0].Content {
		t.Fatal("System Prompt 不能动")
	}
	if !strings.Contains(out[3].Content, "早期的工具输出已被系统强制清理") {
		t.Fatalf("远期工具输出应被掩码: %q", out[3].Content[:50])
	}
	if out[2].Content != "...[早期的推理思考过程已折叠]..." || len(out[2].ToolCalls) != 1 {
		t.Fatal("远期思考应折叠，ToolCalls 必须保留")
	}
	if !strings.Contains(out[5].Content, "中间") || len(out[5].Content) > 1200 || !utf8.ValidString(out[5].Content) {
		t.Fatalf("保护区应掐头去尾且 UTF-8 合法: len=%d", len(out[5].Content))
	}
	if msgs[3].Content != big {
		t.Fatal("不得修改入参")
	}
	if small := c.Compact(msgs[1:3]); len(small) != 2 || &small[0] != &msgs[1] {
		t.Fatal("未超水位时应原样返回")
	}
}

func TestRecovery(t *testing.T) {
	rm := NewRecoveryManager()
	cases := []struct{ tool, err, want string }{
		{"edit_file", "Error executing edit_file: 在文件中未找到 old_text，请检查内容和缩进", "read_file"},
		{"edit_file", "old_text 匹配到了多处 2 处", "上下相邻"},
		{"read_file", "打开文件失败: open /w/x: no such file or directory", "find ."},
		{"bash", "执行报错: exit status 127\n输出:\nbash: foo: command not found", "替代命令"},
		{"bash", "命令执行超时(30s)，已被系统强制终止 (DeadlineExceeded)", "nohup"},
		{"write_file", "路径 \"../x\" 越出了工作区边界 /w，已被拒绝", "相对工作区"},
	}
	for _, c := range cases {
		got := rm.AnalyzeAndInject(c.tool, c.err)
		if !strings.Contains(got, "[系统救援指南]") || !strings.Contains(got, c.want) || !strings.HasPrefix(got, c.err) {
			t.Fatalf("%s/%s => %s", c.tool, c.err, got)
		}
	}
	if got := rm.AnalyzeAndInject("bash", "some unknown failure"); got != "some unknown failure" {
		t.Fatal("未命中时应原样返回")
	}
	if got := rm.AnalyzeAndInject("bash", "no such file or directory"); got != "no such file or directory" {
		t.Fatal("文件类规则不应作用于 bash")
	}
}

func TestRepairToolPairs(t *testing.T) {
	msgs := []schema.Message{
		{Role: schema.RoleUser, ToolCallID: "orphan", Content: "x"},
		{Role: schema.RoleUser, Content: "q"},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: schema.RoleUser, ToolCallID: "b", Content: "rb"},
		{Role: schema.RoleUser, Content: "重启后的新指令"},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c"}}}, // 结尾处中断
	}
	out := RepairToolPairs(msgs)
	var ids []string
	for _, m := range out {
		ids = append(ids, string(m.Role)+":"+m.ToolCallID)
	}
	want := "user:,assistant:,user:a,user:b,user:,assistant:,user:c"
	if strings.Join(ids, ",") != want {
		t.Fatalf("got %s", strings.Join(ids, ","))
	}
	if out[2].Content != InterruptedToolOutput || !out[2].IsError || out[3].Content != "rb" {
		t.Fatal("补位/保序错误")
	}
}

func TestSessionFileNoCollision(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(dir)
	m.GetOrCreate("a/b", "").Append(schema.Message{Role: schema.RoleUser, Content: "1"})
	m.GetOrCreate("a_b", "").Append(schema.Message{Role: schema.RoleUser, Content: "2"})
	if n := NewSessionManager(dir).GetOrCreate("a_b", "").Len(); n != 1 {
		t.Fatalf("a/b 与 a_b 串了文件: %d", n)
	}
}

// 回归：codex review —— 尾部半行 JSON 不应导致整段历史丢失。
func TestSessionTruncatedTailRepair(t *testing.T) {
	dir := t.TempDir()
	m := NewSessionManager(dir)
	s := m.GetOrCreate("s", "")
	s.Append(schema.Message{Role: schema.RoleUser, Content: "1"}, schema.Message{Role: schema.RoleAssistant, Content: "2"})
	f, _ := os.OpenFile(s.logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"role":"user","cont`)
	f.Close()

	s2 := NewSessionManager(dir).GetOrCreate("s", "")
	if s2.Len() != 2 {
		t.Fatalf("应保留合法前缀，got %d", s2.Len())
	}
	s2.Append(schema.Message{Role: schema.RoleUser, Content: "3"})
	if n := NewSessionManager(dir).GetOrCreate("s", "").Len(); n != 3 {
		t.Fatalf("修复后应能继续追加并恢复，got %d", n)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "*.corrupt-*"))
	if len(backups) != 1 {
		t.Fatal("原损坏文件应被隔离备份")
	}
}

func TestEnvSection(t *testing.T) {
	wd := t.TempDir()
	os.WriteFile(filepath.Join(wd, "nginx.conf"), nil, 0o644)
	os.MkdirAll(filepath.Join(wd, "logs"), 0o755)
	os.MkdirAll(filepath.Join(wd, ".claw"), 0o755)
	c := NewPromptComposer(wd, false).Build().Content
	if !strings.Contains(c, "# 运行环境") || !strings.Contains(c, wd) || !strings.Contains(c, "nginx.conf") || !strings.Contains(c, "logs/") || strings.Contains(c, ".claw/") {
		t.Fatalf("运行环境段落错误:\n%s", c)
	}
}

// Registry 的统一参数校验（"参数校验失败"）同样要拿到自愈提示，否则 P0 引入的校验层会让类型错误失去救援指南。
func TestRecoveryCoversRegistrySchemaValidation(t *testing.T) {
	rm := NewRecoveryManager()
	for _, raw := range []string{`参数校验失败: 缺少必填参数 "path"`, `参数校验失败: 参数 "limit" 必须是 number`} {
		if got := rm.AnalyzeAndInject("read_file", raw); !strings.Contains(got, "系统救援指南") {
			t.Errorf("schema 校验错误缺少救援提示: %q", got)
		}
	}
}
