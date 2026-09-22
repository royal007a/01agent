package automation

import (
	"time"

	"github.com/royal007a/01agent/internal/workitem"
)

const SchemaVersion = 1

type Status string

const (
	Active Status = "active"
	Paused Status = "paused"
)

type RunState string

const (
	RunDispatched RunState = "dispatched"
	RunSucceeded  RunState = "succeeded"
	RunFailed     RunState = "failed"
	RunSkipped    RunState = "skipped_overlap"
)

type TaskTemplate struct {
	WorkspaceID    string                 `json:"workspace_id"`
	ChannelID      string                 `json:"channel_id"`
	CreatorID      string                 `json:"creator_id"`
	Title          string                 `json:"title"`
	Objective      string                 `json:"objective"`
	Requirements   []workitem.Requirement `json:"requirements"`
	Scope          workitem.Scope         `json:"scope"`
	StopConditions []string               `json:"stop_conditions"`
	AssigneeID     string                 `json:"assignee_id,omitempty"`
	Gate           workitem.GateSpec      `json:"gate"`
}

type Run struct {
	Sequence     int64     `json:"sequence"`
	TaskID       string    `json:"task_id,omitempty"`
	State        RunState  `json:"state"`
	ScheduledAt  time.Time `json:"scheduled_at"`
	DispatchedAt time.Time `json:"dispatched_at,omitempty"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
	Evidence     []string  `json:"evidence,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

type Automation struct {
	SchemaVersion         int          `json:"schema_version"`
	ID                    string       `json:"id"`
	Name                  string       `json:"name"`
	EverySeconds          int64        `json:"every_seconds"`
	NextRunAt             time.Time    `json:"next_run_at"`
	FailurePauseThreshold int          `json:"failure_pause_threshold"`
	ConsecutiveFailures   int          `json:"consecutive_failures"`
	Status                Status       `json:"status"`
	Template              TaskTemplate `json:"template"`
	Runs                  []Run        `json:"runs,omitempty"`
	Revision              int64        `json:"revision"`
	CreatedAt             time.Time    `json:"created_at"`
	UpdatedAt             time.Time    `json:"updated_at"`
}

type Operation struct {
	ID           string    `json:"id"`
	Fingerprint  string    `json:"fingerprint"`
	AutomationID string    `json:"automation_id"`
	Revision     int64     `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
}

type State struct {
	SchemaVersion int                   `json:"schema_version"`
	Revision      int64                 `json:"revision"`
	Automations   map[string]Automation `json:"automations"`
	Operations    []Operation           `json:"operations"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

type CreateInput struct {
	OperationID           string       `json:"operation_id"`
	ID                    string       `json:"id"`
	Name                  string       `json:"name"`
	EverySeconds          int64        `json:"every_seconds"`
	StartAt               time.Time    `json:"start_at,omitempty"`
	FailurePauseThreshold int          `json:"failure_pause_threshold"`
	Template              TaskTemplate `json:"template"`
}

type ReportInput struct {
	OperationID string   `json:"operation_id"`
	TaskID      string   `json:"task_id"`
	Success     bool     `json:"success"`
	Evidence    []string `json:"evidence"`
	Reason      string   `json:"reason"`
}

type TickResult struct {
	Dispatched []Run    `json:"dispatched,omitempty"`
	Skipped    []Run    `json:"skipped,omitempty"`
	Paused     []string `json:"paused,omitempty"`
}
