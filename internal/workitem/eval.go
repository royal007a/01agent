package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type EvaluationSuite struct {
	Name            string   `json:"name"`
	MinimumPassRate float64  `json:"minimum_pass_rate"`
	Cases           []string `json:"cases"`
}

type EvaluationResult struct {
	ID        string   `json:"id"`
	Passed    bool     `json:"passed"`
	Failures  []string `json:"failures,omitempty"`
	LatencyMS int64    `json:"latency_ms"`
}

type EvaluationReport struct {
	Suite       string             `json:"suite"`
	StartedAt   time.Time          `json:"started_at"`
	CompletedAt time.Time          `json:"completed_at"`
	Cases       int                `json:"cases"`
	Passed      int                `json:"passed"`
	PassRate    float64            `json:"pass_rate"`
	Gate        float64            `json:"gate"`
	GatePassed  bool               `json:"gate_passed"`
	Results     []EvaluationResult `json:"results"`
}

func LoadEvaluationSuite(path string) (EvaluationSuite, error) {
	file, err := os.Open(path)
	if err != nil {
		return EvaluationSuite{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var suite EvaluationSuite
	if err := decoder.Decode(&suite); err != nil {
		return EvaluationSuite{}, err
	}
	if suite.Name == "" || len(suite.Cases) < 12 || len(suite.Cases) > 15 {
		return EvaluationSuite{}, errors.New("control-plane evaluation suite must have a name and 12-15 cases")
	}
	if suite.MinimumPassRate <= 0 || suite.MinimumPassRate > 1 {
		return EvaluationSuite{}, errors.New("minimum_pass_rate must be in (0,1]")
	}
	seen := make(map[string]bool, len(suite.Cases))
	for _, id := range suite.Cases {
		if evaluationCases[id] == nil || seen[id] {
			return EvaluationSuite{}, fmt.Errorf("unknown or duplicate control-plane case %q", id)
		}
		seen[id] = true
	}
	return suite, nil
}

func RunEvaluation(ctx context.Context, suite EvaluationSuite) (EvaluationReport, error) {
	report := EvaluationReport{Suite: suite.Name, StartedAt: time.Now().UTC(), Cases: len(suite.Cases), Gate: suite.MinimumPassRate}
	for _, id := range suite.Cases {
		if err := ctx.Err(); err != nil {
			return EvaluationReport{}, err
		}
		started := time.Now()
		err := evaluationCases[id](ctx)
		result := EvaluationResult{ID: id, Passed: err == nil, LatencyMS: time.Since(started).Milliseconds()}
		if err != nil {
			result.Failures = []string{err.Error()}
		} else {
			report.Passed++
		}
		report.Results = append(report.Results, result)
	}
	report.PassRate = float64(report.Passed) / float64(report.Cases)
	report.GatePassed = report.PassRate >= report.Gate
	report.CompletedAt = time.Now().UTC()
	return report, nil
}

var evaluationCases = map[string]func(context.Context) error{
	"create-contract-validation":  evalCreateContractValidation,
	"create-idempotency":          evalCreateIdempotency,
	"operation-semantic-conflict": evalOperationSemanticConflict,
	"revision-cas":                evalRevisionCAS,
	"exclusive-claim":             evalExclusiveClaim,
	"expired-lease-reclaim":       evalExpiredLeaseReclaim,
	"lease-renewal":               evalLeaseRenewal,
	"contract-revision":           evalContractRevision,
	"artifact-immutability":       evalArtifactImmutability,
	"handoff-artifact-binding":    evalHandoffArtifactBinding,
	"parent-child-barrier":        evalParentChildBarrier,
	"closed-child-release":        evalClosedChildRelease,
	"reviewer-authorization":      evalReviewerAuthorization,
	"rejection-rework":            evalRejectionRework,
	"needs-human-then-pass":       evalNeedsHumanThenPass,
}

func withEvalStore(ctx context.Context, run func(*Store) error) error {
	dir, err := os.MkdirTemp("", "01agent-control-eval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store, err := New(dir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(store)
}

func evalCreateContractValidation(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		input := evalCreate("task-create")
		input.Requirements = nil
		_, err := store.Create(ctx, input)
		return expectError(err, "requirements")
	})
}

func evalCreateIdempotency(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		input := evalCreate("task-idempotent")
		first, err := store.Create(ctx, input)
		if err != nil {
			return err
		}
		second, err := store.Create(ctx, input)
		if err != nil {
			return err
		}
		return expect(first.Revision == second.Revision && len(second.Operations) == 1, "create retry changed canonical state")
	})
}

func evalOperationSemanticConflict(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, err := store.Create(ctx, evalCreate("task-op-conflict"))
		if err != nil {
			return err
		}
		claimed, err := store.Claim(ctx, task.ID, "op-shared", "worker", "lease-a", task.Revision, time.Minute)
		if err != nil {
			return err
		}
		_, err = store.Renew(ctx, task.ID, "op-shared", "worker", "lease-a", claimed.Revision, time.Minute)
		return expectIs(err, ErrConflict)
	})
}

func evalRevisionCAS(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, err := store.Create(ctx, evalCreate("task-cas"))
		if err != nil {
			return err
		}
		_, err = store.Claim(ctx, task.ID, "op-claim", "worker", "lease-a", task.Revision+1, time.Minute)
		return expectIs(err, ErrConflict)
	})
}

func evalExclusiveClaim(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, err := store.Create(ctx, evalCreate("task-exclusive"))
		if err != nil {
			return err
		}
		claimed, err := store.Claim(ctx, task.ID, "op-claim-a", "worker", "lease-a", task.Revision, time.Minute)
		if err != nil {
			return err
		}
		_, err = store.Claim(ctx, task.ID, "op-claim-b", "worker", "lease-b", claimed.Revision, time.Minute)
		return expectIs(err, ErrLease)
	})
}

func evalExpiredLeaseReclaim(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		store.now = func() time.Time { return now }
		task, err := store.Create(ctx, evalCreate("task-reclaim"))
		if err != nil {
			return err
		}
		claimed, err := store.Claim(ctx, task.ID, "op-claim-a", "worker", "lease-a", task.Revision, time.Second)
		if err != nil {
			return err
		}
		now = now.Add(2 * time.Second)
		reclaimed, err := store.Claim(ctx, task.ID, "op-claim-b", "worker", "lease-b", claimed.Revision, time.Minute)
		if err != nil {
			return err
		}
		return expect(reclaimed.Claim != nil && reclaimed.Claim.LeaseID == "lease-b", "expired lease was not replaced")
	})
}

func evalLeaseRenewal(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		store.now = func() time.Time { return now }
		task, err := store.Create(ctx, evalCreate("task-renew"))
		if err != nil {
			return err
		}
		claimed, err := store.Claim(ctx, task.ID, "op-claim", "worker", "lease-a", task.Revision, time.Minute)
		if err != nil {
			return err
		}
		now = now.Add(30 * time.Second)
		renewed, err := store.Renew(ctx, task.ID, "op-renew", "worker", "lease-a", claimed.Revision, time.Minute)
		if err != nil {
			return err
		}
		return expect(renewed.Claim.ExpiresAt.Equal(now.Add(time.Minute)), "renewal did not advance expiration")
	})
}

func evalContractRevision(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, err := store.Create(ctx, evalCreate("task-contract"))
		if err != nil {
			return err
		}
		updated, err := store.ReviseContract(ctx, task.ID, ContractUpdate{
			OperationID: "op-revise", ExpectedRevision: task.Revision, ActorID: "creator",
			Requirements: []Requirement{{ID: "R2", Text: "updated"}}, Scope: Scope{Allow: []string{"internal/"}},
			StopConditions: []string{"verified"}, Gate: evalGate(),
		})
		if err != nil {
			return err
		}
		return expect(updated.ContractRevision == 2, "contract revision did not advance")
	})
}

func evalArtifactImmutability(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, claimed, err := evalClaim(ctx, store, "task-artifact")
		if err != nil {
			return err
		}
		input := evalArtifact(claimed.Revision, "op-artifact")
		updated, _, err := store.AddArtifact(ctx, task.ID, input)
		if err != nil {
			return err
		}
		input.OperationID = "op-artifact-mutated"
		input.ExpectedRevision = updated.Revision
		input.URI = "commit://different"
		input.Digest = strings.Repeat("b", 64)
		_, _, err = store.AddArtifact(ctx, task.ID, input)
		return expectError(err, "immutable")
	})
}

func evalHandoffArtifactBinding(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		task, claimed, err := evalClaim(ctx, store, "task-handoff")
		if err != nil {
			return err
		}
		_, err = store.Submit(ctx, task.ID, "op-submit", "worker", "lease-a", claimed.Revision, Handoff{
			ContractRevision: 1, AuthorID: "worker", Summary: "done", Evidence: []string{"tests"}, NextAction: "review",
		})
		return expectError(err, "artifact")
	})
}

func evalParentChildBarrier(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		parent, _, ref, err := evalDeliverable(ctx, store, "task-parent")
		if err != nil {
			return err
		}
		childInput := evalCreate("task-child")
		childInput.ParentTaskID = parent.ID
		if _, err := store.Create(ctx, childInput); err != nil {
			return err
		}
		_, err = store.Submit(ctx, parent.ID, "op-submit", "worker", "lease-a", parent.Revision, evalHandoff(ref))
		return expectIs(err, ErrTransition)
	})
}

func evalClosedChildRelease(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		parent, _, ref, err := evalDeliverable(ctx, store, "task-parent-closed")
		if err != nil {
			return err
		}
		childInput := evalCreate("task-child-closed")
		childInput.ParentTaskID = parent.ID
		child, err := store.Create(ctx, childInput)
		if err != nil {
			return err
		}
		if _, err = store.Close(ctx, child.ID, "op-close", "creator", "canceled scope", child.Revision); err != nil {
			return err
		}
		submitted, err := store.Submit(ctx, parent.ID, "op-submit", "worker", "lease-a", parent.Revision, evalHandoff(ref))
		if err != nil {
			return err
		}
		return expect(submitted.State == InReview, "closed child still blocked parent")
	})
}

func evalReviewerAuthorization(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		submitted, ref, err := evalSubmitted(ctx, store, "task-reviewer")
		if err != nil {
			return err
		}
		_, err = store.Review(ctx, submitted.ID, "op-review", submitted.Revision, evalReview("intruder", GatePass, ref))
		return expectError(err, "wrong reviewer")
	})
}

func evalRejectionRework(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		submitted, ref, err := evalSubmitted(ctx, store, "task-reject")
		if err != nil {
			return err
		}
		rejected, err := store.Review(ctx, submitted.ID, "op-reject", submitted.Revision, evalReview("reviewer", GateReject, ref))
		if err != nil {
			return err
		}
		return expect(rejected.State == InProgress && rejected.Claim == nil && len(rejected.Reviews) == 1, "reject did not reopen task with audit history")
	})
}

func evalNeedsHumanThenPass(ctx context.Context) error {
	return withEvalStore(ctx, func(store *Store) error {
		submitted, ref, err := evalSubmitted(ctx, store, "task-human")
		if err != nil {
			return err
		}
		pending, err := store.Review(ctx, submitted.ID, "op-human", submitted.Revision, evalReview("reviewer", GateNeedsHuman, ref))
		if err != nil {
			return err
		}
		if pending.State != InReview {
			return errors.New("needs_human did not preserve in_review")
		}
		done, err := store.Review(ctx, pending.ID, "op-pass", pending.Revision, evalReview("reviewer", GatePass, ref))
		if err != nil {
			return err
		}
		return expect(done.State == Done && len(done.Reviews) == 2, "human resolution did not complete exact artifact version")
	})
}

func evalCreate(id string) Create {
	return Create{
		OperationID: "op-create-" + id, ID: id, WorkspaceID: "workspace", ChannelID: "channel", CreatorID: "creator",
		Title: "Deliver change", Objective: "Produce a verifiable change", Requirements: []Requirement{{ID: "R1", Text: "preserve behavior"}},
		Scope: Scope{Allow: []string{"internal/"}}, StopConditions: []string{"tests pass"}, Gate: evalGate(),
	}
}

func evalGate() GateSpec {
	return GateSpec{Kind: GateAgent, ReviewerID: "reviewer", Checks: []string{"R1"}, RequiredEvidence: []string{"test log"}, OnReject: "return to worker"}
}

func evalArtifact(revision int64, operationID string) ArtifactInput {
	return ArtifactInput{OperationID: operationID, ExpectedRevision: revision, OwnerID: "worker", LeaseID: "lease-a", ID: "artifact", Version: "v1", Kind: "commit", URI: "commit://abc", Digest: strings.Repeat("a", 64)}
}

func evalClaim(ctx context.Context, store *Store, id string) (Task, Task, error) {
	task, err := store.Create(ctx, evalCreate(id))
	if err != nil {
		return Task{}, Task{}, err
	}
	claimed, err := store.Claim(ctx, task.ID, "op-claim", "worker", "lease-a", task.Revision, time.Minute)
	return task, claimed, err
}

func evalDeliverable(ctx context.Context, store *Store, id string) (Task, Artifact, ArtifactRef, error) {
	task, claimed, err := evalClaim(ctx, store, id)
	if err != nil {
		return Task{}, Artifact{}, ArtifactRef{}, err
	}
	updated, artifact, err := store.AddArtifact(ctx, task.ID, evalArtifact(claimed.Revision, "op-artifact"))
	if err != nil {
		return Task{}, Artifact{}, ArtifactRef{}, err
	}
	return updated, artifact, ArtifactRef{ID: artifact.ID, Version: artifact.Version, Digest: artifact.Digest}, nil
}

func evalSubmitted(ctx context.Context, store *Store, id string) (Task, ArtifactRef, error) {
	deliverable, _, ref, err := evalDeliverable(ctx, store, id)
	if err != nil {
		return Task{}, ArtifactRef{}, err
	}
	submitted, err := store.Submit(ctx, deliverable.ID, "op-submit", "worker", "lease-a", deliverable.Revision, evalHandoff(ref))
	return submitted, ref, err
}

func evalHandoff(ref ArtifactRef) Handoff {
	return Handoff{ContractRevision: 1, AuthorID: "worker", Summary: "implemented", Evidence: []string{"tests pass"}, Artifacts: []ArtifactRef{ref}, NextAction: "review"}
}

func evalReview(reviewer string, decision GateDecision, ref ArtifactRef) GateResult {
	return GateResult{Decision: decision, ReviewerID: reviewer, ArtifactVersion: []ArtifactRef{ref}, Evidence: []string{"review log"}, Reason: "evaluated"}
}

func expect(condition bool, message string) error {
	if !condition {
		return errors.New(message)
	}
	return nil
}

func expectIs(err, target error) error {
	if !errors.Is(err, target) {
		return fmt.Errorf("expected %v, got %v", target, err)
	}
	return nil
}

func expectError(err error, contains string) error {
	if err == nil || !strings.Contains(err.Error(), contains) {
		return fmt.Errorf("expected error containing %q, got %v", contains, err)
	}
	return nil
}
