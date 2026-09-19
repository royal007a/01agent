package contextmanager

import (
	"context"
	"strings"
	"testing"

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

func TestWindowLeavesSmallContextUntouched(t *testing.T) {
	messages := []schema.Message{{Role: schema.RoleSystem, Content: "s"}, {Role: schema.RoleUser, Content: "u"}}
	result, changed, err := (Window{MaxApproxTokens: 100}).Compact(context.Background(), messages)
	if err != nil || changed || len(result) != 2 {
		t.Fatalf("result=%#v changed=%v err=%v", result, changed, err)
	}
}
