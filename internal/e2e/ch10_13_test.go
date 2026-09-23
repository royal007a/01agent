//go:build live

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// 第 10 讲：AGENTS.md + git-workflow Skill 约束下写 ping.go 并提交。
func TestL10_AgentsMDAndSkills(t *testing.T) {
	wd := workspace(t)
	// 模型常会自行拉起 ping 服务做自测（nohup ... &），测试结束时清理掉，避免占用 8080
	t.Cleanup(func() { exec.Command("pkill", "-f", `(/tmp/ping[-a-z]*|/exe/ping)$|go run ping\.go`).Run() })
	write(t, filepath.Join(wd, "AGENTS.md"), "# 项目规范\n- 所有的 API 接口都必须返回 JSON 格式，且包含 `code` 和 `message` 字段。\n- 所有的错误处理，必须返回中文报错信息。\n")
	write(t, filepath.Join(wd, ".claw/skills/git-workflow/SKILL.md"), "---\nname: git-workflow\ndescription: 当人类用户要求你“提交代码”、“保存变更”或执行 Git 相关操作时，必须使用此技能。\n---\n# 提交流程 SOP\n1. 先使用 `bash` 调用 `git status` 确认当前有哪些文件发生了改动。\n2. 你的 commit message 必须使用 Emoji 开头，例如：`🚀 feat: 增加新功能` 或 `🐛 fix: 修复 Bug`。\n3. 严禁使用 `git commit -am \"update\"` 这种敷衍的提交。\n")
	sh(t, wd, "git init -q . && git add . && git -c user.name=t -c user.email=t@t commit -qm 'initial import'")
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), true, false)
	sess := ctxpkg.NewSession("l10", wd)
	rec := newRec()
	err := eng.Chat(ctxTimeout(t), sess, "我需要在当前目录下新建一个 ping.go，提供一个简单的 http ping 接口。写完之后，帮我把代码用 git 提交一下（git 用户名用 claw，邮箱 claw@example.com）。", rec)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(wd, "ping.go"))
	if !strings.Contains(string(src), "code") || !strings.Contains(string(src), "message") {
		t.Fatalf("ping.go 未遵守 AGENTS.md 的 JSON 规范:\n%s", src)
	}
	logMsg := sh(t, wd, "git log -1 --pretty=%s")
	t.Logf("commit message: %s", logMsg)
	if strings.Contains(logMsg, "initial import") {
		t.Fatal("没有产生新的提交")
	}
	if r := []rune(strings.TrimSpace(logMsg)); len(r) == 0 || r[0] < 0x2000 {
		t.Fatalf("commit message 未以 Emoji 开头: %q", logMsg)
	}
	if !strings.Contains(rec.callsJoined(), "git status") {
		t.Fatal("未按 Skill SOP 先执行 git status")
	}
}

// 第 11 讲：两个会话并发互不串味；Working Memory 截断后"忘记"第一轮内容。
func TestL11_SessionIsolationAndWorkingMemory(t *testing.T) {
	front, back := workspace(t), workspace(t)
	write(t, filepath.Join(front, "README.md"), "这是项目 A 的 README，里面包含了一个密钥: token_12345")
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadFileTool(front))
	eng := engine.NewAgentEngine(realProvider(t), reg, false, false)
	eng.WorkingMemoryLimit = 6
	mgr := ctxpkg.NewSessionManager("")
	a, b := mgr.GetOrCreate("chat_front_001", front), mgr.GetOrCreate("chat_back_002", back)

	ctx := ctxTimeout(t)
	done := make(chan error, 1)
	recB := newRec()
	go func() {
		done <- eng.Chat(ctx, b, "别人查到了一个密钥，你这里能看到吗？不准调用工具！直接回答你看到的密钥原文，没有就说没有。", recB)
	}()
	recA := newRec()
	if err := eng.Chat(ctx, a, "帮我看看 README.md 里记录了什么密钥？", recA); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recA.lastMsg(), "token_12345") {
		t.Fatalf("Session A 应读到密钥: %s", recA.lastMsg())
	}
	if strings.Contains(recB.lastMsg(), "token_12345") {
		t.Fatalf("Session B 串味了: %s", recB.lastMsg())
	}
	for i := 0; i < 6; i++ {
		a.Append(ctxpkgUser("这只是一句闲聊占位符。"), ctxpkgAssistant("好的，收到闲聊。"))
	}
	recA2 := newRec()
	if err := eng.Chat(ctx, a, "请直接告诉我，刚才第一轮你查到的那个密钥是什么？不准调用工具！如果不记得就说不记得。", recA2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recA2.lastMsg(), "token_12345") {
		t.Fatalf("Working Memory 截断未生效，模型仍记得: %s", recA2.lastMsg())
	}
	if a.Len() < 15 {
		t.Fatal("Session 全量历史不应被截断")
	}
}

// 第 12 讲：读取巨型日志触发 Compactor，任务仍能继续完成。
func TestL12_ContextCompaction(t *testing.T) {
	wd := workspace(t)
	write(t, filepath.Join(wd, "mock_log.txt"), strings.Repeat("这是一段极其冗长的、无意义的服务器报错日志信息，用来模拟 OOM 场景\n", 2000))
	eng := engine.NewAgentEngine(realProvider(t), fullRegistry(wd), false, false)
	eng.SetCompactor(ctxpkg.NewCompactor(3000, 6))
	lb := captureLog(t)
	rec := newRec()
	sess := ctxpkg.NewSession("l12", wd)
	err := eng.Chat(ctxTimeout(t), sess, "请帮我按顺序执行以下三个步骤（每步一次工具调用）：\n1. 使用 bash 执行 echo \"开始排查日志\"\n2. 使用 read_file 工具读取当前目录下的巨大文件 mock_log.txt\n3. 使用 bash 执行 date 命令获取当前时间，并告诉我任务全部完成。", rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lb.String(), "[Compactor] ✅ 压缩完成") {
		t.Fatal("Compactor 未触发")
	}
	calls := rec.callsJoined()
	if !strings.Contains(calls, "mock_log.txt") || !strings.Contains(calls, "date") {
		t.Fatalf("任务未完整执行:\n%s", calls)
	}
	// Session 中保存的是全量（read_file 已截断到 8000 字节，但未被 Compactor 修改）
	for _, m := range sess.History() {
		if m.IsToolResult() && strings.Contains(m.Content, "系统强制清理") {
			t.Fatal("Compactor 不得修改 Session 全量数据")
		}
	}
}

// 第 13 讲：Plan Mode 生成 PLAN.md/TODO.md；"重启"后从断点继续而不覆盖。
func TestL13_PlanModeResume(t *testing.T) {
	wd := workspace(t)
	p := realProvider(t)
	eng := engine.NewAgentEngine(p, fullRegistry(wd), false, true)
	prompt := "我需要你搭建一个极简的 Go 语言 Web Server 项目（只用标准库，提供 / 和 /healthz 两个路由），不要启动常驻服务，最后用 go build 验证即可。"

	// 第一次运行：只允许跑一部分——通过取消 ctx 模拟进程被 Kill
	sess1 := ctxpkg.NewSession("plan-1", wd)
	rec1 := newRec()
	ctx1, cancel := contextWithTurnBudget(ctxTimeout(t), rec1, 6)
	_ = eng.Chat(ctx1, sess1, prompt, rec1)
	cancel()
	plan, err1 := os.ReadFile(filepath.Join(wd, "PLAN.md"))
	todo, err2 := os.ReadFile(filepath.Join(wd, "TODO.md"))
	if err1 != nil || err2 != nil {
		t.Fatalf("第一次运行应创建 PLAN.md/TODO.md: %v %v", err1, err2)
	}
	t.Logf("中断时 TODO.md:\n%s", todo)

	// 第二次运行：全新 Session（短期记忆清零），同样的指令
	sess2 := ctxpkg.NewSession("plan-2", wd)
	rec2 := newRec()
	if err := eng.Chat(ctxTimeout(t), sess2, prompt, rec2); err != nil {
		t.Fatal(err)
	}
	calls := rec2.callsJoined()
	if !strings.Contains(calls, "TODO.md") {
		t.Fatalf("重启后应读取 TODO.md 续传:\n%s", calls)
	}
	plan2, _ := os.ReadFile(filepath.Join(wd, "PLAN.md"))
	if string(plan2) != string(plan) && strings.Contains(calls, `write_file {"path":"PLAN.md"`) {
		t.Fatal("断点续传不应覆盖 PLAN.md")
	}
	todo2, _ := os.ReadFile(filepath.Join(wd, "TODO.md"))
	t.Logf("完成时 TODO.md:\n%s", todo2)
	if !strings.Contains(string(todo2), "[x]") {
		t.Fatal("TODO.md 应有打勾项")
	}
	sh(t, wd, "ls *.go && go build ./... 2>&1 || (go mod init demo >/dev/null 2>&1; go build ./...)")
}
