package context

import "github.com/weberzhao/go-tiny-claw/internal/schema"

// InterruptedToolOutput 为"有调用、没结果"的工具补上的占位 Observation。
const InterruptedToolOutput = "[系统] 该工具调用在执行完成前被中断（进程重启/任务取消），没有结果。如仍需要，请重新调用。"

// RepairToolPairs 修复工具调用与结果的配对，保证发给模型 API 的序列合法：
//   - assistant 的每个 ToolCall 之后都必须紧跟对应的 tool 结果；缺失的（进程在工具执行中途
//     崩溃/重启、审批等待时被 kill）补一条占位结果；
//   - 找不到发起者的孤儿 tool 结果直接丢弃。
//
// 否则 Session 持久化后，一次中途重启就会让该会话之后的每次请求都被 OpenAI/Anthropic 拒绝。
func RepairToolPairs(msgs []schema.Message) []schema.Message {
	out := make([]schema.Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.IsToolResult() {
			continue // 能走到这里的都是孤儿（正常结果在下面的内层循环里被消费）
		}
		out = append(out, m)
		if m.Role != schema.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		results := map[string]schema.Message{}
		j := i + 1
		for ; j < len(msgs) && msgs[j].IsToolResult(); j++ {
			results[msgs[j].ToolCallID] = msgs[j]
		}
		for _, tc := range m.ToolCalls {
			if r, ok := results[tc.ID]; ok {
				out = append(out, r)
			} else {
				out = append(out, schema.Message{Role: schema.RoleUser, ToolCallID: tc.ID, Content: InterruptedToolOutput, IsError: true})
			}
		}
		i = j - 1
	}
	return out
}
