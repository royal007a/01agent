package taskstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/engine"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Stopping  State = "stopping"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Canceled  State = "canceled"
	Lost      State = "lost"
)

type Task struct {
	Version          int       `json:"version"`
	ID               string    `json:"id"`
	ParentRunID      string    `json:"parent_run_id"`
	ParentTurnID     string    `json:"parent_turn_id,omitempty"`
	Title            string    `json:"title"`
	State            State     `json:"state"`
	OwnerID          string    `json:"owner_id,omitempty"`
	Output           string    `json:"output,omitempty"`
	Failure          string    `json:"failure,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	HeartbeatAt      time.Time `json:"heartbeat_at,omitempty"`
	DeliveredAt      time.Time `json:"delivered_at,omitempty"`
	ConsumedAt       time.Time `json:"consumed_at,omitempty"`
	ConsumedRevision int64     `json:"consumed_revision,omitempty"`
}

type Store struct {
	dir      string
	delivery engine.InputEnqueuer
	status   engine.InputStatusReader
	mu       sync.Mutex
	now      func() time.Time
}

func New(dir string, delivery engine.InputEnqueuer, status engine.InputStatusReader) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: abs, delivery: delivery, status: status, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Create(_ context.Context, task Task) (Task, error) {
	if task.ID == "" {
		task.ID = newID("task")
	}
	if !safeID.MatchString(task.ID) || !safeID.MatchString(task.ParentRunID) || strings.TrimSpace(task.Title) == "" {
		return Task{}, errors.New("task id, parent run id, and title are required")
	}
	now := s.now()
	task.Version = 1
	task.State = Queued
	task.Title = strings.TrimSpace(task.Title)
	task.CreatedAt = now
	task.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	var existing Task
	if err := readJSON(s.path(task.ID), &existing); err == nil {
		if existing.ParentRunID == task.ParentRunID && existing.ParentTurnID == task.ParentTurnID && existing.Title == task.Title {
			return existing, nil
		}
		return Task{}, fmt.Errorf("task %q already exists with different semantics", task.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Task{}, err
	}
	if err := s.writeAtomic(task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) Get(_ context.Context, taskID string) (Task, error) {
	if !safeID.MatchString(taskID) {
		return Task{}, errors.New("invalid task id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var task Task
	if err := readJSON(s.path(taskID), &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) Start(_ context.Context, taskID, ownerID string) (Task, error) {
	if strings.TrimSpace(ownerID) == "" {
		return Task{}, errors.New("owner id is required")
	}
	return s.mutate(taskID, func(task *Task) error {
		if task.State != Queued {
			return transitionError(task.State, Running)
		}
		task.State = Running
		task.OwnerID = ownerID
		task.HeartbeatAt = s.now()
		return nil
	})
}

func (s *Store) Heartbeat(_ context.Context, taskID, ownerID string) (Task, error) {
	return s.mutate(taskID, func(task *Task) error {
		if (task.State != Running && task.State != Stopping) || task.OwnerID != ownerID {
			return errors.New("heartbeat rejected for inactive task owner")
		}
		task.HeartbeatAt = s.now()
		return nil
	})
}

func (s *Store) RequestStop(_ context.Context, taskID string) (Task, error) {
	return s.mutate(taskID, func(task *Task) error {
		if task.State != Running {
			return transitionError(task.State, Stopping)
		}
		task.State = Stopping
		return nil
	})
}

func (s *Store) Complete(ctx context.Context, taskID, ownerID, output string) (Task, error) {
	task, err := s.finish(taskID, ownerID, Succeeded, output, "")
	if err != nil {
		return Task{}, err
	}
	return s.deliver(ctx, task)
}

func (s *Store) Fail(ctx context.Context, taskID, ownerID, failure string) (Task, error) {
	task, err := s.finish(taskID, ownerID, Failed, "", failure)
	if err != nil {
		return Task{}, err
	}
	return s.deliver(ctx, task)
}

func (s *Store) Cancel(ctx context.Context, taskID string) (Task, error) {
	task, err := s.mutate(taskID, func(task *Task) error {
		if task.State != Queued && task.State != Running && task.State != Stopping {
			return transitionError(task.State, Canceled)
		}
		task.State = Canceled
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return s.deliver(ctx, task)
}

func (s *Store) ReconcileLost(ctx context.Context, now time.Time, heartbeatTimeout time.Duration) ([]Task, error) {
	if heartbeatTimeout <= 0 {
		return nil, errors.New("positive heartbeat timeout is required")
	}
	tasks, err := s.list()
	if err != nil {
		return nil, err
	}
	var lost []Task
	for _, candidate := range tasks {
		if candidate.State != Running && candidate.State != Stopping {
			continue
		}
		if candidate.HeartbeatAt.IsZero() || now.Sub(candidate.HeartbeatAt) < heartbeatTimeout {
			continue
		}
		task, err := s.mutate(candidate.ID, func(task *Task) error {
			if task.State != Running && task.State != Stopping {
				return nil
			}
			task.State = Lost
			task.Failure = "executor heartbeat expired"
			return nil
		})
		if err != nil {
			return lost, err
		}
		task, err = s.deliver(ctx, task)
		if err != nil {
			return lost, err
		}
		lost = append(lost, task)
	}
	return lost, nil
}

func (s *Store) DeliverPending(ctx context.Context) error {
	tasks, err := s.list()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if terminal(task.State) && task.DeliveredAt.IsZero() {
			if _, err := s.deliver(ctx, task); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ReconcileConsumption(ctx context.Context) error {
	if s.status == nil {
		return errors.New("input delivery status reader is not configured")
	}
	tasks, err := s.list()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.DeliveredAt.IsZero() || !task.ConsumedAt.IsZero() {
			continue
		}
		status, err := s.status.InputStatus(ctx, task.ParentRunID, task.ID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if status.AckRevision <= 0 {
			continue
		}
		if _, err := s.mutate(task.ID, func(current *Task) error {
			current.ConsumedAt = status.AcknowledgedAt
			current.ConsumedRevision = status.AckRevision
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) finish(taskID, ownerID string, state State, output, failure string) (Task, error) {
	return s.mutate(taskID, func(task *Task) error {
		if task.State != Running && task.State != Stopping {
			return transitionError(task.State, state)
		}
		if task.OwnerID != ownerID {
			return errors.New("terminal update rejected for inactive task owner")
		}
		task.State = state
		task.Output = output
		task.Failure = failure
		return nil
	})
}

func (s *Store) deliver(ctx context.Context, task Task) (Task, error) {
	if !terminal(task.State) || !task.DeliveredAt.IsZero() {
		return task, nil
	}
	if s.delivery == nil {
		return task, errors.New("input delivery queue is not configured")
	}
	content, err := json.Marshal(map[string]any{
		"task_id": task.ID, "title": task.Title, "state": task.State,
		"output": task.Output, "failure": task.Failure,
	})
	if err != nil {
		return task, err
	}
	if _, err := s.delivery.Enqueue(ctx, engine.QueuedInput{
		ID: task.ID, RunID: task.ParentRunID, Kind: engine.InputTask, Content: string(content),
	}); err != nil {
		return task, err
	}
	return s.mutate(task.ID, func(current *Task) error {
		if current.DeliveredAt.IsZero() {
			current.DeliveredAt = s.now()
		}
		return nil
	})
}

func (s *Store) mutate(taskID string, update func(*Task) error) (Task, error) {
	if !safeID.MatchString(taskID) {
		return Task{}, errors.New("invalid task id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var task Task
	if err := readJSON(s.path(taskID), &task); err != nil {
		return Task{}, err
	}
	if err := update(&task); err != nil {
		return Task{}, err
	}
	task.UpdatedAt = s.now()
	if err := s.writeAtomic(task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (s *Store) list() ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".task.json") {
			continue
		}
		var task Task
		if err := readJSON(filepath.Join(s.dir, entry.Name()), &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (s *Store) writeAtomic(task Task) error {
	temporary, err := os.CreateTemp(s.dir, ".task-*.tmp")
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
	encodeErr := encoder.Encode(task)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(name, s.path(task.ID)); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func readJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func transitionError(from, to State) error {
	return fmt.Errorf("invalid task transition %s -> %s", from, to)
}

func terminal(state State) bool {
	return state == Succeeded || state == Failed || state == Canceled || state == Lost
}

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func (s *Store) path(taskID string) string { return filepath.Join(s.dir, taskID+".task.json") }
