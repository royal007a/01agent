// Package app 统一组装 Harness 的全部组件，供 CLI、服务端与 Benchmark 复用。
package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// Options 组装参数。
type Options struct {
	WorkDir  string
	Thinking bool
	PlanMode bool
	Provider provider.Config
	// LLM 非空时直接使用（测试/Benchmark 注入），否则按 Provider 配置构造
	LLM provider.LLMProvider
	// StateDir Harness 状态目录（为空时取 CLAW_HOME 或 ~/.go-tiny-claw）
	StateDir string
	// NoKnowledge 关闭动态知识更新（Benchmark 等一次性任务可关闭以减少一轮交互）
	NoKnowledge bool
}

// App 一次组装出来的所有组件。
type App struct {
	Engine  *engine.AgentEngine
	WorkDir string
	Stats   *observability.Stats
}

func stateDir(d string) string {
	if d != "" {
		return d
	}
	if v := os.Getenv("CLAW_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go-tiny-claw")
}

// Build 像拼乐高一样组装：Provider(+CostTracker) / Registry(+权限中间件+计时) / Subagent。
func Build(o Options) (*App, error) {
	p := o.LLM
	if p == nil {
		var err error
		if p, err = provider.New(o.Provider); err != nil {
			return nil, err
		}
	}
	wd, err := filepath.Abs(o.WorkDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return nil, fmt.Errorf("创建工作区失败: %w", err)
	}
	// 第 18 讲：CostTracker 装饰真实 Provider，引擎毫不知情
	tracker := observability.NewCostTracker(p, o.Provider.Model, nil)

	base := tools.NewRegistry()
	base.Register(tools.NewReadFileTool(wd))
	base.Register(tools.NewWriteFileTool(wd))
	base.Register(tools.NewBashTool(wd))
	base.Register(tools.NewEditFileTool(wd))
	// 第 16 讲：allow/ask/deny 权限中间件，工作区 .claw/permissions.json 可热加载自定义规则
	policy := permission.NewPolicy(filepath.Join(wd, ".claw", "permissions.json"))
	base.Use(permission.Middleware(policy))
	registry := observability.NewTimedRegistry(base, tracker.Stats)

	eng := engine.NewAgentEngine(tracker, registry, o.Thinking, o.PlanMode)

	// 动态知识更新：内层 record_knowledge 写暂存区（Harness 状态目录），外层在 Run 结束后按工作区知识布局归位
	if !o.NoKnowledge {
		km := knowledge.NewManager(filepath.Join(stateDir(o.StateDir), "knowledge", "inbox"))
		base.Register(tools.NewRecordKnowledgeTool(km.Inbox))
		eng.Knowledge = km
	}

	// 第 17 讲：只读子智能体（read_file + 白名单 bash），挂成主 Agent 的一个普通工具
	readOnly := tools.NewRegistry()
	readOnly.Register(tools.NewReadFileTool(wd))
	readOnly.Register(tools.NewBashTool(wd))
	readOnly.Use(tools.ReadOnlyGuard())
	readOnly.Use(permission.Middleware(policy))
	base.Register(tools.NewSubagentTool(eng, observability.NewTimedRegistry(readOnly, tracker.Stats)))

	return &App{Engine: eng, WorkDir: wd, Stats: tracker.Stats}, nil
}
