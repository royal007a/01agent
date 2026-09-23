package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/runctx"
	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
	"github.com/weberzhao/go-tiny-claw/internal/tools"
)

// MaxSubTurns 子智能体最多跑多少个 Turn，防止它自己卡死。
const MaxSubTurns = 10

const explorerPrompt = `你是一个专门负责深度探索的探路者 (Explorer Subagent)。
你的任务是根据主架构师的指令，在当前工作区内仔细阅读代码、查阅日志，搜集足够的信息。

【核心纪律】
1. 你必须、且只能依靠内置工具（如 bash 的 find/grep，或 read_file）去寻找答案。绝对不允许凭空捏造或猜测！
2. 你是只读的：不要尝试修改任何文件。
3. 如果你没有找到确切的答案，你必须继续使用工具深入搜索。
4. 当且仅当你找到了确切的线索后，停止调用工具，直接输出一段精炼的纯文本作为你的终极汇报（包含关键结论、证据所在的文件路径）。主架构师会根据你的汇报来做下一步决策。`

// prefixReporter 给子智能体的事件打上 [Subagent] 标记，让用户看到它在干嘛。
type prefixReporter struct {
	Reporter
	prefix string
}

func (p prefixReporter) OnToolCall(ctx context.Context, name, args string) {
	p.Reporter.OnToolCall(ctx, p.prefix+name, args)
}
func (p prefixReporter) OnToolResult(ctx context.Context, name, result string, isErr bool) {
	p.Reporter.OnToolResult(ctx, p.prefix+name, result, isErr)
}

// RequestApproval 子智能体触发的高危操作同样交给父 Reporter 的审批通道（若有）。
func (p prefixReporter) RequestApproval(ctx context.Context, req permission.ApprovalRequest) (bool, string) {
	if a, ok := p.Reporter.(permission.Approver); ok {
		req.ToolName = p.prefix + req.ToolName
		return a.RequestApproval(ctx, req)
	}
	return false, "子智能体没有可用的审批通道，高危操作已拒绝。"
}

func (p prefixReporter) OnThinking(context.Context)        {}
func (p prefixReporter) OnMessage(context.Context, string) {} // 子智能体的汇报只交给主 Agent

// RunSub 是专为 Subagent 拉起的一次性受限循环：不依赖 Session，打完就走（实现 tools.AgentRunner）。
//
// 与课程的差异：Reporter 从 ctx 中取（课程在构造工具时固定一个 Reporter，
// 飞书多会话场景下子智能体的进度会发错群）；超过轮数时返回已有的部分发现而不是只丢一个错误。
func (e *AgentEngine) RunSub(ctx context.Context, taskPrompt string, registry tools.Registry) (string, error) {
	parent, _ := runctx.Reporter(ctx).(Reporter)
	if parent == nil {
		parent = NopReporter{}
	}
	reporter := prefixReporter{Reporter: parent, prefix: "[Subagent] "}
	ctx = runctx.WithReporter(ctx, reporter)

	history := []schema.Message{
		{Role: schema.RoleSystem, Content: explorerPrompt},
		{Role: schema.RoleUser, Content: taskPrompt},
	}
	var lastText string
	for turn := 1; turn <= MaxSubTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// 子任务要求急速响应：不做慢思考，直接预测行动；只能看到受限注册表
		resp, err := e.provider.Generate(ctx, e.compactor.Compact(history), registry.GetAvailableTools())
		if err != nil {
			return "", fmt.Errorf("子智能体推理失败: %w", err)
		}
		history = append(history, *resp)
		if resp.Content != "" {
			lastText = resp.Content
		}
		if len(resp.ToolCalls) == 0 {
			log.Printf("[Subagent] 第 %d 轮完成汇报: %s", turn, textutil.Truncate(resp.Content, 200, "..."))
			return resp.Content, nil
		}
		history = append(history, e.executeToolsWith(ctx, registry, resp.ToolCalls, reporter)...)
	}
	return fmt.Sprintf("（子智能体探索超过 %d 轮被强制召回，以下是它最后的阶段性发现，请主 Agent 给出更明确的指令后再派出）\n%s", MaxSubTurns, lastText), nil
}

var _ tools.AgentRunner = (*AgentEngine)(nil)
