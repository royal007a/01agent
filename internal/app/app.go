// Package app 统一组装 Harness 的全部组件，供 CLI、服务端与 Benchmark 复用。
package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/weberzhao/go-tiny-claw/internal/engine"
	"github.com/weberzhao/go-tiny-claw/internal/envlock"
	"github.com/weberzhao/go-tiny-claw/internal/knowledge"
	"github.com/weberzhao/go-tiny-claw/internal/observability"
	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/retrieval"
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
	// Model 模型哨兵：记录服务端回报的实际模型版本
	Model *envlock.ModelGuard
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
	// 环境锁定：工作区有 .claw/env.lock.json 时启动先比对漂移；模型哨兵总是挂上，检测运行途中的静默滚动
	guard, err := lockEnv(o, wd, p)
	if err != nil {
		return nil, err
	}
	tracker := observability.NewCostTracker(guard, o.Provider.Model, nil)

	base := tools.NewRegistry()
	base.Register(tools.NewReadFileTool(wd))
	base.Register(tools.NewWriteFileTool(wd))
	base.Register(tools.NewBashTool(wd))
	base.Register(tools.NewEditFileTool(wd))
	stateRoot := os.Getenv("CLAW_HOME")
	if stateRoot == "" && o.StateDir == "" {
		var e error
		stateRoot, e = os.UserConfigDir()
		if e != nil {
			return nil, fmt.Errorf("resolve retrieval state directory: %w", e)
		}
	}
	ledgerDir := filepath.Join(stateRoot, "go-tiny-claw", "retrieval")
	if o.StateDir != "" { // 测试/Benchmark 显式指定状态目录时，检索账本也随之隔离
		ledgerDir = filepath.Join(o.StateDir, "retrieval")
	}
	ledger := retrieval.NewLedgerStore(ledgerDir)
	base.Register(&tools.SearchTool{Store: ledger})
	base.Register(&tools.EvidenceTool{Store: ledger})
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

	return &App{Engine: eng, WorkDir: wd, Stats: tracker.Stats, Model: guard}, nil
}

// lockEnv 按工作区锁文件校验环境；无锁时只挂模型哨兵，锁文件损坏则拒绝启动。
func lockEnv(o Options, wd string, p provider.LLMProvider) (*envlock.ModelGuard, error) {
	g := &envlock.ModelGuard{Inner: p}
	lk, err := envlock.Load(wd)
	if errors.Is(err, envlock.ErrNoLock) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	cur, err := envlock.Capture(envlock.CaptureOptions{WorkDir: wd, Model: envlock.ModelOf(o.Provider), Tools: lk.ToolNames()})
	if err != nil {
		return nil, err
	}
	if o.LLM != nil { // 注入的 Provider 可能没有配置身份：只对未配置的字段豁免，配置了的照常比对
		m := &cur.Model
		for _, f := range []struct {
			cur    *string
			locked string
		}{{&m.Provider, lk.Model.Provider}, {&m.BaseURL, lk.Model.BaseURL}, {&m.Requested, lk.Model.Requested}} {
			if *f.cur == "" {
				*f.cur = f.locked
			}
		}
	}
	cur.Model.Observed = "" // 实际版本要等首次响应，由哨兵在运行时核对
	if err := envlock.Enforce(lk.Mode, envlock.Diff(lk, cur), log.Printf); err != nil {
		return nil, err
	}
	g.Expected, g.Strict = lk.Model.Observed, lk.Mode == envlock.ModeStrict
	return g, nil
}
