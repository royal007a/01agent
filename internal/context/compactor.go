package context

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
	"github.com/weberzhao/go-tiny-claw/internal/textutil"
)

// Compactor 负责监控和压缩上下文内存，防止大模型发生 OOM（第 12 讲）。
// 策略：远期历史 Observation Masking + 短期保护区 Head-Tail Truncation。
type Compactor struct {
	MaxChars       int // 触发压缩的水位线（字符/字节数估算）
	RetainLastMsgs int // Working Memory 保护区：最近 N 条消息
	// 以下阈值课程中是硬编码常量，这里开放出来便于测试调优
	MaskAbove int // 远期工具输出超过该长度即掩码（默认 200）
	KeepHead  int // 保护区单条保留头部（默认 500）
	KeepTail  int // 保护区单条保留尾部（默认 500）
}

func NewCompactor(maxChars, retainLastMsgs int) *Compactor {
	return &Compactor{MaxChars: maxChars, RetainLastMsgs: retainLastMsgs, MaskAbove: 200, KeepHead: 500, KeepTail: 500}
}

// Compact 若总长度超过水位线，对远期历史全量掩码、对保护区超长内容掐头去尾。
// 返回新切片，不修改入参（Session 里的全量数据永远不受影响）。
func (c *Compactor) Compact(msgs []schema.Message) []schema.Message {
	current := EstimateLength(msgs)
	if current < c.MaxChars {
		return msgs
	}
	log.Printf("[Compactor] ⚠️ 内存告警：当前上下文长度 (%d) 超过阈值 (%d)，触发压缩清理...", current, c.MaxChars)

	protectStart := len(msgs) - c.RetainLastMsgs
	if protectStart < 0 {
		protectStart = 0
	}
	compacted := make([]schema.Message, 0, len(msgs))
	for i, msg := range msgs {
		if msg.Role == schema.RoleSystem { // System Prompt 神圣不可侵犯
			compacted = append(compacted, msg)
			continue
		}
		newMsg := msg
		inWorkingMemory := i >= protectStart
		switch {
		case msg.IsToolResult():
			if !inWorkingMemory {
				// 第一道防线：远期工具输出无情替换，但 ToolCallID 保留，逻辑链不断
				if len(msg.Content) > c.MaskAbove {
					newMsg.Content = fmt.Sprintf("...[为了节省内存，早期的工具输出已被系统强制清理。原始长度: %d 字节]...", len(msg.Content))
				}
			} else if keep := c.KeepHead + c.KeepTail; len(msg.Content) > keep {
				// 第二道防线：保护区内单条过长，掐头去尾
				head := textutil.Head(msg.Content, c.KeepHead)
				tail := textutil.Tail(msg.Content, c.KeepTail)
				newMsg.Content = fmt.Sprintf("%s\n\n...[内容过长，中间 %d 字节已被系统截断]...\n\n%s", head, len(msg.Content)-len(head)-len(tail), tail)
			}
		case msg.Role == schema.RoleAssistant && msg.Content != "":
			if !inWorkingMemory && len(msg.Content) > c.MaskAbove {
				newMsg.Content = "...[早期的推理思考过程已折叠]..."
			}
		}
		// ToolCalls 是模型行动的证据，ID/Name 绝不动；但远期超长参数（如 write_file 的大段 content）
		// 掩码掉长字符串值，保留 path 等短字段（第 12 讲作者答疑）。复制切片，不改写原始历史。
		if !inWorkingMemory && len(msg.ToolCalls) > 0 {
			calls := make([]schema.ToolCall, len(msg.ToolCalls))
			copy(calls, msg.ToolCalls)
			for j := range calls {
				if len(calls[j].Arguments) > maskArgsAbove {
					calls[j].Arguments = maskLongArgs(calls[j].Arguments)
				}
			}
			newMsg.ToolCalls = calls
		}
		compacted = append(compacted, newMsg)
	}
	log.Printf("[Compactor] ✅ 压缩完成。上下文长度从 %d 降至 %d。", current, EstimateLength(compacted))
	return compacted
}

// EstimateLength 粗略计算上下文总长度（字节数）。
func EstimateLength(msgs []schema.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments)
		}
	}
	return n
}

// maskArgsAbove 远期 ToolCall 参数超过该字节数时触发掩码（第 12 讲作者答疑：500 字以上）。
const maskArgsAbove = 500

// maskLongArgs 把 JSON 参数里超过 200 字节的字符串值替换为占位说明，其余字段原样保留；
// 解析失败时整体替换为合法 JSON 占位，保证发给模型 API 的参数始终可解析。
func maskLongArgs(raw json.RawMessage) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		b, _ := json.Marshal(map[string]string{"_masked": fmt.Sprintf("[早期工具参数已掩码，原长 %d 字节]", len(raw))})
		return b
	}
	for k, v := range obj {
		if s, ok := v.(string); ok && len(s) > 200 {
			obj[k] = fmt.Sprintf("...[早期参数已掩码，原长 %d 字节]...", len(s))
		}
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return b
}
