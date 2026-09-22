package workitem

import (
	"context"
	"crypto/rand"
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
)

var (
	ErrConflict   = errors.New("work item revision conflict")
	ErrTransition = errors.New("invalid work item transition")
	ErrLease      = errors.New("invalid or expired work item lease")
	safeID        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeDigest    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct {
	dir string
	mu  sync.Mutex
	now func() time.Time
}

func New(dir string) (*Store, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "tasks"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "artifacts"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: abs, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Create(ctx context.Context, input Create) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if input.ID == "" {
		input.ID = newID("work")
	}
	input = normalizeCreate(input)
	if err := validateCreate(input); err != nil {
		return Task{}, err
	}
	fingerprint, err := semanticFingerprint("create", input)
	if err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readTaskUnlocked(input.ID)
	if err == nil {
		if operationMatch(existing.Operations, input.OperationID, fingerprint) {
			return existing, nil
		}
		return Task{}, fmt.Errorf("task %q already exists: %w", input.ID, ErrConflict)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Task{}, err
	}
	if input.ParentTaskID != "" {
		parent, err := s.readTaskUnlocked(input.ParentTaskID)
		if err != nil {
			return Task{}, fmt.Errorf("parent task: %w", err)
		}
		if parent.WorkspaceID != input.WorkspaceID || parent.ChannelID != input.ChannelID || terminal(parent.State) {
			return Task{}, errors.New("parent task must be open in the same workspace and channel")
		}
	}
	now := s.now()
	task := Task{
		SchemaVersion: SchemaVersion, ID: input.ID, WorkspaceID: input.WorkspaceID, ChannelID: input.ChannelID,
		ThreadID: input.ThreadID, ParentTaskID: input.ParentTaskID, CreatorID: input.CreatorID,
		Title: input.Title, Objective: input.Objective, State: Todo, Revision: 1, ContractRevision: 1,
		Requirements: cloneRequirements(input.Requirements), Scope: cloneScope(input.Scope),
		StopConditions: append([]string(nil), input.StopConditions...), AssigneeID: input.AssigneeID, Gate: cloneGate(input.Gate),
		CreatedAt: now, UpdatedAt: now,
	}
	task.Operations = []Operation{{ID: input.OperationID, Action: "create", Fingerprint: fingerprint, Revision: 1, CreatedAt: now}}
	if err := s.writeTaskUnlocked(task); err != nil {
		return Task{}, err
	}
	return cloneTask(task), nil
}

func (s *Store) Get(ctx context.Context, id string) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if !safeID.MatchString(id) {
		return Task{}, errors.New("invalid task id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.readTaskUnlocked(id)
	return cloneTask(task), err
}

func (s *Store) GetArtifact(ctx context.Context, id, version string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readArtifactUnlocked(id, version)
}

func (s *Store) List(ctx context.Context, workspaceID, channelID string) ([]Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.listTasksUnlocked()
	if err != nil {
		return nil, err
	}
	result := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if workspaceID != "" && task.WorkspaceID != workspaceID {
			continue
		}
		if channelID != "" && task.ChannelID != channelID {
			continue
		}
		result = append(result, cloneTask(task))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *Store) Claim(ctx context.Context, taskID, operationID, ownerID, leaseID string, expectedRevision int64, ttl time.Duration) (Task, error) {
	if ttl <= 0 || ttl > 24*time.Hour {
		return Task{}, errors.New("claim ttl must be in (0,24h]")
	}
	if leaseID == "" {
		leaseID = newID("lease")
	}
	semantic := struct {
		Owner string `json:"owner"`
		Lease string `json:"lease"`
		TTL   int64  `json:"ttl_ms"`
	}{ownerID, leaseID, ttl.Milliseconds()}
	return s.mutate(ctx, taskID, operationID, "claim", expectedRevision, semantic, func(task *Task, now time.Time) error {
		if !safeID.MatchString(ownerID) || !safeID.MatchString(leaseID) {
			return errors.New("owner and lease ids are required")
		}
		if task.State != Todo && task.State != InProgress {
			return transition(task.State, InProgress)
		}
		if task.AssigneeID != "" && task.AssigneeID != ownerID {
			return errors.New("task is assigned to another owner")
		}
		if task.Claim != nil && now.Before(task.Claim.ExpiresAt) {
			return ErrLease
		}
		task.State = InProgress
		task.AssigneeID = ownerID
		task.Claim = &Claim{OwnerID: ownerID, LeaseID: leaseID, ExpiresAt: now.Add(ttl)}
		return nil
	})
}

func (s *Store) Renew(ctx context.Context, taskID, operationID, ownerID, leaseID string, expectedRevision int64, ttl time.Duration) (Task, error) {
	if ttl <= 0 || ttl > 24*time.Hour {
		return Task{}, errors.New("renew ttl must be in (0,24h]")
	}
	semantic := struct {
		Owner string `json:"owner"`
		Lease string `json:"lease"`
		TTL   int64  `json:"ttl_ms"`
	}{ownerID, leaseID, ttl.Milliseconds()}
	return s.mutate(ctx, taskID, operationID, "renew", expectedRevision, semantic, func(task *Task, now time.Time) error {
		if task.State != InProgress || !validClaim(task, ownerID, leaseID, now) {
			return ErrLease
		}
		task.Claim.ExpiresAt = now.Add(ttl)
		return nil
	})
}

func (s *Store) ReviseContract(ctx context.Context, taskID string, input ContractUpdate) (Task, error) {
	input.Requirements = normalizeRequirements(input.Requirements)
	input.Scope = normalizeScope(input.Scope)
	input.StopConditions = normalizeStrings(input.StopConditions)
	input.Gate = normalizeGate(input.Gate)
	semantic := struct {
		Actor          string        `json:"actor"`
		Requirements   []Requirement `json:"requirements"`
		Scope          Scope         `json:"scope"`
		StopConditions []string      `json:"stop_conditions"`
		Gate           GateSpec      `json:"gate"`
	}{input.ActorID, input.Requirements, input.Scope, input.StopConditions, input.Gate}
	return s.mutate(ctx, taskID, input.OperationID, "revise_contract", input.ExpectedRevision, semantic, func(task *Task, _ time.Time) error {
		if task.State != Todo && task.State != InProgress {
			return fmt.Errorf("contract cannot change in state %s: %w", task.State, ErrTransition)
		}
		if input.ActorID != task.CreatorID {
			return errors.New("only the task creator may revise its contract")
		}
		if err := validateContract(input.Requirements, input.Scope, input.StopConditions, input.Gate); err != nil {
			return err
		}
		task.Requirements = cloneRequirements(input.Requirements)
		task.Scope = cloneScope(input.Scope)
		task.StopConditions = append([]string(nil), input.StopConditions...)
		task.Gate = cloneGate(input.Gate)
		task.ContractRevision++
		return nil
	})
}

func (s *Store) AddArtifact(ctx context.Context, taskID string, input ArtifactInput) (Task, Artifact, error) {
	input.ID = strings.TrimSpace(input.ID)
	if input.ID == "" {
		input.ID = newID("artifact")
	}
	input.Version = strings.TrimSpace(input.Version)
	input.Kind = strings.TrimSpace(input.Kind)
	input.URI = strings.TrimSpace(input.URI)
	input.Digest = strings.ToLower(strings.TrimSpace(input.Digest))
	semantic := struct {
		Owner   string `json:"owner"`
		Lease   string `json:"lease"`
		ID      string `json:"id"`
		Version string `json:"version"`
		Kind    string `json:"kind"`
		URI     string `json:"uri"`
		Digest  string `json:"digest"`
	}{input.OwnerID, input.LeaseID, input.ID, input.Version, input.Kind, input.URI, input.Digest}
	if !safeID.MatchString(input.ID) || input.Version == "" || input.Kind == "" || input.URI == "" || !safeDigest.MatchString(input.Digest) {
		return Task{}, Artifact{}, errors.New("artifact id, version, kind, uri, and sha256 digest are required")
	}
	fingerprint, err := semanticFingerprint("add_artifact", semantic)
	if err != nil {
		return Task{}, Artifact{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.readTaskUnlocked(taskID)
	if err != nil {
		return Task{}, Artifact{}, err
	}
	if operationMatch(task.Operations, input.OperationID, fingerprint) {
		artifact, err := s.readArtifactUnlocked(input.ID, input.Version)
		return cloneTask(task), artifact, err
	}
	if task.Revision != input.ExpectedRevision {
		return Task{}, Artifact{}, ErrConflict
	}
	now := s.now()
	if task.State != InProgress || !validClaim(&task, input.OwnerID, input.LeaseID, now) {
		return Task{}, Artifact{}, ErrLease
	}
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ID: input.ID, TaskID: task.ID, Version: input.Version,
		Kind: input.Kind, URI: input.URI, Digest: input.Digest, ProducerID: input.OwnerID, CreatedAt: now,
	}
	if existing, err := s.readArtifactUnlocked(input.ID, input.Version); err == nil {
		if existing != artifact {
			return Task{}, Artifact{}, errors.New("artifact version is immutable")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := s.writeArtifactUnlocked(artifact); err != nil {
			return Task{}, Artifact{}, err
		}
	} else {
		return Task{}, Artifact{}, err
	}
	ref := ArtifactRef{ID: artifact.ID, Version: artifact.Version, Digest: artifact.Digest}
	if !containsArtifact(task.ArtifactRefs, ref) {
		task.ArtifactRefs = append(task.ArtifactRefs, ref)
	}
	task.Revision++
	task.UpdatedAt = now
	task.Operations = append(task.Operations, Operation{ID: input.OperationID, Action: "add_artifact", Fingerprint: fingerprint, Revision: task.Revision, CreatedAt: now})
	if err := s.writeTaskUnlocked(task); err != nil {
		return Task{}, Artifact{}, err
	}
	return cloneTask(task), artifact, nil
}

func (s *Store) Submit(ctx context.Context, taskID, operationID, ownerID, leaseID string, expectedRevision int64, handoff Handoff) (Task, error) {
	handoff = normalizeHandoff(handoff)
	semantic := struct {
		Owner   string  `json:"owner"`
		Lease   string  `json:"lease"`
		Handoff Handoff `json:"handoff"`
	}{ownerID, leaseID, handoff}
	return s.mutate(ctx, taskID, operationID, "submit", expectedRevision, semantic, func(task *Task, now time.Time) error {
		if task.State != InProgress {
			return transition(task.State, InReview)
		}
		if !validClaim(task, ownerID, leaseID, now) {
			return ErrLease
		}
		if handoff.ContractRevision != task.ContractRevision || handoff.AuthorID != ownerID {
			return errors.New("handoff must bind the current contract revision and active owner")
		}
		if err := validateHandoff(task, handoff); err != nil {
			return err
		}
		children, err := s.childrenUnlocked(task.ID)
		if err != nil {
			return err
		}
		for _, child := range children {
			if child.State != Done && child.State != Closed {
				return fmt.Errorf("child task %s is %s: %w", child.ID, child.State, ErrTransition)
			}
		}
		handoff.CreatedAt = now
		task.Submissions = append(task.Submissions, handoff)
		task.State = InReview
		task.Claim = nil
		return nil
	})
}

func (s *Store) Review(ctx context.Context, taskID, operationID string, expectedRevision int64, result GateResult) (Task, error) {
	result = normalizeGateResult(result)
	return s.mutate(ctx, taskID, operationID, "review", expectedRevision, result, func(task *Task, now time.Time) error {
		if task.State != InReview || len(task.Submissions) == 0 {
			return transition(task.State, Done)
		}
		if result.ReviewerID != task.Gate.ReviewerID {
			return errors.New("gate result came from the wrong reviewer")
		}
		if result.Decision != GatePass && result.Decision != GateReject && result.Decision != GateNeedsHuman {
			return errors.New("invalid gate decision")
		}
		latest := task.Submissions[len(task.Submissions)-1]
		if !sameArtifactSet(latest.Artifacts, result.ArtifactVersion) {
			return errors.New("gate result artifact versions do not match the submitted handoff")
		}
		if len(result.Evidence) == 0 || result.Reason == "" {
			return errors.New("gate result requires evidence and reason")
		}
		result.CreatedAt = now
		task.Reviews = append(task.Reviews, result)
		switch result.Decision {
		case GatePass:
			task.State = Done
		case GateReject:
			task.State = InProgress
			task.Claim = nil
		case GateNeedsHuman:
			task.State = InReview
		}
		return nil
	})
}

func (s *Store) Close(ctx context.Context, taskID, operationID, actorID, reason string, expectedRevision int64) (Task, error) {
	reason = strings.TrimSpace(reason)
	semantic := struct {
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}{actorID, reason}
	return s.mutate(ctx, taskID, operationID, "close", expectedRevision, semantic, func(task *Task, _ time.Time) error {
		if task.State == Done || task.State == Closed {
			return transition(task.State, Closed)
		}
		if actorID != task.CreatorID || reason == "" {
			return errors.New("only the creator may close an open task with a reason")
		}
		task.State = Closed
		task.ClosedReason = reason
		task.Claim = nil
		return nil
	})
}

func (s *Store) mutate(ctx context.Context, taskID, operationID, action string, expectedRevision int64, semantic any, update func(*Task, time.Time) error) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if !safeID.MatchString(taskID) || !safeID.MatchString(operationID) || expectedRevision <= 0 {
		return Task{}, errors.New("task id, operation id, and positive expected revision are required")
	}
	fingerprint, err := semanticFingerprint(action, semantic)
	if err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.readTaskUnlocked(taskID)
	if err != nil {
		return Task{}, err
	}
	if operationMatch(task.Operations, operationID, fingerprint) {
		return cloneTask(task), nil
	}
	for _, operation := range task.Operations {
		if operation.ID == operationID {
			return Task{}, fmt.Errorf("operation id reused with different semantics: %w", ErrConflict)
		}
	}
	if task.Revision != expectedRevision {
		return Task{}, ErrConflict
	}
	now := s.now()
	if err := update(&task, now); err != nil {
		return Task{}, err
	}
	task.Revision++
	task.UpdatedAt = now
	task.Operations = append(task.Operations, Operation{ID: operationID, Action: action, Fingerprint: fingerprint, Revision: task.Revision, CreatedAt: now})
	if err := validateTask(task); err != nil {
		return Task{}, err
	}
	if err := s.writeTaskUnlocked(task); err != nil {
		return Task{}, err
	}
	return cloneTask(task), nil
}

func (s *Store) childrenUnlocked(parentID string) ([]Task, error) {
	items, err := s.listTasksUnlocked()
	if err != nil {
		return nil, err
	}
	var result []Task
	for _, item := range items {
		if item.ParentTaskID == parentID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *Store) listTasksUnlocked() ([]Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tasks"))
	if err != nil {
		return nil, err
	}
	items := make([]Task, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".task-v2.json") {
			continue
		}
		item, err := s.readTaskUnlocked(strings.TrimSuffix(entry.Name(), ".task-v2.json"))
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) readTaskUnlocked(id string) (Task, error) {
	if !safeID.MatchString(id) {
		return Task{}, errors.New("invalid task id")
	}
	var task Task
	if err := readStrictJSON(s.taskPath(id), &task); err != nil {
		return Task{}, err
	}
	if err := validateTask(task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) readArtifactUnlocked(id, version string) (Artifact, error) {
	if !safeID.MatchString(id) || !safeID.MatchString(version) {
		return Artifact{}, errors.New("invalid artifact identity")
	}
	var artifact Artifact
	if err := readStrictJSON(s.artifactPath(id, version), &artifact); err != nil {
		return Artifact{}, err
	}
	if artifact.SchemaVersion != SchemaVersion || artifact.ID != id || artifact.Version != version || !safeDigest.MatchString(artifact.Digest) {
		return Artifact{}, errors.New("invalid artifact record")
	}
	return artifact, nil
}

func (s *Store) writeTaskUnlocked(task Task) error {
	if err := writeAtomic(s.taskPath(task.ID), task); err != nil {
		return err
	}
	verified, err := s.readTaskUnlocked(task.ID)
	if err != nil {
		return err
	}
	if verified.Revision != task.Revision || len(verified.Operations) != len(task.Operations) || verified.State != task.State {
		return errors.New("task read-back verification failed")
	}
	return nil
}

func (s *Store) writeArtifactUnlocked(artifact Artifact) error {
	if err := writeAtomic(s.artifactPath(artifact.ID, artifact.Version), artifact); err != nil {
		return err
	}
	verified, err := s.readArtifactUnlocked(artifact.ID, artifact.Version)
	if err != nil {
		return err
	}
	if verified.Digest != artifact.Digest || verified.TaskID != artifact.TaskID {
		return errors.New("artifact read-back verification failed")
	}
	return nil
}

func (s *Store) taskPath(id string) string {
	return filepath.Join(s.dir, "tasks", id+".task-v2.json")
}

func (s *Store) artifactPath(id, version string) string {
	return filepath.Join(s.dir, "artifacts", id+"--"+version+".artifact.json")
}

func writeAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".control-*.tmp")
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
	encodeErr := encoder.Encode(value)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func readStrictJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("record contains trailing data")
	}
	return nil
}

func validateCreate(input Create) error {
	for _, value := range []string{input.OperationID, input.ID, input.WorkspaceID, input.ChannelID, input.CreatorID} {
		if !safeID.MatchString(value) {
			return errors.New("operation, task, workspace, channel, and creator ids are required")
		}
	}
	if input.ThreadID != "" && !safeID.MatchString(input.ThreadID) || input.ParentTaskID != "" && !safeID.MatchString(input.ParentTaskID) || input.AssigneeID != "" && !safeID.MatchString(input.AssigneeID) {
		return errors.New("invalid optional task identity")
	}
	if input.Title == "" || input.Objective == "" {
		return errors.New("title and objective are required")
	}
	return validateContract(input.Requirements, input.Scope, input.StopConditions, input.Gate)
}

func validateContract(requirements []Requirement, scope Scope, stop []string, gate GateSpec) error {
	if len(requirements) == 0 || len(stop) == 0 || len(gate.Checks) == 0 || len(gate.RequiredEvidence) == 0 || gate.OnReject == "" {
		return errors.New("requirements, stop conditions, gate checks, evidence contract, and rejection policy are required")
	}
	if gate.Kind != GateHuman && gate.Kind != GateCode && gate.Kind != GateAgent || !safeID.MatchString(gate.ReviewerID) {
		return errors.New("valid gate kind and reviewer are required")
	}
	seen := make(map[string]bool)
	for _, requirement := range requirements {
		if !safeID.MatchString(requirement.ID) || requirement.Text == "" || seen[requirement.ID] {
			return errors.New("requirements need unique ids and non-empty text")
		}
		seen[requirement.ID] = true
	}
	if len(scope.Allow) == 0 && len(scope.Deny) == 0 {
		return errors.New("task scope must have an allow or deny boundary")
	}
	return nil
}

func validateTask(task Task) error {
	if task.SchemaVersion != SchemaVersion || !safeID.MatchString(task.ID) || !safeID.MatchString(task.WorkspaceID) || !safeID.MatchString(task.ChannelID) || !safeID.MatchString(task.CreatorID) || task.Revision <= 0 || task.ContractRevision <= 0 || task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
		return errors.New("invalid canonical task")
	}
	if task.State != Todo && task.State != InProgress && task.State != InReview && task.State != Done && task.State != Closed {
		return errors.New("invalid task state")
	}
	if task.ThreadID != "" && !safeID.MatchString(task.ThreadID) || task.ParentTaskID != "" && !safeID.MatchString(task.ParentTaskID) || task.AssigneeID != "" && !safeID.MatchString(task.AssigneeID) {
		return errors.New("invalid optional canonical task identity")
	}
	if err := validateContract(task.Requirements, task.Scope, task.StopConditions, task.Gate); err != nil {
		return err
	}
	seenOperations := make(map[string]bool, len(task.Operations))
	for index, operation := range task.Operations {
		if !safeID.MatchString(operation.ID) || operation.Action == "" || !safeDigest.MatchString(operation.Fingerprint) || operation.CreatedAt.IsZero() || operation.Revision != int64(index+1) || seenOperations[operation.ID] {
			return errors.New("invalid task operation ledger")
		}
		seenOperations[operation.ID] = true
	}
	if len(task.Operations) == 0 || task.Operations[len(task.Operations)-1].Revision != task.Revision {
		return errors.New("task operation ledger does not reach current revision")
	}
	if task.Claim != nil && (!safeID.MatchString(task.Claim.OwnerID) || !safeID.MatchString(task.Claim.LeaseID) || task.Claim.ExpiresAt.IsZero()) {
		return errors.New("invalid task claim")
	}
	seenArtifacts := make(map[ArtifactRef]bool, len(task.ArtifactRefs))
	for _, ref := range task.ArtifactRefs {
		if !validArtifactRef(ref) || seenArtifacts[ref] {
			return errors.New("invalid or duplicate task artifact reference")
		}
		seenArtifacts[ref] = true
	}
	return nil
}

func validateHandoff(task *Task, handoff Handoff) error {
	if handoff.Summary == "" || len(handoff.Evidence) == 0 || len(handoff.Artifacts) == 0 || handoff.NextAction == "" {
		return errors.New("handoff summary, evidence, artifact versions, and next action are required")
	}
	seen := make(map[ArtifactRef]bool, len(handoff.Artifacts))
	for _, ref := range handoff.Artifacts {
		if !validArtifactRef(ref) || seen[ref] {
			return errors.New("handoff contains invalid or duplicate artifacts")
		}
		seen[ref] = true
		if !containsArtifact(task.ArtifactRefs, ref) {
			return errors.New("handoff references an artifact version not attached to the task")
		}
	}
	if len(task.Gate.RequiredEvidence) > 0 && len(handoff.Evidence) < len(task.Gate.RequiredEvidence) {
		return errors.New("handoff does not provide the gate's required evidence")
	}
	return nil
}

func validArtifactRef(ref ArtifactRef) bool {
	return safeID.MatchString(ref.ID) && safeID.MatchString(ref.Version) && safeDigest.MatchString(ref.Digest)
}

func validClaim(task *Task, owner, lease string, now time.Time) bool {
	return task.Claim != nil && task.Claim.OwnerID == owner && task.Claim.LeaseID == lease && now.Before(task.Claim.ExpiresAt)
}

func operationMatch(operations []Operation, id, fingerprint string) bool {
	for _, operation := range operations {
		if operation.ID == id {
			return operation.Fingerprint == fingerprint
		}
	}
	return false
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

func transition(from, to State) error {
	return fmt.Errorf("cannot transition %s to %s: %w", from, to, ErrTransition)
}

func terminal(state State) bool { return state == Done || state == Closed }

func containsArtifact(items []ArtifactRef, target ArtifactRef) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func sameArtifactSet(left, right []ArtifactRef) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[ArtifactRef]int, len(left))
	for _, item := range left {
		counts[item]++
	}
	for _, item := range right {
		counts[item]--
		if counts[item] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func normalizeCreate(input Create) Create {
	input.OperationID = strings.TrimSpace(input.OperationID)
	input.ID = strings.TrimSpace(input.ID)
	input.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	input.ChannelID = strings.TrimSpace(input.ChannelID)
	input.ThreadID = strings.TrimSpace(input.ThreadID)
	input.ParentTaskID = strings.TrimSpace(input.ParentTaskID)
	input.CreatorID = strings.TrimSpace(input.CreatorID)
	input.AssigneeID = strings.TrimSpace(input.AssigneeID)
	input.Title = strings.TrimSpace(input.Title)
	input.Objective = strings.TrimSpace(input.Objective)
	input.Requirements = normalizeRequirements(input.Requirements)
	input.Scope = normalizeScope(input.Scope)
	input.StopConditions = normalizeStrings(input.StopConditions)
	input.Gate = normalizeGate(input.Gate)
	return input
}

func normalizeRequirements(items []Requirement) []Requirement {
	result := make([]Requirement, 0, len(items))
	for _, item := range items {
		item.ID = strings.TrimSpace(item.ID)
		item.Text = strings.TrimSpace(item.Text)
		result = append(result, item)
	}
	return result
}

func normalizeScope(scope Scope) Scope {
	scope.Allow = normalizeStrings(scope.Allow)
	scope.Deny = normalizeStrings(scope.Deny)
	return scope
}

func normalizeGate(gate GateSpec) GateSpec {
	gate.ReviewerID = strings.TrimSpace(gate.ReviewerID)
	gate.Checks = normalizeStrings(gate.Checks)
	gate.RequiredEvidence = normalizeStrings(gate.RequiredEvidence)
	gate.OnReject = strings.TrimSpace(gate.OnReject)
	return gate
}

func normalizeHandoff(handoff Handoff) Handoff {
	handoff.AuthorID = strings.TrimSpace(handoff.AuthorID)
	handoff.Summary = strings.TrimSpace(handoff.Summary)
	handoff.Decisions = normalizeStrings(handoff.Decisions)
	handoff.Evidence = normalizeStrings(handoff.Evidence)
	handoff.Remaining = normalizeStrings(handoff.Remaining)
	handoff.Risks = normalizeStrings(handoff.Risks)
	handoff.NextAction = strings.TrimSpace(handoff.NextAction)
	handoff.CreatedAt = time.Time{}
	return handoff
}

func normalizeGateResult(result GateResult) GateResult {
	result.ReviewerID = strings.TrimSpace(result.ReviewerID)
	result.Evidence = normalizeStrings(result.Evidence)
	result.Reason = strings.TrimSpace(result.Reason)
	result.CreatedAt = time.Time{}
	return result
}

func normalizeStrings(items []string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func cloneTask(task Task) Task {
	encoded, _ := json.Marshal(task)
	var cloned Task
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func cloneRequirements(items []Requirement) []Requirement {
	return append([]Requirement(nil), items...)
}
func cloneScope(scope Scope) Scope {
	return Scope{Allow: append([]string(nil), scope.Allow...), Deny: append([]string(nil), scope.Deny...)}
}
func cloneGate(gate GateSpec) GateSpec {
	gate.Checks = append([]string(nil), gate.Checks...)
	gate.RequiredEvidence = append([]string(nil), gate.RequiredEvidence...)
	return gate
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
