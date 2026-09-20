package approvalstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/tools"
)

func TestExactCallApprovalIsDurableAndSingleUse(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	input := tools.ApprovalRequest{
		RunID: "run-1", TurnID: "turn-1", LeaseID: "lease-1", Admission: 1,
		CapabilityDigest: "cap-1", ToolName: "write_file",
		Arguments: json.RawMessage(`{"content":"hello","path":"x.txt"}`),
	}
	first, err := store.Resolve(context.Background(), input)
	if err != nil || first.State != string(Pending) || first.ID == "" || first.Allowed {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	reopened, err := New(dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.Decide(context.Background(), first.ID, Approved, "reviewer")
	if err != nil || record.State != Approved {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	input.LeaseID = "lease-2"
	input.Admission = 2
	allowed, err := reopened.Resolve(context.Background(), input)
	if err != nil || !allowed.Allowed || allowed.State != string(Consumed) {
		t.Fatalf("allowed=%#v err=%v", allowed, err)
	}
	again, err := reopened.Resolve(context.Background(), input)
	if err != nil || again.Allowed || again.State != string(Consumed) {
		t.Fatalf("again=%#v err=%v", again, err)
	}
}

func TestApprovalRejectsSemanticChangeAndExpires(t *testing.T) {
	store, err := New(t.TempDir(), time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	input := tools.ApprovalRequest{RunID: "run", TurnID: "turn", LeaseID: "lease", CapabilityDigest: "cap", ToolName: "bash", Arguments: json.RawMessage(`{"command":"true"}`)}
	first, err := store.Resolve(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	expired, err := store.Resolve(context.Background(), input)
	if err != nil || expired.State != string(Expired) || expired.Allowed {
		t.Fatalf("expired=%#v err=%v", expired, err)
	}
	changed := input
	changed.Arguments = json.RawMessage(`{"command":"false"}`)
	second, err := store.Resolve(context.Background(), changed)
	if err != nil || second.ID == first.ID || second.State != string(Pending) {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}
