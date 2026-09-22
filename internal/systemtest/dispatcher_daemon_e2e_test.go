package systemtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/agentregistry"
	"github.com/royal007a/01agent/internal/computer"
	"github.com/royal007a/01agent/internal/daemon"
	"github.com/royal007a/01agent/internal/dispatcher"
	agentserver "github.com/royal007a/01agent/internal/server"
	"github.com/royal007a/01agent/internal/tools"
	"github.com/royal007a/01agent/internal/workitem"
)

type recordingExecutor struct {
	computerID string
	mu         sync.Mutex
	requests   []computer.RunRequest
	failChild  bool
}

func (e *recordingExecutor) Execute(_ context.Context, _ string, request computer.RunRequest) (computer.RunResult, error) {
	e.mu.Lock()
	e.requests = append(e.requests, request)
	shouldFail := e.failChild && request.TaskID == "child" && request.Mode == computer.RunExecute
	if shouldFail {
		e.failChild = false
	}
	e.mu.Unlock()
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte(request.RunID))
	result := computer.RunResult{
		RunID: request.RunID, TaskID: request.TaskID, Success: !shouldFail, Terminal: "completed",
		Summary: "completed " + request.TaskID, Evidence: []string{"executor=" + e.computerID},
		ArtifactURI: "memory://" + request.RunID, ArtifactHash: hex.EncodeToString(digest[:]),
		StartedAt: now, CompletedAt: now,
	}
	if shouldFail {
		result.Terminal, result.Summary = "provider_error", "transient provider failure"
	}
	if request.Mode == computer.RunReview {
		result.Decision, result.Reason = "pass", "independent review passed"
	}
	return result, nil
}

func (e *recordingExecutor) saw(taskID string, mode computer.RunMode) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, request := range e.requests {
		if request.TaskID == taskID && request.Mode == mode {
			return true
		}
	}
	return false
}

func (e *recordingExecutor) count(taskID string, mode computer.RunMode) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, request := range e.requests {
		if request.TaskID == taskID && request.Mode == mode {
			count++
		}
	}
	return count
}

func TestTwoOutboundDaemonsCompleteParentChildWithIndependentRecoveryReview(t *testing.T) {
	root := t.TempDir()
	tasks, err := workitem.New(filepath.Join(root, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := agentregistry.New(filepath.Join(root, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	computers, err := computer.New(filepath.Join(root, "computers"))
	if err != nil {
		t.Fatal(err)
	}
	dispatches, err := dispatcher.New(filepath.Join(root, "dispatcher"), tasks, agents, computers)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"agent-a", "agent-b"} {
		if _, err := agents.Create(context.Background(), agentregistry.CreateInput{OperationID: "create-" + id, ID: id, WorkspaceID: "workspace", Name: id, CreatedBy: "owner", PromptRef: "prompts/" + id + ".md", Model: "test", MyRole: "member", SessionID: "session-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"computer-a", "computer-b"} {
		if _, err := computers.Register(context.Background(), computer.RegisterInput{OperationID: "register-" + id, ID: id, OwnerID: "owner", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := agentserver.New(agentserver.Config{Token: "secret", WorkDir: root, Registry: tools.NewRegistry(), WorkItems: tasks, Agents: agents, Computers: computers, Dispatcher: dispatches})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executorA := &recordingExecutor{computerID: "computer-a"}
	executorB := &recordingExecutor{computerID: "computer-b", failChild: true}
	done := make(chan error, 2)
	for _, config := range []struct {
		id       string
		executor daemon.Executor
	}{{"computer-a", executorA}, {"computer-b", executorB}} {
		config := config
		go func() {
			done <- daemon.Run(ctx, daemon.Config{ServerURL: server.URL, Token: "secret", ComputerID: config.id, RootDir: filepath.Join(root, config.id), PollInterval: 5 * time.Millisecond, Tools: map[string]string{"runtime": "test"}, Sandboxes: []string{"test"}, Executor: config.executor})
		}()
	}
	waitFor(t, 5*time.Second, func() bool {
		state, stateErr := computers.GetState(context.Background())
		return stateErr == nil && state.Computers["computer-a"].Status == computer.Online && state.Computers["computer-b"].Status == computer.Online
	})
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-a", AgentID: "agent-a", TargetComputerID: "computer-a", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-b", AgentID: "agent-b", TargetComputerID: "computer-b", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	createTask(t, tasks, "parent", "", "agent-a", "agent-b")
	createTask(t, tasks, "child", "parent", "agent-b", "agent-a")
	if _, err := dispatches.Start(context.Background(), dispatcher.StartInput{OperationID: "start-parent", ID: "parent-execution", TaskID: "parent", MaxAttempts: 3, TimeoutSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatches.Start(context.Background(), dispatcher.StartInput{OperationID: "start-child", ID: "child-execution", TaskID: "child", MaxAttempts: 3, TimeoutSeconds: 30}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool {
		if _, reconcileErr := dispatches.Reconcile(context.Background()); reconcileErr != nil {
			t.Fatal(reconcileErr)
		}
		executions, listErr := dispatches.List(context.Background())
		if listErr != nil || len(executions) != 2 {
			return false
		}
		return executions[0].Phase == dispatcher.Succeeded && executions[1].Phase == dispatcher.Succeeded
	})
	parent, _ := tasks.Get(context.Background(), "parent")
	child, _ := tasks.Get(context.Background(), "child")
	if parent.State != workitem.Done || child.State != workitem.Done {
		t.Fatalf("parent=%s child=%s", parent.State, child.State)
	}
	if !executorA.saw("parent", computer.RunExecute) || !executorA.saw("child", computer.RunReview) || !executorB.saw("child", computer.RunExecute) || !executorB.saw("parent", computer.RunReview) {
		t.Fatalf("requests did not cross both Computer bindings: a=%+v b=%+v", executorA.requests, executorB.requests)
	}
	if executorB.count("child", computer.RunExecute) != 2 {
		t.Fatalf("child recovery attempts=%d, want 2", executorB.count("child", computer.RunExecute))
	}
	cancel()
	for range 2 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("daemon did not stop")
		}
	}
}

func createTask(t *testing.T, store *workitem.Store, id, parent, assignee, reviewer string) {
	t.Helper()
	_, err := store.Create(context.Background(), workitem.Create{OperationID: "create-" + id, ID: id, WorkspaceID: "workspace", ChannelID: "channel", ParentTaskID: parent, CreatorID: "owner", Title: id, Objective: "complete " + id, Requirements: []workitem.Requirement{{ID: "R1", Text: "produce evidence"}}, Scope: workitem.Scope{Allow: []string{"."}}, StopConditions: []string{"verified"}, AssigneeID: assignee, Gate: workitem.GateSpec{Kind: workitem.GateAgent, ReviewerID: reviewer, Checks: []string{"R1"}, RequiredEvidence: []string{"artifact"}, OnReject: "return"}})
	if err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
