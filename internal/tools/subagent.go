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

func (t *SubagentTool) Name() string { return "spawn_subagent" }

// ReadOnly 子智能体只拿到只读工具，因此多个 spawn_subagent 可以被引擎并发执行
// （第 17 讲思考题："多路侦察兵并行出发"）。
func (t *SubagentTool) ReadOnly() bool { return true }

func (t *SubagentTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "派出一个专门用于深度探索（Exploration）的子智能体。当你需要阅读大量代码、跨文件查找逻辑时请调用此工具。它只有只读能力，探索完毕后会给你返回一份极度精炼的摘要报告。可以在一次回复中同时派出多个子智能体并行探索不同方向。",
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
