package agentregistry

import (
	"context"
	"errors"
	"testing"
)

func TestPersistentIdentityRelationshipRevisionAndSessionHandoff(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.CurrentSessionGeneration != 1 || len(created.AgentRevisions) != 1 || len(created.RelationshipRevisions) != 1 {
		t.Fatalf("created=%+v", created)
	}
	updated, err := store.ReviseRelationships(context.Background(), created.ID, RelationshipUpdate{
		OperationID: "op-relationships", ExpectedRevision: created.Revision, ActorID: "owner", MyRole: "lead engineer",
		Reason: "database delegation proved effective", Teammates: []TeammateRelationship{{
			AgentID: "lucy", Role: "database engineer", DelegateWhen: []string{"schema or query work"},
			Inputs: []string{"goal", "SQL", "scope"}, ReportBackWith: []string{"change", "tests", "rollback"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentRelationshipRevision != "relationship-v2" || len(updated.RelationshipRevisions) != 2 || updated.RelationshipRevisions[0].MyRole != "engineer" {
		t.Fatalf("relationship history=%+v", updated.RelationshipRevisions)
	}
	handoff := SessionHandoff{Scope: "agent/channel", CurrentTask: "task-1", OwnedTasks: []string{"task-1"}, References: []string{"thread-1", "artifact-1"}, ConfirmedFacts: []string{"tests pass"}, UnresolvedItems: []string{"production verification"}}
	rotated, err := store.RotateSession(context.Background(), created.ID, SessionRotation{
		OperationID: "op-rotate", ExpectedRevision: updated.Revision, ExpectedGeneration: 1,
		ActorID: "owner", NewSessionID: "session-2", Handoff: handoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CurrentSessionGeneration != 2 || rotated.Sessions[0].State != SessionRetired || rotated.Sessions[0].Handoff == nil || rotated.Sessions[1].State != SessionActive {
		t.Fatalf("sessions=%+v", rotated.Sessions)
	}
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 3 || loaded.Sessions[0].Handoff.ConfirmedFacts[0] != "tests pass" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestAgentOperationsAreIdempotentAndRevisionChecked(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := testCreate()
	first, err := store.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Create(context.Background(), input)
	if err != nil || replayed.Revision != first.Revision {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	update := RelationshipUpdate{OperationID: "op-update", ExpectedRevision: first.Revision, ActorID: "owner", MyRole: "lead", Reason: "clarify"}
	updated, err := store.ReviseRelationships(context.Background(), first.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := store.ReviseRelationships(context.Background(), first.ID, update)
	if err != nil || retried.Revision != updated.Revision {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	update.OperationID = "op-stale"
	if _, err := store.ReviseRelationships(context.Background(), first.ID, update); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale err=%v", err)
	}
}

func TestSessionRotationRejectsGenerationAndHandoffGaps(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RotateSession(context.Background(), agent.ID, SessionRotation{
		OperationID: "op-bad-generation", ExpectedRevision: agent.Revision, ExpectedGeneration: 2,
		ActorID: "owner", NewSessionID: "session-2", Handoff: SessionHandoff{Scope: "scope", References: []string{"ref"}, ConfirmedFacts: []string{"fact"}},
	})
	if !errors.Is(err, ErrSession) {
		t.Fatalf("generation err=%v", err)
	}
	_, err = store.RotateSession(context.Background(), agent.ID, SessionRotation{
		OperationID: "op-bad-handoff", ExpectedRevision: agent.Revision, ExpectedGeneration: 1,
		ActorID: "owner", NewSessionID: "session-2", Handoff: SessionHandoff{Scope: "scope"},
	})
	if err == nil {
		t.Fatal("rotation accepted incomplete handoff")
	}
}

func testCreate() CreateInput {
	return CreateInput{
		OperationID: "op-create", ID: "agent-lili", WorkspaceID: "workspace", Name: "Lili", CreatedBy: "owner",
		PromptRef: "prompts/lili.md", Skills: []string{"review", "coordination"}, Model: "model-a", MyRole: "engineer", SessionID: "session-1",
	}
}
