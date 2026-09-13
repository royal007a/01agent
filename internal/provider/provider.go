package provider

import (
	"context"

	"github.com/royal007a/01agent/internal/schema"
)

// LLMProvider translates between the harness schema and one model protocol.
type LLMProvider interface {
	Generate(
		ctx context.Context,
		messages []schema.Message,
		availableTools []schema.ToolDefinition,
	) (schema.Generation, error)
}

// Config describes a provider without binding callers to a vendor SDK.
type Config struct {
	APIKey    string
	BaseURL   string
	Model     string
	MaxTokens int64
}
