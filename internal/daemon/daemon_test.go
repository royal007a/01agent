package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/computer"
	agentserver "github.com/royal007a/01agent/internal/server"
	"github.com/royal007a/01agent/internal/tools"
)

type executorFunc func(context.Context, string, computer.RunRequest) (computer.RunResult, error)

func (fn executorFunc) Execute(ctx context.Context, root string, request computer.RunRequest) (computer.RunResult, error) {
	return fn(ctx, root, request)
}

func TestOutboundDaemonExecutesRunAndReturnsStructuredResult(t *testing.T) {
	computers, err := computer.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computers.Register(context.Background(), computer.RegisterInput{OperationID: "register-runner", ID: "runner", OwnerID: "owner", Name: "runner"}); err != nil {
		t.Fatal(err)
	}
	handler, err := agentserver.New(agentserver.Config{Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Computers: computers})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	requests := make(chan computer.RunRequest, 1)
	executor := executorFunc(func(_ context.Context, _ string, request computer.RunRequest) (computer.RunResult, error) {
		requests <- request
		now := time.Now().UTC()
		digest := sha256.Sum256([]byte(request.RunID))
		return computer.RunResult{RunID: request.RunID, TaskID: request.TaskID, Success: true, Terminal: "completed", Summary: "done", Evidence: []string{"scripted"}, ArtifactURI: "memory://" + request.RunID, ArtifactHash: hex.EncodeToString(digest[:]), StartedAt: now, CompletedAt: now}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: server.URL, Token: "secret", ComputerID: "runner", RootDir: t.TempDir(), PollInterval: 10 * time.Millisecond, Tools: map[string]string{"runtime": "test"}, Sandboxes: []string{"test"}, Executor: executor})
	}()
	waitFor(t, func() bool {
		state, stateErr := computers.GetState(context.Background())
		return stateErr == nil && state.Computers["runner"].Status == computer.Online
	})
	state, err := computers.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-agent", AgentID: "agent", TargetComputerID: "runner", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := computers.AcquireRun(context.Background(), computer.RunLeaseInput{OperationID: "acquire-run", RunID: "run-1", AgentID: "agent", ComputerID: "runner", LeaseID: "run-lease", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	request := computer.RunRequest{RunID: "run-1", TaskID: "task-1", AgentID: "agent", Mode: computer.RunExecute, Prompt: "do it", AgentRevisionID: "agent-r1", RelationshipRevisionID: "relationship-r1", ExpectedCapabilityDigest: state.Computers["runner"].CurrentCapability, TimeoutSeconds: 30, MaxTurns: 4, TaskRevision: 1, TaskContractRevision: 1}
	command, err := computers.QueueRun(context.Background(), "queue-run", request)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		current, stateErr := computers.GetState(context.Background())
		return stateErr == nil && current.Commands[command.ID].State == computer.CommandAcked
	})
	select {
	case received := <-requests:
		if received.RunID != request.RunID || received.ExpectedCapabilityDigest != request.ExpectedCapabilityDigest {
			t.Fatalf("received mismatched request: %+v", received)
		}
	default:
		t.Fatal("executor did not receive run")
	}
	current, err := computers.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := current.Commands[command.ID].Result
	if result == nil || !result.Success || result.TaskID != "task-1" || result.ArtifactHash == "" {
		t.Fatalf("structured result=%+v", result)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestOutboundDaemonExecutesTrackedOldComputerCleanup(t *testing.T) {
	computers, err := computer.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "new"} {
		if _, err := computers.Register(context.Background(), computer.RegisterInput{OperationID: "register-" + id, ID: id, OwnerID: "owner", Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := agentserver.New(agentserver.Config{Token: "secret", WorkDir: t.TempDir(), Registry: tools.NewRegistry(), Computers: computers})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	root := t.TempDir()
	agentDir := filepath.Join(root, "agents", "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "memory.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	keepDir := filepath.Join(root, "agents", "keep")
	if err := os.MkdirAll(keepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: server.URL, Token: "secret", ComputerID: "old", RootDir: root, PollInterval: 10 * time.Millisecond, Tools: map[string]string{"runtime": "test"}, Sandboxes: []string{"test"}})
	}()
	waitFor(t, func() bool {
		state, err := computers.GetState(context.Background())
		return err == nil && state.Computers["old"].Status == computer.Online
	})
	if _, err := computers.Connect(context.Background(), computer.Hello{ComputerID: "new", LeaseID: "new-lease", OS: "linux", Arch: "amd64", Runtime: "test"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-old", AgentID: "agent", TargetComputerID: "old", ActorID: "owner", ExpectedBindingRevision: 0}); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := computers.Rebind(context.Background(), computer.RebindInput{OperationID: "bind-new", AgentID: "agent", TargetComputerID: "new", ActorID: "owner", ExpectedBindingRevision: 1})
	if err != nil || cleanup == nil {
		t.Fatalf("cleanup=%+v err=%v", cleanup, err)
	}
	waitFor(t, func() bool {
		state, err := computers.GetState(context.Background())
		return err == nil && state.Commands[cleanup.ID].State == computer.CommandAcked
	})
	if _, err := os.Stat(agentDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old agent directory still exists: %v", err)
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Fatalf("cleanup escaped exact agent directory: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestCleanupRejectsUnknownCommands(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "trash"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execute(root, computer.Command{ID: "command", Kind: "shell", AgentID: "agent"}); err == nil {
		t.Fatal("unknown daemon command was accepted")
	}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
