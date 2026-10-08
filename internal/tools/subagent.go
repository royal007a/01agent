package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// AgentRunner 打破 tools ↔ engine 的循环依赖：由 engine 实现，注入给 SubagentTool（第 17 讲）。
type AgentRunner interface {
	// RunSub 启动一个匿名、一次性、上下文完全隔离的子智能体，返回它最终的纯文本汇报。
	RunSub(ctx context.Context, taskPrompt string, registry Registry) (string, error)
}

// SubagentTool 让主 Agent 把"脏活累活"外包给一个拥有纯净上下文的子智能体。
type SubagentTool struct {
	runner AgentRunner
	// 为子智能体准备的受限注册表（限制爆炸半径）
	registry Registry
}

func NewSubagentTool(runner AgentRunner, readOnlyRegistry Registry) *SubagentTool {
	return &SubagentTool{runner: runner, registry: readOnlyRegistry}
}

// SubagentToolName 子智能体委派工具名。它只做编排（等待子循环），引擎的全局并发信号量不为它占位，
// 否则 5 个并行子 Agent 占满槽位后，它们自己的叶子工具永远拿不到槽位而死锁。
const SubagentToolName = "spawn_subagent"

func (t *SubagentTool) Name() string { return SubagentToolName }

// ReadOnly 子智能体只拿到只读工具，因此多个 spawn_subagent 可以被引擎并发执行
// （第 17 讲思考题："多路侦察兵并行出发"）。
func (t *SubagentTool) ReadOnly() bool { return true }

func (t *SubagentTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "派出一个只读子智能体探索工作区并返回发现；适合可隔离、只读、可并行的调查。不要委派写文件、审批或需要共享中间状态的任务；它只能使用受限只读工具。可以在一次回复中并行派出多个不同方向的探索。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task_prompt": map[string]interface{}{"type": "string", "description": "给子智能体下达的明确指令（目标、范围、需要汇报什么）。"},
			},
			"required": []string{"task_prompt"},
		},
	}
}

type subagentArgs struct {
	TaskPrompt string `json:"task_prompt"`
}

func (t *SubagentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input subagentArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if input.TaskPrompt == "" {
		return "", fmt.Errorf("task_prompt 不能为空")
	}
	log.Printf("[Subagent] 🚀 主 Agent 发起委派！正在拉起探路者: [%s]", textutil.Truncate(input.TaskPrompt, 200, "..."))
	summary, err := t.runner.RunSub(ctx, input.TaskPrompt, t.registry)
	if err != nil {
		return "", fmt.Errorf("子智能体执行失败: %w", err)
	}
	log.Printf("[Subagent] ✅ 子智能体任务结束，报告返回给主干。")
	return "【子智能体探索报告】:\n" + summary, nil
}
