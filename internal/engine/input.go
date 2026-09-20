package engine

import (
	"context"
	"time"
)

type InputKind string

const (
	InputUserSteer InputKind = "user_steer"
	InputTool      InputKind = "tool_input"
	InputTask      InputKind = "task_result"
)

type QueuedInput struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Kind      InputKind `json:"kind"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type InputClaim struct {
	ID        string        `json:"id"`
	RunID     string        `json:"run_id"`
	TurnID    string        `json:"turn_id"`
	Items     []QueuedInput `json:"items"`
	ClaimedAt time.Time     `json:"claimed_at"`
}

// InputQueue uses claim -> canonical history commit -> ack. Reconcile repairs
// the small crash window between a durable commit and its queue acknowledgement.
type InputQueue interface {
	Claim(context.Context, string, string, int) (InputClaim, error)
	Ack(context.Context, string, int64) error
	Release(context.Context, string) error
	Reconcile(context.Context, string, map[string]int64) error
}

type InputEnqueuer interface {
	Enqueue(context.Context, QueuedInput) (QueuedInput, error)
}

type InputDeliveryStatus struct {
	InputID        string    `json:"input_id"`
	ClaimID        string    `json:"claim_id,omitempty"`
	AckRevision    int64     `json:"ack_revision,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
}

type InputStatusReader interface {
	InputStatus(context.Context, string, string) (InputDeliveryStatus, error)
}
