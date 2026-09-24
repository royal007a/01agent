package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	clawapp "github.com/weberzhao/go-tiny-claw/internal/app"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// opsScript 按第 22 讲剧本行动的"脚本化模型"：tail 日志 → 读配置 → edit 修复 → nginx -t → nginx -s reload → 汇报。
type opsScript struct {
	mu        sync.Mutex
	sawSkill  bool
	sawAgents bool
}

func (o *opsScript) Generate(_ context.Context, msgs []schema.Message, _ []schema.ToolDefinition) (*schema.Message, error) {
	o.mu.Lock()
	if strings.Contains(msgs[0].Content, "ops_troubleshoot") {
		o.sawSkill = true
	}
	if strings.Contains(msgs[0].Content, "运维基线守则") {
		o.sawAgents = true
	}
	o.mu.Unlock()
	n := 0
	var last schema.Message
	for _, m := range msgs {
		if m.IsToolResult() {
			n++
			last = m
		}
	}
	call := func(name string, args any) (*schema.Message, error) {
		b, _ := json.Marshal(args)
		return &schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: fmt.Sprint("c", n), Name: name, Arguments: b}}}, nil
	}
	switch n {
	case 0:
		return call("bash", map[string]string{"command": "tail -n 50 error.log"})
	case 1:
		return call("read_file", map[string]string{"path": "nginx.conf"})
	case 2:
		return call("edit_file", map[string]string{"path": "nginx.conf", "old_text": "locat / {", "new_text": "location / {"})
	case 3:
		return call("bash", map[string]string{"command": "nginx -t"})
	case 4:
		return call("bash", map[string]string{"command": "nginx -s reload"})
	default:
		return &schema.Message{Role: schema.RoleAssistant, Content: "排障结束。reload 结果: " + last.Content}, nil
	}
}

// 第 22 讲 AgentOps 全链路（离线）：AGENTS.md + Skill 注入、大日志、edit 审批通过、reload 审批拒绝。
func TestAgentOpsScenario(t *testing.T) {
	repo, _ := filepath.Abs("../..")
	wd := filepath.Join(t.TempDir(), "ws")
	if out, err := exec.Command("bash", filepath.Join(repo, "deploy/agentops/setup-demo.sh"), wd).CombinedOutput(); err != nil {
		t.Fatalf("setup-demo: %v %s", err, out)
	}
	t.Setenv("PATH", filepath.Join(repo, "deploy/agentops/bin")+":"+os.Getenv("PATH"))

	model := &opsScript{}
	a, err := clawapp.Build(clawapp.Options{WorkDir: wd, LLM: model})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Engine: a.Engine, Sessions: ctxpkg.NewSessionManager(""), WorkDir: a.WorkDir, Approvals: permission.NewApprovalManager(10 * time.Second)}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	done := make(chan map[string]any)
	go func() {
		_, out := post(t, srv.URL+"/v1/chat", "", map[string]string{"session": "ops", "prompt": "线上 Nginx 起不来，帮我排查并修复"})
		done <- out
	}()
	decide := func(approve bool, wantSummary string) {
		for i := 0; i < 500; i++ {
			if list := s.Approvals.List(approvalScope("ops")); len(list) == 1 {
				if !strings.Contains(list[0].Summary, wantSummary) {
					t.Fatalf("待审批的应是 %q，实际: %s", wantSummary, list[0].Summary)
				}
				if code, _ := post(t, srv.URL+"/v1/approve", "", map[string]any{"session": "ops", "task_id": list[0].TaskID, "approve": approve}); code != 200 {
					t.Fatal("审批请求失败")
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("等待 %s 的审批超时", wantSummary)
	}
	decide(true, "edit_file")        // 修改配置：批准
	decide(false, "nginx -s reload") // 重启服务：拒绝
	out := <-done

	conf, _ := os.ReadFile(filepath.Join(wd, "nginx.conf"))
	if !strings.Contains(string(conf), "location / {") || strings.Contains(string(conf), "locat /") {
		t.Fatalf("配置未修复:\n%s", conf)
	}
	if _, err := os.Stat(filepath.Join(wd, ".nginx_reloaded")); err == nil {
		t.Fatal("reload 被拒绝后不应执行")
	}
	if !model.sawSkill || !model.sawAgents {
		t.Fatal("AGENTS.md / ops_troubleshoot Skill 未注入 System Prompt")
	}
	reply := out["reply"].(string)
	if !strings.Contains(reply, "执行被系统拦截") {
		t.Fatalf("模型应收到拒绝理由: %s", reply)
	}
	var sawTestOK bool
	for _, e := range out["events"].([]any) {
		ev := e.(map[string]any)
		if ev["type"] == "tool_result" && strings.Contains(fmt.Sprint(ev["content"]), "syntax is ok") {
			sawTestOK = true
		}
	}
	if !sawTestOK {
		t.Fatal("修复后 nginx -t 应通过")
	}
	traces, _ := filepath.Glob(filepath.Join(wd, ".claw/traces/*.perfetto.json"))
	if len(traces) != 1 {
		t.Fatal("应导出 Trace")
	}
}
