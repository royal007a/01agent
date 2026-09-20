package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	t.Setenv("AGENT_PROVIDER", "claude")
	t.Setenv("AGENT_MODEL", "model-from-env")
	var stderr bytes.Buffer
	config, prompt, err := parseFlags([]string{"--thinking", "--plan-mode", "--timeout", "2s", "inspect", "the", "repo"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if config.provider != "claude" || config.model != "model-from-env" || !config.thinking || !config.planMode || config.timeout != 2*time.Second {
		t.Fatalf("config = %#v", config)
	}
	if prompt != "inspect the repo" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestHelpReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage: 01agent") {
		t.Fatalf("help = %q", stderr.String())
	}
}

func TestRunVersionNeedsNoProviderConfiguration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunEndToEndWithOpenAICompatibleServer(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("physical observation"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		turn := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		if turn == 1 {
			fmt.Fprint(writer, `{"id":"one","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"read-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"hello.txt\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
			return
		}
		messages := body["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		if last["role"] != "tool" || !strings.Contains(last["content"].(string), "physical observation") {
			t.Errorf("last message = %#v", last)
		}
		fmt.Fprint(writer, `{"id":"two","object":"chat.completion","created":2,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"verified"}}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}}`)
	}))
	defer server.Close()

	t.Setenv("AGENT_API_KEY", "test")
	t.Setenv("AGENT_MODEL", "test")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--base-url", server.URL + "/v1", "--workdir", workDir, "inspect"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "verified" || requests.Load() != 2 {
		t.Fatalf("stdout = %q, requests = %d", stdout.String(), requests.Load())
	}
}

func TestParseFlagsRequiresPrompt(t *testing.T) {
	var stderr bytes.Buffer
	if _, _, err := parseFlags(nil, &stderr); err == nil {
		t.Fatal("parseFlags accepted an empty prompt")
	}
}
