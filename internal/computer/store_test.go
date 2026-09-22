package computer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCapabilitySnapshotsRebindLeaseAndCleanupLifecycle(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	register(t, store, "old")
	register(t, store, "new")
	old := connect(t, store, "old", "old-lease", "go1")
	connect(t, store, "new", "new-lease", "go1")
	if len(old.Capabilities) != 1 || old.CurrentCapability == "" {
		t.Fatalf("old capabilities=%+v", old.Capabilities)
	}
	changed, err := store.Connect(context.Background(), Hello{ComputerID: "old", LeaseID: "old-lease", OS: "linux", Arch: "amd64", Runtime: "go2", Sandboxes: []string{"landlock"}}, time.Minute)
	if err != nil || len(changed.Capabilities) != 2 {
		t.Fatalf("changed=%+v err=%v", changed, err)
	}
	first, cleanup, err := store.Rebind(context.Background(), RebindInput{OperationID: "op-bind-old", AgentID: "agent", TargetComputerID: "old", ActorID: "owner", ExpectedBindingRevision: 0})
	if err != nil || cleanup != nil || first.Revision != 1 {
		t.Fatalf("first=%+v cleanup=%+v err=%v", first, cleanup, err)
	}
	run, err := store.AcquireRun(context.Background(), RunLeaseInput{OperationID: "op-run", RunID: "run", AgentID: "agent", ComputerID: "old", LeaseID: "run-lease", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Rebind(context.Background(), RebindInput{OperationID: "op-blocked", AgentID: "agent", TargetComputerID: "new", ActorID: "owner", ExpectedBindingRevision: 1}); !errors.Is(err, ErrActiveRun) {
		t.Fatalf("active run did not block rebind: %v", err)
	}
	if err := store.ReleaseRun(context.Background(), run.RunID, "op-release", run.LeaseID); err != nil {
		t.Fatal(err)
	}
	second, cleanup, err := store.Rebind(context.Background(), RebindInput{OperationID: "op-bind-new", AgentID: "agent", TargetComputerID: "new", ActorID: "owner", ExpectedBindingRevision: 1})
	if err != nil || cleanup == nil || second.ComputerID != "new" || cleanup.ComputerID != "old" {
		t.Fatalf("second=%+v cleanup=%+v err=%v", second, cleanup, err)
	}
	command, err := store.PollCommand(context.Background(), "old", "old-lease")
	if err != nil || command.ID != cleanup.ID || command.State != CommandSent {
		t.Fatalf("poll=%+v err=%v", command, err)
	}
	failed, err := store.AckCommand(context.Background(), "old", "old-lease", CommandAck{CommandID: command.ID, Success: false, Error: "disk busy"})
	if err != nil || failed.State != CommandFailed {
		t.Fatalf("ack=%+v err=%v", failed, err)
	}
	state, err := store.GetState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Bindings["agent"].ComputerID != "new" {
		t.Fatal("cleanup failure rolled back the new binding")
	}
}

func TestOfflineTargetAndConnectionLeaseAreFailClosed(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	register(t, store, "computer")
	if _, _, err := store.Rebind(context.Background(), RebindInput{OperationID: "op-bind", AgentID: "agent", TargetComputerID: "computer", ActorID: "owner", ExpectedBindingRevision: 0}); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline bind err=%v", err)
	}
	connect(t, store, "computer", "lease-a", "go1")
	if _, err := store.Connect(context.Background(), Hello{ComputerID: "computer", LeaseID: "lease-b", OS: "linux", Arch: "amd64", Runtime: "go1"}, time.Minute); !errors.Is(err, ErrLease) {
		t.Fatalf("competing connection err=%v", err)
	}
}

func register(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.Register(context.Background(), RegisterInput{OperationID: "register-" + id, ID: id, OwnerID: "owner", Name: id}); err != nil {
		t.Fatal(err)
	}
}

func connect(t *testing.T, store *Store, id, lease, runtime string) Computer {
	t.Helper()
	computer, err := store.Connect(context.Background(), Hello{ComputerID: id, LeaseID: lease, OS: "linux", Arch: "amd64", Runtime: runtime, Sandboxes: []string{"landlock"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return computer
}
