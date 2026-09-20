package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	revision    string
}

type memoryStore struct {
	checkpoint Checkpoint
	events     []Event
	result     RunResult
	revision   int64
}

type memoryInputQueue struct {
	claim       InputClaim
	claimed     bool
	acked       bool
	ackRevision int64
	reconciled  bool
}

func (q *memoryInputQueue) Claim(_ context.Context, runID, turnID string, _ int) (InputClaim, error) {
	if q.claimed || len(q.claim.Items) == 0 {
		return InputClaim{}, nil
	}
	q.claimed = true
	q.claim.RunID = runID
	q.claim.TurnID = turnID
	if q.claim.ID == "" {
		q.claim.ID = "claim-test"
	}
	return q.claim, nil
}
func (q *memoryInputQueue) Ack(_ context.Context, claimID string, revision int64) error {
	if claimID != q.claim.ID {
		return errors.New("wrong claim")
	}
	q.acked = true
	q.ackRevision = revision
	return nil
}
func (q *memoryInputQueue) Release(context.Context, string) error { return nil }
func (q *memoryInputQueue) Reconcile(context.Context, string, map[string]int64) error {
	q.reconciled = true
	return nil
}

func (m *memoryStore) Record(_ context.Context, event Event) error {
	m.events = append(m.events, event)
	return nil
}
func (m *memoryStore) CommitHistory(_ context.Context, commit HistoryCommit) (HistoryAck, error) {
	if commit.ExpectedRevision != m.revision {
		return HistoryAck{}, errors.New("revision conflict")
	}
	m.revision++
	m.checkpoint = commit.Checkpoint
	m.checkpoint.HistoryRevision = m.revision
	m.checkpoint.LastOperationID = commit.OperationID
	m.checkpoint.LastFingerprint = commit.Fingerprint
	return HistoryAck{
		RunID: commit.RunID, OperationID: commit.OperationID, Fingerprint: commit.Fingerprint,
		Revision: m.revision, CommittedAt: time.Now(),
	}, nil
}
func (m *memoryStore) Complete(_ context.Context, result RunResult) error {
	m.result = result
	return nil
}
func (m *memoryStore) LoadCheckpoint(context.Context, string) (Checkpoint, error) {
	return m.checkpoint, nil
}
func (m *memoryStore) LastEventSequence(context.Context, string) (int, error) {
	if len(m.events) == 0 {
		return 0, nil
	}
	return m.events[len(m.events)-1].Sequence, nil
}

func (f *fakeRegistry) Register(toolruntime.BaseTool) error { return nil }
func (f *fakeRegistry) Revision() toolruntime.CapabilityRevision {
	digest := f.revision
	if digest == "" {
		digest = "fake-capability"
	}
	return toolruntime.CapabilityRevision{Sequence: 1, Digest: digest}
}
func (f *fakeRegistry) Snapshot() toolruntime.Runtime { return f }
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

func TestPlanModeIsAnExplicitPromptCapability(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{{Message: schema.Message{Content: "done"}}}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), PlanMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Run(context.Background(), "continue"); err != nil {
		t.Fatal(err)
	}
	if len(model.histories) != 1 || !strings.Contains(model.histories[0][0].Content, "# Plan mode") || !strings.Contains(model.histories[0][0].Content, "expected_revision 0") {
		t.Fatalf("system prompt = %#v", model.histories)
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
	foundReminder := false
	for _, message := range model.histories[len(model.histories)-1] {
		foundReminder = foundReminder || strings.Contains(message.Content, reminderPrefix)
	}
	if !foundReminder {
		t.Fatalf("provider histories did not contain a repeated-call reminder: %#v", model.histories)
	}
}

func TestRunAddsStructuredRecoveryGuidanceAndTraceEvent(t *testing.T) {
	model := &scriptedProvider{generations: []schema.Generation{
		{Message: schema.Message{ToolCalls: []schema.ToolCall{{ID: "edit-1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"x"}`)}}}},
		{Message: schema.Message{Content: "recovered"}},
	}}
	registry := newFakeRegistry()
	registry.definitions = []schema.ToolDefinition{{Name: "edit_file"}}
	registry.result = func(call schema.ToolCall) schema.ToolResult {
		return schema.ToolResult{ToolCallID: call.ID, Output: "Error [no_match]: old text absent", IsError: true, ErrorCode: "no_match", Retryable: true}
	}
	store := &memoryStore{}
	agent, err := New(model, registry, Config{WorkDir: t.TempDir(), Store: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "edit")
	if err != nil || result.Reason != schema.TerminalCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !strings.Contains(result.Messages[3].Content, "Recovery guidance:") || !strings.Contains(result.Messages[3].Content, "Read the current file again") {
		t.Fatalf("tool observation=%#v", result.Messages[3])
	}
	found := false
	for _, event := range store.events {
		found = found || event.Type == EventRecoveryHint
	}
	if !found {
		t.Fatalf("events=%#v", store.events)
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
	lastSequence := 0
	for _, event := range store.events {
		if event.Sequence <= lastSequence {
			t.Fatalf("event sequence reused across resume: previous=%d current=%d event=%s", lastSequence, event.Sequence, event.Type)
		}
		lastSequence = event.Sequence
	}
}

func TestResumeRejectsChangedCapabilitySnapshot(t *testing.T) {
	store := &memoryStore{}
	workDir := t.TempDir()
	firstModel := &scriptedProvider{generations: []schema.Generation{{Message: schema.Message{Content: "done"}}}}
	first, err := New(firstModel, newFakeRegistry(), Config{WorkDir: workDir, Store: store, RunID: "run-capability"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), "inspect"); err != nil {
		t.Fatal(err)
	}
	checkpoint := store.checkpoint
	checkpoint.Reason = schema.TerminalProviderError
	changed := newFakeRegistry()
	changed.revision = "changed-capability"
	second, err := New(&scriptedProvider{}, changed, Config{WorkDir: workDir, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Resume(context.Background(), checkpoint); err == nil || !strings.Contains(err.Error(), "capability revision changed") {
		t.Fatalf("resume error=%v", err)
	}
}

func TestResumeRejectsChangedPromptCapabilitySnapshot(t *testing.T) {
	store := &memoryStore{}
	workDir := t.TempDir()
	agentsPath := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("instruction revision one"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := New(&scriptedProvider{generations: []schema.Generation{{Message: schema.Message{Content: "done"}}}}, newFakeRegistry(), Config{
		WorkDir: workDir, Store: store, RunID: "run-prompt-capability",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), "inspect"); err != nil {
		t.Fatal(err)
	}
	checkpoint := store.checkpoint
	checkpoint.Reason = schema.TerminalProviderError
	if err := os.WriteFile(agentsPath, []byte("instruction revision two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := New(&scriptedProvider{}, newFakeRegistry(), Config{WorkDir: workDir, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Resume(context.Background(), checkpoint); err == nil || !strings.Contains(err.Error(), "capability revision changed") {
		t.Fatalf("resume error=%v", err)
	}
}

func TestQueuedInputIsCommittedBeforeAcknowledgement(t *testing.T) {
	store := &memoryStore{}
	queue := &memoryInputQueue{claim: InputClaim{Items: []QueuedInput{{ID: "input-1", Kind: InputUserSteer, Content: "new constraint"}}}}
	model := &scriptedProvider{generations: []schema.Generation{{Message: schema.Message{Content: "done"}}}}
	agent, err := New(model, newFakeRegistry(), Config{WorkDir: t.TempDir(), Store: store, InputQueue: queue})
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.Run(context.Background(), "original")
	if err != nil || result.Reason != schema.TerminalCompleted {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if !queue.reconciled || !queue.acked || queue.ackRevision <= 0 {
		t.Fatalf("queue=%#v", queue)
	}
	if len(model.histories) != 1 || !strings.Contains(model.histories[0][len(model.histories[0])-1].Content, "new constraint") {
		t.Fatalf("model history=%#v", model.histories)
	}
	if revision := store.checkpoint.CommittedInputClaims[queue.claim.ID]; revision <= 0 {
		t.Fatalf("checkpoint did not retain committed claim: %#v", store.checkpoint)
	}
	claimed, acked := false, false
	for _, event := range store.events {
		claimed = claimed || event.Type == EventInputClaimed
		acked = acked || event.Type == EventInputAcked
	}
	if !claimed || !acked {
		t.Fatalf("claim events missing: %#v", store.events)
	}
}
