package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

// ZhipuBaseURL 是智谱的 OpenAI 兼容端点（第 04 讲）。
const ZhipuBaseURL = "https://open.bigmodel.cn/api/paas/v4/"

// OpenAIProvider 基于官方 openai-go/v3 SDK，可指向任何 OpenAI 兼容端点（智谱、方舟、DeepSeek…）。
type OpenAIProvider struct {
	client openai.Client
	model  string
	// FlattenToolsWhenThinking：慢思考阶段把工具历史降级成文本，兼容严格端点。
	FlattenToolsWhenThinking bool
}

// NewOpenAIProvider 构造一个指向任意 OpenAI 兼容端点的适配器。
func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAIProvider{client: openai.NewClient(opts...), model: model, FlattenToolsWhenThinking: true}
}

// NewZhipuOpenAIProvider 保留课程中的构造函数：读取 ZHIPU_API_KEY，指向智谱底座。
func NewZhipuOpenAIProvider(model string) *OpenAIProvider {
	apiKey := os.Getenv("ZHIPU_API_KEY")
	if apiKey == "" {
		panic("请设置 ZHIPU_API_KEY 环境变量")
	}
	return NewOpenAIProvider(apiKey, ZhipuBaseURL, model)
}

func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	if len(availableTools) == 0 && p.FlattenToolsWhenThinking && hasToolBlocks(msgs) {
		msgs = flattenToolHistory(msgs)
	}

	// 1. 翻译上下文消息
	var openaiMsgs []openai.ChatCompletionMessageParamUnion
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			openaiMsgs = append(openaiMsgs, openai.SystemMessage(msg.Content))
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				content := msg.Content
				if content == "" {
					content = "(空输出)"
				}
				openaiMsgs = append(openaiMsgs, openai.ToolMessage(content, msg.ToolCallID))
			} else {
				openaiMsgs = append(openaiMsgs, openai.UserMessage(msg.Content))
			}
		case schema.RoleAssistant:
			astParam := openai.ChatCompletionAssistantMessageParam{}
			// 第 13 讲踩坑：带 tool_calls 的 assistant 消息即使 content 为空也要显式传 ""，
			// 否则智谱等严格端点会返回 1214 "messages 参数非法"。
			astParam.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
				OfString: openai.String(msg.Content),
			}
			if len(msg.ToolCalls) > 0 {
				var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
				for _, tc := range msg.ToolCalls {
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Arguments),
							},
						},
					})
				}
				astParam.ToolCalls = toolCalls
			}
			openaiMsgs = append(openaiMsgs, openai.ChatCompletionMessageParamUnion{OfAssistant: &astParam})
		}
	}

	// 2. 翻译工具定义
	var openaiTools []openai.ChatCompletionToolUnionParam
	for _, toolDef := range availableTools {
		var params shared.FunctionParameters
		if m, ok := toolDef.InputSchema.(map[string]interface{}); ok {
			params = shared.FunctionParameters(m)
		} else {
			b, _ := json.Marshal(toolDef.InputSchema)
			_ = json.Unmarshal(b, &params)
		}
		openaiTools = append(openaiTools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        toolDef.Name,
			Description: openai.String(toolDef.Description),
			Parameters:  params,
		}))
	}

	// 3. 构建请求并发送；仅当 availableTools 存在时才挂载 Tools（慢思考机制的支撑）
	params := openai.ChatCompletionNewParams{Model: p.model, Messages: openaiMsgs}
	if len(openaiTools) > 0 {
		params.Tools = openaiTools
	}
	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("OpenAI 兼容 API 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("API 返回了空的 Choices")
	}

	// 4. 反向翻译为内部 schema.Message
	choice := resp.Choices[0].Message
	resultMsg := &schema.Message{Role: schema.RoleAssistant, Content: choice.Content, Model: resp.Model}
	for _, tc := range choice.ToolCalls {
		if tc.Type == "function" {
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: normalizeArgs(tc.Function.Arguments),
			})
		}
	}
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		resultMsg.Usage = &schema.Usage{
			PromptTokens:     int(resp.Usage.PromptTokens),
			CompletionTokens: int(resp.Usage.CompletionTokens),
		}
	}
	return resultMsg, nil
}
