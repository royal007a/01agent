package dispatcher

import (
	"time"

	"github.com/royal007a/01agent/internal/computer"
)

const SchemaVersion = 1

type Phase string

const (
	PendingClaim     Phase = "pending_claim"
	Running          Phase = "running"
	AwaitingChildren Phase = "awaiting_children"
	PendingReview    Phase = "pending_review"
	Reviewing        Phase = "reviewing"
	AwaitingHuman    Phase = "awaiting_human"
	Canceling        Phase = "canceling"
	Succeeded        Phase = "succeeded"
	Failed           Phase = "failed"
	Canceled         Phase = "canceled"
)

type Execution struct {
	SchemaVersion   int                 `json:"schema_version"`
	ID              string              `json:"id"`
	TaskID          string              `json:"task_id"`
	AgentID         string              `json:"agent_id"`
	ReviewerID      string              `json:"reviewer_id"`
	Phase           Phase               `json:"phase"`
	Attempt         int                 `json:"attempt"`
	ReviewAttempt   int                 `json:"review_attempt"`
	ReworkCount     int                 `json:"rework_count"`
	MaxAttempts     int                 `json:"max_attempts"`
	TimeoutSeconds  int64               `json:"timeout_seconds"`
	TaskLeaseID     string              `json:"task_lease_id"`
	ActiveRunID     string              `json:"active_run_id,omitempty"`
	ActiveRunLease  string              `json:"active_run_lease_id,omitempty"`
	ActiveCommandID string              `json:"active_command_id,omitempty"`
	Result          *computer.RunResult `json:"result,omitempty"`
	CancelReason    string              `json:"cancel_reason,omitempty"`
	LastError       string              `json:"last_error,omitempty"`
	Revision        int64               `json:"revision"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

type Operation struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	ExecutionID string    `json:"execution_id"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
}

type State struct {
	SchemaVersion int                  `json:"schema_version"`
	Revision      int64                `json:"revision"`
	Executions    map[string]Execution `json:"executions"`
	Operations    []Operation          `json:"operations"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type StartInput struct {
	OperationID    string `json:"operation_id"`
	ID             string `json:"id"`
	TaskID         string `json:"task_id"`
	MaxAttempts    int    `json:"max_attempts"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
}

type CancelInput struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
}

type ReconcileResult struct {
	Advanced []string `json:"advanced,omitempty"`
	Waiting  []string `json:"waiting,omitempty"`
	Failed   []string `json:"failed,omitempty"`
}
