package schema

import "encoding/json"

// Role identifies one entry in the provider-neutral conversation timeline.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the provider-neutral unit exchanged by the engine.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ToolCall is an action requested by a model. Arguments remain raw until the
// registry validates and dispatches them.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolRisk string

const (
	RiskRead     ToolRisk = "read"
	RiskWrite    ToolRisk = "write"
	RiskExecute  ToolRisk = "execute"
	RiskExternal ToolRisk = "external"
	RiskState    ToolRisk = "state"
)

// ToolDefinition is exposed to providers and also drives registry policy.
type ToolDefinition struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	Risk         ToolRisk       `json:"risk"`
	ParallelSafe bool           `json:"parallel_safe"`
}

// ToolResult is always converted to a tool observation. Fatal results stop the
// loop after the observation is recorded.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
	ErrorCode  string `json:"error_code,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
	Fatal      bool   `json:"fatal,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
}

// Usage is provider-reported token usage. Providers that cannot report usage
// leave the fields at zero.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}

func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
}

// Generation is one normalized provider response.
type Generation struct {
	Message Message `json:"message"`
	Usage   Usage   `json:"usage"`
}

type TerminalReason string

const (
	TerminalCompleted        TerminalReason = "completed"
	TerminalMaxTurns         TerminalReason = "max_turns"
	TerminalAborted          TerminalReason = "aborted"
	TerminalTimeout          TerminalReason = "timeout"
	TerminalPermissionDenied TerminalReason = "permission_denied"
	TerminalBudgetExceeded   TerminalReason = "budget_exceeded"
	TerminalFatalToolError   TerminalReason = "fatal_tool_error"
	TerminalProviderError    TerminalReason = "provider_error"
	TerminalPersistenceError TerminalReason = "persistence_error"
	TerminalApprovalRequired TerminalReason = "approval_required"
)
