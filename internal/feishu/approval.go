package feishu

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/permission"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// RequestApproval 让 FeishuReporter 成为 permission.Approver：把审批请求发到触发任务的那个会话，
// 并挂起当前工具协程，直到该会话里有人回复 approve/reject（第 16 讲 Human-in-the-loop）。
func (r *FeishuReporter) RequestApproval(ctx context.Context, req permission.ApprovalRequest) (bool, string) {
	if r.approvals == nil {
		return false, "飞书审批通道未启用，已拒绝高危操作。"
	}
	taskID := permission.NewTaskID()
	return r.approvals.Wait(ctx, taskID, "feishu:"+r.chatID, func() {
		r.Send(fmt.Sprintf("⚠️ 高危操作审批请求\nAgent 试图执行以下动作:\n- 工具: %s\n- 参数: %s\n- 命中规则: %s\n任务 ID: %s\n👉 请回复 \"approve %s\" 或 \"reject %s\" 来决定是否放行。",
			req.ToolName, textutil.Truncate(req.Args, 800, "..."), req.Rule, taskID, taskID, taskID))
	})
}

var _ permission.Approver = (*FeishuReporter)(nil)

var taskIDRe = regexp.MustCompile(`^[0-9a-f]{6}$`)

// ApprovalCommand 拦截 approve/reject（以及中文 同意/拒绝）口令，唤醒挂起的引擎协程。
func ApprovalCommand(m *permission.ApprovalManager, sender Sender) CommandFunc {
	return func(ctx context.Context, chatID, text string) bool {
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return false
		}
		var allowed bool
		switch strings.ToLower(fields[0]) {
		case "approve", "同意", "批准":
			allowed = true
		case "reject", "拒绝", "驳回":
			allowed = false
		default:
			return false
		}
		taskID := fields[1]
		if !taskIDRe.MatchString(taskID) {
			return false // "同意 这个方案" 之类的普通对话照常交给 Agent
		}
		reason := "人类管理员已批准操作"
		if !allowed {
			reason = "人类管理员认为该操作存在极高风险，已拒绝执行。请换一种更安全的方式，或向用户说明原因。"
		}
		if err := m.Resolve(taskID, "feishu:"+chatID, allowed, reason); err != nil {
			_ = sender.SendText(ctx, chatID, "⚠️ "+err.Error())
		} else if allowed {
			_ = sender.SendText(ctx, chatID, "✅ 已批准任务 "+taskID)
		} else {
			_ = sender.SendText(ctx, chatID, "🚫 已拒绝任务 "+taskID)
		}
		return true
	}
}
