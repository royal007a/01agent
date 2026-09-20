package approvalstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/tools"
)

const Version = 1

type State string

const (
	Pending  State = "pending"
	Approved State = "approved"
	Rejected State = "rejected"
	Expired  State = "expired"
	Consumed State = "consumed"
)

type Request struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	State            State           `json:"state"`
	RunID            string          `json:"run_id"`
	TurnID           string          `json:"turn_id"`
	OriginLeaseID    string          `json:"origin_lease_id"`
	Admission        int             `json:"admission"`
	CapabilityDigest string          `json:"capability_digest"`
	ToolName         string          `json:"tool_name"`
	Arguments        json.RawMessage `json:"arguments"`
	ArgumentsDigest  string          `json:"arguments_digest"`
	CreatedAt        time.Time       `json:"created_at"`
	ExpiresAt        time.Time       `json:"expires_at"`
	DecidedAt        time.Time       `json:"decided_at,omitempty"`
	ConsumedAt       time.Time       `json:"consumed_at,omitempty"`
	Actor            string          `json:"actor,omitempty"`
}

type Store struct {
	dir string
	ttl time.Duration
	mu  sync.Mutex
}

func New(dir string, ttl time.Duration) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("approval store directory is required")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: absolute, ttl: ttl}, nil
}

func (s *Store) Resolve(ctx context.Context, input tools.ApprovalRequest) (tools.ApprovalResolution, error) {
	if err := ctx.Err(); err != nil {
		return tools.ApprovalResolution{}, err
	}
	canonical, digest, err := canonicalArguments(input.Arguments)
	if err != nil {
		return tools.ApprovalResolution{}, err
	}
	if input.RunID == "" || input.TurnID == "" || input.LeaseID == "" || input.CapabilityDigest == "" || input.ToolName == "" {
		return tools.ApprovalResolution{}, errors.New("approval request identity is incomplete")
	}
	id := requestID(input.RunID, input.CapabilityDigest, input.ToolName, digest)
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(id)
	if errors.Is(err, os.ErrNotExist) {
		now := time.Now().UTC()
		record = Request{
			Version: Version, ID: id, State: Pending, RunID: input.RunID, TurnID: input.TurnID,
			OriginLeaseID: input.LeaseID, Admission: input.Admission, CapabilityDigest: input.CapabilityDigest,
			ToolName: input.ToolName, Arguments: canonical, ArgumentsDigest: digest,
			CreatedAt: now, ExpiresAt: now.Add(s.ttl),
		}
		if err := s.saveLocked(record); err != nil {
			return tools.ApprovalResolution{}, err
		}
		return resolution(record, false), nil
	}
	if err != nil {
		return tools.ApprovalResolution{}, err
	}
	if record.RunID != input.RunID || record.CapabilityDigest != input.CapabilityDigest || record.ToolName != input.ToolName || record.ArgumentsDigest != digest {
		return tools.ApprovalResolution{}, errors.New("approval semantic fingerprint conflict")
	}
	if record.State == Pending && time.Now().UTC().After(record.ExpiresAt) {
		record.State = Expired
		if err := s.saveLocked(record); err != nil {
			return tools.ApprovalResolution{}, err
		}
	}
	if record.State == Approved {
		record.State = Consumed
		record.ConsumedAt = time.Now().UTC()
		if err := s.saveLocked(record); err != nil {
			return tools.ApprovalResolution{}, err
		}
		return resolution(record, true), nil
	}
	return resolution(record, false), nil
}

func (s *Store) Decide(ctx context.Context, id string, decision State, actor string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if decision != Approved && decision != Rejected {
		return Request{}, errors.New("decision must be approved or rejected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.loadLocked(id)
	if err != nil {
		return Request{}, err
	}
	if record.State == Pending && time.Now().UTC().After(record.ExpiresAt) {
		record.State = Expired
		_ = s.saveLocked(record)
	}
	if record.State != Pending {
		if record.State == decision {
			return record, nil
		}
		return Request{}, fmt.Errorf("approval %s is already %s", id, record.State)
	}
	record.State = decision
	record.Actor = strings.TrimSpace(actor)
	record.DecidedAt = time.Now().UTC()
	if err := s.saveLocked(record); err != nil {
		return Request{}, err
	}
	return record, nil
}

func (s *Store) Get(ctx context.Context, id string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

func (s *Store) List(ctx context.Context, state State) ([]Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	items := make([]Request, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		item, err := s.loadLocked(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if state == "" || item.State == state {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return items, nil
}

func resolution(record Request, allowed bool) tools.ApprovalResolution {
	reason := fmt.Sprintf("approval %s is %s for exact call %s", record.ID, record.State, record.ToolName)
	return tools.ApprovalResolution{ID: record.ID, State: string(record.State), Allowed: allowed, Reason: reason}
}

func canonicalArguments(raw json.RawMessage) (json.RawMessage, string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func requestID(runID, capability, tool, argumentsDigest string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + capability + "\x00" + tool + "\x00" + argumentsDigest))
	return "apr-" + hex.EncodeToString(sum[:12])
}

func (s *Store) loadLocked(id string) (Request, error) {
	if !validID(id) {
		return Request{}, errors.New("invalid approval id")
	}
	content, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return Request{}, err
	}
	var record Request
	if err := json.Unmarshal(content, &record); err != nil {
		return Request{}, err
	}
	if record.Version != Version || record.ID != id {
		return Request{}, errors.New("invalid approval record")
	}
	return record, nil
}

func (s *Store) saveLocked(record Request) error {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temporary := filepath.Join(s.dir, ".approval-"+randomSuffix()+".tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, filepath.Join(s.dir, record.ID+".json")); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func randomSuffix() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "apr-") || len(id) != 28 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "apr-"))
	return err == nil
}
