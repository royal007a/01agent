package contextmanager

import (
	"context"
	"strings"
	"testing"

	"github.com/royal007a/01agent/internal/memory"
	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
)

func TestWindowCompactsAndPreservesToolPair(t *testing.T) {
	large := strings.Repeat("x", 800)
	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: "system"},
		{Role: schema.RoleUser, Content: "task"},
		{Role: schema.RoleAssistant, Content: large},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: schema.RoleTool, ToolCallID: "c1", Content: large},
		{Role: schema.RoleAssistant, Content: "latest"},
	}
	compacted, changed, err := (Window{MaxApproxTokens: 280, MinTailMessages: 2}).Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(compacted) >= len(messages)+1 {
		t.Fatalf("compacted = %#v, changed = %v", compacted, changed)
	}
	var call, result bool
	for _, message := range compacted {
		for _, item := range message.ToolCalls {
			call = call || item.ID == "c1"
		}
		result = result || message.ToolCallID == "c1"
	}
	if call != result {
		t.Fatalf("tool pair was split: call=%v result=%v", call, result)
	}
}

func TestWindowArchivesRawDetailsBeforeTieredCompaction(t *testing.T) {
	archive, err := memory.NewArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	large := "codename ORION " + strings.Repeat("detail ", 300)
	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: "system"},
		{Role: schema.RoleUser, Content: "task"},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "c1", Name: "read_file"}}},
		{Role: schema.RoleTool, ToolCallID: "c1", Content: large},
		{Role: schema.RoleAssistant, Content: strings.Repeat("reasoning ", 200)},
		{Role: schema.RoleUser, Content: "what was the codename?"},
	}
	ctx := runtimecontext.WithMetadata(context.Background(), runtimecontext.Metadata{Scope: "session", RunID: "run", Turn: 3})
	compacted, changed, err := (Window{
		MaxApproxTokens: 240, ReserveTokens: 40, MinTailMessages: 2,
		RecentMessages: 2, MaxToolBytes: 200, Archive: archive,
	}).Compact(ctx, messages)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(compacted) >= len(messages) || !strings.Contains(compacted[1].Content, "recall_context") {
		t.Fatalf("compacted=%#v changed=%v", compacted, changed)
	}
	results, err := archive.Search(context.Background(), "session", "codename", 5)
	found := false
	for _, result := range results {
		found = found || strings.Contains(result.Content, "ORION")
	}
	if err != nil || !found {
		t.Fatalf("results=%#v err=%v", results, err)
	}
}

func TestWindowLeavesSmallContextUntouched(t *testing.T) {
	messages := []schema.Message{{Role: schema.RoleSystem, Content: "s"}, {Role: schema.RoleUser, Content: "u"}}
	result, changed, err := (Window{MaxApproxTokens: 100}).Compact(context.Background(), messages)
	if err != nil || changed || len(result) != 2 {
		t.Fatalf("result=%#v changed=%v err=%v", result, changed, err)
	}
}
