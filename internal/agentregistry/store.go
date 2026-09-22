package agentregistry

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
)

var (
	ErrConflict = errors.New("agent registry revision conflict")
	ErrSession  = errors.New("invalid session generation transition")
	safeID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest   = regexp.MustCompile(`^[a-f0-9]{64}$`)
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
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: abs, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Agent, error) {
	if err := ctx.Err(); err != nil {
		return Agent{}, err
	}
	input = normalizeCreate(input)
	if err := validateCreate(input); err != nil {
		return Agent{}, err
	}
	fingerprint, _ := semanticFingerprint("create", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.readUnlocked(input.ID); err == nil {
		if operationMatches(existing.Operations, input.OperationID, fingerprint) {
			return existing, nil
		}
		return Agent{}, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return Agent{}, err
	}
	now := s.now()
	agentRevision := AgentRevision{ID: revisionID("agent", 1), Version: 1, PromptRef: input.PromptRef, Skills: append([]string(nil), input.Skills...), Model: input.Model, CreatedBy: input.CreatedBy, Reason: "initial", CreatedAt: now}
	relationshipRevision := RelationshipRevision{ID: revisionID("relationship", 1), Version: 1, MyRole: input.MyRole, Teammates: cloneTeammates(input.Teammates), CreatedBy: input.CreatedBy, Reason: "initial", CreatedAt: now}
	agent := Agent{
		SchemaVersion: SchemaVersion, ID: input.ID, WorkspaceID: input.WorkspaceID, Name: input.Name, Revision: 1,
		CurrentAgentRevisionID: agentRevision.ID, CurrentRelationshipRevision: relationshipRevision.ID, CurrentSessionGeneration: 1,
		AgentRevisions: []AgentRevision{agentRevision}, RelationshipRevisions: []RelationshipRevision{relationshipRevision},
		Sessions:   []SessionGeneration{{Generation: 1, SessionID: input.SessionID, State: SessionActive, CreatedAt: now}},
		Operations: []Operation{{ID: input.OperationID, Action: "create", Fingerprint: fingerprint, Revision: 1, CreatedAt: now}},
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := s.writeUnlocked(agent); err != nil {
		return Agent{}, err
	}
	return cloneAgent(agent), nil
}

func (s *Store) Get(ctx context.Context, id string) (Agent, error) {
	if err := ctx.Err(); err != nil {
		return Agent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, err := s.readUnlocked(strings.TrimSpace(id))
	return cloneAgent(agent), err
}

func (s *Store) List(ctx context.Context, workspaceID string) ([]Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var result []Agent
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".agent.json") {
			continue
		}
		agent, err := s.readUnlocked(strings.TrimSuffix(entry.Name(), ".agent.json"))
		if err != nil {
			return nil, err
		}
		if workspaceID == "" || agent.WorkspaceID == workspaceID {
			result = append(result, agent)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func (s *Store) ReviseRelationships(ctx context.Context, agentID string, input RelationshipUpdate) (Agent, error) {
	input = normalizeRelationshipUpdate(input)
	semantic := input
	return s.mutate(ctx, agentID, input.OperationID, "revise_relationships", input.ExpectedRevision, semantic, func(agent *Agent, now time.Time) error {
		if !safeID.MatchString(input.ActorID) || input.MyRole == "" || input.Reason == "" {
			return errors.New("actor, role, and revision reason are required")
		}
		if err := validateTeammates(agent.ID, input.Teammates); err != nil {
			return err
		}
		version := int64(len(agent.RelationshipRevisions) + 1)
		revision := RelationshipRevision{ID: revisionID("relationship", version), Version: version, MyRole: input.MyRole, Teammates: cloneTeammates(input.Teammates), CreatedBy: input.ActorID, Reason: input.Reason, CreatedAt: now}
		agent.RelationshipRevisions = append(agent.RelationshipRevisions, revision)
		agent.CurrentRelationshipRevision = revision.ID
		return nil
	})
}

func (s *Store) RotateSession(ctx context.Context, agentID string, input SessionRotation) (Agent, error) {
	input = normalizeSessionRotation(input)
	return s.mutate(ctx, agentID, input.OperationID, "rotate_session", input.ExpectedRevision, input, func(agent *Agent, now time.Time) error {
		if !safeID.MatchString(input.ActorID) || !safeID.MatchString(input.NewSessionID) || input.ExpectedGeneration != agent.CurrentSessionGeneration {
			return ErrSession
		}
		if err := validateHandoff(input.Handoff); err != nil {
			return err
		}
		for _, generation := range agent.Sessions {
			if generation.SessionID == input.NewSessionID {
				return errors.New("new session id was already used")
			}
		}
		index := len(agent.Sessions) - 1
		if index < 0 || agent.Sessions[index].Generation != input.ExpectedGeneration || agent.Sessions[index].State != SessionActive {
			return ErrSession
		}
		handoff := cloneHandoff(input.Handoff)
		agent.Sessions[index].State, agent.Sessions[index].RetiredAt, agent.Sessions[index].Handoff = SessionRetired, now, &handoff
		next := input.ExpectedGeneration + 1
		agent.Sessions = append(agent.Sessions, SessionGeneration{Generation: next, SessionID: input.NewSessionID, State: SessionActive, CreatedAt: now})
		agent.CurrentSessionGeneration = next
		return nil
	})
}

func (s *Store) mutate(ctx context.Context, agentID, operationID, action string, expectedRevision int64, semantic any, update func(*Agent, time.Time) error) (Agent, error) {
	if err := ctx.Err(); err != nil {
		return Agent{}, err
	}
	agentID, operationID = strings.TrimSpace(agentID), strings.TrimSpace(operationID)
	if !safeID.MatchString(agentID) || !safeID.MatchString(operationID) || expectedRevision <= 0 {
		return Agent{}, errors.New("agent, operation, and positive expected revision are required")
	}
	fingerprint, _ := semanticFingerprint(action, semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, err := s.readUnlocked(agentID)
	if err != nil {
		return Agent{}, err
	}
	if operationMatches(agent.Operations, operationID, fingerprint) {
		return cloneAgent(agent), nil
	}
	for _, operation := range agent.Operations {
		if operation.ID == operationID {
			return Agent{}, ErrConflict
		}
	}
	if agent.Revision != expectedRevision {
		return Agent{}, ErrConflict
	}
	now := s.now()
	if err := update(&agent, now); err != nil {
		return Agent{}, err
	}
	agent.Revision++
	agent.UpdatedAt = now
	agent.Operations = append(agent.Operations, Operation{ID: operationID, Action: action, Fingerprint: fingerprint, Revision: agent.Revision, CreatedAt: now})
	if err := s.writeUnlocked(agent); err != nil {
		return Agent{}, err
	}
	return cloneAgent(agent), nil
}

func (s *Store) readUnlocked(id string) (Agent, error) {
	if !safeID.MatchString(id) {
		return Agent{}, errors.New("invalid agent id")
	}
	file, err := os.Open(filepath.Join(s.dir, id+".agent.json"))
	if err != nil {
		return Agent{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var agent Agent
	if err := decoder.Decode(&agent); err != nil {
		return Agent{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Agent{}, errors.New("agent record has trailing data")
	}
	if err := validateAgent(agent); err != nil {
		return Agent{}, err
	}
	return agent, nil
}

func (s *Store) writeUnlocked(agent Agent) error {
	if err := validateAgent(agent); err != nil {
		return err
	}
	path := filepath.Join(s.dir, agent.ID+".agent.json")
	temporary, err := os.CreateTemp(s.dir, ".agent-*.tmp")
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
	if err := errors.Join(encoder.Encode(agent), temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	verified, err := s.readUnlocked(agent.ID)
	if err != nil {
		return err
	}
	if verified.Revision != agent.Revision || verified.CurrentSessionGeneration != agent.CurrentSessionGeneration {
		return errors.New("agent read-back verification failed")
	}
	return nil
}

func validateAgent(agent Agent) error {
	if agent.SchemaVersion != SchemaVersion || !safeID.MatchString(agent.ID) || !safeID.MatchString(agent.WorkspaceID) || agent.Name == "" || agent.Revision <= 0 || agent.CreatedAt.IsZero() || agent.UpdatedAt.IsZero() {
		return errors.New("invalid canonical agent identity")
	}
	if len(agent.AgentRevisions) == 0 || len(agent.RelationshipRevisions) == 0 || len(agent.Sessions) == 0 || len(agent.Operations) == 0 {
		return errors.New("agent identity is missing revision or session history")
	}
	for index, revision := range agent.AgentRevisions {
		if revision.Version != int64(index+1) || revision.ID != revisionID("agent", revision.Version) || revision.PromptRef == "" || revision.Model == "" || !safeID.MatchString(revision.CreatedBy) || revision.CreatedAt.IsZero() {
			return errors.New("invalid agent revision history")
		}
	}
	if agent.CurrentAgentRevisionID != agent.AgentRevisions[len(agent.AgentRevisions)-1].ID {
		return errors.New("current agent revision is not latest")
	}
	for index, revision := range agent.RelationshipRevisions {
		if revision.Version != int64(index+1) || revision.ID != revisionID("relationship", revision.Version) || revision.MyRole == "" || !safeID.MatchString(revision.CreatedBy) || revision.Reason == "" || revision.CreatedAt.IsZero() || validateTeammates(agent.ID, revision.Teammates) != nil {
			return errors.New("invalid relationship revision history")
		}
	}
	if agent.CurrentRelationshipRevision != agent.RelationshipRevisions[len(agent.RelationshipRevisions)-1].ID {
		return errors.New("current relationship revision is not latest")
	}
	active := 0
	seenSessions := map[string]bool{}
	for index, generation := range agent.Sessions {
		if generation.Generation != int64(index+1) || !safeID.MatchString(generation.SessionID) || seenSessions[generation.SessionID] || generation.CreatedAt.IsZero() {
			return errors.New("invalid session generation history")
		}
		seenSessions[generation.SessionID] = true
		if generation.State == SessionActive {
			active++
			if generation.Handoff != nil || !generation.RetiredAt.IsZero() {
				return errors.New("active session carries retirement data")
			}
		} else if generation.State != SessionRetired || generation.Handoff == nil || generation.RetiredAt.IsZero() || validateHandoff(*generation.Handoff) != nil {
			return errors.New("invalid retired session")
		}
	}
	if active != 1 || agent.CurrentSessionGeneration != agent.Sessions[len(agent.Sessions)-1].Generation || agent.Sessions[len(agent.Sessions)-1].State != SessionActive {
		return errors.New("agent must have exactly one current active session")
	}
	seenOperations := map[string]bool{}
	for index, operation := range agent.Operations {
		if operation.Revision != int64(index+1) || !safeID.MatchString(operation.ID) || seenOperations[operation.ID] || !hexDigest.MatchString(operation.Fingerprint) || operation.CreatedAt.IsZero() {
			return errors.New("invalid agent operation ledger")
		}
		seenOperations[operation.ID] = true
	}
	if int64(len(agent.Operations)) != agent.Revision {
		return errors.New("agent operation ledger does not reach revision")
	}
	return nil
}

func validateCreate(input CreateInput) error {
	for _, id := range []string{input.OperationID, input.ID, input.WorkspaceID, input.CreatedBy, input.SessionID} {
		if !safeID.MatchString(id) {
			return errors.New("operation, agent, workspace, creator, and session ids are required")
		}
	}
	if input.Name == "" || input.PromptRef == "" || input.Model == "" || input.MyRole == "" {
		return errors.New("name, prompt, model, and role are required")
	}
	return validateTeammates(input.ID, input.Teammates)
}

func validateTeammates(self string, teammates []TeammateRelationship) error {
	seen := map[string]bool{}
	for _, teammate := range teammates {
		if !safeID.MatchString(teammate.AgentID) || teammate.AgentID == self || seen[teammate.AgentID] || teammate.Role == "" {
			return errors.New("teammates require unique non-self agent ids and roles")
		}
		seen[teammate.AgentID] = true
	}
	return nil
}

func validateHandoff(handoff SessionHandoff) error {
	if handoff.Scope == "" || len(handoff.References) == 0 || len(handoff.ConfirmedFacts) == 0 {
		return errors.New("session handoff requires scope, references, and confirmed facts")
	}
	return nil
}

func normalizeCreate(input CreateInput) CreateInput {
	input.OperationID, input.ID, input.WorkspaceID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ID), strings.TrimSpace(input.WorkspaceID)
	input.Name, input.CreatedBy, input.PromptRef = strings.TrimSpace(input.Name), strings.TrimSpace(input.CreatedBy), strings.TrimSpace(input.PromptRef)
	input.Model, input.MyRole, input.SessionID = strings.TrimSpace(input.Model), strings.TrimSpace(input.MyRole), strings.TrimSpace(input.SessionID)
	input.Skills = normalizeStrings(input.Skills)
	input.Teammates = normalizeTeammates(input.Teammates)
	return input
}

func normalizeRelationshipUpdate(input RelationshipUpdate) RelationshipUpdate {
	input.OperationID, input.ActorID, input.MyRole, input.Reason = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ActorID), strings.TrimSpace(input.MyRole), strings.TrimSpace(input.Reason)
	input.Teammates = normalizeTeammates(input.Teammates)
	return input
}

func normalizeSessionRotation(input SessionRotation) SessionRotation {
	input.OperationID, input.ActorID, input.NewSessionID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ActorID), strings.TrimSpace(input.NewSessionID)
	input.Handoff = cloneHandoff(input.Handoff)
	input.Handoff.Scope = strings.TrimSpace(input.Handoff.Scope)
	input.Handoff.CurrentTask = strings.TrimSpace(input.Handoff.CurrentTask)
	return input
}

func normalizeTeammates(items []TeammateRelationship) []TeammateRelationship {
	result := make([]TeammateRelationship, len(items))
	for index, item := range items {
		item.AgentID, item.Role = strings.TrimSpace(item.AgentID), strings.TrimSpace(item.Role)
		item.DelegateWhen, item.CollaborateWhen = normalizeStrings(item.DelegateWhen), normalizeStrings(item.CollaborateWhen)
		item.Inputs, item.ReportBackWith = normalizeStrings(item.Inputs), normalizeStrings(item.ReportBackWith)
		result[index] = item
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AgentID < result[j].AgentID })
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

func cloneTeammates(items []TeammateRelationship) []TeammateRelationship {
	encoded, _ := json.Marshal(items)
	var result []TeammateRelationship
	_ = json.Unmarshal(encoded, &result)
	return result
}

func cloneHandoff(handoff SessionHandoff) SessionHandoff {
	handoff.OwnedTasks = normalizeStrings(handoff.OwnedTasks)
	handoff.PendingReviews = normalizeStrings(handoff.PendingReviews)
	handoff.AvailableTasks = normalizeStrings(handoff.AvailableTasks)
	handoff.References = normalizeStrings(handoff.References)
	handoff.ConfirmedFacts = normalizeStrings(handoff.ConfirmedFacts)
	handoff.UnresolvedItems = normalizeStrings(handoff.UnresolvedItems)
	return handoff
}

func cloneAgent(agent Agent) Agent {
	encoded, _ := json.Marshal(agent)
	var result Agent
	_ = json.Unmarshal(encoded, &result)
	return result
}

func revisionID(kind string, version int64) string { return fmt.Sprintf("%s-v%d", kind, version) }

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

func operationMatches(operations []Operation, id, fingerprint string) bool {
	for _, operation := range operations {
		if operation.ID == id {
			return operation.Fingerprint == fingerprint
		}
	}
	return false
}
