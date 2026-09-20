package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/royal007a/01agent/internal/schema"
)

// Error lets a tool classify failures without leaking implementation types to
// the engine.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	Fatal     bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// BaseTool is the complete contract a physical capability must implement.
type BaseTool interface {
	Name() string
	Definition() schema.ToolDefinition
	Validate(arguments json.RawMessage) error
	Execute(ctx context.Context, arguments json.RawMessage) (string, error)
}

type PermissionDecision struct {
	Allowed          bool
	Reason           string
	RequiresApproval bool
	ApprovalID       string
}

type PermissionPolicy interface {
	CanUse(ctx context.Context, definition schema.ToolDefinition, arguments json.RawMessage) PermissionDecision
}

// ReadOnlyPolicy is the secure default for the initial course implementation.
type ReadOnlyPolicy struct{}

func (ReadOnlyPolicy) CanUse(_ context.Context, definition schema.ToolDefinition, _ json.RawMessage) PermissionDecision {
	if definition.Risk == schema.RiskRead || definition.Risk == schema.RiskState {
		return PermissionDecision{Allowed: true}
	}
	return PermissionDecision{Reason: "only read-only tools are permitted by the active policy"}
}
