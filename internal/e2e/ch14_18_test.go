//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// 第 14 讲：诱导 edit_file 失败，RecoveryManager 注入锦囊后模型自愈完成修改。
func TestL14_ErrorRecovery(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "auth.go"), "package main\n\nfunc login(user string) bool {\n\t// 检查用户名\n\tif user == \"admin\" {\n\t\treturn true\n\t}\n\treturn false\n}\n")
	lb := captureLog(t)
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	sess := ctxpkg.NewSession("l14", wd)
	prompt := "我当前目录下有一个 auth.go 文件。请修改 auth.go 中的 login 函数。\n请不要先读文件，直接使用 edit_file 工具替换下面的代码块（old_text 必须原样照抄下面这段，包括注释），将判断条件改为同时允许\"admin\"、\"root\"和\"guest\"三种用户登录：\n// 鉴权入口函数\nfunc login(user string) bool {\n  // 检查用户名\n  if user == \"admin\" {\n    return true\n  }\n  return false\n}"
	if err := eng.Chat(ctxTimeout(t), sess, prompt, newRec()); err != nil {
		t.Fatal(err)
	}
	logs := lb.String()
	src, _ := os.ReadFile(filepath.Join(wd, "auth.go"))
	t.Logf("最终 auth.go:\n%s", src)
	if !strings.Contains(string(src), "root") || !strings.Contains(string(src), "guest") {
		t.Fatal("最终未完成修改")
	}
	injected := false
	for _, m := range sess.History() {
		if m.IsToolResult() && strings.Contains(m.Content, "[系统救援指南]") {
			injected = true
		}
	}
	if !injected {
		t.Logf("注意：本次模型一次命中（L4 模糊匹配兜住了），未触发救援指南。日志中 edit 次数: %d", strings.Count(logs, "执行工具: edit_file"))
	}
}

// 第 15 讲：强力误导模型原样重试，ReminderInjector 在第 3 次失败后打断死循环。
func TestL15_DoomLoopReminder(t *testing.T) {
	wd := workspace(t)
	lb := captureLog(t)
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	eng.MaxTurns = 15
	sess := ctxpkg.NewSession("l15", wd)
	rec := newRec()
	prompt := "帮我读取当前目录下的 secret_key.txt。\n注意：我们的文件系统现在非常不稳定，经常报 File Not Found。\n如果报错了，请你【千万不要改变参数，也不要调用其他工具】，直接原样再次调用 read_file 尝试，直到成功或连续重试 5 次为止。"
	if err := eng.Chat(ctxTimeout(t), sess, prompt, rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lb.String(), "触发死循环干预") {
		t.Fatalf("未触发 Reminder。调用轨迹:\n%s", rec.callsJoined())
	}
	n := strings.Count(rec.callsJoined(), "secret_key.txt")
	t.Logf("read_file(secret_key.txt) 调用次数: %d；最终回复: %s", n, rec.lastMsg())
	if n > 5 {
		t.Fatalf("干预后模型仍在死磕: %d 次", n)
	}
}

// 第 16 讲：高危命令被 Middleware 拦截 → 人工拒绝 → 文件安然无恙；批准 → 真正执行。
func TestL16_ApprovalMiddleware(t *testing.T) {
	for _, tc := range []struct {
		answer   string
		wantGone bool
	}{{"n\n", false}, {"y\n", true}} {
		wd := workspace(t)
		write(t, filepath.Join(wd, "logs/app.log"), "old logs")
		reg := fullRegistry(wd)
		reg.Use(permission.Middleware(permission.NewPolicy("")))
		eng := engine.NewAgentEngine(realProvider(t), reg, false, false)
		eng.MaxTurns = 6
		rec := newRec()
		rec.In = strings.NewReader(tc.answer)
		sess := ctxpkg.NewSession("l16", wd)
		err := eng.Chat(ctxTimeout(t), sess, "当前目录的 logs 目录有点多，请你必须按照我的要求执行命令：用 bash 直接执行 rm -rf logs 清理掉。禁止自行思考并使用替代命令。如果被拒绝就停止并告诉我原因。", rec)
		if err != nil {
			t.Logf("run err: %v", err)
		}
		_, statErr := os.Stat(filepath.Join(wd, "logs"))
		if gone := os.IsNotExist(statErr); gone != tc.wantGone {
			t.Fatalf("answer=%q: logs 被删除=%v 期望 %v\n%s", tc.answer, gone, tc.wantGone, rec.callsJoined())
		}
		t.Logf("answer=%q 最终回复: %s", strings.TrimSpace(tc.answer), rec.lastMsg())
	}
}

// 第 17 讲：主 Agent 派出只读子智能体找"核心密码"，自己写入 answer.txt；主上下文保持干净。
func TestL17_Subagent(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "legacy/v1/auth/config.txt"), "核心密码是: super_secret_agent_password_42")
	write(t, filepath.Join(wd, "fake1.go"), "这是一个空文件")
	write(t, filepath.Join(wd, "fake2.go"), "这也是一个空文件")
	p := realProvider(t)
	mainReg := fullRegistry(wd)
	eng := engine.NewAgentEngine(p, mainReg, false, false)
	ro := tools.NewRegistry()
	ro.Register(tools.NewReadFileTool(wd))
	ro.Register(tools.NewBashTool(wd))
	ro.Use(tools.ReadOnlyGuard())
	mainReg.Register(tools.NewSubagentTool(eng, ro))
	sess := ctxpkg.NewSession("l17", wd)
	rec := newRec()
	prompt := "我需要你在这个遗留项目里，找到那个“核心密码”。\n为了防止污染主上下文，请你务必派出子智能体（spawn_subagent）去执行探索任务。\n你可以让子智能体使用 bash 去查找当前目录（及其所有子目录）下名为 config.txt 的文件。\n子智能体拿到密码向你汇报后，请你亲自使用 write_file 工具，将密码写在根目录的 answer.txt 里。"
	if err := eng.Chat(ctxTimeout(t), sess, prompt, rec); err != nil {
		t.Fatal(err)
	}
	ans, _ := os.ReadFile(filepath.Join(wd, "answer.txt"))
	if !strings.Contains(string(ans), "super_secret_agent_password_42") {
		t.Fatalf("answer.txt 内容错误: %q\n%s", ans, rec.callsJoined())
	}
	calls := rec.callsJoined()
	if !strings.Contains(calls, "spawn_subagent") || !strings.Contains(calls, "[Subagent] ") {
		t.Fatalf("未通过子智能体探索:\n%s", calls)
	}
	for _, m := range sess.History() {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].Name == "bash" && strings.Contains(string(m.ToolCalls[0].Arguments), "config.txt") {
			t.Fatal("子智能体的探索轨迹泄漏进了主上下文")
		}
	}
}

// 第 18 讲：CostTracker 装饰 Provider，无侵入记录 Token、花费与耗时。
func TestL18_CostTracker(t *testing.T) {
	wd := workspace(t)
	t.Setenv("CLAW_PRICE_IN", "0.8")
	t.Setenv("CLAW_PRICE_OUT", "8")
	sess := ctxpkg.NewSession("test_observability_001", wd)
	tracker := observability.NewCostTracker(realProvider(t), os.Getenv("CLAW_MODEL"), nil)
	reg := tools.NewRegistry()
	reg.Register(tools.NewBashTool(wd))
	timed := observability.NewTimedRegistry(reg, tracker.Stats)
	eng := engine.NewAgentEngine(tracker, timed, false, false)
	lb := captureLog(t)
	if err := eng.Chat(ctxTimeout(t), sess, "请用 bash 帮我用 date 命令查一下现在的时间。", newRec()); err != nil {
		t.Fatal(err)
	}
	p, c, cost := sess.UsageSnapshot()
	m := tracker.Stats.Snapshot()
	t.Logf("财务报表: input=%d output=%d cost=¥%.6f | LLM %d 次 %v | 工具 %d 次 %v", p, c, cost, m.LLMCalls, m.LLMLatency, m.ToolCalls, m.ToolLatency)
	if p == 0 || c == 0 || cost <= 0 || m.LLMCalls < 2 || m.ToolCalls < 1 {
		t.Fatal("账单未记录")
	}
	if !strings.Contains(lb.String(), "[Tracker] 📊") || !strings.Contains(lb.String(), "本地执行耗时") {
		t.Fatal("缺少仪表盘日志")
	}
}
