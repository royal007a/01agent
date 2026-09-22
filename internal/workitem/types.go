package workitem

import "time"

const SchemaVersion = 2

type State string

const (
	Todo       State = "todo"
	InProgress State = "in_progress"
	InReview   State = "in_review"
	Done       State = "done"
	Closed     State = "closed"
)

type GateKind string

const (
	GateHuman GateKind = "human"
	GateCode  GateKind = "code"
	GateAgent GateKind = "agent"
)

type GateDecision string

const (
	GatePass       GateDecision = "pass"
	GateReject     GateDecision = "reject"
	GateNeedsHuman GateDecision = "needs_human"
)

type Requirement struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Scope struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type GateSpec struct {
	Kind             GateKind `json:"kind"`
	ReviewerID       string   `json:"reviewer_id"`
	Checks           []string `json:"checks"`
	RequiredEvidence []string `json:"required_evidence"`
	OnReject         string   `json:"on_reject"`
}

type Claim struct {
	OwnerID   string    `json:"owner_id"`
	LeaseID   string    `json:"lease_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ArtifactRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type Artifact struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	Version       string    `json:"version"`
	Kind          string    `json:"kind"`
	URI           string    `json:"uri"`
	Digest        string    `json:"digest"`
	ProducerID    string    `json:"producer_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type Handoff struct {
	ContractRevision int64         `json:"contract_revision"`
	AuthorID         string        `json:"author_id"`
	Summary          string        `json:"summary"`
	Decisions        []string      `json:"decisions,omitempty"`
	Evidence         []string      `json:"evidence"`
	Artifacts        []ArtifactRef `json:"artifacts"`
	Remaining        []string      `json:"remaining,omitempty"`
	Risks            []string      `json:"risks,omitempty"`
	NextAction       string        `json:"next_action"`
	CreatedAt        time.Time     `json:"created_at"`
}

type GateResult struct {
	Decision        GateDecision  `json:"decision"`
	ReviewerID      string        `json:"reviewer_id"`
	ArtifactVersion []ArtifactRef `json:"artifact_versions"`
	Evidence        []string      `json:"evidence"`
	Reason          string        `json:"reason"`
	CreatedAt       time.Time     `json:"created_at"`
}

type Operation struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Fingerprint string    `json:"fingerprint"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type Task struct {
	SchemaVersion    int           `json:"schema_version"`
	ID               string        `json:"id"`
	WorkspaceID      string        `json:"workspace_id"`
	ChannelID        string        `json:"channel_id"`
	ThreadID         string        `json:"thread_id,omitempty"`
	ParentTaskID     string        `json:"parent_task_id,omitempty"`
	CreatorID        string        `json:"creator_id"`
	Title            string        `json:"title"`
	Objective        string        `json:"objective"`
	State            State         `json:"state"`
	Revision         int64         `json:"revision"`
	ContractRevision int64         `json:"contract_revision"`
	Requirements     []Requirement `json:"requirements"`
	Scope            Scope         `json:"scope"`
	StopConditions   []string      `json:"stop_conditions"`
	AssigneeID       string        `json:"assignee_id,omitempty"`
	Claim            *Claim        `json:"claim,omitempty"`
	Gate             GateSpec      `json:"gate"`
	ArtifactRefs     []ArtifactRef `json:"artifact_refs,omitempty"`
	Submissions      []Handoff     `json:"submissions,omitempty"`
	Reviews          []GateResult  `json:"reviews,omitempty"`
	ClosedReason     string        `json:"closed_reason,omitempty"`
	Operations       []Operation   `json:"operations"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type Create struct {
	OperationID    string        `json:"operation_id"`
	ID             string        `json:"id,omitempty"`
	WorkspaceID    string        `json:"workspace_id"`
	ChannelID      string        `json:"channel_id"`
	ThreadID       string        `json:"thread_id,omitempty"`
	ParentTaskID   string        `json:"parent_task_id,omitempty"`
	CreatorID      string        `json:"creator_id"`
	Title          string        `json:"title"`
	Objective      string        `json:"objective"`
	Requirements   []Requirement `json:"requirements"`
	Scope          Scope         `json:"scope"`
	StopConditions []string      `json:"stop_conditions"`
	AssigneeID     string        `json:"assignee_id,omitempty"`
	Gate           GateSpec      `json:"gate"`
}

type ContractUpdate struct {
	OperationID      string        `json:"operation_id"`
	ExpectedRevision int64         `json:"expected_revision"`
	ActorID          string        `json:"actor_id"`
	Requirements     []Requirement `json:"requirements"`
	Scope            Scope         `json:"scope"`
	StopConditions   []string      `json:"stop_conditions"`
	Gate             GateSpec      `json:"gate"`
}

type ArtifactInput struct {
	OperationID      string `json:"operation_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	OwnerID          string `json:"owner_id"`
	LeaseID          string `json:"lease_id"`
	ID               string `json:"id,omitempty"`
	Version          string `json:"version"`
	Kind             string `json:"kind"`
	URI              string `json:"uri"`
	Digest           string `json:"digest"`
}
