package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/royal007a/01agent/internal/schema"
)

// OpenAIProvider implements the OpenAI-compatible Chat Completions protocol.
// Chat Completions is used intentionally because many compatible endpoints do
// not yet implement the Responses API.
type OpenAIProvider struct {
	client    openai.Client
	model     string
	maxTokens int64
}

func NewOpenAI(config Config) (*OpenAIProvider, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("openai provider: API key is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("openai provider: model is required")
	}
	options := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if strings.TrimSpace(config.BaseURL) != "" {
		options = append(options, option.WithBaseURL(config.BaseURL))
	}
	return &OpenAIProvider{
		client:    openai.NewClient(options...),
		model:     config.Model,
		maxTokens: config.MaxTokens,
	}, nil
}

func (p *OpenAIProvider) Generate(
	ctx context.Context,
	messages []schema.Message,
	availableTools []schema.ToolDefinition,
) (schema.Generation, error) {
	openAIMessages, err := toOpenAIMessages(messages)
	if err != nil {
		return schema.Generation{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(p.model),
		Messages: openAIMessages,
	}
	if p.maxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(p.maxTokens)
	}
	for _, definition := range availableTools {
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        definition.Name,
				Description: openai.String(definition.Description),
				Parameters:  shared.FunctionParameters(definition.InputSchema),
			},
		))
	}

	response, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return schema.Generation{}, fmt.Errorf("openai-compatible request: %w", err)
	}
	if len(response.Choices) == 0 {
		return schema.Generation{}, errors.New("openai-compatible response contains no choices")
	}

	choice := response.Choices[0].Message
	message := schema.Message{Role: schema.RoleAssistant, Content: choice.Content}
	for _, call := range choice.ToolCalls {
		if call.Type != "function" {
			continue
		}
		message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: []byte(call.Function.Arguments),
		})
	}
	return schema.Generation{
		Message: message,
		Usage: schema.Usage{
			InputTokens:  response.Usage.PromptTokens,
			OutputTokens: response.Usage.CompletionTokens,
		},
	}, nil
}

func toOpenAIMessages(messages []schema.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	result := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case schema.RoleSystem:
			result = append(result, openai.SystemMessage(message.Content))
		case schema.RoleUser:
			result = append(result, openai.UserMessage(message.Content))
		case schema.RoleTool:
			if message.ToolCallID == "" {
				return nil, errors.New("openai message translation: tool result is missing tool_call_id")
			}
			result = append(result, openai.ToolMessage(message.Content, message.ToolCallID))
		case schema.RoleAssistant:
			assistant := openai.ChatCompletionAssistantMessageParam{}
			if message.Content != "" {
				assistant.Content.OfString = openai.String(message.Content)
			}
			for _, call := range message.ToolCalls {
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      call.Name,
							Arguments: string(call.Arguments),
						},
					},
				})
			}
			result = append(result, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		default:
			return nil, fmt.Errorf("openai message translation: unsupported role %q", message.Role)
		}
	}
	return result, nil
}
