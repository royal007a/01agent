package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/envlock"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

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
