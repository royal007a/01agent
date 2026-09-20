package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/royal007a/01agent/internal/contextmanager"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/memory"
	"github.com/royal007a/01agent/internal/plan"
	promptcontext "github.com/royal007a/01agent/internal/prompt"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/runstore"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

var version = "dev"

type cliConfig struct {
	provider           string
	apiKey             string
	baseURL            string
	model              string
	workDir            string
	thinking           bool
	planMode           bool
	showThinking       bool
	jsonOutput         bool
	showVersion        bool
	maxTurns           int
	maxOutputTokens    int64
	tokenBudget        int64
	maxRepeatedCall    int
	maxParallel        int
	timeout            time.Duration
	toolTimeout        time.Duration
	runDir             string
	runID              string
	resumeRunID        string
	contextTokens      int
	providerAttempts   int
	providerBaseDelay  time.Duration
	providerMaxDelay   time.Duration
	providerSpacing    time.Duration
	providerConcurrent int
	enableDangerous    bool
	approvedTools      stringList
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	config, prompt, err := parseFlags(arguments, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "01agent: %v\n", err)
		return 2
	}
	if config.showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	model, err := buildProvider(config)
	if err != nil {
		fmt.Fprintf(stderr, "01agent: %v\n", err)
		return 2
	}
	store, err := runstore.New(config.runDir)
	if err != nil {
		fmt.Fprintf(stderr, "01agent: initialize run store: %v\n", err)
		return 2
	}
	archive, err := memory.NewArchive(filepath.Join(store.Dir(), "memory"))
	if err != nil {
		fmt.Fprintf(stderr, "01agent: initialize memory archive: %v\n", err)
		return 2
	}
	plans, err := plan.NewStore(filepath.Join(store.Dir(), "plans"))
	if err != nil {
		fmt.Fprintf(stderr, "01agent: initialize plan store: %v\n", err)
		return 2
	}
	var checkpoint engine.Checkpoint
	if config.resumeRunID != "" {
		checkpoint, err = store.LoadCheckpoint(context.Background(), config.resumeRunID)
		if err != nil {
			fmt.Fprintf(stderr, "01agent: load checkpoint: %v\n", err)
			return 2
		}
		config.workDir = checkpoint.WorkDir
	}
	readFile, err := tools.NewReadFileTool(config.workDir)
	if err != nil {
		fmt.Fprintf(stderr, "01agent: initialize read_file: %v\n", err)
		return 2
	}
	policy := tools.PermissionPolicy(tools.ReadOnlyPolicy{})
	if config.enableDangerous {
		policy = tools.ApprovalPolicy{}
	}
	registry := tools.NewRegistry(
		tools.WithPermissionPolicy(policy),
		tools.WithMaxParallel(config.maxParallel),
		tools.WithToolTimeout(config.toolTimeout),
	)
	if err := registry.Register(readFile); err != nil {
		fmt.Fprintf(stderr, "01agent: register read_file: %v\n", err)
		return 2
	}
	if err := registry.Register(promptcontext.NewReadSkillTool()); err != nil {
		fmt.Fprintf(stderr, "01agent: register read_skill: %v\n", err)
		return 2
	}
	if err := registry.Register(memory.NewRecallTool(archive)); err != nil {
		fmt.Fprintf(stderr, "01agent: register recall_context: %v\n", err)
		return 2
	}
	for _, tool := range []tools.BaseTool{plan.NewReadTool(plans), plan.NewUpdateTool(plans)} {
		if err := registry.Register(tool); err != nil {
			fmt.Fprintf(stderr, "01agent: register %s: %v\n", tool.Name(), err)
			return 2
		}
	}
	if config.enableDangerous {
		writeFile, toolErr := tools.NewWriteFileTool(config.workDir)
		if toolErr != nil {
			fmt.Fprintf(stderr, "01agent: initialize write_file: %v\n", toolErr)
			return 2
		}
		editFile, toolErr := tools.NewEditFileTool(config.workDir)
		if toolErr != nil {
			fmt.Fprintf(stderr, "01agent: initialize edit_file: %v\n", toolErr)
			return 2
		}
		bashTool, toolErr := tools.NewBashTool(config.workDir)
		if toolErr != nil {
			fmt.Fprintf(stderr, "01agent: initialize bash: %v\n", toolErr)
			return 2
		}
		for _, tool := range []tools.BaseTool{writeFile, editFile, bashTool} {
			if toolErr := registry.Register(tool); toolErr != nil {
				fmt.Fprintf(stderr, "01agent: register %s: %v\n", tool.Name(), toolErr)
				return 2
			}
		}
	}

	handler := eventPrinter(stderr, config.showThinking, config.jsonOutput)
	var compactor engine.ContextCompactor
	if config.contextTokens > 0 {
		compactor = contextmanager.Window{MaxApproxTokens: config.contextTokens, ReserveTokens: config.contextTokens / 5, Archive: archive}
	}
	agent, err := engine.New(model, registry, engine.Config{
		WorkDir:         config.workDir,
		EnableThinking:  config.thinking,
		PlanMode:        config.planMode,
		MaxTurns:        config.maxTurns,
		MaxTokens:       config.tokenBudget,
		MaxRepeatedCall: config.maxRepeatedCall,
		Timeout:         config.timeout,
		OnEvent:         handler,
		RunID:           config.runID,
		Store:           store,
		Compactor:       compactor,
		InputQueue:      store,
	})
	if err != nil {
		fmt.Fprintf(stderr, "01agent: initialize engine: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = tools.WithApprovedTools(ctx, config.approvedTools)
	var result engine.RunResult
	var runErr error
	if config.resumeRunID != "" {
		result, runErr = agent.Resume(ctx, checkpoint)
	} else {
		result, runErr = agent.Run(ctx, prompt)
	}
	if config.jsonOutput {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "01agent: encode result: %v\n", err)
			return 1
		}
	} else if result.FinalMessage.Content != "" {
		fmt.Fprintln(stdout, result.FinalMessage.Content)
	} else {
		fmt.Fprintf(stderr, "01agent stopped: %s (turns=%d, tokens=%d)\n", result.Reason, result.Turns, result.Usage.TotalTokens())
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "01agent: %v\n", runErr)
		return 1
	}
	if result.Reason != schema.TerminalCompleted {
		return 1
	}
	return 0
}

func parseFlags(arguments []string, stderr io.Writer) (cliConfig, string, error) {
	config := cliConfig{}
	flags := flag.NewFlagSet("01agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&config.provider, "provider", envOr("AGENT_PROVIDER", "openai"), "provider protocol: openai or claude")
	flags.StringVar(&config.apiKey, "api-key", os.Getenv("AGENT_API_KEY"), "API key (prefer AGENT_API_KEY)")
	flags.StringVar(&config.baseURL, "base-url", os.Getenv("AGENT_BASE_URL"), "optional compatible API base URL")
	flags.StringVar(&config.model, "model", os.Getenv("AGENT_MODEL"), "model identifier (required)")
	flags.StringVar(&config.workDir, "workdir", ".", "workspace boundary")
	flags.BoolVar(&config.thinking, "thinking", false, "run a tool-free planning call before each action call")
	flags.BoolVar(&config.planMode, "plan-mode", false, "externalize long-task objective and progress with the canonical plan store")
	flags.BoolVar(&config.showThinking, "show-thinking", false, "print Thinking phase text to stderr")
	flags.BoolVar(&config.jsonOutput, "json", false, "write the complete structured result as JSON")
	flags.BoolVar(&config.showVersion, "version", false, "print version and exit")
	flags.IntVar(&config.maxTurns, "max-turns", 32, "maximum model/action turns")
	flags.Int64Var(&config.maxOutputTokens, "max-output-tokens", 4096, "maximum tokens generated by one provider request")
	flags.Int64Var(&config.tokenBudget, "token-budget", 0, "total reported token budget; 0 disables the budget")
	flags.IntVar(&config.maxRepeatedCall, "max-repeated-call", 3, "maximum equivalent tool calls before doom-loop termination")
	flags.IntVar(&config.maxParallel, "max-parallel", 4, "maximum concurrent read-only tool calls")
	flags.DurationVar(&config.timeout, "timeout", 10*time.Minute, "overall run timeout")
	flags.DurationVar(&config.toolTimeout, "tool-timeout", 30*time.Second, "timeout for one tool call")
	flags.StringVar(&config.runDir, "run-dir", envOr("AGENT_RUN_DIR", filepath.Join(os.TempDir(), "01agent-runs")), "trace and checkpoint directory")
	flags.StringVar(&config.runID, "run-id", "", "optional stable run identifier")
	flags.StringVar(&config.resumeRunID, "resume", "", "resume a saved run identifier")
	flags.IntVar(&config.contextTokens, "context-tokens", 0, "approximate context window; 0 disables compaction")
	flags.IntVar(&config.providerAttempts, "provider-attempts", 3, "maximum provider attempts for transient failures")
	flags.DurationVar(&config.providerBaseDelay, "provider-base-delay", 250*time.Millisecond, "initial provider retry delay")
	flags.DurationVar(&config.providerMaxDelay, "provider-max-delay", 5*time.Second, "maximum provider retry delay")
	flags.DurationVar(&config.providerSpacing, "provider-spacing", 0, "minimum spacing between provider requests")
	flags.IntVar(&config.providerConcurrent, "provider-concurrency", 4, "maximum concurrent provider requests")
	flags.BoolVar(&config.enableDangerous, "enable-dangerous-tools", false, "register write_file, edit_file, and bash (still requires approval)")
	flags.Var(&config.approvedTools, "approve-tool", "approve one dangerous tool for this run; repeatable")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: 01agent [flags] <prompt>\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return config, "", err
	}
	if config.showVersion {
		return config, "", nil
	}
	if config.maxTurns <= 0 || config.maxRepeatedCall <= 0 || config.maxParallel <= 0 || config.providerAttempts <= 0 || config.providerConcurrent <= 0 {
		return config, "", errors.New("max-turns, max-repeated-call, and max-parallel must be positive")
	}
	if config.maxOutputTokens <= 0 {
		return config, "", errors.New("max-output-tokens must be positive")
	}
	if config.tokenBudget < 0 || config.contextTokens < 0 {
		return config, "", errors.New("token-budget and context-tokens must be non-negative")
	}
	if config.timeout <= 0 || config.toolTimeout <= 0 {
		return config, "", errors.New("timeout and tool-timeout must be positive")
	}
	prompt := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if prompt == "" && config.resumeRunID == "" {
		return config, "", errors.New("prompt is required")
	}
	if prompt != "" && config.resumeRunID != "" {
		return config, "", errors.New("prompt and --resume cannot be used together")
	}
	absolute, err := filepath.Abs(config.workDir)
	if err != nil {
		return config, "", fmt.Errorf("resolve workdir: %w", err)
	}
	config.workDir = absolute
	return config, prompt, nil
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("tool name must not be empty")
	}
	*s = append(*s, value)
	return nil
}

func buildProvider(config cliConfig) (provider.LLMProvider, error) {
	key := strings.TrimSpace(config.apiKey)
	var model provider.LLMProvider
	var err error
	switch strings.ToLower(strings.TrimSpace(config.provider)) {
	case "openai":
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		model, err = provider.NewOpenAI(provider.Config{APIKey: key, BaseURL: config.baseURL, Model: config.model, MaxTokens: config.maxOutputTokens})
	case "claude", "anthropic":
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		model, err = provider.NewClaude(provider.Config{APIKey: key, BaseURL: config.baseURL, Model: config.model, MaxTokens: config.maxOutputTokens})
	default:
		return nil, fmt.Errorf("unsupported provider %q (want openai or claude)", config.provider)
	}
	if err != nil {
		return nil, err
	}
	return provider.WithResilience(model, provider.ResilienceConfig{
		MaxAttempts: config.providerAttempts, BaseDelay: config.providerBaseDelay,
		MaxDelay: config.providerMaxDelay, MinRequestSpacing: config.providerSpacing,
		MaxConcurrent: config.providerConcurrent,
	}), nil
}

func eventPrinter(output io.Writer, showThinking, quiet bool) func(engine.Event) {
	if quiet {
		return nil
	}
	return func(event engine.Event) {
		switch event.Type {
		case engine.EventTurnStarted:
			fmt.Fprintf(output, "[turn %d]\n", event.Turn)
		case engine.EventThinking:
			if showThinking {
				fmt.Fprintf(output, "[thinking] %s\n", event.Message.Content)
			}
		case engine.EventAssistant:
			if event.Message.Content != "" && len(event.Message.ToolCalls) > 0 {
				fmt.Fprintf(output, "[assistant] %s\n", event.Message.Content)
			}
		case engine.EventToolStarted:
			fmt.Fprintf(output, "[tool] %s %s\n", event.ToolCall.Name, string(event.ToolCall.Arguments))
		case engine.EventToolResult:
			status := "ok"
			if event.ToolResult.IsError {
				status = event.ToolResult.ErrorCode
			}
			fmt.Fprintf(output, "[tool:%s] %s (%d bytes)\n", status, event.ToolResult.ToolCallID, len(event.ToolResult.Output))
		}
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
