package automation

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

	"github.com/royal007a/01agent/internal/workitem"
)

var (
	ErrConflict = errors.New("automation revision conflict")
	ErrRun      = errors.New("invalid automation run report")
	safeID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct {
	path  string
	tasks *workitem.Store
	mu    sync.Mutex
	now   func() time.Time
}

func New(dir string, tasks *workitem.Store) (*Store, error) {
	if tasks == nil {
		return nil, errors.New("product task store is required")
	}
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(abs, "automations-v1.json"), tasks: tasks, now: func() time.Time { return time.Now().UTC() }}
	if _, err := os.Stat(store.path); errors.Is(err, os.ErrNotExist) {
		if err := store.writeUnlocked(State{SchemaVersion: SchemaVersion, Automations: map[string]Automation{}, UpdatedAt: store.now()}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := store.readUnlocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Automation, error) {
	if err := ctx.Err(); err != nil {
		return Automation{}, err
	}
	input = normalizeCreate(input)
	if err := validateCreate(input); err != nil {
		return Automation{}, err
	}
	fingerprint, _ := semanticFingerprint("create", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Automation{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Automation{}, ErrConflict
		}
		return state.Automations[operation.AutomationID], nil
	}
	if _, found := state.Automations[input.ID]; found {
		return Automation{}, ErrConflict
	}
	now := s.now()
	if input.StartAt.IsZero() {
		input.StartAt = now
	}
	item := Automation{SchemaVersion: SchemaVersion, ID: input.ID, Name: input.Name, EverySeconds: input.EverySeconds, NextRunAt: input.StartAt, FailurePauseThreshold: input.FailurePauseThreshold, Status: Active, Template: input.Template, Revision: 1, CreatedAt: now, UpdatedAt: now}
	state.Automations[item.ID] = item
	appendOperation(&state, input.OperationID, fingerprint, item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Automation{}, err
	}
	return item, nil
}

func (s *Store) Tick(ctx context.Context, at time.Time) (TickResult, error) {
	if err := ctx.Err(); err != nil {
		return TickResult{}, err
	}
	if at.IsZero() {
		at = s.now()
	}
	at = at.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return TickResult{}, err
	}
	ids := make([]string, 0, len(state.Automations))
	for id := range state.Automations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := TickResult{}
	changed := false
	for _, id := range ids {
		item := state.Automations[id]
		if item.Status != Active || at.Before(item.NextRunAt) {
			continue
		}
		if finalizeTerminal(ctx, s.tasks, &item, at) {
			changed = true
		}
		if item.Status == Paused {
			item.Revision++
			item.UpdatedAt = at
			state.Automations[id] = item
			result.Paused = append(result.Paused, id)
			continue
		}
		if activeRun(item) != nil {
			run := Run{Sequence: int64(len(item.Runs) + 1), State: RunSkipped, ScheduledAt: item.NextRunAt, CompletedAt: at, Reason: "previous automation task is still open"}
			item.Runs = append(item.Runs, run)
			advance(&item, at)
			item.Revision++
			item.UpdatedAt = at
			state.Automations[id] = item
			result.Skipped = append(result.Skipped, run)
			changed = true
			continue
		}
		sequence := int64(len(item.Runs) + 1)
		taskID := fmt.Sprintf("%s-run-%d", item.ID, sequence)
		created, err := s.tasks.Create(ctx, workitem.Create{
			OperationID: "dispatch-" + taskID, ID: taskID, WorkspaceID: item.Template.WorkspaceID, ChannelID: item.Template.ChannelID,
			CreatorID: item.Template.CreatorID, Title: item.Template.Title, Objective: item.Template.Objective,
			Requirements: item.Template.Requirements, Scope: item.Template.Scope, StopConditions: item.Template.StopConditions,
			AssigneeID: item.Template.AssigneeID, Gate: item.Template.Gate,
		})
		if err != nil {
			return TickResult{}, fmt.Errorf("dispatch automation %s: %w", item.ID, err)
		}
		run := Run{Sequence: sequence, TaskID: created.ID, State: RunDispatched, ScheduledAt: item.NextRunAt, DispatchedAt: at}
		item.Runs = append(item.Runs, run)
		advance(&item, at)
		item.Revision++
		item.UpdatedAt = at
		state.Automations[id] = item
		result.Dispatched = append(result.Dispatched, run)
		changed = true
	}
	if !changed {
		return result, nil
	}
	now := s.now()
	fingerprint, _ := semanticFingerprint("tick", struct{ At time.Time }{at})
	appendOperation(&state, internalOperationID("tick", at.Format(time.RFC3339Nano), fmt.Sprint(state.Revision+1)), fingerprint, "scheduler", now)
	if err := s.writeUnlocked(state); err != nil {
		return TickResult{}, err
	}
	return result, nil
}

func (s *Store) Report(ctx context.Context, automationID string, input ReportInput) (Automation, error) {
	if err := ctx.Err(); err != nil {
		return Automation{}, err
	}
	automationID = strings.TrimSpace(automationID)
	input.OperationID, input.TaskID, input.Reason = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.TaskID), strings.TrimSpace(input.Reason)
	input.Evidence = normalizeStrings(input.Evidence)
	if !safeID.MatchString(automationID) || !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.TaskID) || len(input.Evidence) == 0 || input.Reason == "" {
		return Automation{}, errors.New("automation, operation, task, evidence, and reason are required")
	}
	fingerprint, _ := semanticFingerprint("report", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Automation{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint || operation.AutomationID != automationID {
			return Automation{}, ErrConflict
		}
		return state.Automations[automationID], nil
	}
	item, found := state.Automations[automationID]
	if !found {
		return Automation{}, os.ErrNotExist
	}
	index := -1
	for current := len(item.Runs) - 1; current >= 0; current-- {
		if item.Runs[current].TaskID == input.TaskID {
			index = current
			break
		}
	}
	if index < 0 || item.Runs[index].State != RunDispatched {
		return Automation{}, ErrRun
	}
	task, err := s.tasks.Get(ctx, input.TaskID)
	if err != nil {
		return Automation{}, err
	}
	if input.Success {
		if task.State != workitem.Done {
			return Automation{}, errors.New("successful automation report requires a done product task")
		}
		item.Runs[index].State = RunSucceeded
		item.ConsecutiveFailures = 0
	} else {
		if task.State != workitem.Closed {
			return Automation{}, errors.New("failed automation report requires a closed product task")
		}
		item.Runs[index].State = RunFailed
		item.ConsecutiveFailures++
		if item.ConsecutiveFailures >= item.FailurePauseThreshold {
			item.Status = Paused
		}
	}
	now := s.now()
	item.Runs[index].Evidence, item.Runs[index].Reason, item.Runs[index].CompletedAt = append([]string(nil), input.Evidence...), input.Reason, now
	item.Revision++
	item.UpdatedAt = now
	state.Automations[item.ID] = item
	appendOperation(&state, input.OperationID, fingerprint, item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Automation{}, err
	}
	return item, nil
}

func (s *Store) SetStatus(ctx context.Context, automationID, operationID string, status Status) (Automation, error) {
	if err := ctx.Err(); err != nil {
		return Automation{}, err
	}
	automationID, operationID = strings.TrimSpace(automationID), strings.TrimSpace(operationID)
	if !safeID.MatchString(automationID) || !safeID.MatchString(operationID) {
		return Automation{}, errors.New("valid automation and operation IDs are required")
	}
	semantic := struct {
		Automation string `json:"automation"`
		Status     Status `json:"status"`
	}{automationID, status}
	fingerprint, _ := semanticFingerprint("set_status", semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Automation{}, err
	}
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return Automation{}, ErrConflict
		}
		return state.Automations[automationID], nil
	}
	item, found := state.Automations[automationID]
	if !found {
		return Automation{}, os.ErrNotExist
	}
	if status != Active && status != Paused {
		return Automation{}, errors.New("status must be active or paused")
	}
	now := s.now()
	item.Status, item.UpdatedAt = status, now
	item.Revision++
	if status == Active {
		item.ConsecutiveFailures = 0
	}
	state.Automations[item.ID] = item
	appendOperation(&state, operationID, fingerprint, item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Automation{}, err
	}
	return item, nil
}

func (s *Store) List(ctx context.Context) ([]Automation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return nil, err
	}
	result := make([]Automation, 0, len(state.Automations))
	for _, item := range state.Automations {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func activeRun(item Automation) *Run {
	for index := len(item.Runs) - 1; index >= 0; index-- {
		if item.Runs[index].State == RunDispatched {
			return &item.Runs[index]
		}
	}
	return nil
}

func finalizeTerminal(ctx context.Context, tasks *workitem.Store, item *Automation, at time.Time) bool {
	for index := len(item.Runs) - 1; index >= 0; index-- {
		if item.Runs[index].State != RunDispatched {
			continue
		}
		task, err := tasks.Get(ctx, item.Runs[index].TaskID)
		if err != nil {
			return false
		}
		switch task.State {
		case workitem.Done:
			item.Runs[index].State, item.Runs[index].CompletedAt = RunSucceeded, at
			item.Runs[index].Reason = "product task reached done"
			item.ConsecutiveFailures = 0
			return true
		case workitem.Closed:
			item.Runs[index].State, item.Runs[index].CompletedAt = RunFailed, at
			item.Runs[index].Reason = "product task closed before delivery"
			item.ConsecutiveFailures++
			if item.ConsecutiveFailures >= item.FailurePauseThreshold {
				item.Status = Paused
			}
			return true
		default:
			return false
		}
	}
	return false
}

func advance(item *Automation, at time.Time) {
	step := time.Duration(item.EverySeconds) * time.Second
	for !item.NextRunAt.After(at) {
		item.NextRunAt = item.NextRunAt.Add(step)
	}
}

func normalizeCreate(input CreateInput) CreateInput {
	input.OperationID, input.ID, input.Name = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ID), strings.TrimSpace(input.Name)
	template := &input.Template
	template.WorkspaceID, template.ChannelID, template.CreatorID = strings.TrimSpace(template.WorkspaceID), strings.TrimSpace(template.ChannelID), strings.TrimSpace(template.CreatorID)
	template.Title, template.Objective, template.AssigneeID = strings.TrimSpace(template.Title), strings.TrimSpace(template.Objective), strings.TrimSpace(template.AssigneeID)
	for index := range template.Requirements {
		template.Requirements[index].ID = strings.TrimSpace(template.Requirements[index].ID)
		template.Requirements[index].Text = strings.TrimSpace(template.Requirements[index].Text)
	}
	template.Scope.Allow = normalizeStrings(template.Scope.Allow)
	template.Scope.Deny = normalizeStrings(template.Scope.Deny)
	template.StopConditions = normalizeStrings(template.StopConditions)
	template.Gate.ReviewerID = strings.TrimSpace(template.Gate.ReviewerID)
	template.Gate.Checks = normalizeStrings(template.Gate.Checks)
	template.Gate.RequiredEvidence = normalizeStrings(template.Gate.RequiredEvidence)
	template.Gate.OnReject = strings.TrimSpace(template.Gate.OnReject)
	return input
}

func validateCreate(input CreateInput) error {
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.ID) || len(input.ID) > 96 || input.Name == "" || input.EverySeconds <= 0 || input.FailurePauseThreshold <= 0 {
		return errors.New("valid operation, automation, interval, and failure threshold are required")
	}
	if !safeID.MatchString(input.Template.WorkspaceID) || !safeID.MatchString(input.Template.ChannelID) || !safeID.MatchString(input.Template.CreatorID) || input.Template.Title == "" || input.Template.Objective == "" || len(input.Template.Requirements) == 0 || len(input.Template.StopConditions) == 0 {
		return errors.New("automation task template is incomplete")
	}
	for _, requirement := range input.Template.Requirements {
		if !safeID.MatchString(requirement.ID) || requirement.Text == "" {
			return errors.New("automation task requirement is invalid")
		}
	}
	gate := input.Template.Gate
	if gate.Kind != workitem.GateHuman && gate.Kind != workitem.GateCode && gate.Kind != workitem.GateAgent || gate.ReviewerID == "" || len(gate.Checks) == 0 || len(gate.RequiredEvidence) == 0 || gate.OnReject == "" {
		return errors.New("automation task gate is incomplete")
	}
	return nil
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

func appendOperation(state *State, id, fingerprint, automationID string, now time.Time) {
	state.Revision++
	state.UpdatedAt = now
	state.Operations = append(state.Operations, Operation{ID: id, Fingerprint: fingerprint, AutomationID: automationID, Revision: state.Revision, CreatedAt: now})
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
		Action string
		Value  any
	}{action, value})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func internalOperationID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
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
		return State{}, errors.New("automation state has trailing data")
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
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".automations-*.tmp")
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
		return errors.New("automation state read-back verification failed")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != SchemaVersion || state.Revision < 0 || state.Automations == nil {
		return errors.New("invalid canonical automation state")
	}
	for id, item := range state.Automations {
		if id != item.ID || item.SchemaVersion != SchemaVersion || !safeID.MatchString(id) || len(id) > 96 || item.Name == "" || item.EverySeconds <= 0 || item.FailurePauseThreshold <= 0 || item.ConsecutiveFailures < 0 || item.Revision <= 0 || item.NextRunAt.IsZero() || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() || item.Status != Active && item.Status != Paused {
			return errors.New("invalid canonical automation")
		}
		if err := validateCreate(CreateInput{OperationID: "validate", ID: item.ID, Name: item.Name, EverySeconds: item.EverySeconds, FailurePauseThreshold: item.FailurePauseThreshold, Template: item.Template}); err != nil {
			return fmt.Errorf("invalid canonical automation template: %w", err)
		}
		for index, run := range item.Runs {
			if run.Sequence != int64(index+1) || run.ScheduledAt.IsZero() {
				return errors.New("invalid canonical automation run ordering")
			}
			switch run.State {
			case RunDispatched:
				if !safeID.MatchString(run.TaskID) || run.DispatchedAt.IsZero() || !run.CompletedAt.IsZero() {
					return errors.New("invalid dispatched automation run")
				}
			case RunSucceeded, RunFailed:
				if !safeID.MatchString(run.TaskID) || run.DispatchedAt.IsZero() || run.CompletedAt.IsZero() {
					return errors.New("invalid completed automation run")
				}
			case RunSkipped:
				if run.TaskID != "" || run.CompletedAt.IsZero() {
					return errors.New("invalid skipped automation run")
				}
			default:
				return errors.New("invalid automation run state")
			}
		}
	}
	seen := map[string]bool{}
	for index, operation := range state.Operations {
		if operation.Revision != int64(index+1) || !safeID.MatchString(operation.ID) || seen[operation.ID] || !hexDigest.MatchString(operation.Fingerprint) || operation.CreatedAt.IsZero() {
			return errors.New("invalid automation operation ledger")
		}
		seen[operation.ID] = true
	}
	if int64(len(state.Operations)) != state.Revision {
		return errors.New("automation operation ledger does not reach revision")
	}
	return nil
}
