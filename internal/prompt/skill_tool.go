package prompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type ReadSkillTool struct{}

func NewReadSkillTool() *ReadSkillTool { return &ReadSkillTool{} }

func (*ReadSkillTool) Name() string { return "read_skill" }

func (*ReadSkillTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "read_skill",
		Description: "Load the complete instructions for one skill listed in the system prompt. The content comes from the immutable skill snapshot for this run.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}},
			"required":   []string{"name"},
		},
		Risk: schema.RiskRead, ParallelSafe: true,
	}
}

func (*ReadSkillTool) Validate(arguments json.RawMessage) error {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if !safeSkillName.MatchString(strings.TrimSpace(input.Name)) {
		return errors.New("skill name is invalid")
	}
	return nil
}

func (*ReadSkillTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	snapshot, ok := SnapshotFromContext(ctx)
	if !ok {
		return "", &tools.Error{Code: "skill_snapshot_missing", Message: "the run has no pinned skill snapshot", Fatal: true}
	}
	skill, exists := snapshot.Skills[strings.TrimSpace(input.Name)]
	if !exists {
		return "", &tools.Error{Code: "unknown_skill", Message: fmt.Sprintf("skill %q is not present in this run's capability snapshot", input.Name), Retryable: true}
	}
	encoded, _ := json.Marshal(map[string]string{
		"name": skill.Name, "description": skill.Description, "digest": skill.Digest, "content": skill.Body,
	})
	return string(encoded), nil
}
