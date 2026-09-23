// Package engine 是核心引擎层：维护上下文时间线，驱动 ReAct Main Loop。
package engine

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// 慢思考阶段的两条"临时提示"：只拼进当次请求，不写入上下文时间线。
//
// 实测（方舟 doubao / Anthropic 协议）发现课程原版的纯架构剥离有两个坑：
//  1. 没有工具 Schema 时，模型会在文本里"脑补"伪工具调用（课程第 04 讲也观察到了 <invoke> 现象）；
//  2. Phase 2 的请求以 assistant(思考) 结尾，Anthropic 协议会把它当作 prefill 续写，
//     模型只会接着写文字而不再发起工具调用。
//
// 因此 Phase 1 追加一条"只规划、不调用"的提示，Phase 2 追加一条"按计划行动"的提示。
const (
	thinkingNudge = "[系统: 规划阶段] 现在请只做分析与规划：梳理已知信息、下一步要做什么以及原因。此阶段没有任何工具可用，不要输出或模拟任何工具调用语法。"
	actionNudge   = "[系统: 行动阶段] 请按照你上面的规划立即行动：需要工具就直接发起工具调用；如果任务已经完成，则直接给出最终答复。"
)

// AgentEngine 是微型 OS 的核心驱动。
type AgentEngine struct {
	provider provider.LLMProvider
	registry tools.Registry
	// WorkDir 工作区：Agent 必须有一个明确的物理边界
	WorkDir string
	// EnableThinking 慢思考模式开关（第 03 讲 Two-Stage ReAct）
	EnableThinking bool
}

func NewAgentEngine(p provider.LLMProvider, r tools.Registry, workDir string, enableThinking bool) *AgentEngine {
	return &AgentEngine{provider: p, registry: r, WorkDir: workDir, EnableThinking: enableThinking}
}

// Run 启动 Agent 的生命周期。
func (e *AgentEngine) Run(ctx context.Context, userPrompt string, reporter Reporter) error {
	if reporter == nil {
		reporter = NopReporter{}
	}
	log.Printf("[Engine] 引擎启动，锁定工作区: %s", e.WorkDir)
	log.Printf("[Engine] 慢思考模式 (Thinking Phase): %v", e.EnableThinking)

	contextHistory := []schema.Message{
		{Role: schema.RoleSystem, Content: "You are go-tiny-claw, an expert coding assistant. You have full access to tools in the workspace."},
		{Role: schema.RoleUser, Content: userPrompt},
	}

	for turn := 1; ; turn++ {
		log.Printf("========== [Turn %d] 开始 ==========", turn)
		availableTools := e.registry.GetAvailableTools()

		// Phase 1: 慢思考 —— 剥夺工具，强制规划
		if e.EnableThinking {
			log.Println("[Engine][Phase 1] 剥夺工具访问权，强制进入慢思考与规划阶段...")
			reporter.OnThinking(ctx)
			thinkReq := append(append([]schema.Message{}, contextHistory...), schema.Message{Role: schema.RoleUser, Content: thinkingNudge})
			thinkResp, err := e.provider.Generate(ctx, thinkReq, nil)
			if err != nil {
				return fmt.Errorf("Thinking 阶段生成失败: %w", err)
			}
			if thinkResp.Content != "" {
				log.Printf("[Engine] 🧠 内部思考 Trace: %s", textutil.Truncate(thinkResp.Content, 300, "..."))
				contextHistory = append(contextHistory, *thinkResp)
			}
		}

		// Phase 2: 行动 —— 恢复工具，顺着规划执行
		log.Println("[Engine][Phase 2] 恢复工具挂载，等待模型采取行动...")
		actionReq := contextHistory
		if e.EnableThinking && contextHistory[len(contextHistory)-1].Role == schema.RoleAssistant {
			actionReq = append(append([]schema.Message{}, contextHistory...), schema.Message{Role: schema.RoleUser, Content: actionNudge})
		}
		actionResp, err := e.provider.Generate(ctx, actionReq, availableTools)
		if err != nil {
			return fmt.Errorf("Action 阶段生成失败: %w", err)
		}
		contextHistory = append(contextHistory, *actionResp)
		if actionResp.Content != "" {
			reporter.OnMessage(ctx, actionResp.Content)
		}

		// 退出条件：模型没有请求任何工具调用
		if len(actionResp.ToolCalls) == 0 {
			log.Println("[Engine] 模型未请求调用工具，任务宣告完成。")
			return nil
		}

		// 执行行动 (Action) 与获取观察结果 (Observation)：Fork-Join 并发执行（第 08 讲）
		log.Printf("[Engine] 模型请求调用 %d 个工具...", len(actionResp.ToolCalls))
		observations := e.executeTools(ctx, actionResp.ToolCalls, reporter)
		contextHistory = append(contextHistory, observations...)
	}
}

// MaxParallelTools 单轮最多同时运行的工具数（第 08 讲思考题：信号量限流）。
const MaxParallelTools = 5

// executeTools 执行一批工具调用，并按模型原始顺序返回 Observation。
//
// 课程采用"独立性假设"无脑并行；第 08 讲的思维实验同时指出了同一文件读写竞争的风险，
// 并推荐"只读并发、涉写串行"。这里取两者之长：整批都是只读工具时并发（带信号量限流），
// 只要混入任何有副作用的工具（write/edit/bash），就退化为按顺序串行执行。
// 两种路径都写入预分配切片的固定下标，天然保序、无需加锁。
func (e *AgentEngine) executeTools(ctx context.Context, calls []schema.ToolCall, reporter Reporter) []schema.Message {
	observations := make([]schema.Message, len(calls))
	run := func(idx int, call schema.ToolCall) {
		log.Printf("  -> [Go-%d] 🛠️ 执行工具: %s, 参数: %s", idx, call.Name, textutil.Truncate(string(call.Arguments), 300, "..."))
		reporter.OnToolCall(ctx, call.Name, string(call.Arguments))
		result := e.registry.Execute(ctx, call)
		// 人类看到的是缩略版（防止飞书消息过长）；传给模型的 Observation 依然是完整数据
		reporter.OnToolResult(ctx, call.Name, textutil.Truncate(result.Output, 200, "... (已截断)"), result.IsError)
		if result.IsError {
			log.Printf("  -> [Go-%d] ❌ 工具执行报错: %s", idx, textutil.Truncate(result.Output, 300, "..."))
		} else {
			log.Printf("  -> [Go-%d] ✅ 工具执行成功 (返回 %d 字节)", idx, len(result.Output))
		}
		observations[idx] = schema.Message{
			Role:       schema.RoleUser,
			Content:    result.Output,
			ToolCallID: call.ID,
			IsError:    result.IsError,
		}
	}

	parallel := len(calls) > 1
	for _, c := range calls {
		if !e.registry.IsReadOnly(c.Name) {
			parallel = false
			break
		}
	}
	if !parallel {
		for i, c := range calls {
			run(i, c)
		}
		return observations
	}

	log.Printf("[Engine] 本批 %d 个工具均为只读，Fork 并发执行", len(calls))
	sem := make(chan struct{}, MaxParallelTools)
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(idx int, call schema.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run(idx, call)
		}(i, c)
	}
	wg.Wait()
	log.Println("[Engine] 所有并发工具执行完毕，开始聚合观察结果 (Observation)...")
	return observations
}
