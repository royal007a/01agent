package team

import "time"

const SchemaVersion = 1

type RoleRequirement struct {
	Role                string   `json:"role"`
	RequiredSkills      []string `json:"required_skills,omitempty"`
	PreferredSkills     []string `json:"preferred_skills,omitempty"`
	RequiredTools       []string `json:"required_tools,omitempty"`
	RequiredPermissions []string `json:"required_permissions,omitempty"`
}

type Template struct {
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Roles []RoleRequirement `json:"roles"`
}

type MemberLock struct {
	Role                   string  `json:"role"`
	AgentID                string  `json:"agent_id"`
	AgentRevisionID        string  `json:"agent_revision_id"`
	RelationshipRevisionID string  `json:"relationship_revision_id"`
	Model                  string  `json:"model"`
	EstimatedCostUSD       float64 `json:"estimated_cost_usd"`
	SelectionScore         float64 `json:"selection_score"`
}

type Lockfile struct {
	SchemaVersion int          `json:"schema_version"`
	TeamID        string       `json:"team_id"`
	Version       int64        `json:"version"`
	WorkspaceID   string       `json:"workspace_id"`
	Objective     string       `json:"objective"`
	Template      Template     `json:"template"`
	Members       []MemberLock `json:"members"`
	TotalCostUSD  float64      `json:"total_cost_usd"`
	Evidence      []string     `json:"evidence"`
	CreatedBy     string       `json:"created_by"`
	CreatedAt     time.Time    `json:"created_at"`
}

type Team struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	CurrentVersion int64      `json:"current_version"`
	Lockfiles      []Lockfile `json:"lockfiles"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Operation struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	TeamID      string    `json:"team_id"`
	Version     int64     `json:"version"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type State struct {
	SchemaVersion int             `json:"schema_version"`
	Revision      int64           `json:"revision"`
	Teams         map[string]Team `json:"teams"`
	Operations    []Operation     `json:"operations"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type SelectInput struct {
	OperationID         string             `json:"operation_id"`
	TeamID              string             `json:"team_id"`
	WorkspaceID         string             `json:"workspace_id"`
	ExpectedTeamVersion int64              `json:"expected_team_version"`
	Objective           string             `json:"objective"`
	Template            Template           `json:"template"`
	CandidateAgentIDs   []string           `json:"candidate_agent_ids"`
	DeliveryScores      map[string]float64 `json:"delivery_scores,omitempty"`
	ModelCostsUSD       map[string]float64 `json:"model_costs_usd"`
	BudgetUSD           float64            `json:"budget_usd"`
	CreatedBy           string             `json:"created_by"`
}
