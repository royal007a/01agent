package envlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func writeSkill(t *testing.T, wd, rel, body string) {
	t.Helper()
	p := filepath.Join(wd, ".claw", "skills", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func capture(t *testing.T, wd string, m Model) *Lock {
	t.Helper()
	l, err := Capture(CaptureOptions{WorkDir: wd, Model: m, Tools: []string{"bash", "definitely-not-a-tool-xyz"}})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func find(ds []Drift, cat, key string) *Drift {
	for i := range ds {
		if ds[i].Category == cat && ds[i].Key == key {
			return &ds[i]
		}
	}
	return nil
}

func TestCaptureSaveLoadRoundTrip(t *testing.T) {
	wd := t.TempDir()
	writeSkill(t, wd, "third-party/nginx/SKILL.md", "---\nname: nginx\n---\nbody")
	l := capture(t, wd, Model{Provider: "openai", Requested: "m"})
	if l.Runtime.Tools["bash"].Path == "" || l.Runtime.Tools["definitely-not-a-tool-xyz"].Path != "" {
		t.Fatalf("工具探测不对: %+v", l.Runtime.Tools)
	}
	if len(l.Skills) != 1 {
		t.Fatalf("Skill 哈希缺失: %+v", l.Skills)
	}
	if err := Save(wd, l); err != nil {
		t.Fatal(err)
	}
	got, err := Load(wd)
	if err != nil {
		t.Fatal(err)
	}
	if ds := Diff(got, capture(t, wd, Model{Provider: "openai", Requested: "m"})); len(ds) != 0 {
		t.Fatalf("同一环境不应有漂移: %v", ds)
	}
}

func TestLoadRefusesCorruptLock(t *testing.T) {
	wd := t.TempDir()
	if _, err := Load(wd); !errors.Is(err, ErrNoLock) {
		t.Fatalf("无锁应返回 ErrNoLock，得到 %v", err)
	}
	p := filepath.Join(wd, LockFile)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	for _, body := range []string{"{bad", `{"version":1,"mode":"loose"}`, `{"version":99,"mode":"warn"}`} {
		_ = os.WriteFile(p, []byte(body), 0o644)
		if _, err := Load(wd); err == nil || errors.Is(err, ErrNoLock) {
			t.Fatalf("损坏锁 %q 必须报错且不能当作未锁定放行，得到 %v", body, err)
		}
	}
}

func TestDiffCategories(t *testing.T) {
	base := &Lock{
		Runtime: Runtime{OS: "darwin", Arch: "arm64", Go: "go1.26.4", Tools: map[string]Tool{
			"bash": {Path: "/bin/bash", Version: "3.2"},
			"rg":   {Path: "/usr/bin/rg", Version: "14"},
			"jq":   {},
			"git":  {Path: "/usr/bin/git", Version: "2.54"},
		}},
		Deps:   map[string]string{"a": "v1 h1:x", "b": "v1 h1:y"},
		Skills: map[string]string{".claw/skills/x/SKILL.md": "h1"},
		Model:  Model{Provider: "openai", Requested: "deepseek-v4-flash", Observed: "deepseek-v4-flash-0801"},
	}
	cur := &Lock{
		Runtime: Runtime{OS: "linux", Arch: "arm64", Go: "go1.26.5", Tools: map[string]Tool{
			"bash": {Path: "/usr/bin/bash", Version: "5.2"},
			"jq":   {Path: "/usr/bin/jq", Version: "1.7"},
			"git":  {Path: "/opt/git", Version: "2.54"},
		}},
		Deps:   map[string]string{"a": "v2 h1:z", "c": "v1 h1:w"},
		Skills: map[string]string{".claw/skills/x/SKILL.md": "h2"},
		Model:  Model{Provider: "openai", Requested: "deepseek-v4-flash", Observed: "deepseek-v4.1-flash"},
	}
	ds := Diff(base, cur)
	want := map[[2]string]Severity{
		{"runtime", "os"}:                    SevError,
		{"runtime", "go"}:                    SevWarn,
		{"tool", "bash"}:                     SevError, // 版本变了
		{"tool", "rg"}:                       SevError, // 消失
		{"tool", "jq"}:                       SevWarn,  // 新出现
		{"tool", "git.path"}:                 SevWarn,  // 同版本换位置
		{"dep", "a"}:                         SevError,
		{"dep", "b"}:                         SevError,
		{"dep", "c"}:                         SevError,
		{"skill", ".claw/skills/x/SKILL.md"}: SevError,
		{"model", "observed"}:                SevError, // 名字没变，行为基础已变
	}
	for k, sev := range want {
		d := find(ds, k[0], k[1])
		if d == nil || d.Severity != sev {
			t.Errorf("缺少漂移 %v(%s)，得到 %+v", k, sev, d)
		}
	}
	if len(ds) != len(want) {
		t.Errorf("漂移条数 %d != %d: %v", len(ds), len(want), ds)
	}
	// Observed 任一方未知时不比较（未探测不等于漂移）
	cur.Model.Observed = ""
	if find(Diff(base, cur), "model", "observed") != nil {
		t.Error("未探测的实际版本不应报漂移")
	}
}

func TestSymlinkedSkillIsViolation(t *testing.T) {
	wd, global := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(global, "SKILL.md"), []byte("global"), 0o644)
	_ = os.MkdirAll(filepath.Join(wd, ".claw", "skills"), 0o755)
	if err := os.Symlink(global, filepath.Join(wd, ".claw", "skills", "global")); err != nil {
		t.Fatal(err)
	}
	l := capture(t, wd, Model{})
	if v := l.Skills[".claw/skills/global"]; !strings.HasPrefix(v, "symlink:") {
		t.Fatalf("符号链接应记为 symlink:，得到 %q", v)
	}
	// 即使锁里已经记录了这个链接，也判为错误：全局 Skill 不能靠锁"合法化"
	d := find(Diff(l, capture(t, wd, Model{})), "skill", ".claw/skills/global")
	if d == nil || d.Severity != SevError {
		t.Fatalf("链接到外部的 Skill 必须报错: %+v", d)
	}
}

func TestEnforce(t *testing.T) {
	warn := []Drift{{Category: "runtime", Key: "go", Severity: SevWarn}}
	errs := append(warn, Drift{Category: "dep", Key: "a", Severity: SevError})
	var logged int
	logf := func(string, ...any) { logged++ }
	if err := Enforce(ModeWarn, errs, logf); err != nil || logged != 2 {
		t.Fatalf("warn 模式只报告: err=%v logged=%d", err, logged)
	}
	if err := Enforce(ModeStrict, warn, logf); err != nil {
		t.Fatalf("strict 模式下仅警告级漂移不应拒绝: %v", err)
	}
	if err := Enforce(ModeStrict, errs, logf); err == nil {
		t.Fatal("strict 模式下错误级漂移必须拒绝")
	}
}

type fakeLLM struct{ models []string }

func (f *fakeLLM) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (*schema.Message, error) {
	m := f.models[0]
	if len(f.models) > 1 {
		f.models = f.models[1:]
	}
	return &schema.Message{Role: schema.RoleAssistant, Content: "ok", Model: m}, nil
}

func TestModelGuard(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	ctx := context.Background()

	// 锁定了实际版本：同名换代 → 报告一次、计数每次
	g := &ModelGuard{Inner: &fakeLLM{models: []string{"v4-0801", "v4.1", "v4.1"}}, Expected: "v4-0801", Logf: logf}
	for i := 0; i < 3; i++ {
		if _, err := g.Generate(ctx, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if g.Drifts() != 2 || len(logs) != 1 {
		t.Fatalf("drifts=%d logs=%v", g.Drifts(), logs)
	}

	// strict：直接拒绝
	gs := &ModelGuard{Inner: &fakeLLM{models: []string{"v4.1"}}, Expected: "v4-0801", Strict: true, Logf: logf}
	if _, err := gs.Generate(ctx, nil, nil); err == nil {
		t.Fatal("strict 下模型漂移必须报错")
	}

	// 未锁定实际版本：以首次观测为准，发现运行途中的滚动；未回报模型的响应不参与
	logs = nil
	gl := &ModelGuard{Inner: &fakeLLM{models: []string{"a", "", "a", "b"}}, Logf: logf}
	for i := 0; i < 4; i++ {
		_, _ = gl.Generate(ctx, nil, nil)
	}
	if gl.Observed() != "a" || gl.Drifts() != 2 || len(logs) != 2 {
		t.Fatalf("已观测到版本后，空模型与换代都应计为漂移: observed=%q drifts=%d logs=%v", gl.Observed(), gl.Drifts(), logs)
	}

	// 有期望版本时响应不回报模型：strict 视为无法核验并拒绝（不能靠空字段绕过）
	ge := &ModelGuard{Inner: &fakeLLM{models: []string{""}}, Expected: "v4-0801", Strict: true, Logf: logf}
	if _, err := ge.Generate(ctx, nil, nil); err == nil || !strings.Contains(err.Error(), "无法核验") {
		t.Fatalf("strict 下空模型必须拒绝，得到 %v", err)
	}
	// 厂商从不回报模型且没有锁定版本：无从比对，不误报
	gn := &ModelGuard{Inner: &fakeLLM{models: []string{""}}, Strict: true, Logf: logf}
	for i := 0; i < 2; i++ {
		if _, err := gn.Generate(ctx, nil, nil); err != nil || gn.Drifts() != 0 {
			t.Fatalf("无任何版本信息时不应报漂移: err=%v drifts=%d", err, gn.Drifts())
		}
	}
}
