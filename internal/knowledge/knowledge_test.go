package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dec(subject, content string, rule bool) Entry {
	return Entry{Kind: Decision, Subject: subject, Content: content, Reason: "用户明确裁决", Scope: "本项目所有 Go 代码", Source: "user", Rule: rule, SessionID: "s1"}
}

func TestValidate(t *testing.T) {
	bad := []Entry{
		{Kind: "x", Subject: "a", Content: "b"},
		{Kind: UserTriage, Subject: "a", Content: "b"},                         // 缺 scope
		{Kind: UserTriage, Subject: "a", Content: "b", Scope: "c", Source: "model"}, // 统筹只能来自用户
		{Kind: Discovery, Subject: "a", Content: "b"},                          // 缺来源
		{Kind: Decision, Subject: "a", Content: "b", Scope: "c", Source: "user"}, // 缺理由
		{Kind: Decision, Subject: "a", Content: "b", Reason: "r", Scope: "c", Source: "someone"},
		{Kind: Decision, Content: "b", Reason: "r", Scope: "c", Source: "model"}, // 缺主题
	}
	for i, e := range bad {
		if err := e.Validate(); err == nil {
			t.Errorf("case %d 应校验失败: %+v", i, e)
		}
	}
	ok := []Entry{{Kind: None}, {Kind: UserTriage, Subject: "a", Content: "b", Scope: "c"}, {Kind: Discovery, Subject: "a", Content: "b", Source: "go test ./..."}, dec("a", "b", false)}
	for i, e := range ok {
		if err := e.Validate(); err != nil {
			t.Errorf("case %d 应通过: %v", i, err)
		}
	}
}

func TestInboxTakeRetainsOnFailure(t *testing.T) {
	in := NewInbox(t.TempDir())
	since := time.Now()
	if in.CheckedSince("s1", since) {
		t.Fatal("还没记录")
	}
	in.Record(Entry{Kind: None, SessionID: "s1"})
	if !in.CheckedSince("s1", since) {
		t.Fatal("none 也算清点过")
	}
	in.Record(dec("日志库", "统一用 slog", true))
	if err := in.Take("s1", func([]Entry) error { return os.ErrPermission }); err == nil {
		t.Fatal("commit 失败应返回错误")
	}
	if es, _ := in.Pending("s1"); len(es) != 2 {
		t.Fatal("归位失败时条目必须保留")
	}
	var got int
	in.Take("s1", func(es []Entry) error { got = len(es); return nil })
	if es, _ := in.Pending("s1"); got != 2 || len(es) != 0 {
		t.Fatal("成功后应清空")
	}
	if _, err := in.Record(Entry{Kind: None}); err == nil {
		t.Fatal("没有会话身份应拒绝")
	}
}

func TestPlacerLayoutDedupSupersede(t *testing.T) {
	wd := t.TempDir()
	p := &Placer{}
	res, err := p.Place(wd, []Entry{
		dec("日志库选型", "不要引入 logrus，统一使用标准库 log/slog", true),
		{Kind: UserTriage, Subject: "回复风格", Content: "回答尽量简短，先给结论", Scope: "所有对话", Source: "user", SessionID: "s1"},
		{Kind: Discovery, Subject: "测试命令", Content: "本项目用 make test 跑离线测试", Source: "Makefile", SessionID: "s1"},
		{Kind: None, SessionID: "s1"},
	})
	if err != nil || res.Added != 3 || res.Checkpoints != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	adr := filepath.Join(wd, ".claw/knowledge/decisions/0001-日志库选型.md")
	b, err := os.ReadFile(adr)
	if err != nil || !strings.Contains(string(b), "不要引入 logrus") || !strings.Contains(string(b), "用户裁决") || !strings.Contains(string(b), "适用范围：本项目所有 Go 代码") || !strings.Contains(string(b), "## 理由") {
		t.Fatalf("ADR 内容不完整: %v\n%s", err, b)
	}
	rules, _ := os.ReadFile(filepath.Join(wd, ".claw/knowledge/rules.md"))
	prefs, _ := os.ReadFile(filepath.Join(wd, ".claw/knowledge/preferences.md"))
	discs, _ := os.ReadFile(filepath.Join(wd, ".claw/knowledge/discoveries.md"))
	if !strings.Contains(string(rules), "不要引入 logrus") || !strings.Contains(string(prefs), "先给结论") || !strings.Contains(string(discs), "来源：Makefile") {
		t.Fatalf("归位错误:\n%s\n%s\n%s", rules, prefs, discs)
	}
	// 重复条目只计重复
	res, _ = p.Place(wd, []Entry{dec("日志库选型", "不要引入  logrus，统一使用标准库 log/slog", true)})
	if res.Duplicates != 1 || res.Added != 0 {
		t.Fatalf("应去重: %+v", res)
	}
	// 同主题新决策取代旧决策：旧 ADR 标注被取代，规则文件只保留新的
	res, _ = p.Place(wd, []Entry{dec("日志库选型", "改用 zerolog（性能要求）", true)})
	if res.Added != 1 || res.Superseded != 1 {
		t.Fatalf("应取代: %+v", res)
	}
	b, _ = os.ReadFile(adr)
	rules, _ = os.ReadFile(filepath.Join(wd, ".claw/knowledge/rules.md"))
	if !strings.Contains(string(b), "已被 ADR-0002 取代") || strings.Contains(string(rules), "logrus") || !strings.Contains(string(rules), "zerolog") {
		t.Fatalf("取代未生效:\n%s\n%s", b, rules)
	}
	prompt := RenderPrompt(wd)
	ri, pi := strings.Index(prompt, "必须遵守的规则"), strings.Index(prompt, "用户偏好")
	if ri < 0 || pi < 0 || ri > pi || !strings.Contains(prompt, "zerolog") || strings.Contains(prompt, "logrus") {
		t.Fatalf("注入内容错误:\n%s", prompt)
	}
}

func TestLayoutOverridesAndBoundary(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, "docs", "adr"), 0o755)
	if l := LoadLayout(wd); l.ADRDir != "docs/adr" {
		t.Fatalf("已有 docs/adr 时应沿用: %s", l.ADRDir)
	}
	os.MkdirAll(filepath.Join(wd, ".claw"), 0o755)
	os.WriteFile(filepath.Join(wd, ".claw/knowledge.json"), []byte(`{"preferences_file":"docs/user-prefs.md"}`), 0o644)
	p := &Placer{}
	if _, err := p.Place(wd, []Entry{{Kind: UserTriage, Subject: "a", Content: "b", Scope: "c", Source: "user", SessionID: "s"}, dec("x", "y", false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "docs/user-prefs.md")); err != nil {
		t.Fatal("自定义偏好文件路径未生效")
	}
	if files, _ := filepath.Glob(filepath.Join(wd, "docs/adr/0001-*.md")); len(files) != 1 {
		t.Fatal("ADR 应写到 docs/adr")
	}
	os.WriteFile(filepath.Join(wd, ".claw/knowledge.json"), []byte(`{"rules_file":"../escape.md"}`), 0o644)
	if _, err := p.Place(wd, []Entry{dec("z", "w", true)}); err == nil {
		t.Fatal("布局指向工作区外应失败")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(wd), "escape.md")); err == nil {
		t.Fatal("写出了工作区")
	}
}

func TestCorruptIndexRefused(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, ".claw/knowledge"), 0o755)
	os.WriteFile(filepath.Join(wd, ".claw/knowledge/index.json"), []byte("{broken"), 0o644)
	m := NewManager(t.TempDir())
	m.Inbox.Record(dec("a", "b", false))
	if _, err := m.Settle("s1", wd); err == nil {
		t.Fatal("索引损坏时应拒绝覆盖")
	}
	if es, _ := m.Inbox.Pending("s1"); len(es) != 1 {
		t.Fatal("失败时条目应保留")
	}
}
