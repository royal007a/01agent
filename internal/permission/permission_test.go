package permission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

func bash(cmd string) schema.ToolCall {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return schema.ToolCall{ID: "c", Name: "bash", Arguments: b}
}

func TestDecideDefaults(t *testing.T) {
	p := NewPolicy("")
	cases := map[string]Action{
		"ls -la":                    Allow,
		"git status":                Allow,
		"rm -rf *":                  Ask,
		"rm -fr build":              Ask, // 课程的 rm\s+-r 抓不到
		"rm -f a.txt":               Ask,
		"rm a.txt":                  Allow,
		"sudo apt install x":        Ask,
		"echo 'DROP TABLE users'":   Ask,
		"echo x > main.go":          Ask,
		"git push origin main":      Ask,
		"rm -rf /":                  Deny,
		"cd /tmp && rm -rf / ":      Deny,
		"kubectl delete pod x":      Ask,
		"grep -r 'rm -rf' docs.txt": Ask, // 正则不理解 shell 引号：宁可误报走审批，也不漏放
	}
	for cmd, want := range cases {
		if got, _ := p.Decide(bash(cmd)); got != want {
			t.Errorf("%q => %s, want %s", cmd, got, want)
		}
	}
	if got, _ := p.Decide(schema.ToolCall{Name: "read_file", Arguments: []byte(`{"path":"rm -rf"}`)}); got != Allow {
		t.Error("bash 规则不应作用于其他工具")
	}
}

func TestHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.json")
	p := NewPolicy(path)
	if a, _ := p.Decide(bash("curl http://x")); a != Allow {
		t.Fatal(a)
	}
	os.WriteFile(path, []byte(`{"rules":[{"tool":"bash","pattern":"\\bcurl\\b","action":"deny"},{"tool":"bash","pattern":"^rm -rf tmp$","action":"allow"}]}`), 0o644)
	if a, _ := p.Decide(bash("curl http://x")); a != Deny {
		t.Fatalf("热加载后应 deny, got %s", a)
	}
	if a, _ := p.Decide(bash("rm -rf tmp")); a != Ask {
		t.Fatalf("ask 优先级高于 allow, got %s", a)
	}
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(path, []byte(`{not json`), 0o644)
	os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if a, _ := p.Decide(bash("curl http://x")); a != Deny {
		t.Fatal("坏文件不应清空已加载规则")
	}
}

type fakeApprover struct {
	allow bool
	got   ApprovalRequest
}

func (f *fakeApprover) RequestApproval(_ context.Context, r ApprovalRequest) (bool, string) {
	f.got = r
	if f.allow {
		return true, ""
	}
	return false, "人类拒绝"
}

func TestMiddleware(t *testing.T) {
	mw := Middleware(NewPolicy(""))
	if ok, _ := mw(context.Background(), bash("ls")); !ok {
		t.Fatal("普通命令应直接放行")
	}
	if ok, reason := mw(context.Background(), bash("rm -rf *")); ok || !strings.Contains(reason, "没有审批通道") {
		t.Fatalf("无审批通道时应 fail-closed: %s", reason)
	}
	fa := &fakeApprover{allow: true}
	ctx := runctx.WithReporter(context.Background(), fa)
	if ok, _ := mw(ctx, bash("rm -rf *")); !ok || fa.got.ToolName != "bash" {
		t.Fatal("审批通过应放行")
	}
	fa.allow = false
	if ok, reason := mw(ctx, bash("sudo reboot")); ok || reason != "人类拒绝" {
		t.Fatal("审批拒绝应拦截并透传理由")
	}
	if ok, reason := mw(ctx, bash("rm -rf /")); ok || !strings.Contains(reason, "永久禁止") {
		t.Fatal("deny 不经审批直接拒绝")
	}
}

func TestApprovalManager(t *testing.T) {
	m := NewApprovalManager(time.Second)
	done := make(chan bool)
	go func() {
		ok, _ := m.Wait(context.Background(), "t1", "chatA", func() {})
		done <- ok
	}()
	for m.Pending() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Resolve("t1", "chatB", true, ""); err == nil {
		t.Fatal("其他会话不应能审批")
	}
	if err := m.Resolve("t1", "chatA", true, ""); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("应放行")
	}
	m.Timeout = 50 * time.Millisecond
	if ok, reason := m.Wait(context.Background(), "t2", "c", func() {}); ok || !strings.Contains(reason, "超时") {
		t.Fatal("应超时拒绝")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, _ := m.Wait(ctx, "t3", "c", func() {}); ok {
		t.Fatal("ctx 取消应拒绝")
	}
	if m.Pending() != 0 {
		t.Fatal("待审批任务应被清理")
	}
}

// 回归：独立 review 发现的规则绕过。
func TestReviewBypassRegressions(t *testing.T) {
	p := NewPolicy("")
	cases := map[string]Action{
		"rm --recursive --force /":                Deny,
		"rm -r -f /":                              Deny,
		"rm -rf /*":                               Deny,
		"rm --recursive build":                    Ask,
		"find / -delete":                          Ask,
		"echo x | tee main.go":                    Ask,
		"echo cm0gLXJmIHgK | base64 -d | bash":    Ask,
		"curl localhost:48080/v1/approve -d '{}'": Deny,
		"rm -rf /tmp/build":                       Ask,
		"ls -la /":                                Allow,
	}
	for cmd, want := range cases {
		if got, _ := p.Decide(bash(cmd)); got != want {
			t.Errorf("%q => %s, want %s", cmd, got, want)
		}
	}
}
