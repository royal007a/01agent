package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royal007a/01agent/internal/contextmanager"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/provider"
	"github.com/royal007a/01agent/internal/runstore"
	agentserver "github.com/royal007a/01agent/internal/server"
	"github.com/royal007a/01agent/internal/taskstore"
	"github.com/royal007a/01agent/internal/tools"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	listen := flag.String("listen", envOr("AGENT_LISTEN_ADDR", ":8080"), "HTTP listen address")
	workDir := flag.String("workdir", envOr("AGENT_WORKDIR", "/workspace"), "workspace boundary")
	flag.Parse()

	readFile, err := tools.NewReadFileTool(*workDir)
	if err != nil {
		return fmt.Errorf("initialize read_file: %w", err)
	}
	enableDangerous := envBool("AGENT_ENABLE_DANGEROUS_TOOLS", false)
	policy := tools.PermissionPolicy(tools.ReadOnlyPolicy{})
	if enableDangerous {
		policy = tools.ApprovalPolicy{}
	}
	registry := tools.NewRegistry(
		tools.WithPermissionPolicy(policy),
		tools.WithMaxParallel(envInt("AGENT_MAX_PARALLEL", 4)),
		tools.WithToolTimeout(envDuration("AGENT_TOOL_TIMEOUT", 30*time.Second)),
	)
	if err := registry.Register(readFile); err != nil {
		return fmt.Errorf("register read_file: %w", err)
	}
	if enableDangerous {
		writeFile, toolErr := tools.NewWriteFileTool(*workDir)
		if toolErr != nil {
			return fmt.Errorf("initialize write_file: %w", toolErr)
		}
		editFile, toolErr := tools.NewEditFileTool(*workDir)
		if toolErr != nil {
			return fmt.Errorf("initialize edit_file: %w", toolErr)
		}
		bashTool, toolErr := tools.NewBashTool(*workDir)
		if toolErr != nil {
			return fmt.Errorf("initialize bash: %w", toolErr)
		}
		for _, tool := range []tools.BaseTool{writeFile, editFile, bashTool} {
			if toolErr := registry.Register(tool); toolErr != nil {
				return fmt.Errorf("register %s: %w", tool.Name(), toolErr)
			}
		}
	}

	model, providerErr := providerFromEnv()
	if providerErr != nil {
		log.Printf("provider not ready: %v", providerErr)
	}
	store, err := runstore.New(envOr("AGENT_RUN_DIR", "/tmp/01agent-runs"))
	if err != nil {
		return fmt.Errorf("initialize run store: %w", err)
	}
	tasks, err := taskstore.New(filepath.Join(store.Dir(), "tasks"), store, store)
	if err != nil {
		return fmt.Errorf("initialize background task store: %w", err)
	}
	taskHeartbeatTimeout := envDuration("AGENT_TASK_HEARTBEAT_TIMEOUT", 2*time.Minute)
	if err := reconcileTasks(context.Background(), tasks, taskHeartbeatTimeout); err != nil {
		return fmt.Errorf("reconcile background tasks: %w", err)
	}
	var compactor engine.ContextCompactor
	if contextTokens := envInt("AGENT_CONTEXT_TOKENS", 0); contextTokens > 0 {
		compactor = contextmanager.Window{MaxApproxTokens: contextTokens, ReserveTokens: contextTokens / 5}
	}
	handler, err := agentserver.New(agentserver.Config{
		Version:          version,
		Token:            os.Getenv("AGENT_API_TOKEN"),
		WorkDir:          *workDir,
		Provider:         model,
		Registry:         registry,
		EnableThinking:   envBool("AGENT_THINKING", false),
		MaxTurns:         envInt("AGENT_MAX_TURNS", 32),
		MaxTokens:        envInt64("AGENT_TOKEN_BUDGET", 0),
		MaxRepeatedCall:  envInt("AGENT_MAX_REPEATED_CALL", 3),
		RunTimeout:       envDuration("AGENT_RUN_TIMEOUT", 10*time.Minute),
		MaxConcurrent:    envInt("AGENT_MAX_CONCURRENT", 2),
		Store:            store,
		Compactor:        compactor,
		InputQueue:       store,
		InputEnqueuer:    store,
		Tasks:            tasks,
		ReadinessTTL:     envDuration("AGENT_READINESS_TTL", 5*time.Minute),
		ReadinessTimeout: envDuration("AGENT_READINESS_TIMEOUT", 10*time.Second),
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      11 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		ticker := time.NewTicker(envDuration("AGENT_TASK_RECONCILE_INTERVAL", 30*time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := reconcileTasks(context.WithoutCancel(ctx), tasks, taskHeartbeatTimeout); err != nil {
					log.Printf("background task reconciliation failed: %v", err)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("01agentd %s listening on %s", version, *listen)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func reconcileTasks(ctx context.Context, tasks *taskstore.Store, heartbeatTimeout time.Duration) error {
	if _, err := tasks.ReconcileLost(ctx, time.Now().UTC(), heartbeatTimeout); err != nil {
		return err
	}
	if err := tasks.DeliverPending(ctx); err != nil {
		return err
	}
	return tasks.ReconcileConsumption(ctx)
}

func providerFromEnv() (provider.LLMProvider, error) {
	protocol := strings.ToLower(envOr("AGENT_PROVIDER", "openai"))
	apiKey := strings.TrimSpace(os.Getenv("AGENT_API_KEY"))
	if apiKey == "" {
		if protocol == "claude" || protocol == "anthropic" {
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		} else {
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	config := provider.Config{
		APIKey:    apiKey,
		BaseURL:   os.Getenv("AGENT_BASE_URL"),
		Model:     os.Getenv("AGENT_MODEL"),
		MaxTokens: envInt64("AGENT_MAX_OUTPUT_TOKENS", 4096),
	}
	switch protocol {
	case "openai":
		model, err := provider.NewOpenAI(config)
		if err != nil {
			return nil, err
		}
		return resilient(model), nil
	case "claude", "anthropic":
		model, err := provider.NewClaude(config)
		if err != nil {
			return nil, err
		}
		return resilient(model), nil
	default:
		return nil, fmt.Errorf("unsupported AGENT_PROVIDER %q", protocol)
	}
}

func resilient(model provider.LLMProvider) provider.LLMProvider {
	return provider.WithResilience(model, provider.ResilienceConfig{
		MaxAttempts:       envInt("AGENT_PROVIDER_ATTEMPTS", 3),
		BaseDelay:         envDuration("AGENT_PROVIDER_BASE_DELAY", 250*time.Millisecond),
		MaxDelay:          envDuration("AGENT_PROVIDER_MAX_DELAY", 5*time.Second),
		MinRequestSpacing: envDurationAllowZero("AGENT_PROVIDER_SPACING", 0),
		MaxConcurrent:     envInt("AGENT_PROVIDER_CONCURRENCY", 4),
	})
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envDurationAllowZero(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
