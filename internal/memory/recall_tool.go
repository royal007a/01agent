package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type RecallTool struct{ archive *Archive }

func NewRecallTool(archive *Archive) *RecallTool { return &RecallTool{archive: archive} }

func (*RecallTool) Name() string { return "recall_context" }

func (*RecallTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "recall_context",
		Description: "Recall exact archived conversation or tool-output details from the current session using ranked lexical search. Use when a compaction marker says details were archived.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "minLength": 1},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
			},
			"required": []string{"query"},
		},
		Risk: schema.RiskRead, ParallelSafe: true,
	}
}

func (*RecallTool) Validate(arguments json.RawMessage) error {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if strings.TrimSpace(input.Query) == "" {
		return errors.New("query is required")
	}
	if input.Limit < 0 || input.Limit > 20 {
		return errors.New("limit must be between 1 and 20")
	}
	return nil
}

func (t *RecallTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	if t.archive == nil {
		return "", &tools.Error{Code: "memory_unavailable", Message: "memory archive is not configured", Fatal: true}
	}
	metadata, ok := runtimecontext.FromContext(ctx)
	if !ok || metadata.Scope == "" {
		return "", &tools.Error{Code: "memory_scope_missing", Message: "the run has no memory scope", Fatal: true}
	}
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	results, err := t.archive.Search(ctx, metadata.Scope, input.Query, input.Limit)
	if err != nil {
		return "", &tools.Error{Code: "memory_search", Message: err.Error(), Retryable: true}
	}
	encoded, _ := json.Marshal(map[string]any{"query": input.Query, "results": results})
	return string(encoded), nil
}
