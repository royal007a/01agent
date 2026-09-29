package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/weberzhao/go-tiny-claw/internal/schema"
)

var history = []schema.Message{
	{Role: schema.RoleSystem, Content: "sys"},
	{Role: schema.RoleUser, Content: "查天气"},
	{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{
		{ID: "c1", Name: "get_weather", Arguments: []byte(`{"city":"北京"}`)},
		{ID: "c2", Name: "get_weather", Arguments: []byte(`{"city":"上海"}`)},
	}},
	{Role: schema.RoleUser, ToolCallID: "c1", Content: "晴"},
	{Role: schema.RoleUser, ToolCallID: "c2", Content: "雨"},
}

var weatherTool = []schema.ToolDefinition{{
	Name: "get_weather", Description: "天气",
	InputSchema: map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"city": map[string]interface{}{"type": "string"}},
		"required":   []string{"city"},
	},
}}

func captureServer(t *testing.T, reply string, got *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = map[string]any{}
		if err := json.Unmarshal(b, got); err != nil {
			t.Errorf("bad json: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
}

const openaiReply = `{"id":"x","object":"chat.completion","created":1,"model":"m-0801","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"c3","type":"function","function":{"name":"get_weather","arguments":""}}]}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`

func TestOpenAITranslation(t *testing.T) {
	var got map[string]any
	srv := captureServer(t, openaiReply, &got)
	defer srv.Close()
	p := NewOpenAIProvider("k", srv.URL, "m")

	resp, err := p.Generate(context.Background(), history, weatherTool)
	if err == nil && resp.Model != wantServedModel(t) {
		t.Errorf("服务端回报的模型版本未透传: %q", resp.Model)
	}
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	ast := msgs[2].(map[string]any)
	if c, ok := ast["content"]; !ok || c != "" {
		t.Fatalf("assistant tool_calls 消息必须显式带 content:\"\"（1214 修复）, got %#v", ast)
	}
	if msgs[3].(map[string]any)["role"] != "tool" || msgs[3].(map[string]any)["tool_call_id"] != "c1" {
		t.Fatalf("tool 消息翻译错误: %#v", msgs[3])
	}
	if _, ok := got["tools"]; !ok {
		t.Fatal("Action 阶段必须挂载 tools")
	}
	if len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != "{}" {
		t.Fatalf("空参数应归一化为 {}: %#v", resp.ToolCalls)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("usage 未透传: %#v", resp.Usage)
	}

	// Thinking 阶段：不挂 tools，且工具历史降级为文本
	if _, err := p.Generate(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("Thinking 阶段不得挂载 tools")
	}
	raw, _ := json.Marshal(got["messages"])
	if strings.Contains(string(raw), `"tool_calls"`) || strings.Contains(string(raw), `"role":"tool"`) {
		t.Fatalf("Thinking 阶段应把工具历史降级成文本: %s", raw)
	}
}

const claudeReply = `{"id":"msg","type":"message","role":"assistant","model":"m-0901","content":[{"type":"text","text":"好"},{"type":"tool_use","id":"t9","name":"get_weather","input":{"city":"广州"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":5}}`

func TestClaudeTranslation(t *testing.T) {
	var got map[string]any
	srv := captureServer(t, claudeReply, &got)
	defer srv.Close()
	p := NewClaudeProvider("k", srv.URL, "m")
	resp, err := p.Generate(context.Background(), history, weatherTool)
	if err == nil && resp.Model != wantServedModel(t) {
		t.Errorf("服务端回报的模型版本未透传: %q", resp.Model)
	}
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	// user, assistant(2 tool_use), user(2 tool_result 合并)
	if len(msgs) != 3 {
		t.Fatalf("相邻同角色消息应合并，got %d 条: %v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 2 || last[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("并行 tool_result 应合并到同一条 user 消息: %v", last)
	}
	tools := got["tools"].([]any)
	req := tools[0].(map[string]any)["input_schema"].(map[string]any)["required"].([]any)
	if len(req) != 1 || req[0] != "city" {
		t.Fatalf("required 未透传: %v", req)
	}
	if resp.Content != "好" || len(resp.ToolCalls) != 1 || !strings.Contains(string(resp.ToolCalls[0].Arguments), "广州") {
		t.Fatalf("反向解析错误: %#v", resp)
	}
	if resp.Usage.PromptTokens != 20 {
		t.Fatalf("usage: %#v", resp.Usage)
	}
	if _, err := p.Generate(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["tools"]; ok {
		t.Fatal("Thinking 阶段不得挂载 tools")
	}
	raw, _ := json.Marshal(got["messages"])
	if strings.Contains(string(raw), "tool_use") || strings.Contains(string(raw), "tool_result") {
		t.Fatalf("Claude Thinking 阶段必须降级工具块，否则 API 会拒绝: %s", raw)
	}
}

// wantServedModel 按调用方测试名给出期望的服务端回报模型（环境锁定：检测同名模型静默滚动）。
func wantServedModel(t *testing.T) string {
	if strings.Contains(strings.ToLower(t.Name()), "claude") {
		return "m-0901"
	}
	return "m-0801"
}
