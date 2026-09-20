package plan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

func TestPlanCanonicalCASIdempotencyAndProjections(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	update := Update{
		Scope: "session-one", OperationID: "op-one", ExpectedRevision: 0, Objective: "Ship safely",
		Steps: []Step{
			{ID: "build", Title: "Build feature", Status: InProgress},
			{ID: "test", Title: "Run tests", Status: Pending},
		},
	}
	state, err := store.Update(context.Background(), update)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 || state.LastOperationID != "op-one" {
		t.Fatalf("state=%#v", state)
	}
	dir := store.scopeDir("session-one")
	for _, name := range []string{"plan.json", "PLAN.md", "TODO.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	planText, _ := os.ReadFile(filepath.Join(dir, "PLAN.md"))
	todoText, _ := os.ReadFile(filepath.Join(dir, "TODO.md"))
	if !strings.Contains(string(planText), "Revision: 1") || !strings.Contains(string(todoText), "Run tests") {
		t.Fatalf("PLAN=%q TODO=%q", planText, todoText)
	}

	if err := os.Remove(filepath.Join(dir, "TODO.md")); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Update(context.Background(), update)
	if err != nil || replayed.Revision != 1 {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "TODO.md")); err != nil {
		t.Fatalf("projection was not repaired: %v", err)
	}
	conflict := update
	conflict.OperationID = "op-two"
	if _, err := store.Update(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision error=%v", err)
	}
	semanticConflict := update
	semanticConflict.Objective = "Different"
	if _, err := store.Update(context.Background(), semanticConflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("operation conflict error=%v", err)
	}
}

func TestPlanToolsUseSessionScopeWithoutDangerousApproval(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(tools.WithPermissionPolicy(tools.ReadOnlyPolicy{}))
	if err := registry.Register(NewUpdateTool(store)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewReadTool(store)); err != nil {
		t.Fatal(err)
	}
	ctx := runtimecontext.WithMetadata(context.Background(), runtimecontext.Metadata{Scope: "chat", RunID: "run"})
	update := schema.ToolCall{ID: "u", Name: "update_plan", Arguments: json.RawMessage(`{
        "operation_id":"tool-op","expected_revision":0,"objective":"Test plan",
        "steps":[{"id":"one","title":"First","status":"pending"}]
    }`)}
	if result := registry.Execute(ctx, update); result.IsError || !strings.Contains(result.Output, `"revision":1`) {
		t.Fatalf("update result=%#v", result)
	}
	read := schema.ToolCall{ID: "r", Name: "read_plan", Arguments: json.RawMessage(`{}`)}
	if result := registry.Execute(ctx, read); result.IsError || !strings.Contains(result.Output, "Test plan") {
		t.Fatalf("read result=%#v", result)
	}
}
