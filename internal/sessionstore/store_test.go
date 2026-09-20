package sessionstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/schema"
)

func TestSessionBeginCommitAndIdempotentReplay(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	admission, err := store.Begin(context.Background(), "session-one", "message-one", "first prompt", workDir, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if admission.Resuming || admission.Cached != nil || admission.State.Revision != 1 || admission.Pending.RunID != "run-one" {
		t.Fatalf("admission=%#v", admission)
	}
	now := time.Now().UTC()
	result := engine.RunResult{
		RunID: "run-one", Reason: schema.TerminalCompleted, FinalMessage: schema.Message{Role: schema.RoleAssistant, Content: "first answer"},
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "system"}, {Role: schema.RoleUser, Content: "first prompt"},
			{Role: schema.RoleAssistant, Content: "first answer"},
		},
		Turns: 1, StartedAt: now.Add(-time.Second), CompletedAt: now,
	}
	state, err := store.Commit(context.Background(), "session-one", "message-one", admission.State.Revision, result)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || state.Pending != nil || len(state.Turns) != 1 || len(state.Messages) != 3 {
		t.Fatalf("state=%#v", state)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.Begin(context.Background(), "session-one", "message-one", "first prompt", workDir, "different-run")
	if err != nil {
		t.Fatal(err)
	}
	if replay.Cached == nil || replay.Cached.RunID != "run-one" || replay.Pending.RunID != "" {
		t.Fatalf("replay=%#v", replay)
	}
	if _, err := reopened.Begin(context.Background(), "session-one", "message-one", "changed prompt", workDir, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay error=%v", err)
	}
}

func TestSessionPendingTurnSurvivesRestartAndBlocksOtherOperations(t *testing.T) {
	dir := t.TempDir()
	workDir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Begin(context.Background(), "session-two", "message-one", "prompt", workDir, "run-pending")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := reopened.Begin(context.Background(), "session-two", "message-one", "prompt", workDir, "unused")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resuming || resumed.Pending.RunID != first.Pending.RunID || resumed.State.Revision != first.State.Revision {
		t.Fatalf("resumed=%#v first=%#v", resumed, first)
	}
	if _, err := reopened.Begin(context.Background(), "session-two", "message-two", "later", workDir, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy error=%v", err)
	}
	otherWorkDir := filepath.Join(workDir, "other")
	if _, err := reopened.Begin(context.Background(), "session-two", "message-one", "prompt", otherWorkDir, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("workspace conflict error=%v", err)
	}
}

func TestOperationIDRejectsChangedExecutionSemantics(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if _, err := store.BeginWithSemantics(context.Background(), "session-policy", "message-one", "same prompt", workDir, "run-one", `{"plan_mode":true}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginWithSemantics(context.Background(), "session-policy", "message-one", "same prompt", workDir, "run-two", `{"plan_mode":false}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed semantics error = %v, want ErrConflict", err)
	}
}

func TestSessionLanesSerializeOneSessionButNotOthers(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := store.Acquire(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease, err := store.Acquire(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.Acquire(ctx, "same"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-session acquire error=%v", err)
	}
	releaseFirst()
	releaseAgain, err := store.Acquire(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	releaseAgain()
}
