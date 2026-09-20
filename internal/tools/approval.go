package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/royal007a/01agent/internal/schema"
)

type ApprovalRequest struct {
	RunID            string
	TurnID           string
	LeaseID          string
	Admission        int
	CapabilityDigest string
	ToolName         string
	Arguments        json.RawMessage
}

type ApprovalResolution struct {
	ID      string
	State   string
	Allowed bool
	Reason  string
}

type ApprovalBackend interface {
	Resolve(context.Context, ApprovalRequest) (ApprovalResolution, error)
}

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

type ApprovalPolicy struct {
	Backend ApprovalBackend
}

func (p ApprovalPolicy) CanUse(ctx context.Context, definition schema.ToolDefinition, arguments json.RawMessage) PermissionDecision {
	if definition.Risk == schema.RiskRead || definition.Risk == schema.RiskState {
		return PermissionDecision{Allowed: true}
	}
	approved, _ := ctx.Value(approvalKey{}).(map[string]bool)
	if approved[definition.Name] {
		return PermissionDecision{Allowed: true}
	}
	if p.Backend != nil {
		scope, ok := ApprovalScopeFromContext(ctx)
		if !ok {
			return PermissionDecision{Reason: "durable approval scope is unavailable"}
		}
		resolution, err := p.Backend.Resolve(ctx, ApprovalRequest{
			RunID: scope.RunID, TurnID: scope.TurnID, LeaseID: scope.LeaseID, Admission: scope.Admission,
			CapabilityDigest: scope.CapabilityDigest, ToolName: definition.Name, Arguments: append(json.RawMessage(nil), arguments...),
		})
		if err != nil {
			return PermissionDecision{Reason: "durable approval failed closed: " + err.Error()}
		}
		if resolution.Allowed {
			return PermissionDecision{Allowed: true, ApprovalID: resolution.ID}
		}
		reason := resolution.Reason
		if reason == "" {
			reason = fmt.Sprintf("approval %s is %s", resolution.ID, resolution.State)
		}
		return PermissionDecision{
			Reason: reason, RequiresApproval: resolution.State == "pending", ApprovalID: resolution.ID,
		}
	}
	return PermissionDecision{Reason: "tool requires explicit per-run approval: " + definition.Name, RequiresApproval: true}
}
