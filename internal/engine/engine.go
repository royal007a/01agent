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
	"sync"
	"time"

	promptcontext "github.com/royal007a/01agent/internal/prompt"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

const defaultSystemPrompt = `You are 01agent, a careful coding assistant operating inside one workspace.
Use tools when facts from the workspace are required. Treat tool errors as observations and correct recoverable mistakes.
Do not repeat an equivalent tool call after it has already returned the needed result. When the task is complete, respond with a concise final answer and no tool calls.`

const planModePrompt = `

# Plan mode
This run is a long-lived multi-step task. Externalize its objective and progress with read_plan and update_plan instead of relying on the context window.
At the beginning, call read_plan. If no plan exists, create one with update_plan using expected_revision 0. If it exists, continue from its first pending or in-progress step and respect its current canonical content.
After completing or blocking a step, immediately read the current revision and update the canonical plan with a new operation_id. Never reuse an operation_id for different content and never guess through a revision conflict.
The structured plan is authoritative; PLAN.md and TODO.md are human-readable projections. Thinking is still required for local decisions and is not replaced by the plan.`

type Config struct {
	WorkDir         string
	SystemPrompt    string
	EnableThinking  bool
	PlanMode        bool
	MaxTurns        int
	MaxTokens       int64
	MaxRepeatedCall int
	Timeout         time.Duration
	OnEvent         func(Event)
	RunID           string
	Store           RunStore
	Compactor       ContextCompactor
	InputQueue      InputQueue
	PromptComposer  promptcontext.Composer
	MemoryScope     string
}

type EventType string

const (
	EventTurnStarted  EventType = "turn_started"
	EventRunStarted   EventType = "run_started"
	EventCapability   EventType = "capability_snapshotted"
	EventThinking     EventType = "thinking"
	EventAssistant    EventType = "assistant"
	EventToolStarted  EventType = "tool_started"
	EventToolResult   EventType = "tool_result"
	EventRecoveryHint EventType = "recovery_hint"
	EventReminder     EventType = "system_reminder"
	EventCompacted    EventType = "context_compacted"
	EventCheckpoint   EventType = "checkpoint"
	EventCommitted    EventType = "history_committed"
	EventInputClaimed EventType = "input_claimed"
	EventInputAcked   EventType = "input_acknowledged"
	EventCompleted    EventType = "completed"
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
	Version              int                      `json:"version"`
	RunID                string                   `json:"run_id"`
	TurnID               string                   `json:"turn_id"`
	Admission            int                      `json:"admission"`
	LeaseID              string                   `json:"lease_id"`
	Prompt               string                   `json:"prompt"`
	WorkDir              string                   `json:"work_dir"`
	MemoryScope          string                   `json:"memory_scope,omitempty"`
	Capability           tools.CapabilityRevision `json:"capability"`
	Messages             []schema.Message         `json:"messages"`
	Turn                 int                      `json:"turn"`
	Usage                schema.Usage             `json:"usage"`
	Reason               schema.TerminalReason    `json:"reason,omitempty"`
	RepeatedCalls        map[string]int           `json:"repeated_calls,omitempty"`
	Sequence             int                      `json:"sequence"`
	OperationSequence    int                      `json:"operation_sequence"`
	HistoryRevision      int64                    `json:"history_revision"`
	LastOperationID      string                   `json:"last_operation_id,omitempty"`
	LastFingerprint      string                   `json:"last_fingerprint,omitempty"`
	CommittedInputClaims map[string]int64         `json:"committed_input_claims,omitempty"`
	UpdatedAt            time.Time                `json:"updated_at"`
}

type HistoryCommit struct {
	RunID            string     `json:"run_id"`
	OperationID      string     `json:"operation_id"`
	Fingerprint      string     `json:"fingerprint"`
	ExpectedRevision int64      `json:"expected_revision"`
	Checkpoint       Checkpoint `json:"checkpoint"`
}

type HistoryAck struct {
	RunID       string    `json:"run_id"`
	OperationID string    `json:"operation_id"`
	Fingerprint string    `json:"fingerprint"`
	Revision    int64     `json:"revision"`
	CommittedAt time.Time `json:"committed_at"`
}

type RunStore interface {
	Record(context.Context, Event) error
	CommitHistory(context.Context, HistoryCommit) (HistoryAck, error)
	Complete(context.Context, RunResult) error
	LoadCheckpoint(context.Context, string) (Checkpoint, error)
}

// EventSequenceReader lets a resumed admission continue after trace records
// that were durably appended after the last canonical checkpoint. Those
// records are observations, not committed history, but their sequence numbers
// must never be reused.
type EventSequenceReader interface {
	LastEventSequence(context.Context, string) (int, error)
}

type ContextCompactor interface {
	Compact(context.Context, []schema.Message) ([]schema.Message, bool, error)
}

type RunResult struct {
	RunID           string                   `json:"run_id"`
	TurnID          string                   `json:"turn_id"`
	Admission       int                      `json:"admission"`
	Capability      tools.CapabilityRevision `json:"capability"`
	HistoryRevision int64                    `json:"history_revision"`
	MemoryScope     string                   `json:"memory_scope,omitempty"`
	Reason          schema.TerminalReason    `json:"reason"`
	FinalMessage    schema.Message           `json:"final_message"`
	Messages        []schema.Message         `json:"messages"`
	Turns           int                      `json:"turns"`
	Usage           schema.Usage             `json:"usage"`
	StartedAt       time.Time                `json:"started_at"`
	CompletedAt     time.Time                `json:"completed_at"`
	DurationMS      int64                    `json:"duration_ms"`
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
	if config.InputQueue != nil && config.Store == nil {
		return nil, errors.New("engine: input queue requires a durable run store")
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
	if config.PlanMode {
		config.SystemPrompt = strings.TrimSpace(config.SystemPrompt) + planModePrompt
	}
	if config.PromptComposer == nil {
		config.PromptComposer = promptcontext.FilesystemComposer{}
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
	promptSnapshot, err := e.config.PromptComposer.Snapshot(parent, e.config.WorkDir, e.config.SystemPrompt)
	if err != nil {
		return RunResult{}, fmt.Errorf("engine: compose prompt: %w", err)
	}
	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: promptSnapshot.SystemPrompt},
		{Role: schema.RoleUser, Content: userPrompt},
	}
	return e.run(parent, userPrompt, messages, promptSnapshot, nil)
}

// RunWithHistory executes one new user turn on top of conversation state owned
// by a ConversationManager. The engine still owns only this query-loop run;
// callers remain responsible for serializing and durably committing sessions.
func (e *AgentEngine) RunWithHistory(parent context.Context, userPrompt string, history []schema.Message) (RunResult, error) {
	userPrompt = strings.TrimSpace(userPrompt)
	if userPrompt == "" {
		return RunResult{}, errors.New("engine: user prompt is empty")
	}
	if len(history) == 0 {
		return e.Run(parent, userPrompt)
	}
	if history[0].Role != schema.RoleSystem || strings.TrimSpace(history[0].Content) == "" {
		return RunResult{}, errors.New("engine: conversation history must start with a non-empty system message")
	}
	for index, message := range history[1:] {
		if message.Role == schema.RoleSystem {
			return RunResult{}, fmt.Errorf("engine: conversation history contains a system message at index %d", index+1)
		}
	}
	promptSnapshot, err := e.config.PromptComposer.Snapshot(parent, e.config.WorkDir, e.config.SystemPrompt)
	if err != nil {
		return RunResult{}, fmt.Errorf("engine: compose prompt: %w", err)
	}
	messages := append([]schema.Message(nil), history...)
	messages[0].Content = promptSnapshot.SystemPrompt
	messages = append(messages, schema.Message{Role: schema.RoleUser, Content: userPrompt})
	return e.run(parent, userPrompt, messages, promptSnapshot, nil)
}

func (e *AgentEngine) Resume(parent context.Context, checkpoint Checkpoint) (RunResult, error) {
	if checkpoint.Version != 2 || checkpoint.RunID == "" || checkpoint.TurnID == "" || checkpoint.Prompt == "" || checkpoint.WorkDir == "" || len(checkpoint.Messages) < 2 {
		return RunResult{}, errors.New("engine: invalid checkpoint")
	}
	if filepath.Clean(checkpoint.WorkDir) != e.config.WorkDir {
		return RunResult{}, fmt.Errorf("engine: checkpoint workspace %q does not match %q", checkpoint.WorkDir, e.config.WorkDir)
	}
	if checkpoint.Reason == schema.TerminalCompleted {
		return RunResult{}, errors.New("engine: completed checkpoint cannot be resumed")
	}
	promptSnapshot, err := e.config.PromptComposer.Snapshot(parent, e.config.WorkDir, e.config.SystemPrompt)
	if err != nil {
		return RunResult{}, fmt.Errorf("engine: compose prompt: %w", err)
	}
	e.config.RunID = checkpoint.RunID
	return e.run(parent, checkpoint.Prompt, append([]schema.Message(nil), checkpoint.Messages...), promptSnapshot, &checkpoint)
}

func (e *AgentEngine) run(parent context.Context, userPrompt string, messages []schema.Message, promptSnapshot promptcontext.Snapshot, restored *Checkpoint) (RunResult, error) {
	ctx := parent
	cancel := func() {}
	if e.config.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, e.config.Timeout)
	}
	defer cancel()
	ctx = promptcontext.WithSnapshot(ctx, promptSnapshot)

	runID := strings.TrimSpace(e.config.RunID)
	if runID == "" {
		runID = newRunID()
	}
	memoryScope := strings.TrimSpace(e.config.MemoryScope)
	if restored != nil && restored.MemoryScope != "" {
		if memoryScope != "" && memoryScope != restored.MemoryScope {
			return RunResult{}, fmt.Errorf("engine: checkpoint memory scope %q does not match %q", restored.MemoryScope, memoryScope)
		}
		memoryScope = restored.MemoryScope
	}
	if memoryScope == "" {
		memoryScope = runID
	}
	ctx = runtimecontext.WithMetadata(ctx, runtimecontext.Metadata{Scope: memoryScope, RunID: runID})
	runtime := e.registry.Snapshot()
	capability := combinedCapability(runtime.Revision(), promptSnapshot)
	turnID := newID("turn")
	admission := 1
	completedTurns := 0
	completedSequence := 0
	operationSequence := 0
	historyRevision := int64(0)
	usage := schema.Usage{}
	var repeatedState map[string]int
	committedInputClaims := make(map[string]int64)
	if restored != nil {
		if !restored.Capability.Equivalent(capability) {
			return RunResult{}, fmt.Errorf("engine: capability revision changed: checkpoint=%s current=%s", restored.Capability.Digest, capability.Digest)
		}
		turnID = restored.TurnID
		admission = restored.Admission + 1
		completedTurns = restored.Turn
		completedSequence = restored.Sequence
		if reader, ok := e.config.Store.(EventSequenceReader); ok {
			lastEventSequence, err := reader.LastEventSequence(context.WithoutCancel(ctx), restored.RunID)
			if err != nil {
				return RunResult{}, fmt.Errorf("engine: load trace sequence: %w", err)
			}
			if lastEventSequence > completedSequence {
				completedSequence = lastEventSequence
			}
		}
		operationSequence = restored.OperationSequence
		historyRevision = restored.HistoryRevision
		usage = restored.Usage
		repeatedState = restored.RepeatedCalls
		for claimID, revision := range restored.CommittedInputClaims {
			committedInputClaims[claimID] = revision
		}
	}
	lease := newTurnLease(runID, turnID, newID("lease"), admission)
	ctx = tools.WithExecutionLease(ctx, lease)
	defer lease.Deactivate()
	startedAt := time.Now().UTC()
	result := RunResult{
		RunID: runID, TurnID: turnID, Admission: admission, Capability: capability,
		HistoryRevision: historyRevision, MemoryScope: memoryScope, Messages: messages, Usage: usage, StartedAt: startedAt,
	}
	repeatedCalls := make(map[[32]byte]int)
	for key, count := range repeatedState {
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
	commit := func(kind string, turn int, extraReservedEvents ...int) error {
		if e.config.Store == nil {
			return nil
		}
		reservedEvents := 1 // history_committed
		if len(extraReservedEvents) > 0 && extraReservedEvents[0] > reservedEvents {
			reservedEvents = extraReservedEvents[0]
		}
		encoded := make(map[string]int, len(repeatedCalls))
		for fingerprint, count := range repeatedCalls {
			encoded[fmt.Sprintf("%x", fingerprint)] = count
		}
		operationSequence++
		operationID := fmt.Sprintf("%s/op-%06d/%s", runID, operationSequence, kind)
		checkpoint := Checkpoint{
			Version: 2, RunID: runID, TurnID: turnID, Admission: admission, LeaseID: lease.ID,
			Prompt: userPrompt, WorkDir: e.config.WorkDir, MemoryScope: memoryScope, Capability: capability,
			Messages: append([]schema.Message(nil), messages...), Turn: turn,
			// Reserve the trace events that follow this barrier. If append fails
			// after the canonical commit, a resumed admission starts after the
			// reserved values instead of reusing an event number.
			Usage: result.Usage, Reason: result.Reason, RepeatedCalls: encoded, Sequence: sequence + reservedEvents,
			OperationSequence: operationSequence, HistoryRevision: historyRevision, UpdatedAt: time.Now().UTC(),
			CommittedInputClaims: cloneStringInt64Map(committedInputClaims),
		}
		fingerprint, err := historyFingerprint(kind, checkpoint)
		if err != nil {
			return err
		}
		checkpoint.LastOperationID = operationID
		checkpoint.LastFingerprint = fingerprint
		ack, err := e.config.Store.CommitHistory(context.WithoutCancel(ctx), HistoryCommit{
			RunID: runID, OperationID: operationID, Fingerprint: fingerprint,
			ExpectedRevision: historyRevision, Checkpoint: checkpoint,
		})
		if err != nil {
			return err
		}
		if ack.RunID != runID || ack.OperationID != operationID || ack.Fingerprint != fingerprint || ack.Revision <= historyRevision {
			return fmt.Errorf("engine: invalid history acknowledgement for %s", operationID)
		}
		historyRevision = ack.Revision
		result.HistoryRevision = historyRevision
		return emit(Event{Type: EventCommitted, Turn: turn, Metadata: map[string]any{
			"operation_id": operationID, "fingerprint": fingerprint, "revision": historyRevision, "kind": kind,
		}})
	}
	finish := func(reason schema.TerminalReason, err error) (RunResult, error) {
		lease.Deactivate()
		result.Reason = reason
		result.Messages = append([]schema.Message(nil), messages...)
		result.CompletedAt = time.Now().UTC()
		result.DurationMS = result.CompletedAt.Sub(startedAt).Milliseconds()
		// Reserve history_committed and completed so a later resume cannot
		// reuse either trace sequence even if the process exits between them.
		if storeErr := commit("terminal-"+string(reason), result.Turns, 2); storeErr != nil && err == nil {
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
	if e.config.InputQueue != nil {
		if err := e.config.InputQueue.Reconcile(context.WithoutCancel(ctx), runID, committedInputClaims); err != nil {
			return finish(schema.TerminalPersistenceError, fmt.Errorf("reconcile input claims: %w", err))
		}
	}
	if err := commit("admission", completedTurns); err != nil {
		return finish(schema.TerminalPersistenceError, err)
	}
	if err := emit(Event{Type: EventRunStarted, Turn: completedTurns, Metadata: map[string]any{
		"resumed": restored != nil, "turn_id": turnID, "admission": admission, "lease_id": lease.ID,
	}}); err != nil {
		return finish(schema.TerminalPersistenceError, err)
	}
	if err := emit(Event{Type: EventCapability, Turn: completedTurns, Metadata: map[string]any{
		"sequence": capability.Sequence, "digest": capability.Digest, "tool_digest": capability.ToolDigest,
		"prompt_digest": capability.PromptDigest, "agents_digest": capability.AgentsDigest, "skills_digest": capability.SkillsDigest,
	}}); err != nil {
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
		if e.config.InputQueue != nil {
			claim, err := e.config.InputQueue.Claim(ctx, runID, turnID, 32)
			if err != nil {
				return finish(schema.TerminalPersistenceError, fmt.Errorf("claim queued inputs: %w", err))
			}
			if claim.ID != "" {
				if err := emit(Event{Type: EventInputClaimed, Turn: turn, Metadata: map[string]any{
					"claim_id": claim.ID, "items": len(claim.Items),
				}}); err != nil {
					_ = e.config.InputQueue.Release(context.WithoutCancel(ctx), claim.ID)
					return finish(schema.TerminalPersistenceError, err)
				}
				for _, input := range claim.Items {
					messages = append(messages, schema.Message{
						Role: schema.RoleUser, Content: formatQueuedInput(input),
					})
				}
				committedInputClaims[claim.ID] = historyRevision + 1
				if err := commit(fmt.Sprintf("step-%d-input-claim", turn), turn-1); err != nil {
					delete(committedInputClaims, claim.ID)
					_ = e.config.InputQueue.Release(context.WithoutCancel(ctx), claim.ID)
					return finish(schema.TerminalPersistenceError, err)
				}
				committedInputClaims[claim.ID] = historyRevision
				if err := e.config.InputQueue.Ack(context.WithoutCancel(ctx), claim.ID, historyRevision); err != nil {
					return finish(schema.TerminalPersistenceError, fmt.Errorf("ack queued inputs: %w", err))
				}
				if err := emit(Event{Type: EventInputAcked, Turn: turn, Metadata: map[string]any{
					"claim_id": claim.ID, "history_revision": historyRevision,
				}}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
			}
		}

		if e.config.Compactor != nil {
			compactContext := runtimecontext.WithMetadata(ctx, runtimecontext.Metadata{Scope: memoryScope, RunID: runID, Turn: turn})
			compacted, changed, err := e.config.Compactor.Compact(compactContext, messages)
			if err != nil {
				return finish(schema.TerminalFatalToolError, fmt.Errorf("compact context: %w", err))
			}
			if changed {
				before := len(messages)
				messages = compacted
				if err := emit(Event{Type: EventCompacted, Turn: turn, Metadata: map[string]any{"messages_before": before, "messages_after": len(messages)}}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
				if err := commit(fmt.Sprintf("step-%d-compaction", turn), turn-1); err != nil {
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
				if err := commit(fmt.Sprintf("step-%d-thinking", turn), turn-1); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
			}
		}

		generation, err := e.provider.Generate(ctx, messages, runtime.GetAvailableTools())
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
		if err := commit(fmt.Sprintf("step-%d-assistant", turn), turn-1); err != nil {
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

		toolResults := runtime.ExecuteBatch(ctx, generation.Message.ToolCalls)
		for index, rawResult := range toolResults {
			toolResult, hint := attachRecoveryHint(rawResult)
			messages = append(messages, schema.Message{
				Role:       schema.RoleTool,
				Content:    toolResult.Output,
				ToolCallID: toolResult.ToolCallID,
				IsError:    toolResult.IsError,
			})
			if err := emit(Event{Type: EventToolResult, Turn: turn, ToolResult: toolResult}); err != nil {
				return finish(schema.TerminalPersistenceError, err)
			}
			if hint != "" {
				if err := emit(Event{Type: EventRecoveryHint, Turn: turn, ToolResult: toolResult, Metadata: map[string]any{
					"error_code": toolResult.ErrorCode, "hint": hint,
				}}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
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
			call := generation.Message.ToolCalls[index]
			count := repeatedCalls[toolFingerprint(call)]
			if count == e.config.MaxRepeatedCall {
				reminder := repeatedCallReminder(call.Name, count, e.config.MaxRepeatedCall)
				messages = append(messages, reminder)
				if err := emit(Event{Type: EventReminder, Turn: turn, Message: reminder, Metadata: map[string]any{
					"kind": "repeated_call", "tool": call.Name, "count": count, "hard_limit": e.config.MaxRepeatedCall,
				}}); err != nil {
					return finish(schema.TerminalPersistenceError, err)
				}
			}
		}
		if err := commit(fmt.Sprintf("step-%d-tool-results", turn), turn); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}
		if err := emit(Event{Type: EventCheckpoint, Turn: turn}); err != nil {
			return finish(schema.TerminalPersistenceError, err)
		}
	}

	return finish(schema.TerminalMaxTurns, nil)
}

func combinedCapability(toolRevision tools.CapabilityRevision, snapshot promptcontext.Snapshot) tools.CapabilityRevision {
	toolDigest := toolRevision.ToolDigest
	if toolDigest == "" {
		toolDigest = toolRevision.Digest
	}
	manifest := struct {
		Tool   string `json:"tool"`
		Prompt string `json:"prompt"`
		Agents string `json:"agents,omitempty"`
		Skills string `json:"skills"`
	}{toolDigest, snapshot.Digest, snapshot.AgentsDigest, snapshot.SkillsDigest}
	encoded, _ := json.Marshal(manifest)
	digest := sha256.Sum256(encoded)
	return tools.CapabilityRevision{
		Sequence: toolRevision.Sequence, Digest: hex.EncodeToString(digest[:]), ToolDigest: toolDigest,
		PromptDigest: snapshot.Digest, AgentsDigest: snapshot.AgentsDigest, SkillsDigest: snapshot.SkillsDigest,
	}
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
	return newID("run")
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return fmt.Sprintf("%s-%x", prefix, value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

type turnLease struct {
	mu        sync.RWMutex
	RunID     string
	TurnID    string
	ID        string
	Admission int
	active    bool
}

func newTurnLease(runID, turnID, leaseID string, admission int) *turnLease {
	return &turnLease{RunID: runID, TurnID: turnID, ID: leaseID, Admission: admission, active: true}
}

func (l *turnLease) Validate() error {
	l.mu.RLock()
	active := l.active
	l.mu.RUnlock()
	if !active {
		return tools.ErrStaleExecutionLease
	}
	return nil
}

func (l *turnLease) Deactivate() {
	l.mu.Lock()
	l.active = false
	l.mu.Unlock()
}

func historyFingerprint(kind string, checkpoint Checkpoint) (string, error) {
	// Volatile commit metadata must not influence semantic identity.
	checkpoint.UpdatedAt = time.Time{}
	checkpoint.HistoryRevision = 0
	checkpoint.LastOperationID = ""
	checkpoint.LastFingerprint = ""
	encoded, err := json.Marshal(struct {
		Kind       string     `json:"kind"`
		Checkpoint Checkpoint `json:"checkpoint"`
	}{Kind: kind, Checkpoint: checkpoint})
	if err != nil {
		return "", fmt.Errorf("engine: encode history fingerprint: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:]), nil
}

func cloneStringInt64Map(input map[string]int64) map[string]int64 {
	if len(input) == 0 {
		return nil
	}
	cloned := make(map[string]int64, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func formatQueuedInput(input QueuedInput) string {
	return fmt.Sprintf("[queued %s id=%s]\n%s", input.Kind, input.ID, input.Content)
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
