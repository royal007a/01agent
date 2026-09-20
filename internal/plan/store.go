package plan

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
	"strings"
	"sync"
	"time"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var ErrConflict = errors.New("plan revision conflict")

type StepStatus string

const (
	Pending    StepStatus = "pending"
	InProgress StepStatus = "in_progress"
	Completed  StepStatus = "completed"
	Blocked    StepStatus = "blocked"
)

type Step struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status StepStatus `json:"status"`
	Note   string     `json:"note,omitempty"`
}

type State struct {
	Version         int       `json:"version"`
	Scope           string    `json:"scope"`
	Revision        int64     `json:"revision"`
	Objective       string    `json:"objective"`
	Steps           []Step    `json:"steps"`
	LastOperationID string    `json:"last_operation_id"`
	LastFingerprint string    `json:"last_fingerprint"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Update struct {
	Scope            string
	OperationID      string
	ExpectedRevision int64
	Objective        string
	Steps            []Step
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: abs}, nil
}

func (s *Store) Get(_ context.Context, scope string) (State, error) {
	if strings.TrimSpace(scope) == "" {
		return State{}, errors.New("plan scope is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked(scope)
	if err != nil {
		return State{}, err
	}
	if err := s.repairProjectionsUnlocked(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Store) Update(_ context.Context, update Update) (State, error) {
	update.Scope = strings.TrimSpace(update.Scope)
	update.OperationID = strings.TrimSpace(update.OperationID)
	update.Objective = strings.TrimSpace(update.Objective)
	if update.Scope == "" || !safeID.MatchString(update.OperationID) || update.Objective == "" || update.ExpectedRevision < 0 {
		return State{}, errors.New("scope, safe operation_id, objective, and non-negative expected_revision are required")
	}
	if len(update.Steps) == 0 || len(update.Steps) > 64 {
		return State{}, errors.New("plan requires 1-64 steps")
	}
	seen := make(map[string]bool, len(update.Steps))
	for index := range update.Steps {
		step := &update.Steps[index]
		step.ID = strings.TrimSpace(step.ID)
		step.Title = strings.TrimSpace(step.Title)
		step.Note = strings.TrimSpace(step.Note)
		if !safeID.MatchString(step.ID) || step.Title == "" || seen[step.ID] {
			return State{}, fmt.Errorf("step %d has an invalid or duplicate id/title", index)
		}
		seen[step.ID] = true
		switch step.Status {
		case Pending, InProgress, Completed, Blocked:
		default:
			return State{}, fmt.Errorf("step %q has invalid status %q", step.ID, step.Status)
		}
	}
	fingerprint, err := updateFingerprint(update)
	if err != nil {
		return State{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readUnlocked(update.Scope)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	if exists && current.LastOperationID == update.OperationID {
		if current.LastFingerprint != fingerprint {
			return State{}, fmt.Errorf("operation %q was reused with different semantics: %w", update.OperationID, ErrConflict)
		}
		if err := s.repairProjectionsUnlocked(current); err != nil {
			return State{}, err
		}
		return current, nil
	}
	currentRevision := int64(0)
	createdAt := time.Now().UTC()
	if exists {
		currentRevision = current.Revision
		createdAt = current.CreatedAt
	}
	if currentRevision != update.ExpectedRevision {
		return State{}, fmt.Errorf("expected revision %d, current revision %d: %w", update.ExpectedRevision, currentRevision, ErrConflict)
	}
	now := time.Now().UTC()
	next := State{
		Version: 1, Scope: update.Scope, Revision: currentRevision + 1, Objective: update.Objective,
		Steps: append([]Step(nil), update.Steps...), LastOperationID: update.OperationID,
		LastFingerprint: fingerprint, CreatedAt: createdAt, UpdatedAt: now,
	}
	if err := s.writeJSONAtomicUnlocked(next); err != nil {
		return State{}, err
	}
	if err := s.repairProjectionsUnlocked(next); err != nil {
		return State{}, err
	}
	verified, err := s.readUnlocked(update.Scope)
	if err != nil {
		return State{}, err
	}
	if verified.Revision != next.Revision || verified.LastFingerprint != fingerprint {
		return State{}, errors.New("plan read-back verification failed")
	}
	return verified, nil
}

func (s *Store) readUnlocked(scope string) (State, error) {
	file, err := os.Open(filepath.Join(s.scopeDir(scope), "plan.json"))
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
		return State{}, errors.New("plan JSON contains trailing data")
	}
	if state.Version != 1 || state.Scope != scope || state.Revision <= 0 || state.LastOperationID == "" || state.LastFingerprint == "" {
		return State{}, errors.New("invalid canonical plan")
	}
	return state, nil
}

func (s *Store) writeJSONAtomicUnlocked(state State) error {
	dir := s.scopeDir(state.Scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeAtomic(dir, "plan.json", encoded)
}

func (s *Store) repairProjectionsUnlocked(state State) error {
	dir := s.scopeDir(state.Scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var planText strings.Builder
	fmt.Fprintf(&planText, "# Plan\n\nObjective: %s\n\nRevision: %d\n\n", state.Objective, state.Revision)
	for _, step := range state.Steps {
		fmt.Fprintf(&planText, "- [%s] **%s**: %s", statusMark(step.Status), step.ID, step.Title)
		if step.Note != "" {
			fmt.Fprintf(&planText, " — %s", step.Note)
		}
		planText.WriteByte('\n')
	}
	var todoText strings.Builder
	fmt.Fprintf(&todoText, "# TODO\n\nObjective: %s\n\n", state.Objective)
	for _, step := range state.Steps {
		if step.Status == Completed {
			continue
		}
		fmt.Fprintf(&todoText, "- [%s] %s (%s)", statusMark(step.Status), step.Title, step.ID)
		if step.Note != "" {
			fmt.Fprintf(&todoText, " — %s", step.Note)
		}
		todoText.WriteByte('\n')
	}
	if err := writeAtomic(dir, "PLAN.md", []byte(planText.String())); err != nil {
		return err
	}
	return writeAtomic(dir, "TODO.md", []byte(todoText.String()))
}

func writeAtomic(dir, name string, content []byte) error {
	temporary, err := os.CreateTemp(dir, ".plan-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func updateFingerprint(update Update) (string, error) {
	encoded, err := json.Marshal(struct {
		Scope     string `json:"scope"`
		Objective string `json:"objective"`
		Steps     []Step `json:"steps"`
	}{update.Scope, update.Objective, update.Steps})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func statusMark(status StepStatus) string {
	switch status {
	case Completed:
		return "x"
	case InProgress:
		return ">"
	case Blocked:
		return "!"
	default:
		return " "
	}
}

func (s *Store) scopeDir(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:]))
}
