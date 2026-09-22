package agentregistry

import "time"

const SchemaVersion = 1

type SessionState string

const (
	SessionActive  SessionState = "active"
	SessionRetired SessionState = "retired"
)

type AgentRevision struct {
	ID        string    `json:"id"`
	Version   int64     `json:"version"`
	PromptRef string    `json:"prompt_ref"`
	Skills    []string  `json:"skills,omitempty"`
	Model     string    `json:"model"`
	CreatedBy string    `json:"created_by"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type TeammateRelationship struct {
	AgentID         string   `json:"agent_id"`
	Role            string   `json:"role"`
	DelegateWhen    []string `json:"delegate_when,omitempty"`
	CollaborateWhen []string `json:"collaborate_when,omitempty"`
	Inputs          []string `json:"inputs,omitempty"`
	ReportBackWith  []string `json:"report_back_with,omitempty"`
}

type RelationshipRevision struct {
	ID        string                 `json:"id"`
	Version   int64                  `json:"version"`
	MyRole    string                 `json:"my_role"`
	Teammates []TeammateRelationship `json:"teammates,omitempty"`
	CreatedBy string                 `json:"created_by"`
	Reason    string                 `json:"reason"`
	CreatedAt time.Time              `json:"created_at"`
}

type SessionHandoff struct {
	Scope           string   `json:"scope"`
	CurrentTask     string   `json:"current_task,omitempty"`
	OwnedTasks      []string `json:"owned_tasks,omitempty"`
	PendingReviews  []string `json:"pending_reviews,omitempty"`
	AvailableTasks  []string `json:"available_tasks,omitempty"`
	References      []string `json:"references,omitempty"`
	ConfirmedFacts  []string `json:"confirmed_facts,omitempty"`
	UnresolvedItems []string `json:"unresolved_items,omitempty"`
}

type SessionGeneration struct {
	Generation int64           `json:"generation"`
	SessionID  string          `json:"session_id"`
	State      SessionState    `json:"state"`
	CreatedAt  time.Time       `json:"created_at"`
	RetiredAt  time.Time       `json:"retired_at,omitempty"`
	Handoff    *SessionHandoff `json:"handoff,omitempty"`
}

type Operation struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Fingerprint string    `json:"fingerprint"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type Agent struct {
	SchemaVersion               int                    `json:"schema_version"`
	ID                          string                 `json:"id"`
	WorkspaceID                 string                 `json:"workspace_id"`
	Name                        string                 `json:"name"`
	Revision                    int64                  `json:"revision"`
	CurrentAgentRevisionID      string                 `json:"current_agent_revision_id"`
	CurrentRelationshipRevision string                 `json:"current_relationship_revision_id"`
	CurrentSessionGeneration    int64                  `json:"current_session_generation"`
	AgentRevisions              []AgentRevision        `json:"agent_revisions"`
	RelationshipRevisions       []RelationshipRevision `json:"relationship_revisions"`
	Sessions                    []SessionGeneration    `json:"sessions"`
	Operations                  []Operation            `json:"operations"`
	CreatedAt                   time.Time              `json:"created_at"`
	UpdatedAt                   time.Time              `json:"updated_at"`
}

type CreateInput struct {
	OperationID string                 `json:"operation_id"`
	ID          string                 `json:"id"`
	WorkspaceID string                 `json:"workspace_id"`
	Name        string                 `json:"name"`
	CreatedBy   string                 `json:"created_by"`
	PromptRef   string                 `json:"prompt_ref"`
	Skills      []string               `json:"skills,omitempty"`
	Model       string                 `json:"model"`
	MyRole      string                 `json:"my_role"`
	Teammates   []TeammateRelationship `json:"teammates,omitempty"`
	SessionID   string                 `json:"session_id"`
}

type RelationshipUpdate struct {
	OperationID      string                 `json:"operation_id"`
	ExpectedRevision int64                  `json:"expected_revision"`
	ActorID          string                 `json:"actor_id"`
	MyRole           string                 `json:"my_role"`
	Teammates        []TeammateRelationship `json:"teammates,omitempty"`
	Reason           string                 `json:"reason"`
}

type SessionRotation struct {
	OperationID        string         `json:"operation_id"`
	ExpectedRevision   int64          `json:"expected_revision"`
	ExpectedGeneration int64          `json:"expected_generation"`
	ActorID            string         `json:"actor_id"`
	NewSessionID       string         `json:"new_session_id"`
	Handoff            SessionHandoff `json:"handoff"`
}
