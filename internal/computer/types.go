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
	ID         string       `json:"id"`
	ComputerID string       `json:"computer_id"`
	Kind       string       `json:"kind"`
	AgentID    string       `json:"agent_id"`
	State      CommandState `json:"state"`
	Attempt    int          `json:"attempt"`
	Error      string       `json:"error,omitempty"`
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
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
	CommandID string `json:"command_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
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
