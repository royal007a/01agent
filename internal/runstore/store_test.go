package runstore

import (
	"context"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/schema"
)

func TestFileStoreCheckpointTraceAndReplay(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-test"
	events := []engine.Event{
		{Type: engine.EventRunStarted, RunID: runID, Sequence: 1, Timestamp: time.Now()},
		{Type: engine.EventToolStarted, RunID: runID, Sequence: 2, Timestamp: time.Now(), ToolCall: schema.ToolCall{ID: "call-1", Name: "read_file"}},
		{Type: engine.EventToolResult, RunID: runID, Sequence: 3, Timestamp: time.Now(), ToolResult: schema.ToolResult{ToolCallID: "call-1", Output: "ok"}},
	}
	for _, event := range events {
		if err := store.Record(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint := engine.Checkpoint{Version: 1, RunID: runID, Prompt: "inspect", WorkDir: t.TempDir(), Messages: []schema.Message{{Role: schema.RoleUser, Content: "inspect"}}, Sequence: 3}
	if err := store.SaveCheckpoint(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	result := engine.RunResult{RunID: runID, Reason: schema.TerminalCompleted}
	if err := store.Complete(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCheckpoint(context.Background(), runID)
	if err != nil || loaded.Sequence != 3 {
		t.Fatalf("checkpoint = %#v, err = %v", loaded, err)
	}
	trace, err := store.LoadTrace(runID)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Replay(trace)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ToolCalls != 1 || summary.ToolResults != 1 || summary.FinalReason != "completed" {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestRejectsUnsafeRunID(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), engine.Event{RunID: "../escape"}); err == nil {
		t.Fatal("unsafe run id was accepted")
	}
}
