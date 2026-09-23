// go-tiny-claw：一个从 0 构建的 Agent Harness。
//
//	claw -prompt "任务" [-thinking] [-workdir dir]   终端单次运行
//	claw serve [-addr 127.0.0.1:48080]              飞书机器人服务端
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	ctxpkg "github.com/weberzhao/go-tiny-claw/internal/context"
	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

func defaultStateDir() string {
	if d := os.Getenv("CLAW_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go-tiny-claw")
}

// app 聚合一次组装出来的所有组件。
type app struct {
	eng     *engine.AgentEngine
	workDir string
	stats   *observability.Stats
}

// buildEngine 像拼乐高一样组装 Harness：Provider(+Tracker) / Registry(+权限中间件+计时) / Subagent。
func buildEngine(workDir string, thinking, planMode bool) *app {
	cfg := provider.ConfigFromEnv()
	p, err := provider.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	wd, _ := filepath.Abs(workDir)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		log.Fatal(err)
	}
	// 第 18 讲：用 CostTracker 装饰真实 Provider，引擎毫不知情
	tracker := observability.NewCostTracker(p, cfg.Model, nil)

	base := tools.NewRegistry()
	base.Register(tools.NewReadFileTool(wd))
	base.Register(tools.NewWriteFileTool(wd))
	base.Register(tools.NewBashTool(wd))
	base.Register(tools.NewEditFileTool(wd))
	// 第 16 讲：allow/ask/deny 权限中间件，工作区 .claw/permissions.json 可热加载自定义规则
	base.Use(permission.Middleware(permission.NewPolicy(filepath.Join(wd, ".claw", "permissions.json"))))
	registry := observability.NewTimedRegistry(base, tracker.Stats)

	eng := engine.NewAgentEngine(tracker, registry, thinking, planMode)

	// 第 17 讲：只读的子智能体兵器库（read_file + 只读 bash），挂成主 Agent 的一个普通工具
	readOnly := tools.NewRegistry()
	readOnly.Register(tools.NewReadFileTool(wd))
	readOnly.Register(tools.NewBashTool(wd))
	readOnly.Use(tools.ReadOnlyGuard())
	base.Register(tools.NewSubagentTool(eng, observability.NewTimedRegistry(readOnly, tracker.Stats)))

	return &app{eng: eng, workDir: wd, stats: tracker.Stats}
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
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
