package automation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/royal007a/01agent/internal/workitem"
)

func TestTickDispatchesSkipsOverlapAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tasks, err := workitem.New(root + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(root+"/automations", tasks)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	created, err := store.Create(ctx, createInput("daily", start, 3))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != Active || !created.NextRunAt.Equal(start) {
		t.Fatalf("created=%#v", created)
	}

	first, err := store.Tick(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Dispatched) != 1 || first.Dispatched[0].TaskID != "daily-run-1" {
		t.Fatalf("first tick=%#v", first)
	}
	if _, err := tasks.Get(ctx, "daily-run-1"); err != nil {
		t.Fatalf("dispatched task missing: %v", err)
	}

	restarted, err := New(root+"/automations", tasks)
	if err != nil {
		t.Fatal(err)
	}
	same, err := restarted.Tick(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(same.Dispatched) != 0 || len(same.Skipped) != 0 {
		t.Fatalf("restart duplicated a dispatch: %#v", same)
	}

	overlap, err := restarted.Tick(ctx, start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(overlap.Skipped) != 1 || overlap.Skipped[0].State != RunSkipped {
		t.Fatalf("overlap=%#v", overlap)
	}
	items, err := restarted.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Runs) != 2 || items[0].Runs[0].State != RunDispatched {
		t.Fatalf("items=%#v", items)
	}
}

func TestTerminalTaskAllowsNextDispatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tasks, err := workitem.New(root + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(root+"/automations", tasks)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	if _, err := store.Create(ctx, createInput("regression", start, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Tick(ctx, start); err != nil {
		t.Fatal(err)
	}
	completeTask(t, tasks, "regression-run-1")

	result, err := store.Tick(ctx, start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Dispatched) != 1 || result.Dispatched[0].TaskID != "regression-run-2" {
		t.Fatalf("next tick=%#v", result)
	}
	items, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Runs[0].State != RunSucceeded || items[0].Runs[1].State != RunDispatched {
		t.Fatalf("runs=%#v", items[0].Runs)
	}
}

func TestConsecutiveFailuresPauseAutomation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tasks, err := workitem.New(root + "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(root+"/automations", tasks)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	if _, err := store.Create(ctx, createInput("pause", start, 2)); err != nil {
		t.Fatal(err)
	}
	first, err := store.Tick(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	closeTask(t, tasks, first.Dispatched[0].TaskID)
	item, err := store.Report(ctx, "pause", ReportInput{OperationID: "report-1", TaskID: first.Dispatched[0].TaskID, Evidence: []string{"provider timeout"}, Reason: "retryable provider failure"})
	if err != nil || item.Status != Active || item.ConsecutiveFailures != 1 {
		t.Fatalf("first report item=%#v err=%v", item, err)
	}
	second, err := store.Tick(ctx, start.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	closeTask(t, tasks, second.Dispatched[0].TaskID)
	item, err = store.Report(ctx, "pause", ReportInput{OperationID: "report-2", TaskID: second.Dispatched[0].TaskID, Evidence: []string{"provider timeout"}, Reason: "retry exhausted"})
	if err != nil || item.Status != Paused || item.ConsecutiveFailures != 2 {
		t.Fatalf("second report item=%#v err=%v", item, err)
	}
	result, err := store.Tick(ctx, start.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Dispatched) != 0 {
		t.Fatalf("paused automation dispatched: %#v", result)
	}
}

func createInput(id string, start time.Time, failureThreshold int) CreateInput {
	return CreateInput{
		OperationID: "create-" + id, ID: id, Name: "Scheduled regression", EverySeconds: 60, StartAt: start,
		FailurePauseThreshold: failureThreshold,
		Template: TaskTemplate{
			WorkspaceID: "workspace", ChannelID: "channel", CreatorID: "scheduler", Title: "Run regression", Objective: "Deliver verified regression results",
			Requirements: []workitem.Requirement{{ID: "R1", Text: "run the fixed suite"}}, Scope: workitem.Scope{Allow: []string{"evals/"}},
			StopConditions: []string{"suite result recorded"}, AssigneeID: "runner",
			Gate: workitem.GateSpec{Kind: workitem.GateCode, ReviewerID: "evaluator", Checks: []string{"R1"}, RequiredEvidence: []string{"result log"}, OnReject: "return to runner"},
		},
	}
}

func completeTask(t *testing.T, tasks *workitem.Store, taskID string) {
	t.Helper()
	ctx := context.Background()
	task, err := tasks.Claim(ctx, taskID, "claim-"+taskID, "runner", "lease-"+taskID, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	task, artifact, err := tasks.AddArtifact(ctx, taskID, workitem.ArtifactInput{OperationID: "artifact-" + taskID, ExpectedRevision: task.Revision, OwnerID: "runner", LeaseID: "lease-" + taskID, ID: "result-" + taskID, Version: "v1", Kind: "report", URI: "memory://" + taskID, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	task, err = tasks.Submit(ctx, taskID, "submit-"+taskID, "runner", "lease-"+taskID, task.Revision, workitem.Handoff{ContractRevision: task.ContractRevision, AuthorID: "runner", Summary: "regression complete", Evidence: []string{"suite passed"}, Artifacts: []workitem.ArtifactRef{{ID: artifact.ID, Version: artifact.Version, Digest: artifact.Digest}}, NextAction: "review"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tasks.Review(ctx, taskID, "review-"+taskID, task.Revision, workitem.GateResult{Decision: workitem.GatePass, ReviewerID: "evaluator", ArtifactVersion: []workitem.ArtifactRef{{ID: artifact.ID, Version: artifact.Version, Digest: artifact.Digest}}, Evidence: []string{"all assertions passed"}, Reason: "meets R1"})
	if err != nil {
		t.Fatal(err)
	}
}

func closeTask(t *testing.T, tasks *workitem.Store, taskID string) {
	t.Helper()
	task, err := tasks.Get(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Close(context.Background(), taskID, "close-"+taskID, "scheduler", "execution failed", task.Revision); err != nil {
		t.Fatal(err)
	}
}
