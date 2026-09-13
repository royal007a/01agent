package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/tools"
)

type testProvider struct {
	calls atomic.Int32
	t     *testing.T
}

func (p *testProvider) Generate(_ context.Context, messages []schema.Message, definitions []schema.ToolDefinition) (schema.Generation, error) {
	if p.calls.Add(1) == 1 {
		if len(definitions) != 1 {
			p.t.Errorf("definitions = %d, want 1", len(definitions))
		}
		return schema.Generation{Message: schema.Message{ToolCalls: []schema.ToolCall{{
			ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"hello.txt"}`),
		}}}}, nil
	}
	last := messages[len(messages)-1]
	if last.Role != schema.RoleTool || !strings.Contains(last.Content, "observation") {
		p.t.Errorf("last message = %#v", last)
	}
	return schema.Generation{Message: schema.Message{Content: "done"}}, nil
}

func TestHandlerHealthAuthAndRun(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("observation"), 0o600); err != nil {
		t.Fatal(err)
	}
	readFile, err := tools.NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(readFile); err != nil {
		t.Fatal(err)
	}
	model := &testProvider{t: t}
	handler, err := New(Config{Version: "test", Token: "secret", WorkDir: workDir, Provider: model, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"version":"test"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"prompt":"inspect"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized code = %d", unauthorized.Code)
	}

	runRequest := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"prompt":"inspect"}`))
	runRequest.Header.Set("Authorization", "Bearer secret")
	runRequest.Header.Set("Content-Type", "application/json")
	run := httptest.NewRecorder()
	handler.ServeHTTP(run, runRequest)
	if run.Code != http.StatusOK || !strings.Contains(run.Body.String(), `"content":"done"`) {
		t.Fatalf("run = %d %s", run.Code, run.Body.String())
	}
	if model.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", model.calls.Load())
	}
}

func TestHandlerReportsNotReadyWithoutProvider(t *testing.T) {
	registry := tools.NewRegistry()
	handler, err := New(Config{Token: "secret", WorkDir: t.TempDir(), Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready code = %d", ready.Code)
	}
}

func TestHandlerRequiresToken(t *testing.T) {
	if _, err := New(Config{WorkDir: t.TempDir(), Registry: tools.NewRegistry()}); err == nil {
		t.Fatal("New accepted an empty token")
	}
}
