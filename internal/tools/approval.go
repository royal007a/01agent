package tools

import (
	"context"
	"encoding/json"

	"github.com/royal007a/01agent/internal/schema"
)

type approvalKey struct{}

func WithApprovedTools(ctx context.Context, names []string) context.Context {
	approved := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			approved[name] = true
		}
	}
	return context.WithValue(ctx, approvalKey{}, approved)
}

type ApprovalPolicy struct{}

func (ApprovalPolicy) CanUse(ctx context.Context, definition schema.ToolDefinition, _ json.RawMessage) PermissionDecision {
	if definition.Risk == schema.RiskRead {
		return PermissionDecision{Allowed: true}
	}
	approved, _ := ctx.Value(approvalKey{}).(map[string]bool)
	if approved[definition.Name] {
		return PermissionDecision{Allowed: true}
	}
	return PermissionDecision{Reason: "tool requires explicit per-run approval: " + definition.Name, RequiresApproval: true}
}
