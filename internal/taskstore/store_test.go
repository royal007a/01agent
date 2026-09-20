package taskstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/runstore"
)

func TestTaskLifecycleDeliveryAndConsumption(t *testing.T) {
	inbox, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.TempDir(), inbox, inbox)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create(context.Background(), Task{ID: "task-test", ParentRunID: "run-parent", ParentTurnID: "turn-parent", Title: "inspect callers"})
	if err != nil || task.State != Queued {
		t.Fatalf("created=%#v err=%v", task, err)
	}
	task, err = store.Start(context.Background(), task.ID, "worker-1")
	if err != nil || task.State != Running {
		t.Fatalf("started=%#v err=%v", task, err)
	}
	if _, err := store.Heartbeat(context.Background(), task.ID, "wrong-worker"); err == nil {
		t.Fatal("heartbeat from the wrong owner succeeded")
	}
	task, err = store.Complete(context.Background(), task.ID, "worker-1", "all callers are safe")
	if err != nil || task.State != Succeeded || task.DeliveredAt.IsZero() {
		t.Fatalf("completed=%#v err=%v", task, err)
	}
	claim, err := inbox.Claim(context.Background(), "run-parent", "turn-parent", 10)
	if err != nil || len(claim.Items) != 1 || claim.Items[0].ID != task.ID || claim.Items[0].Kind != engine.InputTask {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if err := inbox.Ack(context.Background(), claim.ID, 9); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConsumption(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err = store.Get(context.Background(), task.ID)
	if err != nil || task.ConsumedRevision != 9 || task.ConsumedAt.IsZero() {
		t.Fatalf("consumed=%#v err=%v", task, err)
	}
}

func TestReconcileMarksExpiredExecutorLostAndDelivers(t *testing.T) {
	inbox, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.TempDir(), inbox, inbox)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	task, err := store.Create(context.Background(), Task{ID: "task-lost", ParentRunID: "run-parent", Title: "background work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(context.Background(), task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	lost, err := store.ReconcileLost(context.Background(), clock, time.Minute)
	if err != nil || len(lost) != 1 || lost[0].State != Lost || lost[0].DeliveredAt.IsZero() {
		t.Fatalf("lost=%#v err=%v", lost, err)
	}
	claim, err := inbox.Claim(context.Background(), "run-parent", "turn-parent", 10)
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("lost task was not delivered: %#v err=%v", claim, err)
	}
}

type failingDelivery struct{ err error }

func (f failingDelivery) Enqueue(context.Context, engine.QueuedInput) (engine.QueuedInput, error) {
	return engine.QueuedInput{}, f.err
}

func TestPendingTerminalDeliveryCanBeReconciled(t *testing.T) {
	inbox, err := runstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.TempDir(), failingDelivery{err: errors.New("offline")}, inbox)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create(context.Background(), Task{ID: "task-pending", ParentRunID: "run-parent", Title: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(context.Background(), task.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(context.Background(), task.ID, "worker-1", "boom"); err == nil {
		t.Fatal("delivery failure was hidden")
	}
	terminal, err := store.Get(context.Background(), task.ID)
	if err != nil || terminal.State != Failed || !terminal.DeliveredAt.IsZero() {
		t.Fatalf("terminal=%#v err=%v", terminal, err)
	}
	store.delivery = inbox
	if err := store.DeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	delivered, err := store.Get(context.Background(), task.ID)
	if err != nil || delivered.DeliveredAt.IsZero() {
		t.Fatalf("delivered=%#v err=%v", delivered, err)
	}
}
