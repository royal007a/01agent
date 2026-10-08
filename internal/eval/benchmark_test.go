package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// scripted：先读一个不存在的文件（制造一次工具报错），再写 answer.txt，然后收工。
type scripted struct{}

func (scripted) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	n := 0
	for _, m := range msgs {
		if m.IsToolResult() {
			n++
		}
	}
	u := &schema.Usage{PromptTokens: 100, CompletionTokens: 10}
	switch n {
	case 0:
		return &schema.Message{Role: schema.RoleAssistant, Usage: u, ToolCalls: []schema.ToolCall{{ID: "1", Name: "read_file", Arguments: []byte(`{"path":"nope.txt"}`)}}}, nil
	case 1:
		return &schema.Message{Role: schema.RoleAssistant, Usage: u, ToolCalls: []schema.ToolCall{{ID: "2", Name: "write_file", Arguments: []byte(`{"path":"answer.txt","content":"42"}`)}}}, nil
	default:
		return &schema.Message{Role: schema.RoleAssistant, Usage: u, Content: "done"}, nil
	}
}

func TestRunSuiteAndReport(t *testing.T) {
	t.Setenv("CLAW_PRICE_IN", "1")
	t.Setenv("CLAW_PRICE_OUT", "1")
	r := &Runner{Provider: provider.Config{Model: "fake", Kind: "openai"}, LLM: scripted{}, WorkRoot: t.TempDir(), Parallelism: 2}
	cases := []TestCase{
		{ID: "ok", Name: "写答案", Setup: "echo seed > seed.txt", Prompt: "写 42", Validate: `[ "$(cat answer.txt)" = 42 ] && test -f seed.txt`},
		{ID: "bad", Name: "判卷失败", Prompt: "写 43", Validate: `[ "$(cat answer.txt)" = 43 ]`},
		{ID: "setupfail", Name: "靶机失败", Setup: "exit 3", Prompt: "x", Validate: "true"},
	}
	rep := r.RunSuite(context.Background(), cases)
	if rep.Total != 3 || rep.Passed != 1 {
		t.Fatalf("passed=%d", rep.Passed)
	}
	ok := rep.Results[0]
	if !ok.Passed || ok.Turns != 3 || ok.ToolCalls != 2 || ok.ToolErrors != 1 || ok.RecoveryHints != 1 || ok.FirstTry || ok.LLMCalls != 3 {
		t.Fatalf("轨迹指标错误: %+v", ok)
	}
	if ok.PromptTokens != 300 || ok.CostCNY <= 0 || ok.WorkDir != "" {
		t.Fatalf("成本或清理错误: %+v", ok)
	}
	if bad := rep.Results[1]; bad.Passed || !strings.Contains(bad.Error, "验证脚本未通过") || bad.WorkDir == "" || bad.TracePath == "" {
		t.Fatalf("失败用例应保留工作区与 trace: %+v", bad)
	}
	if !strings.Contains(rep.Results[2].Error, "Setup 失败") {
		t.Fatal(rep.Results[2].Error)
	}
	baseline := rep
	baseline.Results = append([]Result(nil), rep.Results...)
	baseline.Results[1].Passed = true
	md := rep.Markdown(&baseline)
	for _, want := range []string{"1/3", "⚠️ 回归", "| ok | ✅ | 95.3 | 3 | 2(1) | 1 | 0 | 0 |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("报告缺少 %q:\n%s", want, md)
		}
	}
	if reg := rep.Regressions(baseline); len(reg) != 1 || reg[0] != "bad" {
		t.Fatal(reg)
	}
}

func TestLoadCases(t *testing.T) {
	cs, err := LoadCases("../../bench/cases.json", "^t0[12]")
	if err != nil || len(cs) != 2 || cs[0].Validate == "" {
		t.Fatal(cs, err)
	}
}

// 回归：空判卷脚本不能"自动满分"。
func TestEmptyValidateRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cases.json")
	for _, bad := range []string{
		`[{"id":"a","prompt":"x","validate":"  "}]`,
		`[{"id":"a","prompt":"x"}]`,
		`[{"id":"a","prompt":"","validate":"true"}]`,
		`[{"id":"a","prompt":"x","validate":"true"},{"id":"a","prompt":"y","validate":"true"}]`,
	} {
		os.WriteFile(path, []byte(bad), 0o644)
		if _, err := LoadCases(path, ""); err == nil {
			t.Errorf("应拒绝: %s", bad)
		}
	}
	r := &Runner{LLM: scripted{}, WorkRoot: t.TempDir()}
	rep := r.RunSuite(context.Background(), []TestCase{{ID: "empty", Prompt: "写 42", Validate: ""}})
	if rep.Passed != 0 || !strings.Contains(rep.Results[0].Error, "用例定义无效") {
		t.Fatalf("空判卷被判通过: %+v", rep.Results[0])
	}
}

// rmScript：请求 rm -rf build（ask 规则）后收工。
type rmScript struct{}

func (rmScript) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	if msgs[len(msgs)-1].IsToolResult() {
		return &schema.Message{Role: schema.RoleAssistant, Content: "done"}, nil
	}
	return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "1", Name: "bash", Arguments: []byte(`{"command":"rm -rf build"}`)}}}, nil
}

func TestApprovalPolicy(t *testing.T) {
	r := &Runner{LLM: rmScript{}, WorkRoot: t.TempDir()}
	cases := []TestCase{
		{ID: "approve", Setup: "mkdir build", Prompt: "清理", Validate: "! test -d build"},
		{ID: "reject", Setup: "mkdir build", Prompt: "清理", Validate: "test -d build", Approval: "reject", NoPrecheck: true},
		{ID: "bad", Prompt: "x", Validate: "true", Approval: "maybe"},
	}
	rep := r.RunSuite(context.Background(), cases)
	if !rep.Results[0].Passed || !rep.Results[1].Passed || rep.Results[0].Approvals != 1 || rep.Results[2].Passed {
		t.Fatalf("%+v", rep.Results)
	}
}

// 第 20 讲 Fail-to-Pass：判卷脚本在 Agent 动手前就通过的用例是无效用例，不能计为通过。
func TestPrecheckRejectsAlreadyPassingCase(t *testing.T) {
	r := &Runner{LLM: scripted{}, WorkRoot: t.TempDir(), Parallelism: 1}
	rep := r.RunSuite(context.Background(), []TestCase{{ID: "trivial", Setup: "echo 42 > answer.txt", Prompt: "写 42", Validate: `[ "$(cat answer.txt)" = 42 ]`}})
	if rep.Passed != 0 || !strings.Contains(rep.Results[0].Error, "Fail-to-Pass") {
		t.Fatalf("动手前就通过的用例应判为无效: %+v", rep.Results[0])
	}
}

// 第 20 讲第 4 步：综合得分结合成本、耗时、轮数；失败 0 分，越省分越高。
func TestScore(t *testing.T) {
	if Score(Result{Passed: false, Turns: 1}) != 0 {
		t.Fatal("失败用例应为 0 分")
	}
	fast := Score(Result{Passed: true, Turns: 1, CostCNY: 0.001, DurationMs: 1000})
	slow := Score(Result{Passed: true, Turns: 30, CostCNY: 0.5, DurationMs: 600000})
	if !(fast > 90 && fast <= 100 && slow > 60 && slow < fast) {
		t.Fatalf("得分不合理: fast=%.1f slow=%.1f", fast, slow)
	}
	if got := Score(Result{Passed: true, Turns: 10, CostCNY: 0.05, DurationMs: 120000}); got != 80 {
		t.Fatalf("三项都等于参考值时应为 60+10+5+5=80, got %.1f", got)
	}
}
