package runstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
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
	checkpoint := engine.Checkpoint{
		Version: 2, RunID: runID, TurnID: "turn-test", Admission: 1, Prompt: "inspect", WorkDir: t.TempDir(),
		Capability: tools.CapabilityRevision{Sequence: 1, Digest: "test"},
		Messages:   []schema.Message{{Role: schema.RoleUser, Content: "inspect"}}, Sequence: 3,
	}
	ack, err := store.CommitHistory(context.Background(), engine.HistoryCommit{
		RunID: runID, OperationID: "run-test/op-000001/test",
		Fingerprint: strings.Repeat("a", 64), ExpectedRevision: 0, Checkpoint: checkpoint,
	})
	if err != nil || ack.Revision != 1 {
		t.Fatal(err)
	}
	result := engine.RunResult{RunID: runID, Reason: schema.TerminalCompleted}
	if err := store.Complete(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCheckpoint(context.Background(), runID)
	if err != nil || loaded.Sequence != 3 || loaded.HistoryRevision != 1 {
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

func TestCanonicalHistoryIdempotencyAndConflicts(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := engine.Checkpoint{
		Version: 2, RunID: "run-history", TurnID: "turn-history", Admission: 1,
		Prompt: "inspect", WorkDir: t.TempDir(), Messages: []schema.Message{{Role: schema.RoleUser, Content: "inspect"}},
		Capability: tools.CapabilityRevision{Sequence: 1, Digest: "capability"},
	}
	commit := engine.HistoryCommit{
		RunID: "run-history", OperationID: "run-history/op-000001/admission",
		Fingerprint: strings.Repeat("b", 64), ExpectedRevision: 0, Checkpoint: checkpoint,
	}
	first, err := store.CommitHistory(context.Background(), commit)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CommitHistory(context.Background(), commit)
	if err != nil || replayed != first {
		t.Fatalf("replayed=%#v first=%#v err=%v", replayed, first, err)
	}
	conflictingIdentity := commit
	conflictingIdentity.Fingerprint = strings.Repeat("c", 64)
	if _, err := store.CommitHistory(context.Background(), conflictingIdentity); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("identity conflict error=%v", err)
	}
	staleRevision := commit
	staleRevision.OperationID = "run-history/op-000002/stale"
	staleRevision.ExpectedRevision = 0
	if _, err := store.CommitHistory(context.Background(), staleRevision); !errors.Is(err, ErrHistoryConflict) {
		t.Fatalf("revision conflict error=%v", err)
	}
}

func TestReplayValidatesCapabilityCommitsAndInputAcknowledgements(t *testing.T) {
	digest := strings.Repeat("d", 64)
	fingerprintOne := strings.Repeat("1", 64)
	fingerprintTwo := strings.Repeat("2", 64)
	trace := Trace{
		Events: []engine.Event{
			{Type: engine.EventCommitted, RunID: "run-replay", Sequence: 1, Metadata: map[string]any{
				"operation_id": "run-replay/op-000001/admission", "fingerprint": fingerprintOne, "revision": int64(1),
			}},
			{Type: engine.EventCapability, RunID: "run-replay", Sequence: 2, Metadata: map[string]any{"digest": digest}},
			{Type: engine.EventInputClaimed, RunID: "run-replay", Sequence: 3, Metadata: map[string]any{"claim_id": "claim-1"}},
			{Type: engine.EventCommitted, RunID: "run-replay", Sequence: 4, Metadata: map[string]any{
				"operation_id": "run-replay/op-000002/input", "fingerprint": fingerprintTwo, "revision": float64(2),
			}},
			{Type: engine.EventInputAcked, RunID: "run-replay", Sequence: 5, Metadata: map[string]any{
				"claim_id": "claim-1", "history_revision": float64(2),
			}},
		},
		Result: engine.RunResult{
			RunID: "run-replay", Reason: schema.TerminalCompleted, HistoryRevision: 2,
			Capability: tools.CapabilityRevision{Sequence: 1, Digest: digest},
		},
	}
	summary, err := Replay(trace)
	if err != nil {
		t.Fatal(err)
	}
	if summary.HistoryCommits != 2 || summary.LastHistoryRevision != 2 || summary.CapabilitySnapshots != 1 || summary.InputClaims != 1 || summary.InputAcknowledgements != 1 || summary.PendingInputClaims != 0 {
		t.Fatalf("summary=%#v", summary)
	}

	trace.Events[3].Metadata["revision"] = float64(4)
	if _, err := Replay(trace); err == nil || !strings.Contains(err.Error(), "does not follow") {
		t.Fatalf("revision corruption was not rejected: %v", err)
	}
}

func TestInboxClaimAckAndReconcile(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-inbox"
	for _, content := range []string{"first", "second"} {
		if _, err := store.Enqueue(context.Background(), engine.QueuedInput{RunID: runID, Kind: engine.InputUserSteer, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := store.Claim(context.Background(), runID, "turn-one", 10)
	if err != nil || len(claim.Items) != 2 {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if err := store.Release(context.Background(), claim.ID); err != nil {
		t.Fatal(err)
	}
	claim, err = store.Claim(context.Background(), runID, "turn-two", 10)
	if err != nil || len(claim.Items) != 2 {
		t.Fatalf("reclaim=%#v err=%v", claim, err)
	}
	if err := store.Ack(context.Background(), claim.ID, 4); err != nil {
		t.Fatal(err)
	}
	if empty, err := store.Claim(context.Background(), runID, "turn-two", 10); err != nil || empty.ID != "" {
		t.Fatalf("acked inputs reclaimed: %#v err=%v", empty, err)
	}

	if _, err := store.Enqueue(context.Background(), engine.QueuedInput{RunID: runID, Kind: engine.InputTool, Content: "machine input"}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.Claim(context.Background(), runID, "turn-three", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reconcile(context.Background(), runID, map[string]int64{pending.ID: 7}); err != nil {
		t.Fatal(err)
	}
	if empty, err := store.Claim(context.Background(), runID, "turn-four", 10); err != nil || empty.ID != "" {
		t.Fatalf("committed claim was not acknowledged: %#v err=%v", empty, err)
	}

	if _, err := store.Enqueue(context.Background(), engine.QueuedInput{RunID: runID, Kind: engine.InputTask, Content: "task output"}); err != nil {
		t.Fatal(err)
	}
	orphan, err := store.Claim(context.Background(), runID, "turn-five", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reconcile(context.Background(), runID, nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Claim(context.Background(), runID, "turn-six", 10)
	if err != nil || recovered.ID == "" || recovered.ID == orphan.ID || len(recovered.Items) != 1 {
		t.Fatalf("orphaned claim not released: orphan=%#v recovered=%#v err=%v", orphan, recovered, err)
	}
}
