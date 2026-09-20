package tools

import (
	"context"
	"errors"
)

type ApprovalScope struct {
	RunID            string
	TurnID           string
	LeaseID          string
	Admission        int
	CapabilityDigest string
}

type approvalScopeKey struct{}

func WithApprovalScope(ctx context.Context, scope ApprovalScope) context.Context {
	return context.WithValue(ctx, approvalScopeKey{}, scope)
}

func ApprovalScopeFromContext(ctx context.Context) (ApprovalScope, bool) {
	scope, ok := ctx.Value(approvalScopeKey{}).(ApprovalScope)
	return scope, ok && scope.RunID != "" && scope.TurnID != "" && scope.LeaseID != "" && scope.CapabilityDigest != ""
}

var ErrStaleExecutionLease = errors.New("tool execution lease is no longer active")

// ExecutionLease is revalidated after permission/approval and immediately
// before a physical tool is invoked. This rejects callbacks from cancelled or
// superseded turns even when their permission decision arrives late.
type ExecutionLease interface {
	Validate() error
}

type executionLeaseKey struct{}

func WithExecutionLease(ctx context.Context, lease ExecutionLease) context.Context {
	return context.WithValue(ctx, executionLeaseKey{}, lease)
}

func ValidateExecutionLease(ctx context.Context) error {
	lease, _ := ctx.Value(executionLeaseKey{}).(ExecutionLease)
	if lease == nil {
		return nil
	}
	return lease.Validate()
}
