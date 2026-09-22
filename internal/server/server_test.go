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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/agentregistry"
	"github.com/royal007a/01agent/internal/approvalstore"
	"github.com/royal007a/01agent/internal/attention"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/runstore"
	"github.com/royal007a/01agent/internal/schema"
	"github.com/royal007a/01agent/internal/sessionstore"
	"github.com/royal007a/01agent/internal/taskstore"
	"github.com/royal007a/01agent/internal/tools"
	"github.com/royal007a/01agent/internal/workitem"
)

type testProvider struct {
	calls atomic.Int32
	t     *testing.T
}

type failingProvider struct{}

type countingFailProvider struct{ calls atomic.Int32 }

func (p *countingFailProvider) Generate(context.Context, []schema.Message, []schema.ToolDefinition) (schema.Generation, error) {
	p.calls.Add(1)
	return schema.Generation{}, errors.New("provider must not be called during result recovery")
}

type sessionProvider struct {
	mu    sync.Mutex
	calls int
	t     *testing.T
}

func (p *sessionProvider) Generate(_ context.Context, messages []schema.Message, _ []schema.ToolDefinition) (schema.Generation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch p.calls {
	case 1:
		if messages[len(messages)-1].Role != schema.RoleUser || messages[len(messages)-1].Content != "remember blue" {
			p.t.Errorf("first session messages=%#v", messages)
		}
		return schema.Generation{Message: schema.Message{Content: "remembered blue"}}, nil
	case 2:
		seenMemory := false
		for _, message := range messages {
			if message.Role == schema.RoleAssistant && message.Content == "remembered blue" {
				seenMemory = true
			}
		}
		if !seenMemory || messages[len(messages)-1].Role != schema.RoleUser || messages[len(messages)-1].Content != "what color" {
			p.t.Errorf("second session messages=%#v", messages)
		}
		return schema.Generation{Message: schema.Message{Content: "blue"}}, nil
	default:
		p.t.Errorf("unexpected provider call %d", p.calls)
		return schema.Generation{Message: schema.Message{Content: "unexpected"}}, nil
	}
}

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

func TestApprovalDecisionHTTPAPI(t *testing.T) {
	approvals, err := approvalstore.New(t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := approvals.Resolve(context.Background(), tools.ApprovalRequest{
		RunID: "run-http", TurnID: "turn-http", LeaseID: "lease-http", CapabilityDigest: "cap-http",
		ToolName: "bash", Arguments: json.RawMessage(`{"command":"true"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{
		Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Approvals: approvals,
		InputQueue: inbox, InputEnqueuer: inbox, Store: inbox,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/approvals/"+pending.ID+"/decision", strings.NewReader(`{"decision":"approved","actor":"operator"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"approved"`) {
		t.Fatalf("decision=%d %s", response.Code, response.Body.String())
	}
	listRequest := httptest.NewRequest(http.MethodGet, "/v1/approvals?state=approved", nil)
	listRequest.Header.Set("Authorization", "Bearer secret")
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, listRequest)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), pending.ID) {
		t.Fatalf("list=%d %s", listed.Code, listed.Body.String())
	}
	claim, err := inbox.Claim(context.Background(), "run-http", "turn-resume", 10)
	if err != nil || len(claim.Items) != 1 || claim.Items[0].Kind != engine.InputTool || !strings.Contains(claim.Items[0].Content, "Retry that exact tool call") {
		t.Fatalf("approval input=%#v err=%v", claim, err)
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

func TestProductTaskV2HTTPDeliveryLifecycle(t *testing.T) {
	workItems, err := workitem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), WorkItems: workItems})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	created := call(http.MethodPost, "/v2/tasks", `{
		"operation_id":"op-create","id":"product-task","workspace_id":"workspace","channel_id":"channel",
		"creator_id":"creator","title":"Ship","objective":"Deliver verified change",
		"requirements":[{"id":"R1","text":"preserve behavior"}],"scope":{"allow":["internal/"]},
		"stop_conditions":["tests pass"],"gate":{"kind":"agent","reviewer_id":"reviewer","checks":["R1"],"required_evidence":["tests"],"on_reject":"return"}
	}`)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"state":"todo"`) {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	claimed := call(http.MethodPost, "/v2/tasks/product-task/actions", `{"action":"claim","operation_id":"op-claim","expected_revision":1,"owner_id":"worker","lease_id":"lease-a","ttl_seconds":60}`)
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"state":"in_progress"`) {
		t.Fatalf("claim=%d %s", claimed.Code, claimed.Body.String())
	}
	digest := strings.Repeat("a", 64)
	artifact := call(http.MethodPost, "/v2/tasks/product-task/artifacts", `{"operation_id":"op-artifact","expected_revision":2,"owner_id":"worker","lease_id":"lease-a","id":"commit","version":"abc123","kind":"commit","uri":"git://abc123","digest":"`+digest+`"}`)
	if artifact.Code != http.StatusCreated || !strings.Contains(artifact.Body.String(), `"version":"abc123"`) {
		t.Fatalf("artifact=%d %s", artifact.Code, artifact.Body.String())
	}
	submitted := call(http.MethodPost, "/v2/tasks/product-task/actions", `{
		"action":"submit","operation_id":"op-submit","expected_revision":3,"owner_id":"worker","lease_id":"lease-a",
		"handoff":{"contract_revision":1,"author_id":"worker","summary":"done","evidence":["tests pass"],"artifacts":[{"id":"commit","version":"abc123","digest":"`+digest+`"}],"next_action":"review"}
	}`)
	if submitted.Code != http.StatusOK || !strings.Contains(submitted.Body.String(), `"state":"in_review"`) {
		t.Fatalf("submit=%d %s", submitted.Code, submitted.Body.String())
	}
	reviewed := call(http.MethodPost, "/v2/tasks/product-task/actions", `{
		"action":"review","operation_id":"op-review","expected_revision":4,
		"gate_result":{"decision":"pass","reviewer_id":"reviewer","artifact_versions":[{"id":"commit","version":"abc123","digest":"`+digest+`"}],"evidence":["review passed"],"reason":"meets R1"}
	}`)
	if reviewed.Code != http.StatusOK || !strings.Contains(reviewed.Body.String(), `"state":"done"`) {
		t.Fatalf("review=%d %s", reviewed.Code, reviewed.Body.String())
	}
	listed := call(http.MethodGet, "/v2/tasks?workspace_id=workspace&channel_id=channel", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"product-task"`) {
		t.Fatalf("list=%d %s", listed.Code, listed.Body.String())
	}
	loadedArtifact := call(http.MethodGet, "/v2/artifacts/commit/versions/abc123", "")
	if loadedArtifact.Code != http.StatusOK || !strings.Contains(loadedArtifact.Body.String(), `"digest":"`+digest+`"`) {
		t.Fatalf("get artifact=%d %s", loadedArtifact.Code, loadedArtifact.Body.String())
	}
}

func TestAgentInboxFreshnessHTTPBarrier(t *testing.T) {
	attentionStore, err := attention.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Attention: attentionStore})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	published := call(http.MethodPost, "/v2/conversations/chat/messages", `{"operation_id":"op-publish-1","message_id":"message-1","author_id":"human","kind":"human","content":"start","deliveries":[{"agent_id":"lucy","priority":"direct"}]}`)
	if published.Code != http.StatusCreated || !strings.Contains(published.Body.String(), `"seq":1`) {
		t.Fatalf("publish=%d %s", published.Code, published.Body.String())
	}
	claimed := call(http.MethodPost, "/v2/agents/lucy/inbox/claim", `{"operation_id":"op-claim","lease_id":"lease","ttl_seconds":60}`)
	if claimed.Code != http.StatusOK || !strings.Contains(claimed.Body.String(), `"claim_read_seq":1`) {
		t.Fatalf("claim=%d %s", claimed.Code, claimed.Body.String())
	}
	state, err := attentionStore.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var itemID string
	for id := range state.Inbox {
		itemID = id
	}
	corrected := call(http.MethodPost, "/v2/conversations/chat/messages", `{"operation_id":"op-publish-2","message_id":"message-2","author_id":"human","kind":"human","content":"change direction","deliveries":[{"agent_id":"lucy","priority":"human_correction"}]}`)
	if corrected.Code != http.StatusCreated {
		t.Fatalf("correction=%d %s", corrected.Code, corrected.Body.String())
	}
	stale := call(http.MethodPost, "/v2/inbox/"+itemID+"/fresh-replies", `{"operation_id":"op-stale","message_id":"reply-old","agent_id":"lucy","lease_id":"lease","read_seq":1,"content":"old answer"}`)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"message-2"`) || !strings.Contains(stale.Body.String(), `"old answer"`) {
		t.Fatalf("stale=%d %s", stale.Code, stale.Body.String())
	}
	refreshed := call(http.MethodPost, "/v2/inbox/"+itemID+"/actions", `{"action":"refresh","operation_id":"op-refresh","agent_id":"lucy","lease_id":"lease"}`)
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), `"claim_read_seq":2`) {
		t.Fatalf("refresh=%d %s", refreshed.Code, refreshed.Body.String())
	}
	sent := call(http.MethodPost, "/v2/inbox/"+itemID+"/fresh-replies", `{"operation_id":"op-reply","message_id":"reply-new","agent_id":"lucy","lease_id":"lease","read_seq":2,"content":"revised answer"}`)
	if sent.Code != http.StatusCreated || !strings.Contains(sent.Body.String(), `"seq":3`) {
		t.Fatalf("send=%d %s", sent.Code, sent.Body.String())
	}
}

func TestPersistentAgentRelationshipAndSessionGenerationHTTP(t *testing.T) {
	agents, err := agentregistry.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Agents: agents})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	created := call(http.MethodPost, "/v2/agents", `{"operation_id":"op-create","id":"lili","workspace_id":"workspace","name":"Lili","created_by":"owner","prompt_ref":"prompts/lili.md","skills":["review"],"model":"model-a","my_role":"engineer","session_id":"session-1"}`)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"current_session_generation":1`) {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	revised := call(http.MethodPost, "/v2/agents/lili/relationships/revisions", `{"operation_id":"op-rel","expected_revision":1,"actor_id":"owner","my_role":"lead","reason":"proven delegation","teammates":[{"agent_id":"lucy","role":"database","delegate_when":["schema"],"report_back_with":["tests","rollback"]}]}`)
	if revised.Code != http.StatusCreated || !strings.Contains(revised.Body.String(), `"current_relationship_revision_id":"relationship-v2"`) {
		t.Fatalf("revise=%d %s", revised.Code, revised.Body.String())
	}
	rotated := call(http.MethodPost, "/v2/agents/lili/sessions/rotate", `{"operation_id":"op-rotate","expected_revision":2,"expected_generation":1,"actor_id":"owner","new_session_id":"session-2","handoff":{"scope":"agent/channel","current_task":"task-1","references":["thread-1"],"confirmed_facts":["tests pass"],"unresolved_items":["production"]}}`)
	if rotated.Code != http.StatusCreated || !strings.Contains(rotated.Body.String(), `"current_session_generation":2`) || !strings.Contains(rotated.Body.String(), `"state":"retired"`) {
		t.Fatalf("rotate=%d %s", rotated.Code, rotated.Body.String())
	}
	listed := call(http.MethodGet, "/v2/agents?workspace_id=workspace", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"lili"`) {
		t.Fatalf("list=%d %s", listed.Code, listed.Body.String())
	}
}

func TestSessionTurnsPersistAcrossHandlerRestartAndReplayIdempotently(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	runs, err := runstore.New(runDir)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := sessionstore.New(filepath.Join(runDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	model := &sessionProvider{t: t}
	newHandler := func(sessionStore *sessionstore.Store) *Handler {
		handler, err := New(Config{
			Token: "secret", WorkDir: workDir, Registry: tools.NewRegistry(), Provider: model,
			Store: runs, InputQueue: runs, Sessions: sessionStore,
		})
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	call := func(handler *Handler, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/chat-1/turns", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	first := call(newHandler(sessions), `{"operation_id":"message-1","prompt":"remember blue"}`)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"content":"remembered blue"`) || !strings.Contains(first.Body.String(), `"cached":false`) {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	reopened, err := sessionstore.New(filepath.Join(runDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(reopened)
	second := call(handler, `{"operation_id":"message-2","prompt":"what color"}`)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"content":"blue"`) || !strings.Contains(second.Body.String(), `"session_revision":4`) {
		t.Fatalf("second=%d %s", second.Code, second.Body.String())
	}
	replayed := call(handler, `{"operation_id":"message-2","prompt":"what color"}`)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"cached":true`) || !strings.Contains(replayed.Body.String(), `"content":"blue"`) {
		t.Fatalf("replayed=%d %s", replayed.Code, replayed.Body.String())
	}
	model.mu.Lock()
	calls := model.calls
	model.mu.Unlock()
	if calls != 2 {
		t.Fatalf("provider calls=%d want=2", calls)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/v1/sessions/chat-1", nil)
	getRequest.Header.Set("Authorization", "Bearer secret")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"operation_id":"message-2"`) || !strings.Contains(getResponse.Body.String(), `"revision":4`) {
		t.Fatalf("get=%d %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestSessionTurnRecoversResultCommittedBeforeSessionBarrier(t *testing.T) {
	workDir := t.TempDir()
	runDir := t.TempDir()
	runs, err := runstore.New(runDir)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := sessionstore.New(filepath.Join(runDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	admission, err := sessions.BeginWithSemantics(
		context.Background(), "chat-recover", "message-recover", "recover me", workDir, "run-recover",
		`{"thinking":false,"plan_mode":false,"approved_tools":[]}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	result := engine.RunResult{
		RunID: "run-recover", Reason: schema.TerminalCompleted,
		FinalMessage: schema.Message{Role: schema.RoleAssistant, Content: "already complete"},
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "system"}, {Role: schema.RoleUser, Content: "recover me"},
			{Role: schema.RoleAssistant, Content: "already complete"},
		},
		Turns: 1, StartedAt: now.Add(-time.Second), CompletedAt: now,
	}
	if err := runs.Complete(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if admission.State.Pending == nil {
		t.Fatal("expected a pending session turn")
	}

	model := &countingFailProvider{}
	handler, err := New(Config{
		Token: "secret", WorkDir: workDir, Registry: tools.NewRegistry(), Provider: model,
		Store: runs, InputQueue: runs, Sessions: sessions,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/chat-recover/turns", strings.NewReader(`{"operation_id":"message-recover","prompt":"recover me"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"content":"already complete"`) || !strings.Contains(response.Body.String(), `"cached":false`) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if model.calls.Load() != 0 {
		t.Fatalf("provider calls=%d", model.calls.Load())
	}
	state, err := sessions.Get(context.Background(), "chat-recover")
	if err != nil || state.Pending != nil || len(state.Turns) != 1 || state.Revision != 2 {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}
