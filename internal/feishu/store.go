package feishu

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
	"sort"
	"strings"
	"sync"
	"time"
)

type JobState string

const (
	JobQueued     JobState = "queued"
	JobProcessing JobState = "processing"
	JobSucceeded  JobState = "succeeded"
	JobFailed     JobState = "failed"
)

type Message struct {
	EventID   string `json:"event_id"`
	MessageID string `json:"message_id"`
	ChatID    string `json:"chat_id"`
	UserID    string `json:"user_id,omitempty"`
	Content   string `json:"content"`
}

type Job struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Message      Message   `json:"message"`
	SessionID    string    `json:"session_id"`
	OperationID  string    `json:"operation_id"`
	State        JobState  `json:"state"`
	Attempts     int       `json:"attempts"`
	AgentReply   string    `json:"agent_reply,omitempty"`
	ReplyMessage string    `json:"reply_message_id,omitempty"`
	Failure      string    `json:"failure,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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

func (s *Store) Enqueue(_ context.Context, message Message) (Job, bool, error) {
	message.EventID = strings.TrimSpace(message.EventID)
	message.MessageID = strings.TrimSpace(message.MessageID)
	message.ChatID = strings.TrimSpace(message.ChatID)
	message.Content = strings.TrimSpace(message.Content)
	if message.EventID == "" || message.MessageID == "" || message.ChatID == "" || message.Content == "" {
		return Job{}, false, errors.New("event_id, message_id, chat_id, and content are required")
	}
	if len(message.Content) > 64<<10 {
		return Job{}, false, errors.New("message content exceeds 64 KiB")
	}
	job := Job{
		Version: 1, ID: jobID(message.EventID), Message: message,
		SessionID: sessionID(message.ChatID), OperationID: operationID(message.EventID),
		State: JobQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing Job
	if err := readJSON(s.path(job.ID), &existing); err == nil {
		if existing.Message == message && existing.SessionID == job.SessionID && existing.OperationID == job.OperationID {
			return existing, false, nil
		}
		return Job{}, false, errors.New("event id was reused with different semantics")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Job{}, false, err
	}
	if err := s.writeAtomicUnlocked(job); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) Claim(_ context.Context, id string) (Job, bool, error) {
	return s.mutate(id, func(job *Job) (bool, error) {
		if job.State != JobQueued {
			return false, nil
		}
		job.State = JobProcessing
		job.Attempts++
		job.Failure = ""
		return true, nil
	})
}

func (s *Store) Complete(_ context.Context, id, reply, replyMessageID string) (Job, error) {
	job, changed, err := s.mutate(id, func(job *Job) (bool, error) {
		if job.State == JobSucceeded {
			if job.AgentReply != reply {
				return false, errors.New("completed job reply conflict")
			}
			return false, nil
		}
		if job.State != JobProcessing {
			return false, fmt.Errorf("cannot complete job in state %s", job.State)
		}
		job.State = JobSucceeded
		job.AgentReply = reply
		job.ReplyMessage = replyMessageID
		return true, nil
	})
	if err != nil {
		return Job{}, err
	}
	_ = changed
	return job, nil
}

func (s *Store) Fail(_ context.Context, id string, failure string, retry bool) (Job, error) {
	job, _, err := s.mutate(id, func(job *Job) (bool, error) {
		if job.State != JobProcessing {
			return false, fmt.Errorf("cannot fail job in state %s", job.State)
		}
		if retry {
			job.State = JobQueued
		} else {
			job.State = JobFailed
		}
		job.Failure = strings.TrimSpace(failure)
		return true, nil
	})
	return job, err
}

func (s *Store) Reconcile(_ context.Context) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	pending := make([]Job, 0)
	for _, job := range jobs {
		if job.State == JobProcessing {
			job.State = JobQueued
			job.Failure = "worker stopped before acknowledgement"
			job.UpdatedAt = time.Now().UTC()
			if err := s.writeAtomicUnlocked(job); err != nil {
				return nil, err
			}
		}
		if job.State == JobQueued {
			pending = append(pending, job)
		}
	}
	return pending, nil
}

func (s *Store) Pending(_ context.Context) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs, err := s.listUnlocked()
	if err != nil {
		return nil, err
	}
	pending := make([]Job, 0)
	for _, job := range jobs {
		if job.State == JobQueued {
			pending = append(pending, job)
		}
	}
	return pending, nil
}

func (s *Store) Get(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job Job
	if err := readJSON(s.path(id), &job); err != nil {
		return Job{}, err
	}
	return job, validateJob(job)
}

func (s *Store) mutate(id string, update func(*Job) (bool, error)) (Job, bool, error) {
	if !strings.HasPrefix(id, "feishu-") {
		return Job{}, false, errors.New("invalid job id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var job Job
	if err := readJSON(s.path(id), &job); err != nil {
		return Job{}, false, err
	}
	changed, err := update(&job)
	if err != nil || !changed {
		return job, changed, err
	}
	job.UpdatedAt = time.Now().UTC()
	if err := s.writeAtomicUnlocked(job); err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func (s *Store) listUnlocked() ([]Job, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".feishu.json") {
			continue
		}
		var job Job
		if err := readJSON(filepath.Join(s.dir, entry.Name()), &job); err != nil {
			return nil, err
		}
		if err := validateJob(job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	return jobs, nil
}

func (s *Store) writeAtomicUnlocked(job Job) error {
	if err := validateJob(job); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(s.dir, ".feishu-*.tmp")
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
	if err := encoder.Encode(job); err != nil {
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
	if err := os.Rename(name, s.path(job.ID)); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return err
	}
	var verified Job
	if err := readJSON(s.path(job.ID), &verified); err != nil {
		return err
	}
	if verified.ID != job.ID || verified.State != job.State || verified.Attempts != job.Attempts {
		return errors.New("feishu job read-back verification failed")
	}
	return nil
}

func validateJob(job Job) error {
	if job.Version != 1 || job.ID == "" || job.Message.EventID == "" || job.SessionID == "" || job.OperationID == "" || job.CreatedAt.IsZero() || job.UpdatedAt.IsZero() {
		return errors.New("invalid feishu job")
	}
	switch job.State {
	case JobQueued, JobProcessing, JobSucceeded, JobFailed:
		return nil
	default:
		return errors.New("invalid feishu job state")
	}
}

func readJSON(path string, target any) error {
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
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func jobID(eventID string) string       { return "feishu-" + shortDigest(eventID) }
func sessionID(chatID string) string    { return "feishu-chat-" + shortDigest(chatID) }
func operationID(eventID string) string { return "feishu-event-" + shortDigest(eventID) }
func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}
func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".feishu.json") }
