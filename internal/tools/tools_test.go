package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

func args(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestFuzzyReplaceLevels(t *testing.T) {
	src := "func main() {\n    // TODO: 增加鉴权逻辑\n    if true {\n        fmt.Println(\"No auth\")\n    }\n}\n"
	// L1 精确
	out, err := fuzzyReplace(src, "if true {", "if ok {")
	if err != nil || !strings.Contains(out, "    if ok {") {
		t.Fatalf("L1: %v %q", err, out)
	}
	// L2 CRLF
	crlf := strings.ReplaceAll(src, "\n", "\r\n")
	out, err = fuzzyReplace(crlf, "if true {\n        fmt", "if ok {\n        fmt")
	if err != nil || !strings.Contains(out, "if ok {") {
		t.Fatalf("L2: %v", err)
	}
	// L3 首尾空白
	out, err = fuzzyReplace(src, "\n\n  fmt.Println(\"No auth\")  \n\n", "fmt.Println(\"Forbidden\")")
	if err != nil || !strings.Contains(out, "        fmt.Println(\"Forbidden\")") {
		t.Fatalf("L3: %v %q", err, out)
	}
	// L4 丢失缩进 + 自动补齐基础缩进（第 07 讲思考题）
	old := "// TODO: 增加鉴权逻辑\nif true {\nfmt.Println(\"No auth\")\n}"
	newT := "if user == nil {\n    fmt.Println(\"Forbidden!\")\n    return\n}"
	out, err = fuzzyReplace(src, old, newT)
	if err != nil {
		t.Fatalf("L4: %v", err)
	}
	want := "func main() {\n    if user == nil {\n        fmt.Println(\"Forbidden!\")\n        return\n    }\n}\n"
	if out != want {
		t.Fatalf("L4 缩进对齐失败:\n%s\nwant:\n%s", out, want)
	}
	// 唯一性底线
	dup := "a\n  x := 1\nb\n  x := 1\n"
	if _, err := fuzzyReplace(dup, "x := 1", "y"); err == nil || !strings.Contains(err.Error(), errEditAmbiguous) {
		t.Fatalf("多处匹配必须报错: %v", err)
	}
	if _, err := fuzzyReplace(dup, "x:=1\nzzz", "y"); err == nil || !strings.Contains(err.Error(), errEditNotFound) {
		t.Fatalf("未找到必须报错: %v", err)
	}
}

func TestPathBoundary(t *testing.T) {
	wd := t.TempDir()
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../x"} {
		if _, _, err := workspace(wd, bad); err == nil {
			t.Fatalf("%s 应被拒绝", bad)
		}
	}
	if r, p, err := workspace(wd, filepath.Join(wd, "a/b.txt")); err != nil || p != "a/b.txt" {
		t.Fatalf("工作区内的绝对路径应允许: %v", err)
	} else {
		r.Close()
	}
}

func TestFileTools(t *testing.T) {
	wd := t.TempDir()
	ctx := context.Background()
	w, r, e := NewWriteFileTool(wd), NewReadFileTool(wd), NewEditFileTool(wd)
	if _, err := w.Execute(ctx, args(map[string]string{"path": "d/a.go", "content": "x := 1\n"})); err != nil {
		t.Fatal(err)
	}
	if out, _ := r.Execute(ctx, args(map[string]string{"path": "d/a.go"})); out != "x := 1\n" {
		t.Fatal(out)
	}
	if _, err := e.Execute(ctx, args(map[string]string{"path": "d/a.go", "old_text": "x := 1", "new_text": "x := 2"})); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wd, "d/a.go")); string(b) != "x := 2\n" {
		t.Fatal(string(b))
	}
	if _, err := r.Execute(ctx, args(map[string]string{"path": "d"})); err == nil {
		t.Fatal("读目录应报错")
	}
	big := strings.Repeat("中", 5000) // 15000 字节
	os.WriteFile(filepath.Join(wd, "big.txt"), []byte(big), 0o644)
	out, _ := r.Execute(ctx, args(map[string]string{"path": "big.txt"}))
	if !utf8.ValidString(out) || !strings.Contains(out, "截断") || len(out) > ReadFileMaxBytes+300 {
		t.Fatalf("大文件截断异常: len=%d", len(out))
	}
}

func TestBashTool(t *testing.T) {
	wd := t.TempDir()
	b := NewBashTool(wd)
	ctx := context.Background()
	out, err := b.Execute(ctx, args(map[string]string{"command": "pwd"}))
	if err != nil || !strings.Contains(out, filepath.Base(wd)) {
		t.Fatalf("工作区绑定失败: %v %q", err, out)
	}
	if _, err := b.Execute(ctx, args(map[string]string{"command": "nosuchcmd_xyz"})); err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("非零退出应返回 error（IsError=true）: %v", err)
	}
	b.Timeout = 500 * time.Millisecond
	if _, err := b.Execute(ctx, args(map[string]string{"command": "sleep 5"})); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时应被强杀: %v", err)
	}
	// 第 13 讲的坑：后台进程持有管道不能卡死 Main Loop
	b.Timeout = 10 * time.Second
	start := time.Now()
	out, err = b.Execute(ctx, args(map[string]string{"command": "sleep 30 & echo started"}))
	if err != nil || !strings.Contains(out, "started") || time.Since(start) > 5*time.Second {
		t.Fatalf("后台进程导致阻塞: %v %q %v", err, out, time.Since(start))
	}
	out, _ = b.Execute(ctx, args(map[string]string{"command": "yes 错误日志 | head -c 30000"}))
	if len(out) > BashMaxBytes+200 || !utf8.ValidString(out) {
		t.Fatalf("输出截断异常 len=%d", len(out))
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	wd := t.TempDir()
	r.Register(NewReadFileTool(wd))
	r.Register(NewWriteFileTool(wd))
	r.Register(NewBashTool(wd))
	for i := 0; i < 20; i++ {
		defs := r.GetAvailableTools()
		if defs[0].Name != "read_file" || defs[2].Name != "bash" {
			t.Fatal("工具顺序必须稳定")
		}
	}
	res := r.Execute(context.Background(), schema.ToolCall{ID: "1", Name: "nope"})
	if !res.IsError || !strings.Contains(res.Output, "不存在") {
		t.Fatal(res)
	}
	if !r.IsReadOnly("read_file") || r.IsReadOnly("bash") || r.IsReadOnly("nope") {
		t.Fatal("只读判定错误")
	}
}

func TestRegistryPreservesBashTailAfterToolTruncation(t *testing.T) {
	r := NewRegistry()
	r.Register(NewBashTool(t.TempDir()))
	res := r.Execute(context.Background(), schema.ToolCall{Name: "bash", Arguments: args(map[string]string{
		"command": "head -c 20000 /dev/zero | tr '\\0' a; echo; echo FINAL_SUMMARY_LINE",
	})})
	if res.IsError || !strings.Contains(res.Output, "FINAL_SUMMARY_LINE") {
		t.Fatalf("registry must preserve command tail: error=%v output tail=%q", res.IsError, textutil.Tail(res.Output, 100))
	}
	if len(res.Output) > 8000 {
		t.Fatalf("registry output exceeds cap: %d", len(res.Output))
	}
}

func TestReadOnlyGuardRegressions(t *testing.T) {
	g := ReadOnlyGuard()
	for _, bad := range []string{"kubectl delete pod x", "echo cm0g | base64 -d | bash", "sed -Ei 's/a/b/' f", "perl -pi -e 's/a/b/' f",
		`python3 -c "open('f','w')"`, "find . -delete", "ls &>f", "ls 2>f", "curl -o f http://x", "git apply p.diff", "echo x > f"} {
		b, _ := json.Marshal(map[string]string{"command": bad})
		if ok, _ := g(context.Background(), schema.ToolCall{Name: "bash", Arguments: b}); ok {
			t.Errorf("只读守卫漏放: %s", bad)
		}
	}
	for _, good := range []string{"find . -name '*.go' 2>/dev/null", "grep -rn foo . | head", "ls -la 2>&1", "cat a.txt", "wc -l *.go"} {
		b, _ := json.Marshal(map[string]string{"command": good})
		if ok, r := g(context.Background(), schema.ToolCall{Name: "bash", Arguments: b}); !ok {
			t.Errorf("只读命令被误拦: %s (%s)", good, r)
		}
	}
}

func TestSymlinkEscape(t *testing.T) {
	wd, outside := t.TempDir(), t.TempDir()
	os.Symlink(outside, filepath.Join(wd, "link"))
	w := NewWriteFileTool(wd)
	if _, err := w.Execute(context.Background(), args(map[string]string{"path": "link/pwned.txt", "content": "x"})); err == nil {
		t.Fatal("经符号链接写到工作区外应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatal("文件被写到了工作区外")
	}
	if _, err := w.Execute(context.Background(), args(map[string]string{"path": "new/dir/ok.txt", "content": "x"})); err != nil {
		t.Fatalf("工作区内新建多级目录应允许: %v", err)
	}
}

func TestBashScrubsSecrets(t *testing.T) {
	t.Setenv("CLAW_API_KEY", "sk-secret")
	t.Setenv("CLAW_HTTP_TOKEN", "tk")
	t.Setenv("MY_PASSWORD", "p")
	out, err := NewBashTool(t.TempDir()).Execute(context.Background(), args(map[string]string{"command": "env"}))
	if err != nil || strings.Contains(out, "sk-secret") || strings.Contains(out, "CLAW_HTTP_TOKEN") || strings.Contains(out, "MY_PASSWORD") || !strings.Contains(out, "PATH=") {
		t.Fatalf("环境变量未脱敏: %v", err)
	}
}

// 回归：codex review —— source / . / 直接执行脚本 绕过只读守卫。
func TestReadOnlyGuardIndirectExecution(t *testing.T) {
	wd := t.TempDir()
	os.WriteFile(filepath.Join(wd, "mutate.sh"), []byte("touch pwned\n"), 0o755)
	r := NewRegistry()
	r.Register(NewBashTool(wd))
	r.Use(ReadOnlyGuard())
	for _, cmd := range []string{"source ./mutate.sh", ". ./mutate.sh", "ls; . mutate.sh", "./mutate.sh", "bash mutate.sh", "sh mutate.sh",
		"exec touch pwned", "command touch pwned", "env touch pwned", "awk 'BEGIN{system(\"touch pwned\")}'", "$(./mutate.sh)", "/bin/touch pwned"} {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		if res := r.Execute(context.Background(), schema.ToolCall{Name: "bash", Arguments: b}); !res.IsError {
			t.Errorf("应拦截: %s", cmd)
		}
	}
	if _, err := os.Stat(filepath.Join(wd, "pwned")); err == nil {
		t.Fatal("只读子智能体产生了写操作")
	}
}

func TestFileToolsRejectSymlinkEscapeAllOps(t *testing.T) {
	wd, outside := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	os.Symlink(outside, filepath.Join(wd, "link"))
	os.Mkdir(filepath.Join(wd, "in"), 0o755)
	os.Symlink("in", filepath.Join(wd, "inlink")) // 工作区内的相对链接
	ctx := context.Background()
	if _, err := NewReadFileTool(wd).Execute(ctx, args(map[string]string{"path": "link/secret.txt"})); err == nil || !strings.Contains(err.Error(), "越出了工作区边界") {
		t.Fatalf("read 经链接逃逸应拒绝: %v", err)
	}
	if _, err := NewEditFileTool(wd).Execute(ctx, args(map[string]string{"path": "link/secret.txt", "old_text": "s", "new_text": "x"})); err == nil {
		t.Fatal("edit 经链接逃逸应拒绝")
	}
	if _, err := NewWriteFileTool(wd).Execute(ctx, args(map[string]string{"path": "inlink/ok.txt", "content": "ok"})); err != nil {
		t.Fatalf("指向工作区内部的链接应允许: %v", err)
	}
	if _, err := NewEditFileTool(wd).Execute(ctx, args(map[string]string{"path": "gone.txt", "old_text": "a", "new_text": "b"})); err == nil {
		t.Fatal("编辑不存在的文件应返回错误而不是 panic")
	}
	ents, _ := os.ReadDir(filepath.Join(wd, "in"))
	for _, e := range ents {
		if strings.Contains(e.Name(), "claw-tmp") {
			t.Fatal("原子写残留临时文件")
		}
	}
}

// 回归：codex review 第二轮 —— 解释器 stdin/heredoc 等绕过，改为白名单后一律拒绝。
func TestReadOnlyGuardWhitelist(t *testing.T) {
	wd := t.TempDir()
	r := NewRegistry()
	r.Register(NewBashTool(wd))
	r.Use(ReadOnlyGuard())
	run := func(cmd string) schema.ToolResult {
		b, _ := json.Marshal(map[string]string{"command": cmd})
		return r.Execute(context.Background(), schema.ToolCall{Name: "bash", Arguments: b})
	}
	for _, bad := range []string{
		"python <<'PY'\nopen('pwned','w').write('x')\nPY",
		"python3 - <<< \"open('pwned','w')\"", "echo \"open('pwned','w').write('x')\" | python3", "echo x | python3 -",
		"lua -e 'io.open(\"pwned\",\"w\")'", "echo 'print 1' | perl -", "ruby pwn.rb", "node pwn.js", "osascript x",
		"cat <(touch pwned)", "ls $(touch pwned)", "FOO=1 touch pwned", "git -c core.pager=touch log", "go run x.go",
		"find . -fprint pwned", "vim -es pwned", "tar cf pwned .",
	} {
		if res := run(bad); !res.IsError {
			t.Errorf("应拦截: %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(wd, "pwned")); err == nil {
		t.Fatal("产生了写操作")
	}
	os.WriteFile(filepath.Join(wd, "a.go"), []byte("package a\n"), 0o644)
	for _, good := range []string{"ls -la", "cat a.go | head -5", "grep -rn package . 2>/dev/null | wc -l", "find . -name '*.go' -type f",
		"git status 2>&1 || true", "go version", "wc -l a.go && echo done"} {
		if res := run(good); res.IsError && strings.Contains(res.Output, "只读") {
			t.Errorf("只读命令被误拦: %q => %s", good, res.Output)
		}
	}
}

// 回归：codex review 第三轮 —— 白名单命令的"写文件/改配置"参数。
func TestReadOnlyGuardArgSideEffects(t *testing.T) {
	for _, bad := range []string{"sort -o pwned /etc/hosts", "sort --output=pwned a", "sort -uo pwned a", "git diff --output=pwned",
		"git log --output pwned", "git grep -Otouch x", "git -c core.pager=x log", "git branch newbranch", "git branch -D main",
		"git branch --edit-description", "git branch --set-upstream-to=origin/main", "git branch --unset-upstream",
		"git branch -m old new", "git branch -u origin/main", "git branch --track x",
		"go env -w GOTOOLCHAIN=local", "go env -u GOFLAGS", "go list ./...", "tree -o pwned", "file -C -m magic",
		"date -s 2020", "rg --pre ./x foo", "uniq a pwned"} {
		if why := checkReadOnly(bad); why == "" {
			t.Errorf("应拦截: %q", bad)
		}
	}
	for _, good := range []string{"sort -u a.txt", "sort -n -r a | head", "git diff HEAD~1", "git log --oneline -5", "git branch -a", "git branch", "git branch -vv --list",
		"git branch --show-current", "go env GOPATH", "go version", "tree -L 2", "file a.go", "date +%F", "rg -n foo", "uniq -c a.txt"} {
		if why := checkReadOnly(good); why != "" {
			t.Errorf("误拦 %q: %s", good, why)
		}
	}
}

type contractTool struct{}

func (contractTool) Name() string { return "contract" }
func (contractTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "contract", InputSchema: map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{"mode": map[string]interface{}{"type": "string"}}, "required": []string{"mode"},
	}}
}
func (contractTool) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func TestRegistrySchemaContractAndWriteLock(t *testing.T) {
	r := NewRegistry()
	r.Register(contractTool{})
	if got := r.Execute(context.Background(), schema.ToolCall{ID: "1", Name: "contract", Arguments: []byte(`{}`)}); !got.IsError || !strings.Contains(got.Output, "mode") {
		t.Fatalf("missing required parameter should be rejected: %#v", got)
	}
	if got := r.Execute(context.Background(), schema.ToolCall{ID: "2", Name: "contract", Arguments: []byte(`{"mode":"safe"}`)}); got.IsError {
		t.Fatalf("valid schema call rejected: %#v", got)
	}
	configurable := r.(interface{ ConfigureWriteLock(*WriteLock) })
	configurable.ConfigureWriteLock(&WriteLock{})
	if got := r.Execute(context.Background(), schema.ToolCall{ID: "3", Name: "contract", Arguments: []byte(`{"mode":"safe"}`)}); got.IsError {
		t.Fatalf("locked valid call rejected: %#v", got)
	}
}

func TestRegistryToolDefinitionsStable(t *testing.T) {
	r := NewRegistry()
	r.Register(contractTool{})
	r.Register(contractTool2{})
	a, _ := json.Marshal(r.GetAvailableTools())
	b, _ := json.Marshal(r.GetAvailableTools())
	if string(a) != string(b) {
		t.Fatalf("tool definitions must be stable: %s != %s", a, b)
	}
}

type contractTool2 struct{}

func (contractTool2) Name() string { return "contract_2" }
func (contractTool2) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "contract_2", Description: "stable"}
}
func (contractTool2) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

type serializedTool struct {
	mu                sync.Mutex
	active, maxActive int
	started           chan struct{}
	block             chan struct{}
}

func (t *serializedTool) Name() string { return "serialized" }
func (t *serializedTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: "serialized", InputSchema: map[string]interface{}{"type": "object"}}
}
func (t *serializedTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.mu.Lock()
	t.active++
	if t.active > t.maxActive {
		t.maxActive = t.active
	}
	t.mu.Unlock()
	select {
	case t.started <- struct{}{}:
	default:
	}
	<-t.block
	t.mu.Lock()
	t.active--
	t.mu.Unlock()
	return "ok", nil
}

func TestWriteLockSerializesConcurrentCallsAndCancellation(t *testing.T) {
	block := make(chan struct{})
	tool := &serializedTool{started: make(chan struct{}, 2), block: block}
	r := NewRegistry()
	r.Register(tool)
	r.(interface{ ConfigureWriteLock(*WriteLock) }).ConfigureWriteLock(&WriteLock{})
	first := make(chan schema.ToolResult, 1)
	go func() {
		first <- r.Execute(context.Background(), schema.ToolCall{Name: "serialized", Arguments: []byte(`{}`)})
	}()
	<-tool.started
	second := make(chan schema.ToolResult, 1)
	go func() {
		second <- r.Execute(context.Background(), schema.ToolCall{Name: "serialized", Arguments: []byte(`{}`)})
	}()
	select {
	case <-tool.started:
		t.Fatal("second writer entered before first released")
	case <-time.After(30 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	third := r.Execute(ctx, schema.ToolCall{Name: "serialized", Arguments: []byte(`{}`)})
	if !third.IsError {
		t.Fatal("cancelled lock waiter should fail")
	}
	close(block)
	<-first
	if secondRes := <-second; secondRes.IsError {
		t.Fatalf("second writer should eventually run: %#v", secondRes)
	}
	tool.mu.Lock()
	max := tool.maxActive
	tool.mu.Unlock()
	if max != 1 {
		t.Fatalf("writes overlapped, max active=%d", max)
	}
}

func TestWriteLockIsAcquiredAfterMiddleware(t *testing.T) {
	blockApproval := make(chan struct{})
	approved := make(chan struct{})
	tool := &serializedTool{started: make(chan struct{}, 2), block: make(chan struct{})}
	close(tool.block)
	r := NewRegistry()
	r.Register(tool)
	r.(interface{ ConfigureWriteLock(*WriteLock) }).ConfigureWriteLock(&WriteLock{})
	r.Use(func(_ context.Context, call schema.ToolCall) (bool, string) {
		if call.ID == "approval" {
			close(approved)
			<-blockApproval
		}
		return true, ""
	})
	go func() {
		_ = r.Execute(context.Background(), schema.ToolCall{ID: "approval", Name: "serialized", Arguments: []byte(`{}`)})
	}()
	<-approved
	res := r.Execute(context.Background(), schema.ToolCall{ID: "other", Name: "serialized", Arguments: []byte(`{}`)})
	if res.IsError {
		t.Fatalf("second call blocked by pending approval: %#v", res)
	}
	close(blockApproval)
}
