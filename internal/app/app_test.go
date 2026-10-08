package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/envlock"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

type retrievalAssemblyModel struct {
	calls int
}

func (m *retrievalAssemblyModel) Generate(_ context.Context, _ []schema.Message, defs []schema.ToolDefinition) (*schema.Message, error) {
	step := m.calls % 3
	m.calls++
	if step == 0 {
		for _, d := range defs {
			if d.Name == "search" || d.Name == "evidence" {
				return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "e1", Name: "evidence", Arguments: json.RawMessage(`{"action":"add_gap","id":"g1","text":"find hello"}`)}}}, nil
			}
		}
		return &schema.Message{Role: schema.RoleAssistant, Content: "检索工具未装配"}, nil
	}
	if step == 1 {
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "s1", Name: "search", Arguments: json.RawMessage(`{"query":"hello","gap_id":"g1","strategy":"new_term"}`)}}}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "完成。"}, nil
}

type servedModel string

func (m servedModel) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (*schema.Message, error) {
	return &schema.Message{Role: schema.RoleAssistant, Content: "完成。", Model: string(m)}, nil
}

func lockWorkspace(t *testing.T, mode envlock.Mode, observed string) string {
	t.Helper()
	wd := t.TempDir()
	skill := filepath.Join(wd, ".claw", "skills", "third-party", "demo", "SKILL.md")
	_ = os.MkdirAll(filepath.Dir(skill), 0o755)
	_ = os.WriteFile(skill, []byte("---\nname: demo\n---\nv1"), 0o644)
	l, err := envlock.Capture(envlock.CaptureOptions{WorkDir: wd, Tools: []string{"bash"}})
	if err != nil {
		t.Fatal(err)
	}
	l.Mode, l.Model.Observed = mode, observed
	if err := envlock.Save(wd, l); err != nil {
		t.Fatal(err)
	}
	return wd
}

func build(wd string, llm servedModel) (*App, error) {
	return Build(Options{WorkDir: wd, LLM: llm, StateDir: filepath.Join(wd, ".claw", "state"), NoKnowledge: true})
}

func TestBuildEnforcesEnvLock(t *testing.T) {
	// 一致：strict 也放行
	wd := lockWorkspace(t, envlock.ModeStrict, "")
	if _, err := build(wd, "m"); err != nil {
		t.Fatalf("环境一致时不应拒绝: %v", err)
	}
	// 第三方 Skill 被悄悄改了：strict 拒绝启动，warn 放行
	skill := filepath.Join(wd, ".claw", "skills", "third-party", "demo", "SKILL.md")
	_ = os.WriteFile(skill, []byte("---\nname: demo\n---\nv2"), 0o644)
	if _, err := build(wd, "m"); err == nil || !strings.Contains(err.Error(), "漂移") {
		t.Fatalf("strict 下 Skill 漂移必须拒绝启动，得到 %v", err)
	}
	wdWarn := lockWorkspace(t, envlock.ModeWarn, "")
	_ = os.WriteFile(filepath.Join(wdWarn, ".claw", "skills", "third-party", "demo", "SKILL.md"), []byte("v2"), 0o644)
	if _, err := build(wdWarn, "m"); err != nil {
		t.Fatalf("warn 下只报告不拒绝: %v", err)
	}
	// 锁文件损坏：拒绝启动，而不是当作未锁定
	_ = os.WriteFile(filepath.Join(wdWarn, envlock.LockFile), []byte("{"), 0o644)
	if _, err := build(wdWarn, "m"); err == nil {
		t.Fatal("锁文件损坏必须拒绝启动")
	}
}

func TestBuildInstallsProductionRunBudget(t *testing.T) {
	a, err := Build(Options{WorkDir: t.TempDir(), LLM: servedModel("m"), StateDir: t.TempDir(), NoKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Engine.MaxDuration <= 0 {
		t.Fatalf("assembled engine must have a production duration budget, got %v", a.Engine.MaxDuration)
	}
}

func TestBuildMainRunCanReachRetrievalTools(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "notes.md"), []byte("# Notes\nhello from the retrieval fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &retrievalAssemblyModel{}
	stateDir := t.TempDir()
	a, err := Build(Options{WorkDir: wd, LLM: model, StateDir: stateDir, NoKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	sess := ctxpkg.NewSession("assembly", wd)
	if err := a.Engine.Chat(context.Background(), sess, "search", engine.NopReporter{}); err != nil {
		t.Fatalf("main run with retrieval tools failed: %v", err)
	}
	if model.calls != 3 {
		t.Fatalf("expected evidence and search tool calls in first run, got %d calls", model.calls)
	}
	if err := a.Engine.Chat(context.Background(), sess, "search again", engine.NopReporter{}); err != nil {
		t.Fatalf("second main run with same query should use a fresh retrieval task: %v", err)
	}
	if model.calls != 6 {
		t.Fatalf("expected two complete runs, got %d calls", model.calls)
	}
	searchResults := 0
	for _, msg := range sess.History() {
		if msg.IsToolResult() && msg.IsError {
			t.Fatalf("assembled retrieval tool returned error: %s", msg.Content)
		}
		if msg.IsToolResult() && strings.Contains(msg.Content, "notes.md") {
			searchResults++
			if strings.Contains(msg.Content, ".state") {
				t.Fatalf("search result leaked state directory: %s", msg.Content)
			}
		}
	}
	if searchResults != 2 {
		t.Fatalf("expected both runs to retrieve notes.md, got %d results", searchResults)
	}
}

func TestStrictLockRejectsSilentModelRoll(t *testing.T) {
	wd := lockWorkspace(t, envlock.ModeStrict, "deepseek-v4-flash-0801")
	a, err := build(wd, "deepseek-v4.1-flash") // 名字没变，服务端已换代
	if err != nil {
		t.Fatal(err)
	}
	sess := ctxpkg.NewSessionManager("").GetOrCreate("s", wd)
	err = a.Engine.Chat(context.Background(), sess, "hi", engine.NewTerminalReporter())
	if err == nil || !strings.Contains(err.Error(), "模型已漂移") {
		t.Fatalf("strict 下模型静默滚动必须中止运行，得到 %v", err)
	}
	if a.Model.Observed() != "deepseek-v4.1-flash" || a.Model.Drifts() != 1 {
		t.Fatalf("哨兵应记录实际版本: %q drifts=%d", a.Model.Observed(), a.Model.Drifts())
	}
}

func TestInjectedLLMStillChecksConfiguredModelIdentity(t *testing.T) {
	wd := t.TempDir()
	l, err := envlock.Capture(envlock.CaptureOptions{WorkDir: wd, Tools: []string{"bash"},
		Model: envlock.Model{Provider: "openai", BaseURL: "https://a/v1", Requested: "m1"}})
	if err != nil {
		t.Fatal(err)
	}
	l.Mode = envlock.ModeStrict
	if err := envlock.Save(wd, l); err != nil {
		t.Fatal(err)
	}
	opts := func(c provider.Config) Options {
		return Options{WorkDir: wd, LLM: servedModel("m1"), Provider: c, StateDir: filepath.Join(wd, ".claw", "state"), NoKnowledge: true}
	}
	for name, c := range map[string]provider.Config{
		"requested": {Kind: "openai", BaseURL: "https://a/v1", Model: "m2"},
		"base_url":  {BaseURL: "https://b/v1"},
		"provider":  {Kind: "claude"},
	} {
		if _, err := Build(opts(c)); err == nil {
			t.Errorf("注入 LLM 但配置了 %s，strict 下配置漂移必须拒绝", name)
		}
	}
	// 只对未配置的字段豁免：无身份注入、部分字段一致都放行
	for _, c := range []provider.Config{{}, {Model: "m1"}, {Kind: "openai", BaseURL: "https://a/v1", Model: "m1"}} {
		if _, err := Build(opts(c)); err != nil {
			t.Errorf("配置 %+v 与锁一致，不应拒绝: %v", c, err)
		}
	}
}

func TestBuildSharesWorkspaceLocks(t *testing.T) {
	a1, err := build(t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := build(t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if a1.Engine.WorkspaceLocks == nil || a1.Engine.WorkspaceLocks != a2.Engine.WorkspaceLocks {
		t.Fatal("所有引擎必须共享同一张进程级工作区锁表")
	}
}
