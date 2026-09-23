package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// TerminalReporter 在终端直观地打印 Agent 的状态（第 10 讲）。
type TerminalReporter struct {
	Out io.Writer
	// ShowThinking 为 true 时把慢思考内容也打印出来（通过 OnThinkingContent）
	ShowThinking bool
}

func NewTerminalReporter() *TerminalReporter { return &TerminalReporter{Out: os.Stdout} }

func (r *TerminalReporter) OnThinking(ctx context.Context) {
	fmt.Fprintf(r.Out, "\n[🤔 思考中] 模型正在推理...\n")
}

func (r *TerminalReporter) OnToolCall(ctx context.Context, toolName string, args string) {
	fmt.Fprintf(r.Out, "[🛠️ 调用工具] %s\n", toolName)
	display := strings.NewReplacer("\n", "\\n", "\r", "\\r").Replace(args)
	fmt.Fprintf(r.Out, "  参数: %s\n", textutil.Truncate(display, 150, "... (已截断)"))
}

func (r *TerminalReporter) OnToolResult(ctx context.Context, toolName string, result string, isError bool) {
	if isError {
		fmt.Fprintf(r.Out, "[❌ 执行失败] %s\n", toolName)
		if result != "" {
			fmt.Fprintf(r.Out, "  错误: %s\n", result)
		}
		return
	}
	fmt.Fprintf(r.Out, "[✅ 执行成功] %s\n", toolName)
}

func (r *TerminalReporter) OnMessage(ctx context.Context, content string) {
	if content == "" {
		return
	}
	fmt.Fprintf(r.Out, "\n🤖 Agent 回复:\n%s\n\n", content)
}
