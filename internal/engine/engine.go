package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	RunID           string
	Store           RunStore
	Compactor       ContextCompactor
}

type EventType string

const (
	EventTurnStarted EventType = "turn_started"
	EventRunStarted  EventType = "run_started"
	EventThinking    EventType = "thinking"
	EventAssistant   EventType = "assistant"
	EventToolStarted EventType = "tool_started"
	EventToolResult  EventType = "tool_result"
	EventCompacted   EventType = "context_compacted"
	EventCheckpoint  EventType = "checkpoint"
	EventCompleted   EventType = "completed"
)

type Event struct {
	Type       EventType         `json:"type"`
	RunID      string            `json:"run_id"`
	Sequence   int               `json:"sequence"`
	Timestamp  time.Time         `json:"timestamp"`
	Turn       int               `json:"turn,omitempty"`
	Message    schema.Message    `json:"message,omitempty"`
	ToolCall   schema.ToolCall   `json:"tool_call,omitempty"`
	ToolResult schema.ToolResult `json:"tool_result,omitempty"`
	Messages   []schema.Message  `json:"messages,omitempty"`
	Usage      schema.Usage      `json:"usage"`
	Metadata   map[string]any    `json:"metadata,omitempty"`
}

type Checkpoint struct {
	Version       int                   `json:"version"`
	RunID         string                `json:"run_id"`
	Prompt        string                `json:"prompt"`
	WorkDir       string                `json:"work_dir"`
	Messages      []schema.Message      `json:"messages"`
	Turn          int                   `json:"turn"`
	Usage         schema.Usage          `json:"usage"`
	Reason        schema.TerminalReason `json:"reason,omitempty"`
	RepeatedCalls map[string]int        `json:"repeated_calls,omitempty"`
	Sequence      int                   `json:"sequence"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

type RunStore interface {
	Record(context.Context, Event) error
	SaveCheckpoint(context.Context, Checkpoint) error
	Complete(context.Context, RunResult) error
	LoadCheckpoint(context.Context, string) (Checkpoint, error)
}

type ContextCompactor interface {
	Compact(context.Context, []schema.Message) ([]schema.Message, bool, error)
}

type RunResult struct {
	RunID        string                `json:"run_id"`
	Reason       schema.TerminalReason `json:"reason"`
	FinalMessage schema.Message        `json:"final_message"`
	Messages     []schema.Message      `json:"messages"`
	Turns        int                   `json:"turns"`
	Usage        schema.Usage          `json:"usage"`
	StartedAt    time.Time             `json:"started_at"`
	CompletedAt  time.Time             `json:"completed_at"`
	DurationMS   int64                 `json:"duration_ms"`
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
	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: e.config.SystemPrompt + "\n\nWorkspace: " + e.config.WorkDir},
		{Role: schema.RoleUser, Content: userPrompt},
	}
	return e.run(parent, userPrompt, messages, 0, 0, schema.Usage{}, nil)
}

func (e *AgentEngine) Resume(parent context.Context, checkpoint Checkpoint) (RunResult, error) {
	if checkpoint.Version != 1 || checkpoint.RunID == "" || checkpoint.Prompt == "" || checkpoint.WorkDir == "" || len(checkpoint.Messages) < 2 {
		return RunResult{}, errors.New("engine: invalid checkpoint")
	}
	if filepath.Clean(checkpoint.WorkDir) != e.config.WorkDir {
		return RunResult{}, fmt.Errorf("engine: checkpoint workspace %q does not match %q", checkpoint.WorkDir, e.config.WorkDir)
	}
	if checkpoint.Reason == schema.TerminalCompleted {
		return RunResult{}, errors.New("engine: completed checkpoint cannot be resumed")
	}
	e.config.RunID = checkpoint.RunID
	return e.run(parent, checkpoint.Prompt, append([]schema.Message(nil), checkpoint.Messages...), checkpoint.Turn, checkpoint.Sequence, checkpoint.Usage, checkpoint.RepeatedCalls)
}

func (e *AgentEngine) run(parent context.Context, userPrompt string, messages []schema.Message, completedTurns, completedSequence int, usage schema.Usage, restored map[string]int) (RunResult, error) {
	ctx := parent
	cancel := func() {}
	if e.config.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, e.config.Timeout)
	}
	defer cancel()

	runID := strings.TrimSpace(e.config.RunID)
	if runID == "" {
		runID = newRunID()
	}
	startedAt := time.Now().UTC()
	result := RunResult{RunID: runID, Messages: messages, Usage: usage, StartedAt: startedAt}
	repeatedCalls := make(map[[32]byte]int)
	for key, count := range restored {
		decoded, err := hex.DecodeString(key)
		if err == nil && len(decoded) == sha256.Size {
			var fingerprint [32]byte
			copy(fingerprint[:], decoded)
			repeatedCalls[fingerprint] = count
		}
	}
	sequence := completedSequence
	emit := func(event Event) error {
		sequence++
		event.RunID = runID
		event.Sequence = sequence
		event.Timestamp = time.Now().UTC()
		event.Messages = append([]schema.Message(nil), messages...)
		event.Usage = result.Usage
		if e.config.OnEvent != nil {
			e.config.OnEvent(event)
		}
		if e.config.Store != nil {
			if err := e.config.Store.Record(ctx, event); err != nil {
				return fmt.Errorf("record trace: %w", err)
			}
		}
		return nil
	}
	save := func(turn int) error {
		if e.config.Store == nil {
			return nil
		}
		encoded := make(map[string]int, len(repeatedCalls))
		for fingerprint, count := range repeatedCalls {
			encoded[fmt.Sprintf("%x", fingerprint)] = count
		}
		return e.config.Store.SaveCheckpoint(ctx, Checkpoint{
			Version: 1, RunID: runID, Prompt: userPrompt, WorkDir: e.config.WorkDir,
			Messages: append([]schema.Message(nil), messages...), Turn: turn,
			Usage: result.Usage, Reason: result.Reason, RepeatedCalls: encoded, Sequence: sequence, UpdatedAt: time.Now().UTC(),
		})
	}
	finish := func(reason schema.TerminalReason, err error) (RunResult, error) {
		result.Reason = reason
		result.Messages = append([]schema.Message(nil), messages...)
		result.CompletedAt = time.Now().UTC()
		result.DurationMS = result.CompletedAt.Sub(startedAt).Milliseconds()
		if storeErr := save(result.Turns); storeErr != nil && err == nil {
			result.Reason = schema.TerminalPersistenceError
			err = storeErr
		}
		if emitErr := emit(Event{Type: EventCompleted, Turn: result.Turns, Metadata: map[string]any{"reason": result.Reason}}); emitErr != nil && err == nil {
			result.Reason = schema.TerminalPersistenceError
			err = emitErr
		}
		if e.config.Store != nil {
			if storeErr := e.config.Store.Complete(context.WithoutCancel(parent), result); storeErr != nil && err == nil {
				result.Reason = schema.TerminalPersistenceError
				err = storeErr
			}
		}
		return result, err
	}
	if err := save(completedTurns); err != nil {
		return finish(schema.TerminalPersistenceError, err)
	}
	if err := emit(Event{Type: EventRunStarted, Turn: completedTurns, Metadata: map[string]any{"resumed": completedTurns > 0}}); err != nil {
		return finish(schema.TerminalPersistenceError, err)
	}

	for turn := completedTurns + 1; turn <= e.config.MaxTurns; turn++ {
		result.Turns = turn
		if reason := contextReason(ctx); reason != "" {
			return finish(reason, ctx.Err())
		}
		if err := emit(Event{Type: EventTurnStarted, Turn: turn}); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}

		if e.config.Compactor != nil {
			compacted, changed, err := e.config.Compactor.Compact(ctx, messages)
			if err != nil {
				return finish(schema.TerminalFatalToolError, fmt.Errorf("compact context: %w", err))
			}
			if changed {
				before := len(messages)
				messages = compacted
				if err := emit(Event{Type: EventCompacted, Turn: turn, Metadata: map[string]any{"messages_before": before, "messages_after": len(messages)}}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
			}
		}

		if e.config.EnableThinking {
			thinking, err := e.provider.Generate(ctx, messages, nil)
			if err != nil {
				reason := schema.TerminalProviderError
				if mapped := contextReasonFromError(err); mapped != "" {
					reason = mapped
				}
				return finish(reason, err)
			}
			result.Usage.Add(thinking.Usage)
			if e.overBudget(result.Usage) {
				return finish(schema.TerminalBudgetExceeded, nil)
			}
			if len(thinking.Message.ToolCalls) > 0 {
				return finish(schema.TerminalProviderError, errors.New("engine: thinking phase returned tool calls although no tools were provided"))
			}
			if thinking.Message.Content != "" {
				thinking.Message.Role = schema.RoleAssistant
				messages = append(messages, thinking.Message)
				if err := emit(Event{Type: EventThinking, Turn: turn, Message: thinking.Message}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
			}
		}

		generation, err := e.provider.Generate(ctx, messages, e.registry.GetAvailableTools())
		if err != nil {
			reason := schema.TerminalProviderError
			if mapped := contextReasonFromError(err); mapped != "" {
				reason = mapped
			}
			return finish(reason, err)
		}
		generation.Message.Role = schema.RoleAssistant
		messages = append(messages, generation.Message)
		result.Usage.Add(generation.Usage)
		if err := emit(Event{Type: EventAssistant, Turn: turn, Message: generation.Message}); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}

		if e.overBudget(result.Usage) {
			result.Reason = schema.TerminalBudgetExceeded
			result.Messages = messages
			return finish(schema.TerminalBudgetExceeded, nil)
		}
		if len(generation.Message.ToolCalls) == 0 {
			result.Reason = schema.TerminalCompleted
			result.FinalMessage = generation.Message
			result.Messages = messages
			return finish(schema.TerminalCompleted, nil)
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
				return finish(schema.TerminalFatalToolError, nil)
			}
			if err := emit(Event{Type: EventToolStarted, Turn: turn, ToolCall: call}); err != nil {
				return finish(schema.TerminalPersistenceError, err)
			}
		}

		toolResults := e.registry.ExecuteBatch(ctx, generation.Message.ToolCalls)
		for _, toolResult := range toolResults {
			messages = append(messages, schema.Message{
				Role:       schema.RoleTool,
				Content:    toolResult.Output,
				ToolCallID: toolResult.ToolCallID,
				IsError:    toolResult.IsError,
			})
			if err := emit(Event{Type: EventToolResult, Turn: turn, ToolResult: toolResult}); err != nil {
				return finish(schema.TerminalPersistenceError, err)
			}
			if toolResult.Fatal {
				result.Reason = schema.TerminalFatalToolError
				switch toolResult.ErrorCode {
				case "permission_denied":
					result.Reason = schema.TerminalPermissionDenied
				case "approval_required":
					result.Reason = schema.TerminalApprovalRequired
				case "timeout":
					result.Reason = schema.TerminalTimeout
				case "cancelled":
					result.Reason = schema.TerminalAborted
				}
				if reason := contextReason(ctx); reason != "" {
					result.Reason = reason
				}
				return finish(result.Reason, nil)
			}
		}
		if err := save(turn); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}
		if err := emit(Event{Type: EventCheckpoint, Turn: turn}); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}
	}

	return finish(schema.TerminalMaxTurns, nil)
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

func newRunID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return fmt.Sprintf("run-%x", value[:])
	}
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
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
