package plan

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type ReadTool struct{ store *Store }
type UpdateTool struct{ store *Store }

func NewReadTool(store *Store) *ReadTool     { return &ReadTool{store: store} }
func NewUpdateTool(store *Store) *UpdateTool { return &UpdateTool{store: store} }

func (*ReadTool) Name() string   { return "read_plan" }
func (*UpdateTool) Name() string { return "update_plan" }

func (*ReadTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "read_plan", Description: "Read the canonical multi-step plan for the current session.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		Risk:        schema.RiskRead, ParallelSafe: true,
	}
}

func (*UpdateTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: "update_plan", Description: "Create or revision-CAS update the canonical plan and its PLAN.md/TODO.md projections for the current session.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"operation_id":      map[string]any{"type": "string", "minLength": 1},
				"expected_revision": map[string]any{"type": "integer", "minimum": 0},
				"objective":         map[string]any{"type": "string", "minLength": 1},
				"steps": map[string]any{
					"type": "array", "minItems": 1, "maxItems": 64,
					"items": map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{
							"id":     map[string]any{"type": "string", "minLength": 1},
							"title":  map[string]any{"type": "string", "minLength": 1},
							"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "blocked"}},
							"note":   map[string]any{"type": "string"},
						},
						"required": []string{"id", "title", "status"},
					},
				},
			},
			"required": []string{"operation_id", "expected_revision", "objective", "steps"},
		},
		Risk: schema.RiskState,
	}
}

func (*ReadTool) Validate(json.RawMessage) error { return nil }

func (*UpdateTool) Validate(arguments json.RawMessage) error {
	var input struct {
		OperationID      string `json:"operation_id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Objective        string `json:"objective"`
		Steps            []Step `json:"steps"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if input.OperationID == "" || input.Objective == "" || len(input.Steps) == 0 {
		return errors.New("operation_id, objective, and steps are required")
	}
	return nil
}

func (t *ReadTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	metadata, ok := runtimecontext.FromContext(ctx)
	if !ok || metadata.Scope == "" {
		return "", &tools.Error{Code: "plan_scope_missing", Message: "the run has no plan scope", Fatal: true}
	}
	state, err := t.store.Get(ctx, metadata.Scope)
	if err != nil {
		return "", &tools.Error{Code: "plan_not_found", Message: err.Error(), Retryable: true}
	}
	encoded, _ := json.Marshal(state)
	return string(encoded), nil
}

func (t *UpdateTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	metadata, ok := runtimecontext.FromContext(ctx)
	if !ok || metadata.Scope == "" {
		return "", &tools.Error{Code: "plan_scope_missing", Message: "the run has no plan scope", Fatal: true}
	}
	var input struct {
		OperationID      string `json:"operation_id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Objective        string `json:"objective"`
		Steps            []Step `json:"steps"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	state, err := t.store.Update(ctx, Update{
		Scope: metadata.Scope, OperationID: input.OperationID, ExpectedRevision: input.ExpectedRevision,
		Objective: input.Objective, Steps: input.Steps,
	})
	if err != nil {
		return "", &tools.Error{Code: "plan_conflict", Message: err.Error(), Retryable: true}
	}
	encoded, _ := json.Marshal(state)
	return string(encoded), nil
}
