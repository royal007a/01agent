package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
)

func TestArchiveIsIdempotentAndBM25RecallIsScoped(t *testing.T) {
	archive, err := NewArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtimecontext.WithMetadata(context.Background(), runtimecontext.Metadata{Scope: "session-a", RunID: "run-a", Turn: 2})
	messages := []schema.Message{
		{Role: schema.RoleSystem, Content: "system"},
		{Role: schema.RoleUser, Content: "The deployment codename is ORION NEBULA."},
		{Role: schema.RoleTool, ToolCallID: "call-1", Content: "unrelated build output"},
	}
	first, err := archive.ArchiveMessages(ctx, messages)
	if err != nil {
		t.Fatal(err)
	}
	second, err := archive.ArchiveMessages(ctx, messages)
	if err != nil {
		t.Fatal(err)
	}
	if first[1] == "" || first[1] != second[1] {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	results, err := archive.Search(context.Background(), "session-a", "deployment codename", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].SourceID != first[1] || !strings.Contains(results[0].Content, "ORION") {
		t.Fatalf("results=%#v", results)
	}
	other, err := archive.Search(context.Background(), "session-b", "deployment codename", 3)
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-scope results=%#v err=%v", other, err)
	}
}

func TestRecallToolUsesRuntimeScope(t *testing.T) {
	archive, err := NewArchive(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := runtimecontext.WithMetadata(context.Background(), runtimecontext.Metadata{Scope: "chat", RunID: "run", Turn: 1})
	if _, err := archive.ArchiveMessages(ctx, []schema.Message{{Role: schema.RoleUser, Content: "数据库密码轮换窗口是周三。"}}); err != nil {
		t.Fatal(err)
	}
	tool := NewRecallTool(archive)
	output, err := tool.Execute(ctx, json.RawMessage(`{"query":"密码 轮换","limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "周三") {
		t.Fatalf("output=%s", output)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"密码"}`)); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("missing scope error=%v", err)
	}
}
