package sessionstore

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
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/schema"
)

const stateVersion = 1

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var (
	ErrConflict = errors.New("session operation conflict")
	ErrBusy     = errors.New("session has an unfinished turn")
)

type State struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	WorkDir   string           `json:"work_dir"`
	Revision  int64            `json:"revision"`
	Messages  []schema.Message `json:"messages,omitempty"`
	Turns     []Turn           `json:"turns,omitempty"`
	Pending   *PendingTurn     `json:"pending,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type PendingTurn struct {
	OperationID string `json:"operation_id"`
	Prompt      string `json:"prompt"`
	// SemanticSHA binds the idempotency key to the prompt and all caller-
	// supplied execution policy, not just to the visible user text.
	SemanticSHA  string    `json:"semantic_sha256"`
	RunID        string    `json:"run_id"`
	BaseRevision int64     `json:"base_revision"`
	StartedAt    time.Time `json:"started_at"`
}

type Turn struct {
	OperationID     string                `json:"operation_id"`
	SemanticSHA     string                `json:"semantic_sha256"`
	RunID           string                `json:"run_id"`
	Reason          schema.TerminalReason `json:"reason"`
	FinalMessage    schema.Message        `json:"final_message,omitempty"`
	Turns           int                   `json:"turns"`
	Usage           schema.Usage          `json:"usage"`
	HistoryRevision int64                 `json:"history_revision"`
	StartedAt       time.Time             `json:"started_at"`
	CompletedAt     time.Time             `json:"completed_at"`
}

type Admission struct {
	State    State
	Pending  PendingTurn
	Cached   *Turn
	Resuming bool
}

type Store struct {
	dir   string
	mu    sync.Mutex
	lanes sync.Map
}

func New(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve session directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	return &Store{dir: abs}, nil
}

func (s *Store) Dir() string { return s.dir }

// Acquire serializes complete turns for one session while allowing unrelated
// sessions to proceed independently.
func (s *Store) Acquire(ctx context.Context, sessionID string) (func(), error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	value, _ := s.lanes.LoadOrStore(sessionID, make(chan struct{}, 1))
	lane := value.(chan struct{})
	select {
	case lane <- struct{}{}:
		return func() { <-lane }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) Begin(_ context.Context, sessionID, operationID, prompt, workDir, runID string) (Admission, error) {
	return s.begin(sessionID, operationID, prompt, workDir, runID, "")
}

// BeginWithSemantics extends operation idempotency to execution policy such as
// Plan Mode, Thinking, and approved tools. semanticAttributes must be a stable
// canonical representation; the store always includes the prompt itself.
func (s *Store) BeginWithSemantics(_ context.Context, sessionID, operationID, prompt, workDir, runID, semanticAttributes string) (Admission, error) {
	return s.begin(sessionID, operationID, prompt, workDir, runID, semanticAttributes)
}

func (s *Store) begin(sessionID, operationID, prompt, workDir, runID, semanticAttributes string) (Admission, error) {
	prompt = strings.TrimSpace(prompt)
	if err := validateID(sessionID); err != nil {
		return Admission{}, err
	}
	if err := validateID(operationID); err != nil {
		return Admission{}, fmt.Errorf("invalid operation id: %w", err)
	}
	if prompt == "" {
		return Admission{}, errors.New("session prompt is required")
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return Admission{}, fmt.Errorf("resolve session workdir: %w", err)
	}
	absWorkDir = filepath.Clean(absWorkDir)
	semanticDigest := digest(prompt + "\x00" + semanticAttributes)

	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists, err := s.readStateUnlocked(sessionID)
	if err != nil {
		return Admission{}, err
	}
	if !exists {
		now := time.Now().UTC()
		state = State{Version: stateVersion, ID: sessionID, WorkDir: absWorkDir, CreatedAt: now, UpdatedAt: now}
	} else if state.WorkDir != absWorkDir {
		return Admission{}, fmt.Errorf("session workspace %q does not match %q: %w", state.WorkDir, absWorkDir, ErrConflict)
	}
	for index := range state.Turns {
		turn := state.Turns[index]
		if turn.OperationID != operationID {
			continue
		}
		if turn.SemanticSHA != semanticDigest {
			return Admission{}, fmt.Errorf("operation %q was reused with different semantics: %w", operationID, ErrConflict)
		}
		return Admission{State: cloneState(state), Cached: &turn}, nil
	}
	if state.Pending != nil {
		if state.Pending.OperationID != operationID {
			return Admission{}, fmt.Errorf("operation %q is still pending as run %q: %w", state.Pending.OperationID, state.Pending.RunID, ErrBusy)
		}
		if state.Pending.SemanticSHA != semanticDigest {
			return Admission{}, fmt.Errorf("pending operation %q was reused with different semantics: %w", operationID, ErrConflict)
		}
		return Admission{State: cloneState(state), Pending: *state.Pending, Resuming: true}, nil
	}
	if runID == "" {
		runID = newRunID(sessionID)
	}
	if err := validateID(runID); err != nil {
		return Admission{}, fmt.Errorf("invalid run id: %w", err)
	}
	now := time.Now().UTC()
	pending := PendingTurn{
		OperationID: operationID, Prompt: prompt, SemanticSHA: semanticDigest, RunID: runID,
		BaseRevision: state.Revision, StartedAt: now,
	}
	state.Pending = &pending
	state.Revision++
	state.UpdatedAt = now
	if err := s.writeStateUnlocked(state); err != nil {
		return Admission{}, err
	}
	return Admission{State: cloneState(state), Pending: pending}, nil
}

func (s *Store) Commit(_ context.Context, sessionID, operationID string, expectedRevision int64, result engine.RunResult) (State, error) {
	if err := validateID(sessionID); err != nil {
		return State{}, err
	}
	if err := validateID(operationID); err != nil {
		return State{}, fmt.Errorf("invalid operation id: %w", err)
	}
	if result.RunID == "" || result.Reason == "" || result.CompletedAt.IsZero() || len(result.Messages) == 0 {
		return State{}, errors.New("cannot commit an incomplete session result")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists, err := s.readStateUnlocked(sessionID)
	if err != nil {
		return State{}, err
	}
	if !exists || state.Pending == nil {
		return State{}, fmt.Errorf("session %q has no pending turn: %w", sessionID, ErrConflict)
	}
	if state.Revision != expectedRevision {
		return State{}, fmt.Errorf("expected session revision %d, current revision %d: %w", expectedRevision, state.Revision, ErrConflict)
	}
	pending := *state.Pending
	if pending.OperationID != operationID || pending.RunID != result.RunID {
		return State{}, fmt.Errorf("pending turn identity does not match result: %w", ErrConflict)
	}
	turn := Turn{
		OperationID: operationID, SemanticSHA: pending.SemanticSHA, RunID: result.RunID, Reason: result.Reason,
		FinalMessage: result.FinalMessage, Turns: result.Turns, Usage: result.Usage,
		HistoryRevision: result.HistoryRevision, StartedAt: result.StartedAt, CompletedAt: result.CompletedAt,
	}
	state.Messages = append([]schema.Message(nil), result.Messages...)
	state.Turns = append(state.Turns, turn)
	state.Pending = nil
	state.Revision++
	state.UpdatedAt = time.Now().UTC()
	if err := s.writeStateUnlocked(state); err != nil {
		return State{}, err
	}
	return cloneState(state), nil
}

func (s *Store) Get(_ context.Context, sessionID string) (State, error) {
	if err := validateID(sessionID); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, exists, err := s.readStateUnlocked(sessionID)
	if err != nil {
		return State{}, err
	}
	if !exists {
		return State{}, os.ErrNotExist
	}
	return cloneState(state), nil
}

func (s *Store) readStateUnlocked(sessionID string) (State, bool, error) {
	file, err := os.Open(s.path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, false, fmt.Errorf("decode session %q: %w", sessionID, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return State{}, false, fmt.Errorf("session %q contains trailing data", sessionID)
	}
	if err := validateState(sessionID, state); err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

func (s *Store) writeStateUnlocked(state State) error {
	temporary, err := os.CreateTemp(s.dir, ".01agent-session-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
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
	path := s.path(state.ID)
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	verified, exists, err := s.readStateUnlocked(state.ID)
	if err != nil || !exists {
		return fmt.Errorf("read back session state: %w", err)
	}
	if verified.Revision != state.Revision || verified.ID != state.ID || (verified.Pending != nil) != (state.Pending != nil) || len(verified.Turns) != len(state.Turns) {
		return errors.New("session state read-back verification failed")
	}
	return nil
}

func validateState(sessionID string, state State) error {
	if state.Version != stateVersion || state.ID != sessionID || state.WorkDir == "" || state.Revision <= 0 || state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return fmt.Errorf("session %q has invalid canonical state", sessionID)
	}
	operations := make(map[string]string, len(state.Turns))
	for _, turn := range state.Turns {
		if err := validateID(turn.OperationID); err != nil || turn.RunID == "" || turn.SemanticSHA == "" || turn.Reason == "" || turn.CompletedAt.IsZero() {
			return fmt.Errorf("session %q has an invalid completed turn", sessionID)
		}
		if previous, exists := operations[turn.OperationID]; exists && previous != turn.SemanticSHA {
			return fmt.Errorf("session %q has conflicting operation records", sessionID)
		}
		operations[turn.OperationID] = turn.SemanticSHA
	}
	if state.Pending != nil {
		if err := validateID(state.Pending.OperationID); err != nil || state.Pending.RunID == "" || state.Pending.Prompt == "" || state.Pending.SemanticSHA == "" || state.Pending.StartedAt.IsZero() {
			return fmt.Errorf("session %q has an invalid pending turn", sessionID)
		}
		if _, exists := operations[state.Pending.OperationID]; exists {
			return fmt.Errorf("session %q has a completed and pending copy of one operation", sessionID)
		}
	}
	return nil
}

func cloneState(state State) State {
	state.Messages = append([]schema.Message(nil), state.Messages...)
	state.Turns = append([]Turn(nil), state.Turns...)
	if state.Pending != nil {
		pending := *state.Pending
		state.Pending = &pending
	}
	return state
}

func validateID(value string) error {
	if !safeID.MatchString(value) {
		return fmt.Errorf("invalid id %q", value)
	}
	return nil
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func newRunID(sessionID string) string {
	var value [10]byte
	if _, err := rand.Read(value[:]); err == nil {
		prefix := sessionID
		if len(prefix) > 32 {
			prefix = prefix[:32]
		}
		return "session-" + prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("session-%d", time.Now().UnixNano())
}

func (s *Store) path(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".session.json")
}
