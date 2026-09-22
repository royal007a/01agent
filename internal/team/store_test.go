package team

import (
	"context"
	"errors"
	"testing"

	"github.com/royal007a/01agent/internal/agentregistry"
)

func TestAutoSelectionCreatesImmutableVersionedTeamLockfiles(t *testing.T) {
	agents, err := agentregistry.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	createAgent(t, agents, "alice", []string{"go", "implementation"}, []string{"write_file"}, []string{"workspace_write"}, "model-a")
	createAgent(t, agents, "bob", []string{"review", "testing"}, []string{"read_file"}, []string{"workspace_read"}, "model-a")
	createAgent(t, agents, "carol", []string{"review", "testing", "security"}, []string{"read_file"}, []string{"workspace_read"}, "model-b")
	store, err := New(t.TempDir(), agents)
	if err != nil {
		t.Fatal(err)
	}
	input := SelectInput{
		OperationID: "op-team-v1", TeamID: "delivery", WorkspaceID: "workspace", Objective: "implement and independently review", CreatedBy: "owner",
		Template: Template{ID: "delivery-template", Name: "Delivery", Roles: []RoleRequirement{
			{Role: "implementer", RequiredSkills: []string{"implementation"}, RequiredTools: []string{"write_file"}, RequiredPermissions: []string{"workspace_write"}},
			{Role: "reviewer", RequiredSkills: []string{"review"}, RequiredTools: []string{"read_file"}, RequiredPermissions: []string{"workspace_read"}, PreferredSkills: []string{"security"}},
		}},
		CandidateAgentIDs: []string{"carol", "bob", "alice"}, DeliveryScores: map[string]float64{"alice": .8, "bob": .9, "carol": 1},
		ModelCostsUSD: map[string]float64{"model-a": 1, "model-b": 3}, BudgetUSD: 3,
	}
	first, err := store.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Members[0].AgentID != "alice" || first.Members[1].AgentID != "bob" || first.TotalCostUSD != 2 {
		t.Fatalf("first=%+v", first)
	}
	alice, err := agents.Get(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := agents.ProposeAgentRevision(context.Background(), "alice", agentregistry.AgentRevisionProposal{
		OperationID: "op-alice-v2", ExpectedRevision: alice.Revision, ActorID: "owner", PromptRef: "prompts/alice-v2.md",
		Skills: []string{"go", "implementation", "testing"}, Tools: []string{"write_file"}, Permissions: []string{"workspace_write"}, Model: "model-a", Reason: "add tested handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agents.SelectAgentRevision(context.Background(), "alice", agentregistry.SelectionInput{
		OperationID: "op-select-alice-v2", ExpectedRevision: proposed.Revision, ID: "select-alice-v2",
		BaselineRevisionID: "agent-v1", CandidateRevisionID: "agent-v2", SuiteID: "regression", Model: "model-a", TokenBudget: 1000,
		BaselineScore: 1, CandidateScore: 1, TeamCompatible: true, Evidence: []string{"15/15"}, Decision: agentregistry.SelectionAccept, Reason: "non-regressing", SelectedBy: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	input.OperationID, input.ExpectedTeamVersion = "op-team-v2", 1
	second, err := store.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Members[0].AgentRevisionID != "agent-v2" {
		t.Fatalf("second=%+v", second)
	}
	old, err := store.Get(context.Background(), "delivery", 1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Members[0].AgentRevisionID != "agent-v1" {
		t.Fatal("new selection rewrote old team lockfile")
	}
}

func TestAutoSelectionFailsWhenCapabilitiesOrBudgetDoNotFit(t *testing.T) {
	agents, err := agentregistry.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	createAgent(t, agents, "reader", []string{"review"}, []string{"read_file"}, []string{"workspace_read"}, "model-a")
	store, err := New(t.TempDir(), agents)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Select(context.Background(), SelectInput{
		OperationID: "op-no-team", TeamID: "team", WorkspaceID: "workspace", Objective: "write", CreatedBy: "owner",
		Template:          Template{ID: "writer", Name: "Writer", Roles: []RoleRequirement{{Role: "writer", RequiredTools: []string{"write_file"}}}},
		CandidateAgentIDs: []string{"reader"}, ModelCostsUSD: map[string]float64{"model-a": 1}, BudgetUSD: 1,
	})
	if !errors.Is(err, ErrNoSelection) {
		t.Fatalf("err=%v", err)
	}
}

func createAgent(t *testing.T, store *agentregistry.Store, id string, skills, tools, permissions []string, model string) {
	t.Helper()
	_, err := store.Create(context.Background(), agentregistry.CreateInput{
		OperationID: "create-" + id, ID: id, WorkspaceID: "workspace", Name: id, CreatedBy: "owner",
		PromptRef: "prompts/" + id + ".md", Skills: skills, Tools: tools, Permissions: permissions,
		Model: model, MyRole: id, SessionID: "session-" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
}
