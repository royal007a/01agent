package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/contextmanager"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/runstore"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type Suite struct {
	Name                string  `json:"name"`
	MinimumPassRate     float64 `json:"minimum_pass_rate"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	Cases               []Case  `json:"cases"`
}

type Case struct {
	ID              string            `json:"id"`
	Prompt          string            `json:"prompt"`
	Files           map[string]string `json:"files,omitempty"`
	EnableDangerous bool              `json:"enable_dangerous,omitempty"`
	ApprovedTools   []string          `json:"approved_tools,omitempty"`
	Script          []ScriptStep      `json:"script"`
	RepeatLast      bool              `json:"repeat_last,omitempty"`
	Thinking        bool              `json:"thinking,omitempty"`
	MaxTurns        int               `json:"max_turns,omitempty"`
	MaxTokens       int64             `json:"max_tokens,omitempty"`
	MaxRepeatedCall int               `json:"max_repeated_call,omitempty"`
	TimeoutMS       int               `json:"timeout_ms,omitempty"`
	ContextTokens   int               `json:"context_tokens,omitempty"`
	Expected        Expected          `json:"expected"`
}

type ScriptStep struct {
	Content   string            `json:"content,omitempty"`
	ToolCalls []schema.ToolCall `json:"tool_calls,omitempty"`
	Usage     schema.Usage      `json:"usage,omitempty"`
	Error     string            `json:"error,omitempty"`
	DelayMS   int               `json:"delay_ms,omitempty"`
}

type Expected struct {
	Reason         schema.TerminalReason `json:"reason"`
	AnswerContains string                `json:"answer_contains,omitempty"`
	ToolSequence   []string              `json:"tool_sequence,omitempty"`
	MinTurns       int                   `json:"min_turns,omitempty"`
	MaxTurns       int                   `json:"max_turns,omitempty"`
	MinCompactions int                   `json:"min_compactions,omitempty"`
}

type CaseResult struct {
	ID               string                `json:"id"`
	Passed           bool                  `json:"passed"`
	Failures         []string              `json:"failures,omitempty"`
	Reason           schema.TerminalReason `json:"reason"`
	Turns            int                   `json:"turns"`
	InputTokens      int64                 `json:"input_tokens"`
	OutputTokens     int64                 `json:"output_tokens"`
	LatencyMS        int64                 `json:"latency_ms"`
	EstimatedCostUSD float64               `json:"estimated_cost_usd"`
	ToolSequence     []string              `json:"tool_sequence,omitempty"`
	ToolCorrect      bool                  `json:"tool_correct"`
	RunID            string                `json:"run_id"`
	TracePath        string                `json:"trace_path"`
	RunError         string                `json:"run_error,omitempty"`
}

type Report struct {
	Suite             string       `json:"suite"`
	StartedAt         time.Time    `json:"started_at"`
	CompletedAt       time.Time    `json:"completed_at"`
	Cases             int          `json:"cases"`
	Passed            int          `json:"passed"`
	PassRate          float64      `json:"pass_rate"`
	Gate              float64      `json:"gate"`
	GatePassed        bool         `json:"gate_passed"`
	TotalInputTokens  int64        `json:"total_input_tokens"`
	TotalOutputTokens int64        `json:"total_output_tokens"`
	EstimatedCostUSD  float64      `json:"estimated_cost_usd"`
	Results           []CaseResult `json:"results"`
}

func LoadSuite(path string) (Suite, error) {
	file, err := os.Open(path)
	if err != nil {
		return Suite{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var suite Suite
	if err := decoder.Decode(&suite); err != nil {
		return Suite{}, err
	}
	if suite.Name == "" || len(suite.Cases) < 15 || len(suite.Cases) > 30 {
		return Suite{}, errors.New("evaluation suite must have a name and 15-30 cases")
	}
	if suite.MinimumPassRate <= 0 || suite.MinimumPassRate > 1 {
		return Suite{}, errors.New("minimum_pass_rate must be in (0,1]")
	}
	seen := map[string]bool{}
	for _, item := range suite.Cases {
		if item.ID == "" || item.Prompt == "" || len(item.Script) == 0 || seen[item.ID] {
			return Suite{}, fmt.Errorf("invalid or duplicate evaluation case %q", item.ID)
		}
		seen[item.ID] = true
	}
	return suite, nil
}

func Run(ctx context.Context, suite Suite, artifactsDir string) (Report, error) {
	started := time.Now().UTC()
	store, err := runstore.New(filepath.Join(artifactsDir, "runs"))
	if err != nil {
		return Report{}, err
	}
	report := Report{Suite: suite.Name, StartedAt: started, Cases: len(suite.Cases), Gate: suite.MinimumPassRate}
	for _, item := range suite.Cases {
		caseResult, runErr := runCase(ctx, store, artifactsDir, suite, item)
		if runErr != nil {
			return Report{}, fmt.Errorf("case %s: %w", item.ID, runErr)
		}
		report.Results = append(report.Results, caseResult)
		if caseResult.Passed {
			report.Passed++
		}
		report.TotalInputTokens += caseResult.InputTokens
		report.TotalOutputTokens += caseResult.OutputTokens
		report.EstimatedCostUSD += caseResult.EstimatedCostUSD
	}
	report.PassRate = float64(report.Passed) / float64(report.Cases)
	report.GatePassed = report.PassRate >= report.Gate
	report.CompletedAt = time.Now().UTC()
	return report, nil
}

func runCase(ctx context.Context, store *runstore.FileStore, artifactsDir string, suite Suite, item Case) (CaseResult, error) {
	workDir, err := os.MkdirTemp(filepath.Join(artifactsDir), "workspace-"+item.ID+"-")
	if err != nil {
		return CaseResult{}, err
	}
	defer os.RemoveAll(workDir)
	for name, content := range item.Files {
		path := filepath.Join(workDir, filepath.Clean(name))
		relative, err := filepath.Rel(workDir, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return CaseResult{}, fmt.Errorf("fixture path %q escapes workspace", name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return CaseResult{}, err
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return CaseResult{}, err
		}
	}
	readFile, err := tools.NewReadFileTool(workDir)
	if err != nil {
		return CaseResult{}, err
	}
	policy := tools.PermissionPolicy(tools.ReadOnlyPolicy{})
	if item.EnableDangerous {
		policy = tools.ApprovalPolicy{}
	}
	registry := tools.NewRegistry(tools.WithPermissionPolicy(policy))
	if err := registry.Register(readFile); err != nil {
		return CaseResult{}, err
	}
	if item.EnableDangerous {
		writeFile, err := tools.NewWriteFileTool(workDir)
		if err != nil {
			return CaseResult{}, err
		}
		editFile, err := tools.NewEditFileTool(workDir)
		if err != nil {
			return CaseResult{}, err
		}
		bashTool, err := tools.NewBashTool(workDir)
		if err != nil {
			return CaseResult{}, err
		}
		for _, tool := range []tools.BaseTool{writeFile, editFile, bashTool} {
			if err := registry.Register(tool); err != nil {
				return CaseResult{}, err
			}
		}
	}
	model := &scriptedProvider{steps: append([]ScriptStep(nil), item.Script...), repeatLast: item.RepeatLast}
	config := engine.Config{
		WorkDir: workDir, EnableThinking: item.Thinking, MaxTurns: item.MaxTurns,
		MaxTokens: item.MaxTokens, MaxRepeatedCall: item.MaxRepeatedCall,
		Timeout: time.Duration(item.TimeoutMS) * time.Millisecond, Store: store,
	}
	if item.ContextTokens > 0 {
		config.Compactor = contextmanager.Window{MaxApproxTokens: item.ContextTokens, ReserveTokens: item.ContextTokens / 5, MinTailMessages: 2}
	}
	agent, err := engine.New(model, registry, config)
	if err != nil {
		return CaseResult{}, err
	}
	runContext := tools.WithApprovedTools(ctx, item.ApprovedTools)
	result, runErr := agent.Run(runContext, item.Prompt)
	trace, traceErr := store.LoadTrace(result.RunID)
	if traceErr != nil {
		return CaseResult{}, traceErr
	}
	if _, err := runstore.Replay(trace); err != nil {
		return CaseResult{}, fmt.Errorf("replay: %w", err)
	}
	actualTools := make([]string, 0)
	compactions := 0
	for _, event := range trace.Events {
		if event.Type == engine.EventToolStarted {
			actualTools = append(actualTools, event.ToolCall.Name)
		}
		if event.Type == engine.EventCompacted {
			compactions++
		}
	}
	failures := make([]string, 0)
	if result.Reason != item.Expected.Reason {
		failures = append(failures, fmt.Sprintf("reason=%s want=%s", result.Reason, item.Expected.Reason))
	}
	if item.Expected.AnswerContains != "" && !strings.Contains(result.FinalMessage.Content, item.Expected.AnswerContains) {
		failures = append(failures, fmt.Sprintf("answer does not contain %q", item.Expected.AnswerContains))
	}
	toolCorrect := slices.Equal(actualTools, item.Expected.ToolSequence)
	if !toolCorrect {
		failures = append(failures, fmt.Sprintf("tools=%v want=%v", actualTools, item.Expected.ToolSequence))
	}
	if item.Expected.MinTurns > 0 && result.Turns < item.Expected.MinTurns {
		failures = append(failures, fmt.Sprintf("turns=%d below %d", result.Turns, item.Expected.MinTurns))
	}
	if item.Expected.MaxTurns > 0 && result.Turns > item.Expected.MaxTurns {
		failures = append(failures, fmt.Sprintf("turns=%d above %d", result.Turns, item.Expected.MaxTurns))
	}
	if compactions < item.Expected.MinCompactions {
		failures = append(failures, fmt.Sprintf("compactions=%d below %d", compactions, item.Expected.MinCompactions))
	}
	cost := float64(result.Usage.InputTokens)*suite.InputUSDPerMillion/1_000_000 + float64(result.Usage.OutputTokens)*suite.OutputUSDPerMillion/1_000_000
	caseResult := CaseResult{
		ID: item.ID, Passed: len(failures) == 0, Failures: failures, Reason: result.Reason,
		Turns: result.Turns, InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		LatencyMS: result.DurationMS, EstimatedCostUSD: cost, ToolSequence: actualTools,
		ToolCorrect: toolCorrect, RunID: result.RunID,
		TracePath: filepath.Join(store.Dir(), result.RunID+".trace.jsonl"),
	}
	if runErr != nil {
		caseResult.RunError = runErr.Error()
	}
	return caseResult, nil
}

type scriptedProvider struct {
	mu         sync.Mutex
	steps      []ScriptStep
	last       ScriptStep
	repeatLast bool
}

func (p *scriptedProvider) Generate(ctx context.Context, _ []schema.Message, _ []schema.ToolDefinition) (schema.Generation, error) {
	p.mu.Lock()
	if len(p.steps) > 0 {
		p.last = p.steps[0]
		p.steps = p.steps[1:]
	} else if !p.repeatLast {
		p.mu.Unlock()
		return schema.Generation{}, errors.New("script exhausted")
	}
	step := p.last
	p.mu.Unlock()
	if step.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(step.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return schema.Generation{}, ctx.Err()
		}
	}
	if step.Error != "" {
		return schema.Generation{}, errors.New(step.Error)
	}
	return schema.Generation{Message: schema.Message{Content: step.Content, ToolCalls: step.ToolCalls}, Usage: step.Usage}, nil
}
