package provider

import (
	"fmt"
	"strings"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// flattenToolHistory 把带有工具调用/工具结果的历史改写为纯文本对话。
//
// 慢思考阶段（第 03 讲）会以 availableTools=nil 发起请求，但历史里可能已经存在
// tool_use / tool_result 块。Anthropic 协议在这种情况下会直接拒绝请求
// （"Requests which include tool_use or tool_result blocks must define tools"），
// 部分 OpenAI 兼容端点同样如此。把它们降级成文本既保留了逻辑链，
// 又真正做到"不给模型看任何工具 Schema"。
func flattenToolHistory(msgs []schema.Message) []schema.Message {
	out := make([]schema.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.IsToolResult():
			status := "成功"
			if m.IsError {
				status = "失败"
			}
			out = append(out, schema.Message{
				Role:    schema.RoleUser,
				Content: fmt.Sprintf("[工具结果 %s | %s]\n%s", m.ToolCallID, status, m.Content),
			})
		case m.Role == schema.RoleAssistant && len(m.ToolCalls) > 0:
			var b strings.Builder
			b.WriteString(m.Content)
			for _, tc := range m.ToolCalls {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				fmt.Fprintf(&b, "[已调用工具 %s | id=%s] 参数: %s", tc.Name, tc.ID, string(tc.Arguments))
			}
			out = append(out, schema.Message{Role: schema.RoleAssistant, Content: b.String()})
		default:
			m.ToolCalls = nil
			out = append(out, m)
		}
	}
	return out
}

// hasToolBlocks 判断历史中是否存在工具调用痕迹。
func hasToolBlocks(msgs []schema.Message) bool {
	for _, m := range msgs {
		if m.IsToolResult() || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// normalizeArgs 保证工具参数是合法 JSON（部分端点对无参工具返回空串）。
func normalizeArgs(s string) []byte {
	if strings.TrimSpace(s) == "" {
		return []byte("{}")
	}
	return []byte(s)
}
