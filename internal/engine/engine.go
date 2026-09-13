package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

const defaultSystemPrompt = `You are 01agent, a careful coding assistant operating inside one workspace.
Use tools when facts from the workspace are required. Treat tool errors as observations and correct recoverable mistakes.
Do not repeat an equivalent tool call after it has already returned the needed result. When the task is complete, respond with a concise final answer and no tool calls.`

type Config struct {
	WorkDir         string
	SystemPrompt    string
	EnableThinking  bool
	MaxTurns        int
	MaxTokens       int64
	MaxRepeatedCall int
	Timeout         time.Duration
	OnEvent         func(Event)
}

type EventType string

const (
	EventTurnStarted EventType = "turn_started"
	EventThinking    EventType = "thinking"
	EventAssistant   EventType = "assistant"
	EventToolStarted EventType = "tool_started"
	EventToolResult  EventType = "tool_result"
)

type Event struct {
	Type       EventType
	Turn       int
	Message    schema.Message
	ToolCall   schema.ToolCall
	ToolResult schema.ToolResult
}

type RunResult struct {
	Reason       schema.TerminalReason `json:"reason"`
	FinalMessage schema.Message        `json:"final_message"`
	Messages     []schema.Message      `json:"messages"`
	Turns        int                   `json:"turns"`
	Usage        schema.Usage          `json:"usage"`
}

type AgentEngine struct {
	provider provider.LLMProvider
	registry tools.Registry
	config   Config
}

func New(model provider.LLMProvider, registry tools.Registry, config Config) (*AgentEngine, error) {
	if model == nil {
		return nil, errors.New("engine: provider is nil")
	}
	if registry == nil {
		return nil, errors.New("engine: registry is nil")
	}
	if strings.TrimSpace(config.WorkDir) == "" {
		return nil, errors.New("engine: workdir is required")
	}
	absolute, err := filepath.Abs(config.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("engine: resolve workdir: %w", err)
	}
	config.WorkDir = filepath.Clean(absolute)
	if strings.TrimSpace(config.SystemPrompt) == "" {
		config.SystemPrompt = defaultSystemPrompt
	}
	if config.MaxTurns <= 0 {
		config.MaxTurns = 32
	}
	if config.MaxRepeatedCall <= 0 {
		config.MaxRepeatedCall = 3
	}
	return &AgentEngine{provider: model, registry: registry, config: config}, nil
}

func (e *AgentEngine) Run(parent context.Context, userPrompt string) (RunResult, error) {
	if strings.TrimSpace(userPrompt) == "" {
		return RunResult{}, errors.New("engine: user prompt is empty")
	}
	ctx := parent
	cancel := func() {}
	if e.config.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, e.config.Timeout)
	}
	defer cancel()

	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: e.config.SystemPrompt + "\n\nWorkspace: " + e.config.WorkDir},
		{Role: schema.RoleUser, Content: userPrompt},
	}
	result := RunResult{Messages: messages}
	repeatedCalls := make(map[[32]byte]int)

	for turn := 1; turn <= e.config.MaxTurns; turn++ {
		result.Turns = turn
		if reason := contextReason(ctx); reason != "" {
			result.Reason = reason
			result.Messages = messages
			return result, ctx.Err()
		}
		e.emit(Event{Type: EventTurnStarted, Turn: turn})

		if e.config.EnableThinking {
			thinking, err := e.provider.Generate(ctx, messages, nil)
			if err != nil {
				return e.providerFailure(result, messages, err)
			}
			result.Usage.Add(thinking.Usage)
			if e.overBudget(result.Usage) {
				result.Reason = schema.TerminalBudgetExceeded
				result.Messages = messages
				return result, nil
			}
			if len(thinking.Message.ToolCalls) > 0 {
				result.Reason = schema.TerminalProviderError
				result.Messages = messages
				return result, errors.New("engine: thinking phase returned tool calls although no tools were provided")
			}
			if thinking.Message.Content != "" {
				thinking.Message.Role = schema.RoleAssistant
				messages = append(messages, thinking.Message)
				e.emit(Event{Type: EventThinking, Turn: turn, Message: thinking.Message})
			}
		}

		generation, err := e.provider.Generate(ctx, messages, e.registry.GetAvailableTools())
		if err != nil {
			return e.providerFailure(result, messages, err)
		}
		generation.Message.Role = schema.RoleAssistant
		messages = append(messages, generation.Message)
		result.Usage.Add(generation.Usage)
		e.emit(Event{Type: EventAssistant, Turn: turn, Message: generation.Message})

		if e.overBudget(result.Usage) {
			result.Reason = schema.TerminalBudgetExceeded
			result.Messages = messages
			return result, nil
		}
		if len(generation.Message.ToolCalls) == 0 {
			result.Reason = schema.TerminalCompleted
			result.FinalMessage = generation.Message
			result.Messages = messages
			return result, nil
		}

		for _, call := range generation.Message.ToolCalls {
			fingerprint := toolFingerprint(call)
			repeatedCalls[fingerprint]++
			if repeatedCalls[fingerprint] > e.config.MaxRepeatedCall {
				observation := schema.Message{
					Role:       schema.RoleTool,
					ToolCallID: call.ID,
					Content:    fmt.Sprintf("Error [repeated_call]: equivalent call repeated more than %d times", e.config.MaxRepeatedCall),
					IsError:    true,
				}
				messages = append(messages, observation)
				result.Reason = schema.TerminalFatalToolError
				result.Messages = messages
				return result, nil
			}
			e.emit(Event{Type: EventToolStarted, Turn: turn, ToolCall: call})
		}

		toolResults := e.registry.ExecuteBatch(ctx, generation.Message.ToolCalls)
		for _, toolResult := range toolResults {
			messages = append(messages, schema.Message{
				Role:       schema.RoleTool,
				Content:    toolResult.Output,
				ToolCallID: toolResult.ToolCallID,
				IsError:    toolResult.IsError,
			})
			e.emit(Event{Type: EventToolResult, Turn: turn, ToolResult: toolResult})
			if toolResult.Fatal {
				result.Reason = schema.TerminalFatalToolError
				switch toolResult.ErrorCode {
				case "permission_denied":
					result.Reason = schema.TerminalPermissionDenied
				case "timeout":
					result.Reason = schema.TerminalTimeout
				case "cancelled":
					result.Reason = schema.TerminalAborted
				}
				if reason := contextReason(ctx); reason != "" {
					result.Reason = reason
				}
				result.Messages = messages
				return result, nil
			}
		}
	}

	result.Reason = schema.TerminalMaxTurns
	result.Messages = messages
	return result, nil
}

func (e *AgentEngine) providerFailure(result RunResult, messages []schema.Message, err error) (RunResult, error) {
	result.Messages = messages
	result.Reason = schema.TerminalProviderError
	if reason := contextReasonFromError(err); reason != "" {
		result.Reason = reason
	}
	return result, err
}

func (e *AgentEngine) overBudget(usage schema.Usage) bool {
	return e.config.MaxTokens > 0 && usage.TotalTokens() > e.config.MaxTokens
}

func (e *AgentEngine) emit(event Event) {
	if e.config.OnEvent != nil {
		e.config.OnEvent(event)
	}
}

func toolFingerprint(call schema.ToolCall) [32]byte {
	arguments := call.Arguments
	var value any
	if json.Unmarshal(call.Arguments, &value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			arguments = canonical
		}
	}
	return sha256.Sum256(append([]byte(call.Name+"\x00"), arguments...))
}

func contextReason(ctx context.Context) schema.TerminalReason {
	return contextReasonFromError(ctx.Err())
}

func contextReasonFromError(err error) schema.TerminalReason {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return schema.TerminalTimeout
	case errors.Is(err, context.Canceled):
		return schema.TerminalAborted
	default:
		return ""
	}
}
