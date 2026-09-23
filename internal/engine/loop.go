// Package engine 是核心引擎层：维护上下文时间线，驱动 ReAct Main Loop。
package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/weberzhao/go-tiny-claw/internal/provider"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
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
func (e *AgentEngine) Run(ctx context.Context, userPrompt string) error {
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
			thinkReq := append(append([]schema.Message{}, contextHistory...), schema.Message{Role: schema.RoleUser, Content: thinkingNudge})
			thinkResp, err := e.provider.Generate(ctx, thinkReq, nil)
			if err != nil {
				return fmt.Errorf("Thinking 阶段生成失败: %w", err)
			}
			if thinkResp.Content != "" {
				fmt.Printf("  🧠 [内部思考 Trace]: %s\n", thinkResp.Content)
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
			fmt.Printf("  🤖 [对外回复]: %s\n", actionResp.Content)
		}

		// 退出条件：模型没有请求任何工具调用
		if len(actionResp.ToolCalls) == 0 {
			log.Println("[Engine] 模型未请求调用工具，任务宣告完成。")
			return nil
		}

		// 执行行动 (Action) 与获取观察结果 (Observation)
		log.Printf("[Engine] 模型请求调用 %d 个工具...", len(actionResp.ToolCalls))
		for _, toolCall := range actionResp.ToolCalls {
			log.Printf("  -> 🛠️ 执行工具: %s, 参数: %s", toolCall.Name, string(toolCall.Arguments))
			result := e.registry.Execute(ctx, toolCall)
			if result.IsError {
				log.Printf("  -> ❌ 工具执行报错: %s", result.Output)
			} else {
				log.Printf("  -> ✅ 工具执行成功 (返回 %d 字节)", len(result.Output))
			}
			contextHistory = append(contextHistory, schema.Message{
				Role:       schema.RoleUser,
				Content:    result.Output,
				ToolCallID: toolCall.ID,
				IsError:    result.IsError,
			})
		}
	}
}
