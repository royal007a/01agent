package team

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

	"github.com/royal007a/01agent/internal/agentregistry"
)

var (
	ErrConflict    = errors.New("team lockfile revision conflict")
	ErrNoSelection = errors.New("no candidate team satisfies capabilities and budget")
	safeID         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Store struct {
	path   string
	agents *agentregistry.Store
	mu     sync.Mutex
	now    func() time.Time
}

func New(dir string, agents *agentregistry.Store) (*Store, error) {
	if agents == nil {
		return nil, errors.New("agent registry is required")
	}
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(abs, "teams-v1.json"), agents: agents, now: func() time.Time { return time.Now().UTC() }}
	if _, err := os.Stat(store.path); errors.Is(err, os.ErrNotExist) {
		if err := store.writeUnlocked(State{SchemaVersion: SchemaVersion, Teams: map[string]Team{}, UpdatedAt: store.now()}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := store.readUnlocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Select(ctx context.Context, input SelectInput) (Lockfile, error) {
	if err := ctx.Err(); err != nil {
		return Lockfile{}, err
	}
	input = normalizeInput(input)
	if err := validateInput(input); err != nil {
		return Lockfile{}, err
	}
	fingerprint, _ := semanticFingerprint(input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Lockfile{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Lockfile{}, ErrConflict
		}
		return state.Teams[operation.TeamID].Lockfiles[operation.Version-1], nil
	}
	current, exists := state.Teams[input.TeamID]
	if !exists && input.ExpectedTeamVersion != 0 || exists && current.CurrentVersion != input.ExpectedTeamVersion || exists && current.WorkspaceID != input.WorkspaceID {
		return Lockfile{}, ErrConflict
	}
	candidates := make([]candidate, 0, len(input.CandidateAgentIDs))
	for _, id := range input.CandidateAgentIDs {
		agent, err := s.agents.Get(ctx, id)
		if err != nil {
			return Lockfile{}, fmt.Errorf("load candidate %s: %w", id, err)
		}
		if agent.WorkspaceID != input.WorkspaceID {
			continue
		}
		revision, found := currentAgentRevision(agent)
		if !found {
			return Lockfile{}, errors.New("candidate current revision is missing")
		}
		cost, found := input.ModelCostsUSD[revision.Model]
		if !found || cost < 0 {
			continue
		}
		candidates = append(candidates, candidate{agent: agent, revision: revision, cost: cost, delivery: input.DeliveryScores[id]})
	}
	members, score, cost, ok := choose(input.Template.Roles, candidates, input.BudgetUSD)
	if !ok {
		return Lockfile{}, ErrNoSelection
	}
	now := s.now()
	version := input.ExpectedTeamVersion + 1
	evidence := []string{fmt.Sprintf("selected %d roles with score %.3f within %.4f USD budget", len(members), score, input.BudgetUSD)}
	for _, member := range members {
		evidence = append(evidence, fmt.Sprintf("%s=%s agent_revision=%s relationship_revision=%s", member.Role, member.AgentID, member.AgentRevisionID, member.RelationshipRevisionID))
	}
	lockfile := Lockfile{SchemaVersion: SchemaVersion, TeamID: input.TeamID, Version: version, WorkspaceID: input.WorkspaceID, Objective: input.Objective, Template: input.Template, Members: members, TotalCostUSD: cost, Evidence: evidence, CreatedBy: input.CreatedBy, CreatedAt: now}
	if !exists {
		current = Team{ID: input.TeamID, WorkspaceID: input.WorkspaceID, CreatedAt: now}
	}
	current.CurrentVersion, current.UpdatedAt = version, now
	current.Lockfiles = append(current.Lockfiles, lockfile)
	state.Teams[current.ID] = current
	state.Revision++
	state.UpdatedAt = now
	state.Operations = append(state.Operations, Operation{ID: input.OperationID, Fingerprint: fingerprint, TeamID: current.ID, Version: version, Revision: state.Revision, CreatedAt: now})
	if err := s.writeUnlocked(state); err != nil {
		return Lockfile{}, err
	}
	return lockfile, nil
}

func (s *Store) Get(ctx context.Context, teamID string, version int64) (Lockfile, error) {
	if err := ctx.Err(); err != nil {
		return Lockfile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Lockfile{}, err
	}
	item, found := state.Teams[strings.TrimSpace(teamID)]
	if !found {
		return Lockfile{}, os.ErrNotExist
	}
	if version == 0 {
		version = item.CurrentVersion
	}
	if version <= 0 || version > int64(len(item.Lockfiles)) {
		return Lockfile{}, os.ErrNotExist
	}
	return item.Lockfiles[version-1], nil
}

type candidate struct {
	agent    agentregistry.Agent
	revision agentregistry.AgentRevision
	cost     float64
	delivery float64
}

func choose(roles []RoleRequirement, candidates []candidate, budget float64) ([]MemberLock, float64, float64, bool) {
	var best []MemberLock
	bestScore, bestCost, bestKey := -1.0, 0.0, ""
	used := map[string]bool{}
	var search func(int, []MemberLock, float64, float64)
	search = func(index int, members []MemberLock, score, cost float64) {
		if cost > budget {
			return
		}
		if index == len(roles) {
			keyParts := make([]string, len(members))
			for i, member := range members {
				keyParts[i] = member.AgentID
			}
			key := strings.Join(keyParts, "|")
			if score > bestScore || score == bestScore && (len(best) == 0 || cost < bestCost || cost == bestCost && key < bestKey) {
				best, bestScore, bestCost, bestKey = append([]MemberLock(nil), members...), score, cost, key
			}
			return
		}
		role := roles[index]
		for _, item := range candidates {
			if used[item.agent.ID] || !qualifies(role, item.revision) {
				continue
			}
			memberScore := item.delivery + float64(matches(role.PreferredSkills, item.revision.Skills))
			used[item.agent.ID] = true
			members = append(members, MemberLock{Role: role.Role, AgentID: item.agent.ID, AgentRevisionID: item.agent.CurrentAgentRevisionID, RelationshipRevisionID: item.agent.CurrentRelationshipRevision, Model: item.revision.Model, EstimatedCostUSD: item.cost, SelectionScore: memberScore})
			search(index+1, members, score+memberScore, cost+item.cost)
			members = members[:len(members)-1]
			delete(used, item.agent.ID)
		}
	}
	search(0, nil, 0, 0)
	return best, bestScore, bestCost, len(best) == len(roles)
}

func qualifies(role RoleRequirement, revision agentregistry.AgentRevision) bool {
	return containsAll(revision.Skills, role.RequiredSkills) && containsAll(revision.Tools, role.RequiredTools) && containsAll(revision.Permissions, role.RequiredPermissions)
}

func containsAll(values, required []string) bool {
	set := map[string]bool{}
	for _, value := range values {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func matches(left, right []string) int {
	set := map[string]bool{}
	for _, value := range right {
		set[value] = true
	}
	count := 0
	for _, value := range left {
		if set[value] {
			count++
		}
	}
	return count
}

func currentAgentRevision(agent agentregistry.Agent) (agentregistry.AgentRevision, bool) {
	for _, revision := range agent.AgentRevisions {
		if revision.ID == agent.CurrentAgentRevisionID {
			return revision, true
		}
	}
	return agentregistry.AgentRevision{}, false
}

func normalizeInput(input SelectInput) SelectInput {
	input.OperationID, input.TeamID, input.WorkspaceID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.TeamID), strings.TrimSpace(input.WorkspaceID)
	input.Objective, input.CreatedBy = strings.TrimSpace(input.Objective), strings.TrimSpace(input.CreatedBy)
	input.Template.ID, input.Template.Name = strings.TrimSpace(input.Template.ID), strings.TrimSpace(input.Template.Name)
	input.CandidateAgentIDs = normalizeStrings(input.CandidateAgentIDs)
	for index := range input.Template.Roles {
		role := &input.Template.Roles[index]
		role.Role = strings.TrimSpace(role.Role)
		role.RequiredSkills, role.PreferredSkills = normalizeStrings(role.RequiredSkills), normalizeStrings(role.PreferredSkills)
		role.RequiredTools, role.RequiredPermissions = normalizeStrings(role.RequiredTools), normalizeStrings(role.RequiredPermissions)
	}
	return input
}

func validateInput(input SelectInput) error {
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.TeamID) || !safeID.MatchString(input.WorkspaceID) || !safeID.MatchString(input.CreatedBy) || input.ExpectedTeamVersion < 0 || input.Objective == "" || !safeID.MatchString(input.Template.ID) || input.Template.Name == "" || len(input.Template.Roles) == 0 || len(input.CandidateAgentIDs) == 0 || input.BudgetUSD < 0 {
		return errors.New("valid selection identity, template, candidates, and budget are required")
	}
	seenRoles := map[string]bool{}
	for _, role := range input.Template.Roles {
		if role.Role == "" || seenRoles[role.Role] {
			return errors.New("template roles must be unique and non-empty")
		}
		seenRoles[role.Role] = true
	}
	return nil
}

func normalizeStrings(items []string) []string {
	result := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" && !seen[item] {
			result, seen[item] = append(result, item), true
		}
	}
	sort.Strings(result)
	return result
}

func semanticFingerprint(input SelectInput) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func findOperation(state State, id string) (Operation, bool) {
	for _, operation := range state.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return Operation{}, false
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
		return State{}, errors.New("team state has trailing data")
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
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".teams-*.tmp")
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
		return errors.New("team lockfile read-back verification failed")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != SchemaVersion || state.Revision < 0 || state.Teams == nil {
		return errors.New("invalid canonical team state")
	}
	for id, item := range state.Teams {
		if id != item.ID || !safeID.MatchString(id) || !safeID.MatchString(item.WorkspaceID) || item.CurrentVersion != int64(len(item.Lockfiles)) || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
			return errors.New("invalid canonical team")
		}
		for index, lockfile := range item.Lockfiles {
			if lockfile.SchemaVersion != SchemaVersion || lockfile.TeamID != id || lockfile.Version != int64(index+1) || lockfile.WorkspaceID != item.WorkspaceID || len(lockfile.Members) != len(lockfile.Template.Roles) || !safeID.MatchString(lockfile.CreatedBy) || lockfile.CreatedAt.IsZero() {
				return errors.New("invalid team lockfile")
			}
		}
	}
	seen := map[string]bool{}
	for index, operation := range state.Operations {
		if operation.Revision != int64(index+1) || !safeID.MatchString(operation.ID) || seen[operation.ID] || !hexDigest.MatchString(operation.Fingerprint) || operation.Version <= 0 || operation.CreatedAt.IsZero() {
			return errors.New("invalid team operation ledger")
		}
		seen[operation.ID] = true
	}
	if int64(len(state.Operations)) != state.Revision {
		return errors.New("team operation ledger does not reach revision")
	}
	return nil
}
