package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/runtimecontext"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

const systemPrompt = `You are an isolated read-only Subagent. Investigate the delegated task using only the supplied read capabilities. Do not ask to mutate files, execute commands, contact external systems, or spawn another agent. Return a concise evidence-based summary for the parent Agent, including uncertainty and relevant paths or source identifiers.`

type Config struct {
	WorkDir       string
	MaxTurns      int
	MaxTokens     int64
	Timeout       time.Duration
	MaxOutput     int
	MaxConcurrent int
	Store         engine.RunStore
}

type Tool struct {
	provider   provider.LLMProvider
	childTools []tools.BaseTool
	config     Config
	semaphore  chan struct{}
}

type arguments struct {
	Task string `json:"task"`
}

type Result struct {
	RunID        string                `json:"run_id"`
	ParentRunID  string                `json:"parent_run_id,omitempty"`
	ParentTurnID string                `json:"parent_turn_id,omitempty"`
	Reason       schema.TerminalReason `json:"reason"`
	Turns        int                   `json:"turns"`
	Usage        schema.Usage          `json:"usage"`
	Summary      string                `json:"summary"`
	Truncated    bool                  `json:"truncated,omitempty"`
}

func NewTool(model provider.LLMProvider, childTools []tools.BaseTool, config Config) (*Tool, error) {
	if model == nil {
		return nil, errors.New("subagent: provider is required")
	}
	if strings.TrimSpace(config.WorkDir) == "" {
		return nil, errors.New("subagent: workdir is required")
	}
	filtered := make([]tools.BaseTool, 0, len(childTools))
	seen := make(map[string]bool)
	for _, item := range childTools {
		if item == nil || item.Name() == "spawn_subagent" || item.Definition().Risk != schema.RiskRead {
			continue
		}
		if seen[item.Name()] {
			return nil, fmt.Errorf("subagent: duplicate child tool %q", item.Name())
		}
		seen[item.Name()] = true
		filtered = append(filtered, item)
	}
	if len(filtered) == 0 {
		return nil, errors.New("subagent: at least one read-only child tool is required")
	}
	if config.MaxTurns <= 0 {
		config.MaxTurns = 8
	}
	if config.MaxTokens <= 0 {
		config.MaxTokens = 16_000
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.MaxOutput <= 0 {
		config.MaxOutput = 8 << 10
	}
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 2
	}
	return &Tool{provider: model, childTools: filtered, config: config, semaphore: make(chan struct{}, config.MaxConcurrent)}, nil
}

func (*Tool) Name() string { return "spawn_subagent" }

func (*Tool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "spawn_subagent",
		Description: "Delegate a complex read-only exploration to a bounded child Agent with a fresh isolated context. The parent receives only the child's concise result.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"task": map[string]any{"type": "string", "minLength": 1, "maxLength": 16384}},
			"required":   []string{"task"},
		},
		Risk: schema.RiskRead, ParallelSafe: true,
	}
}

func (*Tool) Validate(raw json.RawMessage) error {
	var input arguments
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if strings.TrimSpace(input.Task) == "" {
		return errors.New("task is required")
	}
	return nil
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var input arguments
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	select {
	case t.semaphore <- struct{}{}:
		defer func() { <-t.semaphore }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	registry := tools.NewRegistry(tools.WithPermissionPolicy(tools.ReadOnlyPolicy{}), tools.WithMaxParallel(4))
	for _, item := range t.childTools {
		if err := registry.Register(item); err != nil {
			return "", &tools.Error{Code: "subagent_registry", Message: err.Error(), Fatal: true}
		}
	}
	metadata, _ := runtimecontext.FromContext(ctx)
	scope, _ := tools.ApprovalScopeFromContext(ctx)
	childRunID := newID("subrun")
	agent, err := engine.New(t.provider, registry, engine.Config{
		WorkDir: t.config.WorkDir, SystemPrompt: systemPrompt, MaxTurns: t.config.MaxTurns,
		MaxTokens: t.config.MaxTokens, MaxRepeatedCall: 2, Timeout: t.config.Timeout,
		RunID: childRunID, Store: t.config.Store, MemoryScope: metadata.Scope,
		ParentRunID: metadata.RunID, ParentTurnID: scope.TurnID,
	})
	if err != nil {
		return "", &tools.Error{Code: "subagent_init", Message: err.Error(), Fatal: true}
	}
	result, runErr := agent.Run(ctx, strings.TrimSpace(input.Task))
	if runErr != nil {
		return "", &tools.Error{Code: "subagent_run", Message: runErr.Error(), Retryable: true}
	}
	summary := strings.TrimSpace(result.FinalMessage.Content)
	if summary == "" {
		for index := len(result.Messages) - 1; index >= 0; index-- {
			if result.Messages[index].Role == schema.RoleAssistant && strings.TrimSpace(result.Messages[index].Content) != "" {
				summary = strings.TrimSpace(result.Messages[index].Content)
				break
			}
		}
	}
	summary, truncated := truncateUTF8(summary, t.config.MaxOutput)
	encoded, _ := json.Marshal(Result{
		RunID: result.RunID, ParentRunID: result.ParentRunID, ParentTurnID: result.ParentTurnID,
		Reason: result.Reason, Turns: result.Turns, Usage: result.Usage, Summary: summary, Truncated: truncated,
	})
	return string(encoded), nil
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "\n...[subagent summary truncated]", true
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
