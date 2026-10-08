// Package schema 定义 go-tiny-claw 内部统一的"血液"：消息、工具调用与工具结果。
// Main Loop、Provider、Tools 之间只通过这里的类型交流，与任何厂商 SDK 解耦。
package schema

import "time"

import "encoding/json"

// Role 定义消息的角色。
type Role string

const (
	RoleSystem    Role = "system"    // 系统提示词：确立 Agent 的性格与红线
	RoleUser      Role = "user"      // 用户输入 / 工具执行的返回结果 (Observation)
	RoleAssistant Role = "assistant" // 模型输出：推理文本或工具调用
)

// Usage 记录单次大模型 API 调用的 Token 消耗（第 18 讲）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Message 代表上下文中传递的单条消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// 模型决定调用工具时填充（支持单轮并行多个）。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// 对某个工具调用的响应必须携带，维系推理链条。
	ToolCallID string `json:"tool_call_id,omitempty"`
	// IsError 标记工具结果是否失败（Claude 协议的 tool_result.is_error 会用到）。
	IsError bool `json:"is_error,omitempty"`
	// Usage 仅在 Assistant 消息上出现，由 Provider 填充。
	Usage *Usage `json:"usage,omitempty"`
	// Model 服务端在响应里回报的实际模型版本（环境锁定：检测同名模型静默滚动）。
	Model string `json:"model,omitempty"`
}

// IsToolResult 判断该消息是否是一条工具观察结果。
func (m Message) IsToolResult() bool {
	return m.Role == RoleUser && m.ToolCallID != ""
}

// ToolCall 代表模型请求调用某个具体工具。
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Arguments 延迟解析，解析责任交给具体工具。
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult 代表工具在本地执行完毕后返回的物理结果。
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
	// ExecDuration 工具在本地物理执行的耗时（不含审批、写锁等待）；Timed 表示 Registry 已测量过（第 18 讲）
	ExecDuration time.Duration `json:"-"`
	Timed        bool          `json:"-"`
}

// ToolDefinition 描述大模型可以调用的工具元信息。
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"` // JSON Schema
}
