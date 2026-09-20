package runstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/engine"
)

const maximumQueuedInputBytes = 64 << 10

type inboxState struct {
	RunID string      `json:"run_id"`
	Items []inboxItem `json:"items"`
}

type inboxItem struct {
	Input          engine.QueuedInput `json:"input"`
	ClaimID        string             `json:"claim_id,omitempty"`
	ClaimTurnID    string             `json:"claim_turn_id,omitempty"`
	ClaimedAt      time.Time          `json:"claimed_at,omitempty"`
	AckRevision    int64              `json:"ack_revision,omitempty"`
	AcknowledgedAt time.Time          `json:"acknowledged_at,omitempty"`
}

func (s *FileStore) Enqueue(_ context.Context, input engine.QueuedInput) (engine.QueuedInput, error) {
	if err := validateID(input.RunID); err != nil {
		return engine.QueuedInput{}, err
	}
	input.Content = strings.TrimSpace(input.Content)
	if input.Content == "" || len(input.Content) > maximumQueuedInputBytes {
		return engine.QueuedInput{}, fmt.Errorf("queued input content must contain 1-%d bytes", maximumQueuedInputBytes)
	}
	switch input.Kind {
	case engine.InputUserSteer, engine.InputTool, engine.InputTask:
	default:
		return engine.QueuedInput{}, fmt.Errorf("invalid queued input kind %q", input.Kind)
	}
	if input.ID == "" {
		input.ID = newQueueID("input")
	}
	if !safeRunID.MatchString(input.ID) {
		return engine.QueuedInput{}, fmt.Errorf("invalid queued input id %q", input.ID)
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadInbox(input.RunID)
	if err != nil {
		return engine.QueuedInput{}, err
	}
	for _, item := range state.Items {
		if item.Input.ID == input.ID {
			if item.Input.Kind == input.Kind && item.Input.Content == input.Content {
				return item.Input, nil
			}
			return engine.QueuedInput{}, fmt.Errorf("input %q reused with different semantics", input.ID)
		}
	}
	state.Items = append(state.Items, inboxItem{Input: input})
	if err := s.writeAtomicUnlocked(s.inboxPath(input.RunID), state); err != nil {
		return engine.QueuedInput{}, err
	}
	return input, nil
}

func (s *FileStore) Claim(_ context.Context, runID, turnID string, limit int) (engine.InputClaim, error) {
	if err := validateID(runID); err != nil {
		return engine.InputClaim{}, err
	}
	if turnID == "" || limit <= 0 {
		return engine.InputClaim{}, errors.New("turn id and positive claim limit are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadInbox(runID)
	if err != nil {
		return engine.InputClaim{}, err
	}
	indexes := make([]int, 0, limit)
	for index, item := range state.Items {
		if item.ClaimID == "" && item.AckRevision == 0 {
			indexes = append(indexes, index)
			if len(indexes) == limit {
				break
			}
		}
	}
	if len(indexes) == 0 {
		return engine.InputClaim{}, nil
	}
	claim := engine.InputClaim{ID: newQueueID("claim"), RunID: runID, TurnID: turnID, ClaimedAt: time.Now().UTC()}
	for _, index := range indexes {
		state.Items[index].ClaimID = claim.ID
		state.Items[index].ClaimTurnID = turnID
		state.Items[index].ClaimedAt = claim.ClaimedAt
		claim.Items = append(claim.Items, state.Items[index].Input)
	}
	if err := s.writeAtomicUnlocked(s.inboxPath(runID), state); err != nil {
		return engine.InputClaim{}, err
	}
	return claim, nil
}

func (s *FileStore) Ack(_ context.Context, claimID string, revision int64) error {
	if claimID == "" || revision <= 0 {
		return errors.New("claim id and positive history revision are required")
	}
	return s.updateClaim(claimID, func(item *inboxItem) error {
		if item.AckRevision > 0 && item.AckRevision != revision {
			return fmt.Errorf("claim %q already acknowledged at revision %d", claimID, item.AckRevision)
		}
		item.AckRevision = revision
		item.AcknowledgedAt = time.Now().UTC()
		return nil
	})
}

func (s *FileStore) Release(_ context.Context, claimID string) error {
	if claimID == "" {
		return errors.New("claim id is required")
	}
	return s.updateClaim(claimID, func(item *inboxItem) error {
		if item.AckRevision == 0 {
			item.ClaimID = ""
			item.ClaimTurnID = ""
			item.ClaimedAt = time.Time{}
		}
		return nil
	})
}

func (s *FileStore) Reconcile(_ context.Context, runID string, committed map[string]int64) error {
	if err := validateID(runID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadInbox(runID)
	if err != nil {
		return err
	}
	changed := false
	for index := range state.Items {
		item := &state.Items[index]
		if item.ClaimID == "" || item.AckRevision > 0 {
			continue
		}
		if revision := committed[item.ClaimID]; revision > 0 {
			item.AckRevision = revision
			item.AcknowledgedAt = time.Now().UTC()
		} else {
			item.ClaimID = ""
			item.ClaimTurnID = ""
			item.ClaimedAt = time.Time{}
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return s.writeAtomicUnlocked(s.inboxPath(runID), state)
}

func (s *FileStore) InputStatus(_ context.Context, runID, inputID string) (engine.InputDeliveryStatus, error) {
	if err := validateID(runID); err != nil {
		return engine.InputDeliveryStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadInbox(runID)
	if err != nil {
		return engine.InputDeliveryStatus{}, err
	}
	for _, item := range state.Items {
		if item.Input.ID == inputID {
			return engine.InputDeliveryStatus{
				InputID: inputID, ClaimID: item.ClaimID, AckRevision: item.AckRevision, AcknowledgedAt: item.AcknowledgedAt,
			}, nil
		}
	}
	return engine.InputDeliveryStatus{}, os.ErrNotExist
}

func (s *FileStore) updateClaim(claimID string, update func(*inboxItem) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".inbox.json") {
			continue
		}
		path := filepath.Join(s.dir, name)
		var state inboxState
		if err := readJSON(path, &state); err != nil {
			return err
		}
		found := false
		for index := range state.Items {
			if state.Items[index].ClaimID == claimID {
				if err := update(&state.Items[index]); err != nil {
					return err
				}
				found = true
			}
		}
		if found {
			return s.writeAtomicUnlocked(path, state)
		}
	}
	return fmt.Errorf("input claim %q not found", claimID)
}

func (s *FileStore) loadInbox(runID string) (inboxState, error) {
	state := inboxState{RunID: runID}
	if err := readJSON(s.inboxPath(runID), &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inboxState{RunID: runID}, nil
		}
		return inboxState{}, err
	}
	if state.RunID != runID {
		return inboxState{}, errors.New("inbox run id mismatch")
	}
	return state, nil
}

func newQueueID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func (s *FileStore) inboxPath(runID string) string { return filepath.Join(s.dir, runID+".inbox.json") }
