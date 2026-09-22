package attention

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
	ErrConflict = errors.New("attention state conflict")
	ErrLease    = errors.New("invalid or expired inbox lease")
	ErrNoWork   = errors.New("agent inbox has no pending work")
	ErrStale    = errors.New("reply is stale; conversation changed after read_seq")
	safeID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

func New(dir string) (*Store, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(abs, "attention-v1.json"), now: func() time.Time { return time.Now().UTC() }}
	if _, err := os.Stat(store.path); errors.Is(err, os.ErrNotExist) {
		state := emptyState(store.now())
		if err := store.writeUnlocked(state); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := store.readUnlocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Publish(ctx context.Context, input PublishInput) (Message, []InboxItem, error) {
	input = normalizePublish(input)
	if err := validatePublish(input); err != nil {
		return Message{}, nil, err
	}
	fingerprint, err := semanticFingerprint("publish", input)
	if err != nil {
		return Message{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Message{}, nil, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Message{}, nil, ErrConflict
		}
		message, found := findMessage(state, input.ConversationID, input.MessageID)
		if !found {
			return Message{}, nil, errors.New("publish operation has no canonical message")
		}
		return message, deliveryItems(state, input.ConversationID, input.Deliveries), nil
	}
	if _, found := findMessageAny(state, input.MessageID); found {
		return Message{}, nil, fmt.Errorf("message id already exists: %w", ErrConflict)
	}
	now := s.now()
	message := appendMessage(&state, input.MessageID, input.ConversationID, input.AuthorID, input.Kind, input.Content, now)
	items := deliver(&state, message, input.Deliveries, now)
	appendOperation(&state, input.OperationID, "publish", fingerprint, "sent", message.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Message{}, nil, err
	}
	return message, items, nil
}

func (s *Store) Claim(ctx context.Context, agentID, operationID, leaseID string, ttl time.Duration) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	agentID, operationID, leaseID = strings.TrimSpace(agentID), strings.TrimSpace(operationID), strings.TrimSpace(leaseID)
	if !safeID.MatchString(agentID) || !safeID.MatchString(operationID) || ttl <= 0 || ttl > 24*time.Hour {
		return Claim{}, errors.New("agent, operation, and a ttl in (0,24h] are required")
	}
	if leaseID == "" {
		leaseID = newID("inbox-lease")
	}
	if !safeID.MatchString(leaseID) {
		return Claim{}, errors.New("invalid lease id")
	}
	semantic := struct {
		Agent string `json:"agent"`
		Lease string `json:"lease"`
		TTL   int64  `json:"ttl_ms"`
	}{agentID, leaseID, ttl.Milliseconds()}
	fingerprint, _ := semanticFingerprint("claim", semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Claim{}, err
	}
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return Claim{}, ErrConflict
		}
		return claimResult(state, operation.EntityID)
	}
	now := s.now()
	for id, item := range state.Inbox {
		if item.AgentID != agentID || item.State != Claimed {
			continue
		}
		if now.Before(item.LeaseExpiresAt) {
			return Claim{}, ErrLease
		}
		item.State, item.LeaseID, item.LeaseExpiresAt, item.ClaimReadSeq = Pending, "", time.Time{}, 0
		item.UpdatedAt = now
		state.Inbox[id] = item
	}
	candidates := make([]InboxItem, 0)
	for _, item := range state.Inbox {
		if item.AgentID == agentID && item.State == Pending {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return Claim{}, ErrNoWork
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := priorityRank(candidates[i].Priority), priorityRank(candidates[j].Priority)
		if left != right {
			return left < right
		}
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})
	item := candidates[0]
	conversation := state.Conversations[item.ConversationID]
	item.State, item.LeaseID, item.LeaseExpiresAt = Claimed, leaseID, now.Add(ttl)
	item.ClaimReadSeq, item.UpdatedAt = conversation.LastSeq, now
	state.Inbox[item.ID] = item
	state.ReadCursors[cursorKey(agentID, item.ConversationID)] = item.ClaimReadSeq
	appendOperation(&state, operationID, "claim", fingerprint, "claimed", item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Claim{}, err
	}
	return claimResult(state, item.ID)
}

func (s *Store) Ack(ctx context.Context, itemID, operationID, agentID, leaseID string) (InboxItem, error) {
	semantic := struct {
		Item  string `json:"item"`
		Agent string `json:"agent"`
		Lease string `json:"lease"`
	}{itemID, agentID, leaseID}
	return s.mutateItem(ctx, operationID, "ack", semantic, itemID, func(state *State, item *InboxItem, now time.Time) error {
		if !validLease(*item, agentID, leaseID, now) {
			return ErrLease
		}
		item.State, item.LeaseID, item.LeaseExpiresAt = Done, "", time.Time{}
		return nil
	})
}

func (s *Store) Refresh(ctx context.Context, itemID, operationID, agentID, leaseID string) (Claim, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, err
	}
	itemID, operationID, agentID, leaseID = strings.TrimSpace(itemID), strings.TrimSpace(operationID), strings.TrimSpace(agentID), strings.TrimSpace(leaseID)
	if !safeID.MatchString(itemID) || !safeID.MatchString(operationID) || !safeID.MatchString(agentID) || !safeID.MatchString(leaseID) {
		return Claim{}, errors.New("item, operation, agent, and lease ids are required")
	}
	semantic := struct {
		Item  string `json:"item"`
		Agent string `json:"agent"`
		Lease string `json:"lease"`
	}{itemID, agentID, leaseID}
	fingerprint, _ := semanticFingerprint("refresh", semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Claim{}, err
	}
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return Claim{}, ErrConflict
		}
		return claimResult(state, operation.EntityID)
	}
	item, found := state.Inbox[itemID]
	now := s.now()
	if !found || !validLease(item, agentID, leaseID, now) {
		return Claim{}, ErrLease
	}
	previous := item.ClaimReadSeq
	conversation := state.Conversations[item.ConversationID]
	item.ClaimReadSeq, item.UpdatedAt = conversation.LastSeq, now
	state.Inbox[item.ID] = item
	state.ReadCursors[cursorKey(agentID, item.ConversationID)] = item.ClaimReadSeq
	appendOperation(&state, operationID, "refresh", fingerprint, "refreshed", item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Claim{}, err
	}
	return Claim{Item: item, Messages: messagesRange(conversation, previous+1, item.ClaimReadSeq)}, nil
}

func (s *Store) SetWorkMark(ctx context.Context, input WorkMarkInput) (WorkMark, error) {
	input = normalizeWorkMark(input)
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.AgentID) || !safeID.MatchString(input.ConversationID) || input.TaskID != "" && !safeID.MatchString(input.TaskID) || input.Reason == "" {
		return WorkMark{}, errors.New("operation, agent, conversation, and reason are required")
	}
	fingerprint, _ := semanticFingerprint("set_work_mark", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return WorkMark{}, err
	}
	key := markKey(input.AgentID, input.ConversationID)
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return WorkMark{}, ErrConflict
		}
		return state.WorkMarks[key], nil
	}
	now := s.now()
	mark := state.WorkMarks[key]
	mark.AgentID, mark.ConversationID, mark.TaskID, mark.Reason, mark.Active = input.AgentID, input.ConversationID, input.TaskID, input.Reason, true
	mark.Revision++
	mark.UpdatedAt = now
	state.WorkMarks[key] = mark
	appendOperation(&state, input.OperationID, "set_work_mark", fingerprint, "active", key, now)
	if err := s.writeUnlocked(state); err != nil {
		return WorkMark{}, err
	}
	return mark, nil
}

func (s *Store) ClearWorkMark(ctx context.Context, agentID, conversationID, operationID string) (WorkMark, error) {
	input := struct {
		Agent        string `json:"agent"`
		Conversation string `json:"conversation"`
	}{strings.TrimSpace(agentID), strings.TrimSpace(conversationID)}
	if !safeID.MatchString(input.Agent) || !safeID.MatchString(input.Conversation) || !safeID.MatchString(operationID) {
		return WorkMark{}, errors.New("operation, agent, and conversation are required")
	}
	fingerprint, _ := semanticFingerprint("clear_work_mark", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return WorkMark{}, err
	}
	key := markKey(input.Agent, input.Conversation)
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return WorkMark{}, ErrConflict
		}
		return state.WorkMarks[key], nil
	}
	mark, found := state.WorkMarks[key]
	if !found || !mark.Active {
		return WorkMark{}, errors.New("active work mark not found")
	}
	now := s.now()
	mark.Active, mark.Revision, mark.UpdatedAt = false, mark.Revision+1, now
	state.WorkMarks[key] = mark
	appendOperation(&state, operationID, "clear_work_mark", fingerprint, "cleared", key, now)
	if err := s.writeUnlocked(state); err != nil {
		return WorkMark{}, err
	}
	return mark, nil
}

func (s *Store) SendFresh(ctx context.Context, input FreshReplyInput) (FreshReplyResult, error) {
	input = normalizeFreshReply(input)
	if err := validateFreshReply(input); err != nil {
		return FreshReplyResult{}, err
	}
	fingerprint, _ := semanticFingerprint("send_fresh", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return FreshReplyResult{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return FreshReplyResult{}, ErrConflict
		}
		return replayFreshResult(state, operation)
	}
	item, found := state.Inbox[input.ItemID]
	now := s.now()
	if !found || !validLease(item, input.AgentID, input.LeaseID, now) {
		return FreshReplyResult{}, ErrLease
	}
	conversation := state.Conversations[item.ConversationID]
	if input.ReadSeq != item.ClaimReadSeq || conversation.LastSeq != input.ReadSeq {
		draft := Draft{OperationID: input.OperationID, AgentID: input.AgentID, ConversationID: item.ConversationID, ItemID: item.ID, Content: input.Content, BaseReadSeq: input.ReadSeq, LatestSeq: conversation.LastSeq, UpdatedAt: now}
		state.Drafts[input.OperationID] = draft
		appendOperation(&state, input.OperationID, "send_fresh", fingerprint, "stale", item.ID, now)
		if err := s.writeUnlocked(state); err != nil {
			return FreshReplyResult{}, err
		}
		result := FreshReplyResult{Draft: &draft, Missed: messagesAfter(conversation, input.ReadSeq)}
		return result, ErrStale
	}
	if _, exists := findMessageAny(state, input.MessageID); exists {
		return FreshReplyResult{}, fmt.Errorf("message id already exists: %w", ErrConflict)
	}
	message := appendMessage(&state, input.MessageID, item.ConversationID, input.AgentID, "agent", input.Content, now)
	deliver(&state, message, input.Deliveries, now)
	item.State, item.LeaseID, item.LeaseExpiresAt, item.UpdatedAt = Done, "", time.Time{}, now
	state.Inbox[item.ID] = item
	appendOperation(&state, input.OperationID, "send_fresh", fingerprint, "sent", message.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return FreshReplyResult{}, err
	}
	return FreshReplyResult{Sent: &message}, nil
}

func (s *Store) GetState(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readUnlocked()
}

func (s *Store) mutateItem(ctx context.Context, operationID, action string, semantic any, itemID string, update func(*State, *InboxItem, time.Time) error) (InboxItem, error) {
	if err := ctx.Err(); err != nil {
		return InboxItem{}, err
	}
	if !safeID.MatchString(operationID) || !safeID.MatchString(itemID) {
		return InboxItem{}, errors.New("operation and item ids are required")
	}
	fingerprint, _ := semanticFingerprint(action, semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return InboxItem{}, err
	}
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return InboxItem{}, ErrConflict
		}
		return state.Inbox[operation.EntityID], nil
	}
	item, found := state.Inbox[itemID]
	if !found {
		return InboxItem{}, os.ErrNotExist
	}
	now := s.now()
	if err := update(&state, &item, now); err != nil {
		return InboxItem{}, err
	}
	item.UpdatedAt = now
	state.Inbox[item.ID] = item
	appendOperation(&state, operationID, action, fingerprint, "committed", item.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return InboxItem{}, err
	}
	return item, nil
}

func emptyState(now time.Time) State {
	return State{SchemaVersion: SchemaVersion, Conversations: map[string]Conversation{}, Inbox: map[string]InboxItem{}, ReadCursors: map[string]int64{}, WorkMarks: map[string]WorkMark{}, Drafts: map[string]Draft{}, UpdatedAt: now}
}

func appendMessage(state *State, id, conversationID, authorID, kind, content string, now time.Time) Message {
	conversation := state.Conversations[conversationID]
	conversation.ID = conversationID
	conversation.LastSeq++
	message := Message{ID: id, ConversationID: conversationID, Seq: conversation.LastSeq, AuthorID: authorID, Kind: kind, Content: content, CreatedAt: now}
	conversation.Messages = append(conversation.Messages, message)
	state.Conversations[conversationID] = conversation
	return message
}

func deliver(state *State, message Message, deliveries []Delivery, now time.Time) []InboxItem {
	items := make([]InboxItem, 0, len(deliveries))
	for _, delivery := range deliveries {
		var current *InboxItem
		for id, candidate := range state.Inbox {
			if candidate.AgentID == delivery.AgentID && candidate.ConversationID == message.ConversationID && candidate.State != Done {
				copy := candidate
				copy.ID = id
				current = &copy
				break
			}
		}
		if current == nil {
			item := InboxItem{ID: newID("inbox"), AgentID: delivery.AgentID, ConversationID: message.ConversationID, FromSeq: message.Seq, ToSeq: message.Seq, Priority: delivery.Priority, State: Pending, CreatedAt: now, UpdatedAt: now}
			state.Inbox[item.ID] = item
			items = append(items, item)
			continue
		}
		current.ToSeq = message.Seq
		if priorityRank(delivery.Priority) < priorityRank(current.Priority) {
			current.Priority = delivery.Priority
		}
		current.UpdatedAt = now
		state.Inbox[current.ID] = *current
		items = append(items, *current)
	}
	return items
}

func appendOperation(state *State, id, action, fingerprint, outcome, entityID string, now time.Time) {
	state.Revision++
	state.UpdatedAt = now
	state.Operations = append(state.Operations, Operation{ID: id, Action: action, Fingerprint: fingerprint, Outcome: outcome, EntityID: entityID, Revision: state.Revision, CreatedAt: now})
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
		return State{}, errors.New("attention state has trailing data")
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
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".attention-*.tmp")
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
	err = errors.Join(encoder.Encode(state), temporary.Sync(), temporary.Close())
	if err != nil {
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
	if verified.Revision != state.Revision || len(verified.Operations) != len(state.Operations) {
		return errors.New("attention state read-back verification failed")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != SchemaVersion || state.Revision < 0 || state.Conversations == nil || state.Inbox == nil || state.ReadCursors == nil || state.WorkMarks == nil || state.Drafts == nil {
		return errors.New("invalid canonical attention state")
	}
	seenMessages := map[string]bool{}
	for id, conversation := range state.Conversations {
		if !safeID.MatchString(id) || conversation.ID != id || int64(len(conversation.Messages)) != conversation.LastSeq {
			return errors.New("invalid conversation sequence")
		}
		for index, message := range conversation.Messages {
			if message.Seq != int64(index+1) || message.ConversationID != id || !safeID.MatchString(message.ID) || !safeID.MatchString(message.AuthorID) || seenMessages[message.ID] || message.Kind == "" || message.Content == "" || message.CreatedAt.IsZero() {
				return errors.New("invalid canonical message")
			}
			seenMessages[message.ID] = true
		}
	}
	openItems := map[string]bool{}
	for id, item := range state.Inbox {
		conversation, found := state.Conversations[item.ConversationID]
		if id != item.ID || !safeID.MatchString(id) || !safeID.MatchString(item.AgentID) || !found || item.FromSeq <= 0 || item.ToSeq < item.FromSeq || item.ToSeq > conversation.LastSeq || !validPriority(item.Priority) || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			return errors.New("invalid canonical inbox item")
		}
		if item.State != Pending && item.State != Claimed && item.State != Done {
			return errors.New("invalid inbox state")
		}
		if item.State == Claimed {
			if !safeID.MatchString(item.LeaseID) || item.LeaseExpiresAt.IsZero() || item.ClaimReadSeq < item.FromSeq || item.ClaimReadSeq > conversation.LastSeq {
				return errors.New("invalid claimed inbox item")
			}
		} else if item.LeaseID != "" || !item.LeaseExpiresAt.IsZero() {
			return errors.New("non-claimed inbox item carries a lease")
		}
		key := cursorKey(item.AgentID, item.ConversationID)
		if item.State != Done && openItems[key] {
			return errors.New("agent has duplicate open inbox items for one conversation")
		}
		if item.State != Done {
			openItems[key] = true
		}
	}
	for key, mark := range state.WorkMarks {
		if key != markKey(mark.AgentID, mark.ConversationID) || !safeID.MatchString(mark.AgentID) || !safeID.MatchString(mark.ConversationID) || mark.TaskID != "" && !safeID.MatchString(mark.TaskID) || mark.Reason == "" || mark.Revision <= 0 || mark.UpdatedAt.IsZero() {
			return errors.New("invalid persistent work mark")
		}
	}
	for id, draft := range state.Drafts {
		conversation, found := state.Conversations[draft.ConversationID]
		if id != draft.OperationID || !safeID.MatchString(id) || !safeID.MatchString(draft.AgentID) || !safeID.MatchString(draft.ItemID) || !found || draft.Content == "" || draft.BaseReadSeq <= 0 || draft.LatestSeq <= draft.BaseReadSeq || draft.LatestSeq > conversation.LastSeq || draft.UpdatedAt.IsZero() {
			return errors.New("invalid stale draft")
		}
	}
	seenOperations := map[string]bool{}
	for index, operation := range state.Operations {
		if !safeID.MatchString(operation.ID) || seenOperations[operation.ID] || !hexDigest.MatchString(operation.Fingerprint) || operation.Revision != int64(index+1) || operation.CreatedAt.IsZero() {
			return errors.New("invalid attention operation ledger")
		}
		seenOperations[operation.ID] = true
	}
	if int64(len(state.Operations)) != state.Revision {
		return errors.New("attention operation ledger does not reach current revision")
	}
	return nil
}

func normalizePublish(input PublishInput) PublishInput {
	input.OperationID, input.MessageID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.MessageID)
	input.ConversationID, input.AuthorID = strings.TrimSpace(input.ConversationID), strings.TrimSpace(input.AuthorID)
	input.Kind, input.Content = strings.TrimSpace(input.Kind), strings.TrimSpace(input.Content)
	for index := range input.Deliveries {
		input.Deliveries[index].AgentID = strings.TrimSpace(input.Deliveries[index].AgentID)
	}
	sort.Slice(input.Deliveries, func(i, j int) bool { return input.Deliveries[i].AgentID < input.Deliveries[j].AgentID })
	return input
}

func validatePublish(input PublishInput) error {
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.MessageID) || !safeID.MatchString(input.ConversationID) || !safeID.MatchString(input.AuthorID) || input.Kind == "" || input.Content == "" || len(input.Content) > 32768 {
		return errors.New("operation, message, conversation, author, kind, and bounded content are required")
	}
	seen := map[string]bool{}
	for _, delivery := range input.Deliveries {
		if !safeID.MatchString(delivery.AgentID) || !validPriority(delivery.Priority) || seen[delivery.AgentID] {
			return errors.New("deliveries require unique agents and valid priorities")
		}
		seen[delivery.AgentID] = true
	}
	return nil
}

func normalizeWorkMark(input WorkMarkInput) WorkMarkInput {
	input.OperationID, input.AgentID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.AgentID)
	input.ConversationID, input.TaskID, input.Reason = strings.TrimSpace(input.ConversationID), strings.TrimSpace(input.TaskID), strings.TrimSpace(input.Reason)
	return input
}

func normalizeFreshReply(input FreshReplyInput) FreshReplyInput {
	input.OperationID, input.MessageID, input.AgentID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.MessageID), strings.TrimSpace(input.AgentID)
	input.ItemID, input.LeaseID, input.Content = strings.TrimSpace(input.ItemID), strings.TrimSpace(input.LeaseID), strings.TrimSpace(input.Content)
	for index := range input.Deliveries {
		input.Deliveries[index].AgentID = strings.TrimSpace(input.Deliveries[index].AgentID)
	}
	sort.Slice(input.Deliveries, func(i, j int) bool { return input.Deliveries[i].AgentID < input.Deliveries[j].AgentID })
	return input
}

func validateFreshReply(input FreshReplyInput) error {
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.MessageID) || !safeID.MatchString(input.AgentID) || !safeID.MatchString(input.ItemID) || !safeID.MatchString(input.LeaseID) || input.ReadSeq <= 0 || input.Content == "" || len(input.Content) > 32768 {
		return errors.New("operation, message, agent, claimed item, lease, positive read_seq, and bounded content are required")
	}
	seen := map[string]bool{}
	for _, delivery := range input.Deliveries {
		if !safeID.MatchString(delivery.AgentID) || delivery.AgentID == input.AgentID || !validPriority(delivery.Priority) || seen[delivery.AgentID] {
			return errors.New("deliveries require unique agents and valid priorities")
		}
		seen[delivery.AgentID] = true
	}
	return nil
}

func replayFreshResult(state State, operation Operation) (FreshReplyResult, error) {
	if operation.Outcome == "sent" {
		message, found := findMessageAny(state, operation.EntityID)
		if !found {
			return FreshReplyResult{}, errors.New("fresh send operation has no message")
		}
		return FreshReplyResult{Sent: &message}, nil
	}
	draft, found := state.Drafts[operation.ID]
	if !found {
		return FreshReplyResult{}, errors.New("stale send operation has no draft")
	}
	return FreshReplyResult{Draft: &draft, Missed: messagesAfter(state.Conversations[draft.ConversationID], draft.BaseReadSeq)}, ErrStale
}

func claimResult(state State, itemID string) (Claim, error) {
	item, found := state.Inbox[itemID]
	if !found {
		return Claim{}, errors.New("claim operation has no inbox item")
	}
	conversation := state.Conversations[item.ConversationID]
	return Claim{Item: item, Messages: messagesRange(conversation, item.FromSeq, item.ClaimReadSeq)}, nil
}

func deliveryItems(state State, conversationID string, deliveries []Delivery) []InboxItem {
	result := make([]InboxItem, 0, len(deliveries))
	for _, delivery := range deliveries {
		for _, item := range state.Inbox {
			if item.AgentID == delivery.AgentID && item.ConversationID == conversationID && item.State != Done {
				result = append(result, item)
				break
			}
		}
	}
	return result
}

func messagesRange(conversation Conversation, from, to int64) []Message {
	result := make([]Message, 0)
	for _, message := range conversation.Messages {
		if message.Seq >= from && message.Seq <= to {
			result = append(result, message)
		}
	}
	return result
}

func messagesAfter(conversation Conversation, seq int64) []Message {
	return messagesRange(conversation, seq+1, conversation.LastSeq)
}

func findOperation(state State, id string) (Operation, bool) {
	for _, operation := range state.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return Operation{}, false
}

func findMessage(state State, conversationID, messageID string) (Message, bool) {
	for _, message := range state.Conversations[conversationID].Messages {
		if message.ID == messageID {
			return message, true
		}
	}
	return Message{}, false
}

func findMessageAny(state State, messageID string) (Message, bool) {
	for _, conversation := range state.Conversations {
		for _, message := range conversation.Messages {
			if message.ID == messageID {
				return message, true
			}
		}
	}
	return Message{}, false
}

func validLease(item InboxItem, agentID, leaseID string, now time.Time) bool {
	return item.State == Claimed && item.AgentID == agentID && item.LeaseID == leaseID && now.Before(item.LeaseExpiresAt)
}

func validPriority(priority Priority) bool {
	return priority == PriorityCorrection || priority == PriorityDirect || priority == PriorityReview || priority == PrioritySubscription
}

func priorityRank(priority Priority) int {
	switch priority {
	case PriorityCorrection:
		return 0
	case PriorityDirect:
		return 1
	case PriorityReview:
		return 2
	default:
		return 3
	}
}

func cursorKey(agentID, conversationID string) string { return agentID + "::" + conversationID }
func markKey(agentID, conversationID string) string   { return agentID + "::" + conversationID }

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

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
