package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

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
	// ShowThinking 为 true 时把慢思考内容也打印出来（通过 OnThinkingContent，阶段作业要求展示 Thinking 输出）
	ShowThinking bool
	// ApprovalTimeout 终端审批的等待上限（默认 10 分钟，与 HTTP/飞书一致）；超时视为拒绝
	ApprovalTimeout time.Duration

	approveMu sync.Mutex // 并行子 Agent 可能同时请求审批：串行提问，避免抢同一行输入
	readOnce  sync.Once
	lines     chan string
}

// DefaultTerminalApprovalTimeout 终端审批默认等待上限。
const DefaultTerminalApprovalTimeout = 10 * time.Minute

// OnThinkingContent 打印 Phase 1 的慢思考内容（ShowThinking 关闭时静默）。
func (r *TerminalReporter) OnThinkingContent(ctx context.Context, content string) {
	if !r.ShowThinking || content == "" {
		return
	}
	fmt.Fprintf(r.Out, "\n[🧠 Thinking]\n%s\n", content)
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
	r.approveMu.Lock()
	defer r.approveMu.Unlock()
	r.readOnce.Do(func() {
		r.lines = make(chan string, 8)
		go func() { // 单一读取协程：超时/取消后迟到的输入不会被下一次审批误用（提问前会先清空）
			br := bufio.NewReader(in)
			for {
				line, err := br.ReadString('\n')
				if line != "" {
					r.lines <- line
				}
				if err != nil {
					close(r.lines)
					return
				}
			}
		}()
	})
	for drained := false; !drained; { // 丢弃上一次审批超时后才到达的陈旧输入
		select {
		case _, ok := <-r.lines:
			if !ok {
				return false, "终端输入已关闭，高危操作已拒绝。"
			}
		default:
			drained = true
		}
	}
	timeout := r.ApprovalTimeout
	if timeout <= 0 {
		timeout = DefaultTerminalApprovalTimeout
	}
	fmt.Fprintf(r.Out, "\n\033[31m[需要审批]\033[0m 工具 %s 命中高危规则 %s\n  参数: %s\n是否放行? [y/N]（%s 内未答复视为拒绝）: ",
		req.ToolName, req.Rule, textutil.Truncate(req.Args, 300, "..."), timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line, ok := <-r.lines:
		if !ok {
			return false, "终端输入已关闭，高危操作已拒绝。"
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
			return true, ""
		}
		return false, "人类操作员在终端拒绝了该操作。"
	case <-timer.C:
		fmt.Fprintln(r.Out, "\n[审批超时] 已自动拒绝。")
		return false, "终端审批超时未答复，高危操作已自动拒绝。"
	case <-ctx.Done():
		return false, "任务已取消，审批作废：" + ctx.Err().Error()
	}
}

var _ permission.Approver = (*TerminalReporter)(nil)
