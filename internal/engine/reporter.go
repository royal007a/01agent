package engine

import "context"

// Reporter 定义了 Agent 引擎向外界输出信息的规范（第 09 讲 I/O 解耦）。
// 引擎只在生命周期节点"广播"事件，展示载体（终端、飞书、WebUI…）由注入方决定。
type Reporter interface {
	// OnThinking 当模型开始进行慢思考时调用
	OnThinking(ctx context.Context)
	// OnToolCall 当模型决定调用工具时调用
	OnToolCall(ctx context.Context, toolName string, args string)
	// OnToolResult 当工具执行完毕并返回结果时调用（result 已为人类展示做过缩略）
	OnToolResult(ctx context.Context, toolName string, result string, isError bool)
	// OnMessage 当模型输出纯文本（阶段性说明或最终回答）时调用
	OnMessage(ctx context.Context, content string)
}

// NopReporter 什么都不输出，用于测试或静默运行。
type NopReporter struct{}

func (NopReporter) OnThinking(context.Context)                         {}
func (NopReporter) OnToolCall(context.Context, string, string)         {}
func (NopReporter) OnToolResult(context.Context, string, string, bool) {}
func (NopReporter) OnMessage(context.Context, string)                  {}
