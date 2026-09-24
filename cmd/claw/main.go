// go-tiny-claw：一个从 0 构建的 Agent Harness。
//
//	claw -prompt "任务" [-thinking] [-workdir dir]   终端单次运行
//	claw serve [-addr 127.0.0.1:48080]              飞书机器人服务端
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	clawapp "github.com/weberzhao/go-tiny-claw/internal/app"
	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
)

func defaultStateDir() string {
	if d := os.Getenv("CLAW_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go-tiny-claw")
}

// app 是 internal/app.App 的本地别名，保持入口代码简短。
type app struct {
	eng     *engine.AgentEngine
	workDir string
	stats   *observability.Stats
}

func buildEngine(workDir string, thinking, planMode bool) *app {
	a, err := clawapp.Build(clawapp.Options{WorkDir: workDir, Thinking: thinking, PlanMode: planMode, Provider: provider.ConfigFromEnv()})
	if err != nil {
		log.Fatal(err)
	}
	return &app{eng: a.Engine, workDir: a.WorkDir, stats: a.Stats}
}

func printReport(sess *ctxpkg.Session, st *observability.Stats) {
	p, c, cost := sess.UsageSnapshot()
	m := st.Snapshot()
	log.Printf("================ 财务报表 ================")
	log.Printf("会话 ID: %s", sess.ID)
	log.Printf("LLM 调用: %d 次（失败 %d）| 累计推理耗时: %v", m.LLMCalls, m.LLMErrors, m.LLMLatency.Round(time.Millisecond))
	log.Printf("工具调用: %d 次（失败 %d）| 累计本地执行耗时: %v", m.ToolCalls, m.ToolErrors, m.ToolLatency.Round(time.Millisecond))
	log.Printf("总消耗 Input Tokens: %d | Output Tokens: %d", p, c)
	log.Printf("总计费用 (CNY): ¥%.6f", cost)
	log.Printf("==========================================")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	prompt := flag.String("prompt", "", "交给 Agent 的任务")
	thinking := flag.Bool("thinking", false, "开启慢思考")
	workDir := flag.String("workdir", ".", "工作区目录")
	flag.StringVar(workDir, "dir", ".", "工作区目录（-workdir 的别名，第 21 讲）")
	planMode := flag.Bool("plan", false, "开启 Plan Mode（PLAN.md/TODO.md 状态外部化）")
	sessionID := flag.String("session", "", "会话 ID；设置后历史以 JSONL 持久化，可跨进程续聊")
	yes := flag.Bool("yes", false, "自动批准高危操作（仅建议在沙箱中使用）")
	maxTurns := flag.Int("max-turns", 60, "单次任务最大轮数（硬性兜底，0 为不限制）")
	flag.Parse()
	if *prompt == "" {
		log.Fatal("用法: claw -prompt \"任务\" [-thinking] [-workdir dir] | claw serve")
	}
	a := buildEngine(*workDir, *thinking, *planMode)
	a.eng.MaxTurns = *maxTurns
	fmt.Println("==================================================")
	fmt.Println("🚀 启动 go-tiny-claw CLI 引擎...")
	fmt.Printf("📁 锁定工作区: %s\n", a.workDir)
	fmt.Printf("🧠 慢思考: %v | 📋 Plan Mode: %v | ⛔ 最大轮数: %d\n", *thinking, *planMode, *maxTurns)
	fmt.Println("==================================================")
	fmt.Printf("\n🎯 收到任务: %s\n\n", *prompt)
	started := time.Now()
	mgr := ctxpkg.GlobalSessionMgr
	id := *sessionID
	if id != "" {
		mgr = ctxpkg.NewSessionManager(filepath.Join(defaultStateDir(), "sessions"))
	} else {
		id = "cli"
	}
	sess := mgr.GetOrCreate(id, a.workDir)
	reporter := engine.NewTerminalReporter()
	reporter.AutoApprove = *yes
	err := a.eng.Chat(context.Background(), sess, *prompt, reporter)
	printReport(sess, a.stats)
	fmt.Printf("\n✨ 任务结束，总耗时 %v；执行回放见 %s\n", time.Since(started).Round(time.Millisecond), filepath.Join(a.workDir, ".claw", "traces"))
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
