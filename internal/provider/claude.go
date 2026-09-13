package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/royal007a/01agent/internal/schema"
)

// ClaudeProvider implements the Anthropic-compatible Messages protocol.
type ClaudeProvider struct {
	client    anthropic.Client
	model     string
	maxTokens int64
}

func NewClaude(config Config) (*ClaudeProvider, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("claude provider: API key is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("claude provider: model is required")
	}
	options := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if strings.TrimSpace(config.BaseURL) != "" {
		options = append(options, option.WithBaseURL(config.BaseURL))
	}
	maxTokens := config.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return &ClaudeProvider{
		client:    anthropic.NewClient(options...),
		model:     config.Model,
		maxTokens: maxTokens,
	}, nil
}

func (p *ClaudeProvider) Generate(
	ctx context.Context,
	messages []schema.Message,
	availableTools []schema.ToolDefinition,
) (schema.Generation, error) {
	systemPrompt, claudeMessages, err := toClaudeMessages(messages)
	if err != nil {
		return schema.Generation{}, err
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
		Messages:  claudeMessages,
	}
	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: systemPrompt}}
	}
	for _, definition := range availableTools {
		inputSchema, err := toClaudeInputSchema(definition.InputSchema)
		if err != nil {
			return schema.Generation{}, fmt.Errorf("claude tool %q schema: %w", definition.Name, err)
		}
		tool := anthropic.ToolParam{
			Name:        definition.Name,
			Description: anthropic.String(definition.Description),
			InputSchema: inputSchema,
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}

	response, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return schema.Generation{}, fmt.Errorf("claude-compatible request: %w", err)
	}
	message := schema.Message{Role: schema.RoleAssistant}
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			message.Content += block.Text
		case "tool_use":
			message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: append(json.RawMessage(nil), block.Input...),
			})
		}
	}
	return schema.Generation{
		Message: message,
		Usage: schema.Usage{
			InputTokens:  response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens,
			OutputTokens: response.Usage.OutputTokens,
		},
	}, nil
}

func toClaudeMessages(messages []schema.Message) (string, []anthropic.MessageParam, error) {
	var systemParts []string
	result := make([]anthropic.MessageParam, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case schema.RoleSystem:
			systemParts = append(systemParts, message.Content)
		case schema.RoleUser:
			result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(message.Content)))
		case schema.RoleTool:
			if message.ToolCallID == "" {
				return "", nil, errors.New("claude message translation: tool result is missing tool_call_id")
			}
			result = append(result, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(message.ToolCallID, message.Content, message.IsError),
			))
		case schema.RoleAssistant:
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(message.ToolCalls)+1)
			if message.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(message.Content))
			}
			for _, call := range message.ToolCalls {
				var input any
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return "", nil, fmt.Errorf("claude message translation: tool %q arguments: %w", call.Name, err)
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{ID: call.ID, Name: call.Name, Input: input},
				})
			}
			if len(blocks) > 0 {
				result = append(result, anthropic.NewAssistantMessage(blocks...))
			}
		default:
			return "", nil, fmt.Errorf("claude message translation: unsupported role %q", message.Role)
		}
	}
	return strings.Join(systemParts, "\n\n"), result, nil
}

func toClaudeInputSchema(input map[string]any) (anthropic.ToolInputSchemaParam, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}
	var result anthropic.ToolInputSchemaParam
	if err := json.Unmarshal(encoded, &result); err != nil {
		return anthropic.ToolInputSchemaParam{}, err
	}
	return result, nil
}
