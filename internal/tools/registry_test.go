package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/schema"
)

type fakeTool struct {
	name         string
	risk         schema.ToolRisk
	parallelSafe bool
	delay        time.Duration
	calls        atomic.Int64
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: f.name,
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
			"required": []string{"value"},
		},
		Risk:         f.risk,
		ParallelSafe: f.parallelSafe,
	}
}

func (f *fakeTool) Validate(json.RawMessage) error { return nil }

func (f *fakeTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	f.calls.Add(1)
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		var input struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(arguments, &input); err != nil {
			return "", err
		}
		return input.Value, nil
	}
}

type denyPolicy struct{}

func (denyPolicy) CanUse(context.Context, schema.ToolDefinition, json.RawMessage) PermissionDecision {
	return PermissionDecision{Reason: "denied for test"}
}

type blockingPolicy struct {
	started chan struct{}
	release chan struct{}
}

type cancelingPolicy struct{ cancel context.CancelFunc }

func (p cancelingPolicy) CanUse(context.Context, schema.ToolDefinition, json.RawMessage) PermissionDecision {
	p.cancel()
	return PermissionDecision{Allowed: true}
}

func (p blockingPolicy) CanUse(context.Context, schema.ToolDefinition, json.RawMessage) PermissionDecision {
	close(p.started)
	<-p.release
	return PermissionDecision{Allowed: true}
}

type testLease struct{ active atomic.Bool }

func (l *testLease) Validate() error {
	if !l.active.Load() {
		return ErrStaleExecutionLease
	}
	return nil
}

func TestRegistryValidatesBeforeExecution(t *testing.T) {
	tool := &fakeTool{name: "read", risk: schema.RiskRead}
	registry := NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), schema.ToolCall{ID: "1", Name: "read", Arguments: json.RawMessage(`{"wrong":1}`)})
	if result.ErrorCode != "schema_validation" {
		t.Fatalf("result = %#v, want schema_validation", result)
	}
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed despite invalid arguments")
	}
}

func TestRegistryChecksPermissionBeforeExecution(t *testing.T) {
	tool := &fakeTool{name: "write", risk: schema.RiskWrite}
	registry := NewRegistry(WithPermissionPolicy(denyPolicy{}))
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), schema.ToolCall{ID: "1", Name: "write", Arguments: json.RawMessage(`{"value":"x"}`)})
	if result.ErrorCode != "permission_denied" || !result.Fatal {
		t.Fatalf("result = %#v, want fatal permission_denied", result)
	}
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed despite denied permission")
	}
}

func TestRegistryRunsSafeBatchConcurrentlyAndPreservesOrder(t *testing.T) {
	tool := &fakeTool{name: "read", risk: schema.RiskRead, parallelSafe: true, delay: 80 * time.Millisecond}
	registry := NewRegistry(WithMaxParallel(2))
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	calls := []schema.ToolCall{
		{ID: "first", Name: "read", Arguments: json.RawMessage(`{"value":"a"}`)},
		{ID: "second", Name: "read", Arguments: json.RawMessage(`{"value":"b"}`)},
	}
	started := time.Now()
	results := registry.ExecuteBatch(context.Background(), calls)
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("batch took %s, expected concurrent execution", elapsed)
	}
	if results[0].Output != "a" || results[1].Output != "b" {
		t.Fatalf("results = %#v, want original order", results)
	}
}

func TestRegistryRejectsDuplicateName(t *testing.T) {
	registry := NewRegistry()
	tool := &fakeTool{name: "read", risk: schema.RiskRead}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
}

func TestRegistryUnknownToolIsRecoverable(t *testing.T) {
	result := NewRegistry().Execute(context.Background(), schema.ToolCall{ID: "1", Name: "missing", Arguments: json.RawMessage(`{}`)})
	if result.ErrorCode != "unknown_tool" || !result.Retryable || result.Fatal {
		t.Fatalf("result = %#v", result)
	}
}

func TestCapabilitySnapshotIsImmutable(t *testing.T) {
	registry := NewRegistry()
	first := &fakeTool{name: "first", risk: schema.RiskRead}
	second := &fakeTool{name: "second", risk: schema.RiskRead}
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	before := snapshot.Revision()
	if err := registry.Register(second); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GetAvailableTools()) != 1 || len(registry.GetAvailableTools()) != 2 {
		t.Fatalf("snapshot=%v registry=%v", snapshot.GetAvailableTools(), registry.GetAvailableTools())
	}
	if snapshot.Revision() != before || snapshot.Revision().Equivalent(registry.Revision()) {
		t.Fatalf("snapshot revision=%#v registry revision=%#v", snapshot.Revision(), registry.Revision())
	}
	result := snapshot.Execute(context.Background(), schema.ToolCall{ID: "2", Name: "second", Arguments: json.RawMessage(`{"value":"x"}`)})
	if result.ErrorCode != "unknown_tool" {
		t.Fatalf("snapshot dispatched a later tool: %#v", result)
	}
}

func TestExecutionLeaseIsRevalidatedAfterPermission(t *testing.T) {
	tool := &fakeTool{name: "write", risk: schema.RiskWrite}
	policy := blockingPolicy{started: make(chan struct{}), release: make(chan struct{})}
	registry := NewRegistry(WithPermissionPolicy(policy))
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	lease := &testLease{}
	lease.active.Store(true)
	ctx := WithExecutionLease(context.Background(), lease)
	resultChannel := make(chan schema.ToolResult, 1)
	go func() {
		resultChannel <- registry.Execute(ctx, schema.ToolCall{ID: "1", Name: "write", Arguments: json.RawMessage(`{"value":"x"}`)})
	}()
	<-policy.started
	lease.active.Store(false)
	close(policy.release)
	result := <-resultChannel
	if result.ErrorCode != "stale_lease" || !result.Fatal || !errors.Is(lease.Validate(), ErrStaleExecutionLease) {
		t.Fatalf("result=%#v lease=%v", result, lease.Validate())
	}
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed after its turn lease became stale")
	}
}

func TestCancellationIsRevalidatedAfterPermission(t *testing.T) {
	tool := &fakeTool{name: "write", risk: schema.RiskWrite}
	ctx, cancel := context.WithCancel(context.Background())
	registry := NewRegistry(WithPermissionPolicy(cancelingPolicy{cancel: cancel}))
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(ctx, schema.ToolCall{ID: "1", Name: "write", Arguments: json.RawMessage(`{"value":"x"}`)})
	if result.ErrorCode != "cancelled" || !result.Fatal {
		t.Fatalf("result=%#v", result)
	}
	if tool.calls.Load() != 0 {
		t.Fatal("tool executed after its turn was cancelled during approval")
	}
}
