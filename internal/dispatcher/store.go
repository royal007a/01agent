package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/agentregistry"
	"github.com/royal007a/01agent/internal/computer"
	"github.com/royal007a/01agent/internal/workitem"
)

var (
	ErrConflict = errors.New("dispatcher conflict")
	ErrTerminal = errors.New("dispatcher execution is terminal")
	safeID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct {
	path      string
	tasks     *workitem.Store
	agents    *agentregistry.Store
	computers *computer.Store
	mu        sync.Mutex
	now       func() time.Time
}

func New(dir string, tasks *workitem.Store, agents *agentregistry.Store, computers *computer.Store) (*Store, error) {
	if tasks == nil || agents == nil || computers == nil {
		return nil, errors.New("task, agent, and computer stores are required")
	}
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(abs, "dispatches-v1.json"), tasks: tasks, agents: agents, computers: computers, now: func() time.Time { return time.Now().UTC() }}
	if _, err := os.Stat(store.path); errors.Is(err, os.ErrNotExist) {
		if err := store.writeUnlocked(State{SchemaVersion: SchemaVersion, Executions: map[string]Execution{}, UpdatedAt: store.now()}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := store.readUnlocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Start(ctx context.Context, input StartInput) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	input.OperationID, input.ID, input.TaskID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ID), strings.TrimSpace(input.TaskID)
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.ID) || len(input.ID) > 72 || !safeID.MatchString(input.TaskID) || input.MaxAttempts <= 0 || input.MaxAttempts > 10 || input.TimeoutSeconds <= 0 || input.TimeoutSeconds > 86280 {
		return Execution{}, errors.New("valid operation, execution, task, attempts, and timeout are required")
	}
	task, err := s.tasks.Get(ctx, input.TaskID)
	if err != nil {
		return Execution{}, err
	}
	if !safeID.MatchString(task.AssigneeID) || !safeID.MatchString(task.Gate.ReviewerID) || task.AssigneeID == task.Gate.ReviewerID {
		return Execution{}, errors.New("task requires distinct assigned executor and gate reviewer")
	}
	if task.State != workitem.Todo && task.State != workitem.InProgress {
		return Execution{}, errors.New("only todo or in-progress tasks can be dispatched")
	}
	if _, err := s.agents.Get(ctx, task.AssigneeID); err != nil {
		return Execution{}, fmt.Errorf("executor agent: %w", err)
	}
	if _, err := s.agents.Get(ctx, task.Gate.ReviewerID); err != nil {
		return Execution{}, fmt.Errorf("reviewer agent: %w", err)
	}
	fingerprint, _ := semanticFingerprint("start", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Execution{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Execution{}, ErrConflict
		}
		return state.Executions[operation.ExecutionID], nil
	}
	if _, found := state.Executions[input.ID]; found {
		return Execution{}, ErrConflict
	}
	now := s.now()
	execution := Execution{SchemaVersion: SchemaVersion, ID: input.ID, TaskID: task.ID, AgentID: task.AssigneeID, ReviewerID: task.Gate.ReviewerID, Phase: PendingClaim, MaxAttempts: input.MaxAttempts, TimeoutSeconds: input.TimeoutSeconds, TaskLeaseID: internalID("task-lease", input.ID), Revision: 1, CreatedAt: now, UpdatedAt: now}
	state.Executions[execution.ID] = execution
	appendOperation(&state, input.OperationID, fingerprint, execution.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Execution{}, err
	}
	return execution, nil
}

func (s *Store) Cancel(ctx context.Context, executionID string, input CancelInput) (Execution, error) {
	if err := ctx.Err(); err != nil {
		return Execution{}, err
	}
	executionID, input.OperationID, input.Reason = strings.TrimSpace(executionID), strings.TrimSpace(input.OperationID), strings.TrimSpace(input.Reason)
	if !safeID.MatchString(executionID) || !safeID.MatchString(input.OperationID) || input.Reason == "" {
		return Execution{}, errors.New("execution, operation, and reason are required")
	}
	fingerprint, _ := semanticFingerprint("cancel", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Execution{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint || operation.ExecutionID != executionID {
			return Execution{}, ErrConflict
		}
		return state.Executions[executionID], nil
	}
	execution, found := state.Executions[executionID]
	if !found {
		return Execution{}, os.ErrNotExist
	}
	if terminal(execution.Phase) {
		return Execution{}, ErrTerminal
	}
	execution.Phase, execution.CancelReason, execution.LastError = Canceling, input.Reason, ""
	if execution.ActiveRunID != "" {
		if _, err := s.computers.QueueCancel(ctx, internalID("queue-cancel", input.OperationID), execution.ActiveRunID); err != nil && !errors.Is(err, computer.ErrLease) {
			return Execution{}, err
		}
	}
	touch(&execution, s.now())
	state.Executions[execution.ID] = execution
	appendOperation(&state, input.OperationID, fingerprint, execution.ID, s.now())
	if err := s.writeUnlocked(state); err != nil {
		return Execution{}, err
	}
	return execution, nil
}

func (s *Store) List(ctx context.Context) ([]Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return nil, err
	}
	result := make([]Execution, 0, len(state.Executions))
	for _, execution := range state.Executions {
		result = append(result, execution)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *Store) Reconcile(ctx context.Context) (ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return ReconcileResult{}, err
	}
	ids := make([]string, 0, len(state.Executions))
	for id := range state.Executions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := ReconcileResult{}
	changed := false
	for _, id := range ids {
		execution := state.Executions[id]
		before := execution.Revision
		wait, advanceErr := s.advance(ctx, &execution)
		if advanceErr != nil && execution.LastError != advanceErr.Error() {
			execution.LastError = advanceErr.Error()
			touch(&execution, s.now())
		}
		if execution.Revision != before {
			state.Executions[id] = execution
			changed = true
		}
		if execution.Phase == Failed {
			result.Failed = append(result.Failed, id)
		} else if wait {
			result.Waiting = append(result.Waiting, id)
		} else if execution.Revision != before {
			result.Advanced = append(result.Advanced, id)
		}
	}
	if !changed {
		return result, nil
	}
	now := s.now()
	fingerprint, _ := semanticFingerprint("reconcile", struct {
		Executions []string  `json:"executions"`
		At         time.Time `json:"at"`
	}{ids, now})
	appendOperation(&state, internalID("reconcile", fmt.Sprint(state.Revision+1), now.Format(time.RFC3339Nano)), fingerprint, "scheduler", now)
	if err := s.writeUnlocked(state); err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func (s *Store) advance(ctx context.Context, execution *Execution) (bool, error) {
	switch execution.Phase {
	case PendingClaim:
		return false, s.dispatchExecute(ctx, execution)
	case Running:
		return s.observeExecute(ctx, execution)
	case AwaitingChildren:
		return s.submitResult(ctx, execution)
	case PendingReview:
		return false, s.dispatchReview(ctx, execution)
	case Reviewing:
		return s.observeReview(ctx, execution)
	case AwaitingHuman:
		task, err := s.tasks.Get(ctx, execution.TaskID)
		if err != nil {
			return true, err
		}
		switch task.State {
		case workitem.Done:
			execution.Phase, execution.LastError = Succeeded, ""
			touch(execution, s.now())
			return false, nil
		case workitem.InProgress:
			execution.Phase, execution.Result, execution.LastError = PendingClaim, nil, ""
			execution.ReworkCount++
			touch(execution, s.now())
			return false, nil
		default:
			return true, nil
		}
	case Canceling:
		return false, s.finishCancel(ctx, execution)
	case Succeeded, Failed, Canceled:
		return true, nil
	default:
		return false, errors.New("unknown dispatcher phase")
	}
}

func (s *Store) dispatchExecute(ctx context.Context, execution *Execution) error {
	if execution.Attempt >= execution.MaxAttempts {
		return s.failExecution(ctx, execution, "execution attempts exhausted")
	}
	task, err := s.ensureClaim(ctx, execution)
	if err != nil {
		return err
	}
	runID := fmt.Sprintf("%s-exec-%d", execution.ID, execution.Attempt+1)
	command, leaseID, err := s.queueRun(ctx, execution, task, execution.AgentID, runID, computer.RunExecute, executePrompt(task))
	if err != nil {
		return err
	}
	execution.Attempt++
	execution.ActiveRunID, execution.ActiveRunLease, execution.ActiveCommandID = runID, leaseID, command.ID
	execution.Phase, execution.LastError = Running, ""
	touch(execution, s.now())
	return nil
}

func (s *Store) observeExecute(ctx context.Context, execution *Execution) (bool, error) {
	command, err := s.command(ctx, execution.ActiveCommandID)
	if err != nil {
		return true, err
	}
	switch command.State {
	case computer.CommandQueued, computer.CommandSent:
		return true, nil
	case computer.CommandFailed:
		_ = s.releaseRun(ctx, execution)
		if execution.Attempt >= execution.MaxAttempts {
			return false, s.failExecution(ctx, execution, command.Error)
		}
		execution.Phase, execution.LastError = PendingClaim, command.Error
		clearActive(execution)
		touch(execution, s.now())
		return false, nil
	case computer.CommandAcked:
		if command.Result == nil {
			return false, errors.New("acknowledged run has no result")
		}
		result := *command.Result
		if result.Success && (result.ArtifactURI == "" || !hexDigest.MatchString(result.ArtifactHash)) {
			return false, errors.New("successful execution result lacks a durable artifact")
		}
		execution.Result = &result
		_ = s.releaseRun(ctx, execution)
		clearActive(execution)
		if !result.Success {
			if execution.Attempt >= execution.MaxAttempts {
				return false, s.failExecution(ctx, execution, result.Terminal)
			}
			execution.Phase, execution.LastError = PendingClaim, result.Terminal
			touch(execution, s.now())
			return false, nil
		}
		return s.submitResult(ctx, execution)
	default:
		return false, errors.New("invalid command state")
	}
}

func (s *Store) submitResult(ctx context.Context, execution *Execution) (bool, error) {
	if execution.Result == nil {
		return false, errors.New("execution result is missing")
	}
	task, err := s.ensureClaim(ctx, execution)
	if err != nil {
		return false, err
	}
	artifactID := internalID("result", execution.ID)
	artifact, err := s.tasks.GetArtifact(ctx, artifactID, execution.Result.RunID)
	if errors.Is(err, os.ErrNotExist) {
		task, artifact, err = s.tasks.AddArtifact(ctx, task.ID, workitem.ArtifactInput{
			OperationID: internalID("artifact", execution.Result.RunID), ExpectedRevision: task.Revision,
			OwnerID: execution.AgentID, LeaseID: execution.TaskLeaseID, ID: artifactID, Version: execution.Result.RunID,
			Kind: "agent_run", URI: execution.Result.ArtifactURI, Digest: execution.Result.ArtifactHash,
		})
	}
	if err != nil {
		return false, err
	}
	handoff := workitem.Handoff{
		ContractRevision: task.ContractRevision, AuthorID: execution.AgentID, Summary: execution.Result.Summary,
		Evidence: append([]string(nil), execution.Result.Evidence...), Artifacts: []workitem.ArtifactRef{{ID: artifact.ID, Version: artifact.Version, Digest: artifact.Digest}},
		Remaining: nil, Risks: nil, NextAction: "independent gate review",
	}
	task, err = s.tasks.Submit(ctx, task.ID, internalID("submit", execution.Result.RunID), execution.AgentID, execution.TaskLeaseID, task.Revision, handoff)
	if errors.Is(err, workitem.ErrTransition) {
		execution.Phase, execution.LastError = AwaitingChildren, err.Error()
		touch(execution, s.now())
		return true, nil
	}
	if err != nil {
		return false, err
	}
	execution.Phase, execution.LastError = PendingReview, ""
	touch(execution, s.now())
	return false, nil
}

func (s *Store) dispatchReview(ctx context.Context, execution *Execution) error {
	if execution.ReviewAttempt >= execution.MaxAttempts {
		execution.Phase, execution.LastError = AwaitingHuman, "review attempts exhausted"
		touch(execution, s.now())
		return nil
	}
	task, err := s.tasks.Get(ctx, execution.TaskID)
	if err != nil {
		return err
	}
	if task.State != workitem.InReview || len(task.Submissions) == 0 {
		return errors.New("task is not ready for gate review")
	}
	runID := fmt.Sprintf("%s-review-%d", execution.ID, execution.ReviewAttempt+1)
	command, leaseID, err := s.queueRun(ctx, execution, task, execution.ReviewerID, runID, computer.RunReview, reviewPrompt(task))
	if err != nil {
		return err
	}
	execution.ReviewAttempt++
	execution.ActiveRunID, execution.ActiveRunLease, execution.ActiveCommandID = runID, leaseID, command.ID
	execution.Phase, execution.LastError = Reviewing, ""
	touch(execution, s.now())
	return nil
}

func (s *Store) observeReview(ctx context.Context, execution *Execution) (bool, error) {
	command, err := s.command(ctx, execution.ActiveCommandID)
	if err != nil {
		return true, err
	}
	switch command.State {
	case computer.CommandQueued, computer.CommandSent:
		return true, nil
	case computer.CommandFailed:
		_ = s.releaseRun(ctx, execution)
		clearActive(execution)
		if execution.ReviewAttempt >= execution.MaxAttempts {
			execution.Phase, execution.LastError = AwaitingHuman, command.Error
		} else {
			execution.Phase, execution.LastError = PendingReview, command.Error
		}
		touch(execution, s.now())
		return false, nil
	case computer.CommandAcked:
		if command.Result == nil {
			return false, errors.New("acknowledged review has no result")
		}
		result := command.Result
		_ = s.releaseRun(ctx, execution)
		clearActive(execution)
		if !result.Success {
			if execution.ReviewAttempt >= execution.MaxAttempts {
				execution.Phase, execution.LastError = AwaitingHuman, result.Terminal
			} else {
				execution.Phase, execution.LastError = PendingReview, result.Terminal
			}
			touch(execution, s.now())
			return false, nil
		}
		task, err := s.tasks.Get(ctx, execution.TaskID)
		if err != nil {
			return false, err
		}
		decision := workitem.GateDecision(command.Result.Decision)
		if decision != workitem.GatePass && decision != workitem.GateReject && decision != workitem.GateNeedsHuman {
			decision = workitem.GateNeedsHuman
		}
		latest := task.Submissions[len(task.Submissions)-1]
		evidence := append([]string(nil), command.Result.Evidence...)
		if len(evidence) == 0 {
			evidence = []string{"review run " + command.Result.RunID}
		}
		reason := strings.TrimSpace(command.Result.Reason)
		if reason == "" {
			reason = command.Result.Summary
		}
		task, err = s.tasks.Review(ctx, task.ID, internalID("review", command.Result.RunID), task.Revision, workitem.GateResult{Decision: decision, ReviewerID: execution.ReviewerID, ArtifactVersion: latest.Artifacts, Evidence: evidence, Reason: reason})
		if err != nil {
			return false, err
		}
		switch task.State {
		case workitem.Done:
			execution.Phase = Succeeded
		case workitem.InProgress:
			execution.Phase, execution.Result = PendingClaim, nil
			execution.ReworkCount++
		case workitem.InReview:
			execution.Phase = AwaitingHuman
		}
		execution.LastError = ""
		touch(execution, s.now())
		return false, nil
	default:
		return false, errors.New("invalid review command state")
	}
}

func (s *Store) finishCancel(ctx context.Context, execution *Execution) error {
	if execution.ActiveRunID != "" {
		_ = s.releaseRun(ctx, execution)
		clearActive(execution)
	}
	task, err := s.tasks.Get(ctx, execution.TaskID)
	if err != nil {
		return err
	}
	if task.State != workitem.Done && task.State != workitem.Closed {
		if _, err := s.tasks.Close(ctx, task.ID, internalID("close-cancel", execution.ID), task.CreatorID, execution.CancelReason, task.Revision); err != nil && !errors.Is(err, workitem.ErrTransition) {
			return err
		}
	}
	execution.Phase, execution.LastError = Canceled, ""
	touch(execution, s.now())
	return nil
}

func (s *Store) failExecution(ctx context.Context, execution *Execution, reason string) error {
	_ = s.releaseRun(ctx, execution)
	task, err := s.tasks.Get(ctx, execution.TaskID)
	if err == nil && task.State != workitem.Done && task.State != workitem.Closed {
		_, _ = s.tasks.Close(ctx, task.ID, internalID("close-failed", execution.ID), task.CreatorID, reason, task.Revision)
	}
	execution.Phase, execution.LastError = Failed, strings.TrimSpace(reason)
	clearActive(execution)
	touch(execution, s.now())
	return nil
}

func (s *Store) ensureClaim(ctx context.Context, execution *Execution) (workitem.Task, error) {
	task, err := s.tasks.Get(ctx, execution.TaskID)
	if err != nil {
		return workitem.Task{}, err
	}
	now := s.now()
	ttl := time.Duration(execution.TimeoutSeconds+120) * time.Second
	if task.Claim != nil && task.Claim.OwnerID == execution.AgentID && task.Claim.LeaseID == execution.TaskLeaseID && now.Before(task.Claim.ExpiresAt) {
		if task.Claim.ExpiresAt.Sub(now) > time.Minute {
			return task, nil
		}
		return s.tasks.Renew(ctx, task.ID, internalID("renew", execution.ID, fmt.Sprint(task.Revision)), execution.AgentID, execution.TaskLeaseID, task.Revision, ttl)
	}
	return s.tasks.Claim(ctx, task.ID, internalID("claim", execution.ID, fmt.Sprint(task.Revision)), execution.AgentID, execution.TaskLeaseID, task.Revision, ttl)
}

func (s *Store) queueRun(ctx context.Context, execution *Execution, task workitem.Task, agentID, runID string, mode computer.RunMode, prompt string) (computer.Command, string, error) {
	agent, err := s.agents.Get(ctx, agentID)
	if err != nil {
		return computer.Command{}, "", err
	}
	state, err := s.computers.GetState(ctx)
	if err != nil {
		return computer.Command{}, "", err
	}
	binding, found := state.Bindings[agentID]
	if !found {
		return computer.Command{}, "", errors.New("agent has no computer binding")
	}
	node := state.Computers[binding.ComputerID]
	if node.Status != computer.Online || !s.now().Before(node.LeaseExpiresAt) {
		return computer.Command{}, "", computer.ErrOffline
	}
	leaseID := internalID("run-lease", runID)
	if _, err := s.computers.AcquireRun(ctx, computer.RunLeaseInput{OperationID: internalID("acquire", runID), RunID: runID, AgentID: agentID, ComputerID: binding.ComputerID, LeaseID: leaseID, TTLSeconds: execution.TimeoutSeconds + 120}); err != nil {
		return computer.Command{}, "", err
	}
	sessionID := ""
	for _, session := range agent.Sessions {
		if session.Generation == agent.CurrentSessionGeneration {
			sessionID = session.SessionID
			break
		}
	}
	command, err := s.computers.QueueRun(ctx, internalID("queue", runID), computer.RunRequest{
		RunID: runID, TaskID: task.ID, AgentID: agentID, Mode: mode, Prompt: prompt, SessionID: sessionID,
		AgentRevisionID: agent.CurrentAgentRevisionID, RelationshipRevisionID: agent.CurrentRelationshipRevision,
		ExpectedCapabilityDigest: node.CurrentCapability, TimeoutSeconds: execution.TimeoutSeconds, MaxTurns: 32,
		TaskRevision: task.Revision, TaskContractRevision: task.ContractRevision,
	})
	if err != nil {
		return computer.Command{}, "", err
	}
	return command, leaseID, nil
}

func (s *Store) releaseRun(ctx context.Context, execution *Execution) error {
	if execution.ActiveRunID == "" || execution.ActiveRunLease == "" {
		return nil
	}
	return s.computers.ReleaseRun(ctx, execution.ActiveRunID, internalID("release", execution.ActiveRunID), execution.ActiveRunLease)
}

func (s *Store) command(ctx context.Context, id string) (computer.Command, error) {
	state, err := s.computers.GetState(ctx)
	if err != nil {
		return computer.Command{}, err
	}
	command, found := state.Commands[id]
	if !found {
		return computer.Command{}, os.ErrNotExist
	}
	return command, nil
}

func executePrompt(task workitem.Task) string {
	encoded, _ := json.Marshal(struct {
		Objective      string                 `json:"objective"`
		Requirements   []workitem.Requirement `json:"requirements"`
		Scope          workitem.Scope         `json:"scope"`
		StopConditions []string               `json:"stop_conditions"`
	}{task.Objective, task.Requirements, task.Scope, task.StopConditions})
	return "Execute this Product Task. Respect the exact contract, produce verifiable evidence, and summarize completed work. Task contract: " + string(encoded)
}

func reviewPrompt(task workitem.Task) string {
	latest := task.Submissions[len(task.Submissions)-1]
	encoded, _ := json.Marshal(struct {
		Requirements []workitem.Requirement `json:"requirements"`
		Gate         workitem.GateSpec      `json:"gate"`
		Handoff      workitem.Handoff       `json:"handoff"`
	}{task.Requirements, task.Gate, latest})
	return "Independently review the exact submitted artifacts and evidence. Do not trust the author's summary. Return JSON only: {\"decision\":\"pass|reject|needs_human\",\"reason\":\"...\"}. Review packet: " + string(encoded)
}

func clearActive(execution *Execution) {
	execution.ActiveRunID, execution.ActiveRunLease, execution.ActiveCommandID = "", "", ""
}

func touch(execution *Execution, now time.Time) {
	execution.Revision++
	execution.UpdatedAt = now
}

func terminal(phase Phase) bool { return phase == Succeeded || phase == Failed || phase == Canceled }

func appendOperation(state *State, id, fingerprint, executionID string, now time.Time) {
	state.Revision++
	state.UpdatedAt = now
	state.Operations = append(state.Operations, Operation{ID: id, Fingerprint: fingerprint, ExecutionID: executionID, Revision: state.Revision, CreatedAt: now})
}

func findOperation(state State, id string) (Operation, bool) {
	for _, operation := range state.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return Operation{}, false
}

func semanticFingerprint(action string, value any) (string, error) {
	encoded, err := json.Marshal(struct {
		Action string `json:"action"`
		Value  any    `json:"value"`
	}{action, value})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func internalID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}

func (s *Store) readUnlocked() (State, error) {
	file, err := os.Open(s.path)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return State{}, errors.New("dispatcher state has trailing data")
	}
	if err := validateState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Store) writeUnlocked(state State) error {
	if err := validateState(state); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".dispatches-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := errors.Join(encoder.Encode(state), temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	verified, err := s.readUnlocked()
	if err != nil {
		return err
	}
	if verified.Revision != state.Revision {
		return errors.New("dispatcher state read-back verification failed")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != SchemaVersion || state.Revision < 0 || state.Executions == nil {
		return errors.New("invalid canonical dispatcher state")
	}
	for id, execution := range state.Executions {
		if id != execution.ID || execution.SchemaVersion != SchemaVersion || !safeID.MatchString(id) || !safeID.MatchString(execution.TaskID) || !safeID.MatchString(execution.AgentID) || !safeID.MatchString(execution.ReviewerID) || execution.AgentID == execution.ReviewerID || execution.MaxAttempts <= 0 || execution.TimeoutSeconds <= 0 || execution.Revision <= 0 || execution.CreatedAt.IsZero() || execution.UpdatedAt.IsZero() {
			return errors.New("invalid canonical dispatcher execution")
		}
		switch execution.Phase {
		case PendingClaim, Running, AwaitingChildren, PendingReview, Reviewing, AwaitingHuman, Canceling, Succeeded, Failed, Canceled:
		default:
			return errors.New("invalid dispatcher phase")
		}
		if execution.Result != nil && (!safeID.MatchString(execution.Result.RunID) || !safeID.MatchString(execution.Result.TaskID) || execution.Result.TaskID != execution.TaskID) {
			return errors.New("invalid dispatcher result")
		}
	}
	seen := map[string]bool{}
	for index, operation := range state.Operations {
		if operation.Revision != int64(index+1) || !safeID.MatchString(operation.ID) || seen[operation.ID] || !hexDigest.MatchString(operation.Fingerprint) || operation.CreatedAt.IsZero() {
			return errors.New("invalid dispatcher operation ledger")
		}
		seen[operation.ID] = true
	}
	if int64(len(state.Operations)) != state.Revision {
		return errors.New("dispatcher operation ledger does not reach revision")
	}
	return nil
}
