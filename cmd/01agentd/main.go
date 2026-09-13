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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royal007a/01agent/internal/provider"
	agentserver "github.com/royal007a/01agent/internal/server"
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
	registry := tools.NewRegistry(
		tools.WithMaxParallel(envInt("AGENT_MAX_PARALLEL", 4)),
		tools.WithToolTimeout(envDuration("AGENT_TOOL_TIMEOUT", 30*time.Second)),
	)
	if err := registry.Register(readFile); err != nil {
		return fmt.Errorf("register read_file: %w", err)
	}

	model, providerErr := providerFromEnv()
	if providerErr != nil {
		log.Printf("provider not ready: %v", providerErr)
	}
	handler, err := agentserver.New(agentserver.Config{
		Version:         version,
		Token:           os.Getenv("AGENT_API_TOKEN"),
		WorkDir:         *workDir,
		Provider:        model,
		Registry:        registry,
		EnableThinking:  envBool("AGENT_THINKING", false),
		MaxTurns:        envInt("AGENT_MAX_TURNS", 32),
		MaxTokens:       envInt64("AGENT_TOKEN_BUDGET", 0),
		MaxRepeatedCall: envInt("AGENT_MAX_REPEATED_CALL", 3),
		RunTimeout:      envDuration("AGENT_RUN_TIMEOUT", 10*time.Minute),
		MaxConcurrent:   envInt("AGENT_MAX_CONCURRENT", 2),
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
		return model, nil
	case "claude", "anthropic":
		model, err := provider.NewClaude(config)
		if err != nil {
			return nil, err
		}
		return model, nil
	default:
		return nil, fmt.Errorf("unsupported AGENT_PROVIDER %q", protocol)
	}
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

func envBool(name string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
