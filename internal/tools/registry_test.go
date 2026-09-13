package tools

import (
	"context"
	"encoding/json"
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
