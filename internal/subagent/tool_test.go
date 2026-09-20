package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type captureProvider struct {
	mu          sync.Mutex
	messages    []schema.Message
	definitions []schema.ToolDefinition
	content     string
}

func (p *captureProvider) Generate(_ context.Context, messages []schema.Message, definitions []schema.ToolDefinition) (schema.Generation, error) {
	p.mu.Lock()
	p.messages = append([]schema.Message(nil), messages...)
	p.definitions = append([]schema.ToolDefinition(nil), definitions...)
	p.mu.Unlock()
	return schema.Generation{Message: schema.Message{Content: p.content}}, nil
}

type fakeTool struct {
	name string
	risk schema.ToolRisk
}

func (t fakeTool) Name() string { return t.name }
func (t fakeTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{Name: t.name, InputSchema: map[string]any{"type": "object"}, Risk: t.risk, ParallelSafe: true}
}
func (fakeTool) Validate(json.RawMessage) error                           { return nil }
func (fakeTool) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }

func TestSubagentHasFreshContextReadOnlyRegistryAndLineage(t *testing.T) {
	model := &captureProvider{content: "evidence summary"}
	tool, err := NewTool(model, []tools.BaseTool{
		fakeTool{name: "read_only", risk: schema.RiskRead},
		fakeTool{name: "write_file", risk: schema.RiskWrite},
		fakeTool{name: "spawn_subagent", risk: schema.RiskRead},
	}, Config{WorkDir: t.TempDir(), MaxOutput: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtimecontext.WithMetadata(context.Background(), runtimecontext.Metadata{Scope: "session-1", RunID: "parent-run"})
	ctx = tools.WithApprovalScope(ctx, tools.ApprovalScope{RunID: "parent-run", TurnID: "parent-turn", LeaseID: "lease", CapabilityDigest: "cap"})
	output, err := tool.Execute(ctx, json.RawMessage(`{"task":"inspect the workspace"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.ParentRunID != "parent-run" || result.ParentTurnID != "parent-turn" || !strings.HasPrefix(result.RunID, "subrun-") || result.Summary != "evidence summary" {
		t.Fatalf("result=%#v", result)
	}
	if len(model.messages) != 2 || model.messages[0].Role != schema.RoleSystem || model.messages[1].Role != schema.RoleUser || model.messages[1].Content != "inspect the workspace" {
		t.Fatalf("messages=%#v", model.messages)
	}
	if len(model.definitions) != 1 || model.definitions[0].Name != "read_only" {
		t.Fatalf("definitions=%#v", model.definitions)
	}
}

func TestSubagentSummaryIsBoundedOnUTF8Boundary(t *testing.T) {
	model := &captureProvider{content: strings.Repeat("界", 100)}
	tool, err := NewTool(model, []tools.BaseTool{fakeTool{name: "read_only", risk: schema.RiskRead}}, Config{WorkDir: t.TempDir(), MaxOutput: 32})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"task":"summarize"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || !strings.Contains(result.Summary, "truncated") || !utf8.ValidString(result.Summary) {
		t.Fatalf("result=%#v", result)
	}
}
