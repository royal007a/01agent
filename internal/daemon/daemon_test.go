package daemon

import (
	"context"
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
