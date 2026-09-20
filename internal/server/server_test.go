package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/runstore"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/taskstore"
	"github.com/royal007a/01agent/internal/tools"
)

type testProvider struct {
	calls atomic.Int32
	t     *testing.T
}

type failingProvider struct{}

func (failingProvider) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
	return schema.Generation{}, errors.New("invalid provider credentials")
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

func TestReadinessPerformsRealProviderProbe(t *testing.T) {
	handler, err := New(Config{
		Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Provider: failingProvider{},
		ReadinessTTL: time.Minute, ReadinessTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), "invalid provider credentials") {
		t.Fatalf("ready = %d %s", ready.Code, ready.Body.String())
	}
}

func TestHandlerEnqueuesSteerWithClaimAckQueue(t *testing.T) {
	store, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(),
		Store: store, InputQueue: store, InputEnqueuer: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run-steer/inputs", strings.NewReader(`{"id":"steer-1","kind":"user_steer","content":"also update tests"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"id":"steer-1"`) {
		t.Fatalf("enqueue=%d %s", response.Code, response.Body.String())
	}
	claim, err := store.Claim(context.Background(), "run-steer", "turn-test", 10)
	if err != nil || len(claim.Items) != 1 || claim.Items[0].Content != "also update tests" {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
}

func TestBackgroundTaskHTTPStateMachineDeliversTerminalOutput(t *testing.T) {
	inbox, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := taskstore.New(t.TempDir(), inbox, inbox)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(),
		Store: inbox, InputQueue: inbox, InputEnqueuer: inbox, Tasks: tasks,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	created := call(http.MethodPost, "/v1/tasks", `{"id":"task-http","parent_run_id":"run-parent","title":"inspect"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	started := call(http.MethodPost, "/v1/tasks/task-http/events", `{"action":"start","owner_id":"worker"}`)
	if started.Code != http.StatusOK || !strings.Contains(started.Body.String(), `"state":"running"`) {
		t.Fatalf("start=%d %s", started.Code, started.Body.String())
	}
	completed := call(http.MethodPost, "/v1/tasks/task-http/events", `{"action":"complete","owner_id":"worker","output":"done"}`)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("complete=%d %s", completed.Code, completed.Body.String())
	}
	claim, err := inbox.Claim(context.Background(), "run-parent", "turn-parent", 10)
	if err != nil || len(claim.Items) != 1 || claim.Items[0].Kind != engine.InputTask {
		t.Fatalf("terminal output not delivered: claim=%#v err=%v", claim, err)
	}
}
