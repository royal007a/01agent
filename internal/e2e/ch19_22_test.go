//go:build live

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clawapp "github.com/weberzhao/go-tiny-claw/internal/app"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/trace"
)

// 第 19 讲：并行 bash(sleep 2) + write_file，Trace 里应看到两个 Tool.Execute 与逐轮耗时。
func TestL19_Tracing(t *testing.T) {
	realProvider(t)
	wd := workspace(t)
	a, err := clawapp.Build(clawapp.Options{WorkDir: wd, Provider: provider.ConfigFromEnv(), NoKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	var root *trace.Span
	a.Engine.OnRunFinished = func(s *trace.Span) { root = s }
	prompt := "为了加快执行速度，请你在一轮回复中同时完成两件事：1. 用 bash 执行 'sleep 2 && echo 系统环境检查完毕'；2. 用 write_file 创建 trace_test.md，内容为 测试并发的写入。请分别调用两个不同的工具。"
	if err := a.Engine.Chat(ctxTimeout(t), ctxpkg.NewSession("test_trace_001", wd), prompt, newRec()); err != nil {
		t.Fatal(err)
	}
	if root == nil || root.Count("Tool.Execute") < 2 || root.Count("LLM.Action") < 2 {
		t.Fatalf("trace 结构不完整")
	}
	files, _ := filepath.Glob(filepath.Join(wd, ".claw/traces/*.perfetto.json"))
	if len(files) != 1 {
		t.Fatal("未导出 perfetto trace")
	}
	b, _ := os.ReadFile(strings.TrimSuffix(files[0], ".perfetto.json") + ".json")
	t.Logf("trace:\n%s", b)
}

// 第 22 讲：AgentOps 真实模型演练（HTTP 审批通道由 recReporter 的终端审批代替：先同意 edit，再拒绝 reload）。
func TestL22_AgentOps(t *testing.T) {
	realProvider(t)
	repo, _ := filepath.Abs("../..")
	wd := filepath.Join(t.TempDir(), "ws")
	if out, err := exec.Command("bash", filepath.Join(repo, "deploy/agentops/setup-demo.sh"), wd).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Setenv("PATH", filepath.Join(repo, "deploy/agentops/bin")+":"+os.Getenv("PATH"))
	a, err := clawapp.Build(clawapp.Options{WorkDir: wd, Provider: provider.ConfigFromEnv(), NoKnowledge: true})
	if err != nil {
		t.Fatal(err)
	}
	a.Engine.MaxTurns = 20
	rec := newRec()
	// 值班工程师的决策：改配置（edit/write）同意，重启/杀进程等 bash 高危操作拒绝
	rec.approve = func(req permission.ApprovalRequest) bool { return req.ToolName != "bash" }
	if err := a.Engine.Chat(ctxTimeout(t), ctxpkg.NewSession("agentops", wd), "线上似乎出了点问题，帮我去服务器工作区里排查一下为什么 Nginx 起不来，并尝试修复它。", rec); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(filepath.Join(wd, "nginx.conf"))
	if strings.Contains(string(conf), "locat /") {
		t.Fatalf("配置未修复:\n%s", conf)
	}
	if _, err := os.Stat(filepath.Join(wd, ".nginx_reloaded")); err == nil {
		t.Fatal("reload 被拒绝后不应执行")
	}
	calls := rec.callsJoined()
	t.Logf("调用轨迹:\n%s\n最终回复: %s", calls, rec.lastMsg())
	if !strings.Contains(calls, "APPROVAL edit_file") {
		t.Fatal("修改配置应经过审批")
	}
}
