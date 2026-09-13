package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/royal007a/01agent/internal/schema"
)

func TestOpenAIProviderTranslatesMessagesToolsAndUsage(t *testing.T) {
	var captured map[string]any
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{
          "id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test-model",
          "choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"checking","tool_calls":[{"id":"call-2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"go.mod\"}"}}]}}],
          "usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}
        }`)
	}))
	defer server.Close()

	model, err := NewOpenAI(Config{APIKey: "test", BaseURL: server.URL + "/v1", Model: "test-model", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := model.Generate(context.Background(), providerTestMessages(), []schema.ToolDefinition{providerTestTool()})
	if err != nil {
		t.Fatal(err)
	}
	if generation.Message.Content != "checking" || len(generation.Message.ToolCalls) != 1 {
		t.Fatalf("message = %#v", generation.Message)
	}
	if generation.Message.ToolCalls[0].Name != "read_file" || generation.Usage.TotalTokens() != 14 {
		t.Fatalf("generation = %#v", generation)
	}

	mu.Lock()
	defer mu.Unlock()
	messages := captured["messages"].([]any)
	toolResult := messages[len(messages)-1].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call-1" {
		t.Fatalf("tool result request = %#v", toolResult)
	}
	if len(captured["tools"].([]any)) != 1 {
		t.Fatalf("tools = %#v", captured["tools"])
	}
}

func TestClaudeProviderTranslatesMessagesToolsAndUsage(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{
          "id":"msg_test","type":"message","role":"assistant","model":"test-model",
          "content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call-2","name":"read_file","input":{"path":"go.mod"}}],
          "stop_reason":"tool_use","stop_sequence":null,
          "usage":{"input_tokens":12,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}
        }`)
	}))
	defer server.Close()

	model, err := NewClaude(Config{APIKey: "test", BaseURL: server.URL, Model: "test-model", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := model.Generate(context.Background(), providerTestMessages(), []schema.ToolDefinition{providerTestTool()})
	if err != nil {
		t.Fatal(err)
	}
	if generation.Message.Content != "checking" || len(generation.Message.ToolCalls) != 1 {
		t.Fatalf("message = %#v", generation.Message)
	}
	if generation.Message.ToolCalls[0].Name != "read_file" || generation.Usage.TotalTokens() != 17 {
		t.Fatalf("generation = %#v", generation)
	}
	system := captured["system"].([]any)[0].(map[string]any)
	if system["text"] != "system" {
		t.Fatalf("system = %#v", captured["system"])
	}
	if len(captured["tools"].([]any)) != 1 {
		t.Fatalf("tools = %#v", captured["tools"])
	}
}

func TestProvidersRequireCredentialsAndModel(t *testing.T) {
	if _, err := NewOpenAI(Config{Model: "x"}); err == nil {
		t.Fatal("NewOpenAI accepted empty API key")
	}
	if _, err := NewClaude(Config{APIKey: "x"}); err == nil {
		t.Fatal("NewClaude accepted empty model")
	}
}

func providerTestMessages() []schema.Message {
	return []schema.Message{
		{Role: schema.RoleSystem, Content: "system"},
		{Role: schema.RoleUser, Content: "read"},
		{Role: schema.RoleAssistant, Content: "calling", ToolCalls: []schema.ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}}},
		{Role: schema.RoleTool, Content: "contents", ToolCallID: "call-1"},
	}
}

func providerTestTool() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        "read_file",
		Description: "read",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
		Risk:         schema.RiskRead,
		ParallelSafe: true,
	}
}
