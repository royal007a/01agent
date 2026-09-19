package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/schema"
	toolruntime "github.com/royal007a/01agent/internal/tools"
)

type scriptedProvider struct {
	mu          sync.Mutex
	generations []schema.Generation
	generate    func(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error)
	toolCounts  []int
	histories   [][]schema.Message
}

func (s *scriptedProvider) Generate(ctx context.Context, messages []schema.Message, definitions []schema.ToolDefinition) (schema.Generation, error) {
	s.mu.Lock()
	s.toolCounts = append(s.toolCounts, len(definitions))
	s.histories = append(s.histories, append([]schema.Message(nil), messages...))
	custom := s.generate
	if custom == nil {
		if len(s.generations) == 0 {
			s.mu.Unlock()
			return schema.Generation{}, errors.New("script exhausted")
		}
		generation := s.generations[0]
		s.generations = s.generations[1:]
		s.mu.Unlock()
		return generation, nil
	}
	s.mu.Unlock()
	return custom(ctx, messages, definitions)
}

type fakeRegistry struct {
	definitions []schema.ToolDefinition
	result      func(schema.ToolCall) schema.ToolResult
	calls       int
}

type memoryStore struct {
	checkpoint Checkpoint
	events     []Event
	result     RunResult
}

func (m *memoryStore) Record(_ context.Context, event Event) error {
	m.events = append(m.events, event)
	return nil
}
func (m *memoryStore) SaveCheckpoint(_ context.Context, checkpoint Checkpoint) error {
	m.checkpoint = checkpoint
	return nil
}
func (m *memoryStore) Complete(_ context.Context, result RunResult) error {
	m.result = result
	return nil
}
func (m *memoryStore) LoadCheckpoint(context.Context, string) (Checkpoint, error) {
	return m.checkpoint, nil
}

func (f *fakeRegistry) Register(toolruntime.BaseTool) error { return nil }
func (f *fakeRegistry) GetAvailableTools() []schema.ToolDefinition {
	return append([]schema.ToolDefinition(nil), f.definitions...)
}
func (f *fakeRegistry) Execute(_ context.Context, call schema.ToolCall) schema.ToolResult {
	f.calls++
	return f.result(call)
}
func (f *fakeRegistry) ExecuteBatch(ctx context.Context, calls []schema.ToolCall) []schema.ToolResult {
	results := make([]schema.ToolResult, len(calls))
	for i, call := range calls {
		results[i] = f.Execute(ctx, call)
	}
	return results
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		definitions: []schema.ToolDefinition{{Name: "read_file"}},
		result: func(call schema.ToolCall) schema.ToolResult {
			return schema.ToolResult{ToolCallID: call.ID, Output: "file contents"}
		},
	}
}

func TestRunCompletesAfterToolObservation(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{
		{Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}}}},
		{Message: schema.Message{Content: "done"}},
	}}
	registry := newFakeRegistry()
	agent, err := New(model, registry, Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	result, err := agent.Run(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalCompleted || result.FinalMessage.Content != "done" {
		t.Fatalf("result = %#v", result)
	}
	if registry.calls != 1 {
		t.Fatalf("registry calls = %d, want 1", registry.calls)
	}
	if len(result.Messages) != 5 || result.Messages[3].Role != schema.RoleTool || result.Messages[3].ToolCallID != "call-1" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestThinkingPhaseHasNoToolsAndFeedsAction(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{
		{Message: schema.Message{Content: "plan first"}},
		{Message: schema.Message{Content: "done"}},
	}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), EnableThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalCompleted {
		t.Fatalf("reason = %s", result.Reason)
	}
	if len(model.toolCounts) != 2 || model.toolCounts[0] != 0 || model.toolCounts[1] != 1 {
		t.Fatalf("tool counts = %v", model.toolCounts)
	}
	actionHistory := model.histories[1]
	if actionHistory[len(actionHistory)-1].Content != "plan first" {
		t.Fatalf("action history = %#v", actionHistory)
	}
}

func TestRunStopsAtMaxTurns(t *testing.T) {
	model := &scriptedProvider{generate: func(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
		return schema.Generation{Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "call", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}}}, nil
	}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), MaxTurns: 2, MaxRepeatedCall: 10})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "loop")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalMaxTurns || result.Turns != 2 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunStopsOnTokenBudget(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{{
		Message: schema.Message{Content: "would finish"},
		Usage:   schema.Usage{InputTokens: 7, OutputTokens: 4},
	}}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "budget")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalBudgetExceeded || result.Usage.TotalTokens() != 11 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunStopsRepeatedEquivalentCall(t *testing.T) {
	model := &scriptedProvider{generate: func(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
		return schema.Generation{Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "changing-id", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}}}, nil
	}}
	registry := newFakeRegistry()
	agent, err := New(model, registry, Config{WorkDir: t.TempDir(), MaxTurns: 10, MaxRepeatedCall: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "loop")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalFatalToolError || registry.calls != 2 {
		t.Fatalf("result = %#v, registry calls = %d", result, registry.calls)
	}
}

func TestRunMapsDeadlineToTimeout(t *testing.T) {
	model := &scriptedProvider{generate: func(ctx context.Context, _ []schema.Message, _ []schema.ToolDefinition) (schema.Generation, error) {
		<-ctx.Done()
		return schema.Generation{}, ctx.Err()
	}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if result.Reason != schema.TerminalTimeout {
		t.Fatalf("reason = %s", result.Reason)
	}
}

func TestRunStopsOnPermissionDenial(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{{
		Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "call", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}},
	}}}
	registry := newFakeRegistry()
	registry.result = func(call schema.ToolCall) schema.ToolResult {
		return schema.ToolResult{ToolCallID: call.ID, IsError: true, ErrorCode: "permission_denied", Fatal: true}
	}
	agent, err := New(model, registry, Config{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "deny")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != schema.TerminalPermissionDenied {
		t.Fatalf("reason = %s", result.Reason)
	}
}

func TestRunCanResumeFromCheckpoint(t *testing.T) {
	store := &memoryStore{}
	firstModel := &scriptedProvider{generations: []schema.Generation{{Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}}}}}
	workDir := t.TempDir()
	first, err := New(firstModel, newFakeRegistry(), Config{WorkDir: workDir, MaxTurns: 1, Store: store, RunID: "run-resume"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := first.Run(context.Background(), "inspect")
	if err != nil || result.Reason != schema.TerminalMaxTurns {
		t.Fatalf("first result=%#v err=%v", result, err)
	}
	secondModel := &scriptedProvider{generations: []schema.Generation{{Message: schema.Message{Content: "resumed"}}}}
	second, err := New(secondModel, newFakeRegistry(), Config{WorkDir: workDir, MaxTurns: 2, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := second.Resume(context.Background(), store.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Reason != schema.TerminalCompleted || resumed.Turns != 2 || resumed.RunID != "run-resume" || resumed.FinalMessage.Content != "resumed" {
		t.Fatalf("resumed = %#v", resumed)
	}
}
