package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// TerminalReporter 在终端直观地打印 Agent 的状态（第 10 讲）。
type TerminalReporter struct {
	Out io.Writer
	// In 为审批输入源；为 nil 时使用 os.Stdin（仅在交互式终端下才允许审批）
	In io.Reader
	// AutoApprove 为 true 时跳过人工审批（沙箱内 YOLO 场景）
	AutoApprove bool
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

// RequestApproval 让终端 Reporter 也具备 Human-in-the-loop：在 TTY 中询问 y/N。
func (r *TerminalReporter) RequestApproval(ctx context.Context, req permission.ApprovalRequest) (bool, string) {
	if r.AutoApprove {
		return true, ""
	}
	in := r.In
	if in == nil {
		if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false, "非交互式终端无法进行人工审批，高危操作已拒绝（可使用 -yes 在沙箱中放行）。"
		}
		in = os.Stdin
	}
	fmt.Fprintf(r.Out, "\n\033[31m[需要审批]\033[0m 工具 %s 命中高危规则 %s\n  参数: %s\n是否放行? [y/N]: ",
		req.ToolName, req.Rule, textutil.Truncate(req.Args, 300, "..."))
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return true, ""
	}
	return false, "人类操作员在终端拒绝了该操作。"
}

var _ permission.Approver = (*TerminalReporter)(nil)
