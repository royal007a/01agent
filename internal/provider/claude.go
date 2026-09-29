package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// ZhipuAnthropicBaseURL 是智谱的 Anthropic 兼容端点。
// 注：课程原文把它写成了 OpenAI 的 /api/paas/v4/，那是 OpenAI 协议路径，Claude 协议需要 /api/anthropic。
const ZhipuAnthropicBaseURL = "https://open.bigmodel.cn/api/anthropic"

// ClaudeProvider 基于官方 anthropic-sdk-go。
type ClaudeProvider struct {
	client    anthropic.Client
	model     string
	MaxTokens int64
}

func NewClaudeProvider(apiKey, baseURL, model string) *ClaudeProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &ClaudeProvider{client: anthropic.NewClient(opts...), model: model, MaxTokens: 8192}
}

// NewZhipuClaudeProvider 保留课程中的构造函数。
func NewZhipuClaudeProvider(model string) *ClaudeProvider {
	apiKey := os.Getenv("ZHIPU_API_KEY")
	if apiKey == "" {
		panic("请设置 ZHIPU_API_KEY 环境变量")
	}
	return NewClaudeProvider(apiKey, ZhipuAnthropicBaseURL, model)
}

// buildClaudeMessages 把内部消息翻译为 Anthropic MessageParam。
// Anthropic 要求 user/assistant 交替，且同一轮的多个 tool_result 应放在同一条 user 消息里，
// 因此这里会把相邻的同角色消息合并为一条多 block 消息。
func buildClaudeMessages(msgs []schema.Message) (string, []anthropic.MessageParam) {
	var system string
	var out []anthropic.MessageParam
	appendBlocks := func(role anthropic.MessageParamRole, blocks ...anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			if system != "" {
				system += "\n\n"
			}
			system += msg.Content
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				content := msg.Content
				if content == "" {
					content = "(空输出)"
				}
				appendBlocks(anthropic.MessageParamRoleUser, anthropic.NewToolResultBlock(msg.ToolCallID, content, msg.IsError))
			} else if msg.Content != "" {
				appendBlocks(anthropic.MessageParamRoleUser, anthropic.NewTextBlock(msg.Content))
			}
		case schema.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				var input map[string]interface{}
				if err := json.Unmarshal(tc.Arguments, &input); err != nil || input == nil {
					input = map[string]interface{}{}
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{ID: tc.ID, Name: tc.Name, Input: input},
				})
			}
			appendBlocks(anthropic.MessageParamRoleAssistant, blocks...)
		}
	}
	// Anthropic 要求第一条消息必须是 user；Working Memory 截断后可能以 assistant 开头。
	if len(out) > 0 && out[0].Role == anthropic.MessageParamRoleAssistant {
		out = append([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("(以下是此前对话的延续)"))}, out...)
	}
	return system, out
}

func (p *ClaudeProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	if len(availableTools) == 0 && hasToolBlocks(msgs) {
		msgs = flattenToolHistory(msgs)
	}
	systemPrompt, anthropicMsgs := buildClaudeMessages(msgs)

	var anthropicTools []anthropic.ToolUnionParam
	for _, toolDef := range availableTools {
		var properties any
		var required []string
		m, ok := toolDef.InputSchema.(map[string]interface{})
		if !ok {
			b, _ := json.Marshal(toolDef.InputSchema)
			_ = json.Unmarshal(b, &m)
		}
		if m != nil {
			properties = m["properties"]
			switch r := m["required"].(type) {
			case []string:
				required = r
			case []interface{}:
				for _, v := range r {
					if s, ok := v.(string); ok {
						required = append(required, s)
					}
				}
			}
		}
		tp := anthropic.ToolParam{
			Name:        toolDef.Name,
			Description: anthropic.String(toolDef.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: properties, Required: required},
		}
		anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{OfTool: &tp})
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.MaxTokens,
		Messages:  anthropicMsgs,
	}
	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: systemPrompt}}
	}
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
	}
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("Claude 兼容 API 请求失败: %w", err)
	}

	resultMsg := &schema.Message{Role: schema.RoleAssistant, Model: string(resp.Model)}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			resultMsg.Content += block.Text
		case "tool_use":
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: normalizeArgs(string(block.Input)),
			})
		}
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		resultMsg.Usage = &schema.Usage{
			PromptTokens:     int(resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens),
			CompletionTokens: int(resp.Usage.OutputTokens),
		}
	}
	return resultMsg, nil
}
