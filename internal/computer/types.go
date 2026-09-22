package computer

import "time"

const SchemaVersion = 1

type Status string

const (
	Offline Status = "offline"
	Online  Status = "online"
)

type CommandState string

const (
	CommandQueued CommandState = "queued"
	CommandSent   CommandState = "sent"
	CommandAcked  CommandState = "acked"
	CommandFailed CommandState = "failed"
)

type RunMode string

const (
	RunExecute RunMode = "execute"
	RunReview  RunMode = "review"
)

type RunRequest struct {
	RunID                    string    `json:"run_id"`
	TaskID                   string    `json:"task_id"`
	AgentID                  string    `json:"agent_id"`
	Mode                     RunMode   `json:"mode"`
	Prompt                   string    `json:"prompt"`
	SessionID                string    `json:"session_id,omitempty"`
	AgentRevisionID          string    `json:"agent_revision_id"`
	RelationshipRevisionID   string    `json:"relationship_revision_id"`
	ExpectedCapabilityDigest string    `json:"expected_capability_digest"`
	TimeoutSeconds           int64     `json:"timeout_seconds"`
	MaxTurns                 int       `json:"max_turns"`
	ApprovedTools            []string  `json:"approved_tools,omitempty"`
	TaskRevision             int64     `json:"task_revision"`
	TaskContractRevision     int64     `json:"task_contract_revision"`
	DispatchedAt             time.Time `json:"dispatched_at"`
}

type RunResult struct {
	RunID        string    `json:"run_id"`
	TaskID       string    `json:"task_id"`
	Success      bool      `json:"success"`
	Terminal     string    `json:"terminal_reason"`
	Summary      string    `json:"summary"`
	Evidence     []string  `json:"evidence,omitempty"`
	ArtifactURI  string    `json:"artifact_uri,omitempty"`
	ArtifactHash string    `json:"artifact_sha256,omitempty"`
	Decision     string    `json:"decision,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
}

type CapabilitySnapshot struct {
	Revision  int64             `json:"revision"`
	Digest    string            `json:"digest"`
	OS        string            `json:"os"`
	Arch      string            `json:"arch"`
	Runtime   string            `json:"runtime"`
	Tools     map[string]string `json:"tools,omitempty"`
	Sandboxes []string          `json:"sandboxes,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

type Computer struct {
	ID                string               `json:"id"`
	OwnerID           string               `json:"owner_id"`
	Name              string               `json:"name"`
	Status            Status               `json:"status"`
	Revision          int64                `json:"revision"`
	ConnectionLeaseID string               `json:"connection_lease_id,omitempty"`
	LeaseExpiresAt    time.Time            `json:"lease_expires_at,omitempty"`
	CurrentCapability string               `json:"current_capability_digest,omitempty"`
	Capabilities      []CapabilitySnapshot `json:"capabilities,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
}

type Binding struct {
	AgentID    string    `json:"agent_id"`
	ComputerID string    `json:"computer_id"`
	Revision   int64     `json:"revision"`
	BoundBy    string    `json:"bound_by"`
	BoundAt    time.Time `json:"bound_at"`
}

type RunLease struct {
	RunID      string    `json:"run_id"`
	AgentID    string    `json:"agent_id"`
	ComputerID string    `json:"computer_id"`
	LeaseID    string    `json:"lease_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Command struct {
	ID              string       `json:"id"`
	ComputerID      string       `json:"computer_id"`
	Kind            string       `json:"kind"`
	AgentID         string       `json:"agent_id"`
	Run             *RunRequest  `json:"run,omitempty"`
	CancelRunID     string       `json:"cancel_run_id,omitempty"`
	Result          *RunResult   `json:"result,omitempty"`
	State           CommandState `json:"state"`
	Attempt         int          `json:"attempt"`
	DeliveryLeaseID string       `json:"delivery_lease_id,omitempty"`
	Error           string       `json:"error,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

type Operation struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Fingerprint string    `json:"fingerprint"`
	EntityID    string    `json:"entity_id,omitempty"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type State struct {
	SchemaVersion int                 `json:"schema_version"`
	Revision      int64               `json:"revision"`
	Computers     map[string]Computer `json:"computers"`
	Bindings      map[string]Binding  `json:"bindings"`
	RunLeases     map[string]RunLease `json:"run_leases"`
	Commands      map[string]Command  `json:"commands"`
	Operations    []Operation         `json:"operations"`
	UpdatedAt     time.Time           `json:"updated_at"`
}

type RegisterInput struct {
	OperationID string `json:"operation_id"`
	ID          string `json:"id"`
	OwnerID     string `json:"owner_id"`
	Name        string `json:"name"`
}

type Hello struct {
	ComputerID string            `json:"computer_id"`
	LeaseID    string            `json:"lease_id"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Runtime    string            `json:"runtime"`
	Tools      map[string]string `json:"tools,omitempty"`
	Sandboxes  []string          `json:"sandboxes,omitempty"`
}

type RebindInput struct {
	OperationID             string `json:"operation_id"`
	AgentID                 string `json:"agent_id"`
	TargetComputerID        string `json:"target_computer_id"`
	ActorID                 string `json:"actor_id"`
	ExpectedBindingRevision int64  `json:"expected_binding_revision"`
}

type RunLeaseInput struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id"`
	AgentID     string `json:"agent_id"`
	ComputerID  string `json:"computer_id"`
	LeaseID     string `json:"lease_id"`
	TTLSeconds  int64  `json:"ttl_seconds"`
}

type CommandAck struct {
	CommandID string     `json:"command_id"`
	Success   bool       `json:"success"`
	Error     string     `json:"error,omitempty"`
	Result    *RunResult `json:"result,omitempty"`
}

type ClientMessage struct {
	Type  string      `json:"type"`
	Hello *Hello      `json:"hello,omitempty"`
	Ack   *CommandAck `json:"ack,omitempty"`
}

type ServerMessage struct {
	Type    string   `json:"type"`
	Command *Command `json:"command,omitempty"`
}
