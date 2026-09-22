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

func TestAgentSelectionRequiresComparableNonRegressingCandidate(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := store.ProposeAgentRevision(context.Background(), agent.ID, AgentRevisionProposal{
		OperationID: "op-propose", ExpectedRevision: agent.Revision, ActorID: "owner",
		PromptRef: "prompts/lili-v2.md", Skills: []string{"review", "coordination", "database"}, Model: "model-a", Reason: "improve database handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposed.CurrentAgentRevisionID != "agent-v1" || len(proposed.AgentRevisions) != 2 {
		t.Fatalf("proposal activated before selection: %+v", proposed)
	}
	bad := SelectionInput{
		OperationID: "op-bad-select", ExpectedRevision: proposed.Revision, ID: "selection-bad",
		BaselineRevisionID: "agent-v1", CandidateRevisionID: "agent-v2", SuiteID: "suite", Model: "model-a", TokenBudget: 1000,
		BaselineScore: 1, CandidateScore: .9, TeamCompatible: true, Evidence: []string{"report"}, Decision: SelectionAccept, Reason: "try", SelectedBy: "owner",
	}
	if _, err := store.SelectAgentRevision(context.Background(), agent.ID, bad); err == nil {
		t.Fatal("regressing candidate was accepted")
	}
	good := bad
	good.OperationID, good.ID, good.CandidateScore, good.Reason = "op-good-select", "selection-good", 1, "non-regressing and compatible"
	selected, err := store.SelectAgentRevision(context.Background(), agent.ID, good)
	if err != nil {
		t.Fatal(err)
	}
	if selected.CurrentAgentRevisionID != "agent-v2" || len(selected.Selections) != 1 {
		t.Fatalf("selection=%+v", selected.Selections)
	}
}

func TestRelationshipPRAcceptanceAndStaleBaseline(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := store.Create(context.Background(), testCreate())
	if err != nil {
		t.Fatal(err)
	}
	proposalInput := RelationshipPRInput{
		OperationID: "op-pr", ExpectedRevision: agent.Revision, ID: "pr-database", ActorID: "owner", MyRole: "lead",
		Problem: "reviewer asked for rollback evidence", TeamCheck: []string{"reviewer consumes new report format"},
		Teammates: []TeammateRelationship{{AgentID: "lucy", Role: "database", DelegateWhen: []string{"schema"}, ReportBackWith: []string{"tests", "rollback"}}},
	}
	proposed, err := store.CreateRelationshipPR(context.Background(), agent.ID, proposalInput)
	if err != nil {
		t.Fatal(err)
	}
	if proposed.CurrentRelationshipRevision != "relationship-v1" || proposed.RelationshipPRs[0].State != ProposalOpen {
		t.Fatalf("proposal=%+v", proposed.RelationshipPRs)
	}
	accepted, err := store.ReviewRelationshipPR(context.Background(), agent.ID, "pr-database", RelationshipPRReview{
		OperationID: "op-pr-accept", ExpectedRevision: proposed.Revision, ReviewerID: "reviewer", Decision: ProposalAccepted, Reason: "team check passed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.CurrentRelationshipRevision != "relationship-v2" || accepted.RelationshipPRs[0].State != ProposalAccepted {
		t.Fatalf("accepted=%+v", accepted)
	}
	staleInput := proposalInput
	staleInput.OperationID, staleInput.ID, staleInput.ExpectedRevision = "op-pr-stale", "pr-stale", accepted.Revision
	stale, err := store.CreateRelationshipPR(context.Background(), agent.ID, staleInput)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReviseRelationships(context.Background(), agent.ID, RelationshipUpdate{
		OperationID: "op-direct-change", ExpectedRevision: stale.Revision, ActorID: "owner", MyRole: "director", Reason: "urgent", Teammates: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReviewRelationshipPR(context.Background(), agent.ID, "pr-stale", RelationshipPRReview{
		OperationID: "op-stale-review", ExpectedRevision: changed.Revision, ReviewerID: "reviewer", Decision: ProposalAccepted, Reason: "late",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale PR err=%v", err)
	}
}

func testCreate() CreateInput {
	return CreateInput{
		OperationID: "op-create", ID: "agent-lili", WorkspaceID: "workspace", Name: "Lili", CreatedBy: "owner",
		PromptRef: "prompts/lili.md", Skills: []string{"review", "coordination"}, Model: "model-a", MyRole: "engineer", SessionID: "session-1",
	}
}
