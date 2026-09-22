package computer

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
	ErrConflict  = errors.New("computer control-plane conflict")
	ErrOffline   = errors.New("target computer is offline")
	ErrActiveRun = errors.New("agent has an active run lease")
	ErrLease     = errors.New("invalid computer or run lease")
	ErrNoCommand = errors.New("no queued command")
	safeID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hexDigest    = regexp.MustCompile(`^[a-f0-9]{64}$`)
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
	store := &Store{path: filepath.Join(abs, "computers-v1.json"), now: func() time.Time { return time.Now().UTC() }}
	if _, err := os.Stat(store.path); errors.Is(err, os.ErrNotExist) {
		if err := store.writeUnlocked(emptyState(store.now())); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if _, err := store.readUnlocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Register(ctx context.Context, input RegisterInput) (Computer, error) {
	if err := ctx.Err(); err != nil {
		return Computer{}, err
	}
	input.OperationID, input.ID, input.OwnerID, input.Name = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.ID), strings.TrimSpace(input.OwnerID), strings.TrimSpace(input.Name)
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.ID) || !safeID.MatchString(input.OwnerID) || input.Name == "" {
		return Computer{}, errors.New("operation, computer, owner, and name are required")
	}
	fingerprint, _ := semanticFingerprint("register", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Computer{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Computer{}, ErrConflict
		}
		return state.Computers[operation.EntityID], nil
	}
	if _, found := state.Computers[input.ID]; found {
		return Computer{}, ErrConflict
	}
	now := s.now()
	computer := Computer{ID: input.ID, OwnerID: input.OwnerID, Name: input.Name, Status: Offline, Revision: 1, CreatedAt: now, UpdatedAt: now}
	state.Computers[computer.ID] = computer
	appendOperation(&state, input.OperationID, "register", fingerprint, computer.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Computer{}, err
	}
	return computer, nil
}

func (s *Store) Connect(ctx context.Context, hello Hello, ttl time.Duration) (Computer, error) {
	if err := ctx.Err(); err != nil {
		return Computer{}, err
	}
	hello = normalizeHello(hello)
	if !safeID.MatchString(hello.ComputerID) || !safeID.MatchString(hello.LeaseID) || hello.OS == "" || hello.Arch == "" || hello.Runtime == "" || ttl <= 0 || ttl > 10*time.Minute {
		return Computer{}, errors.New("valid computer hello and ttl in (0,10m] are required")
	}
	digest, err := capabilityDigest(hello)
	if err != nil {
		return Computer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Computer{}, err
	}
	computer, found := state.Computers[hello.ComputerID]
	if !found {
		return Computer{}, os.ErrNotExist
	}
	now := s.now()
	if computer.Status == Online && now.Before(computer.LeaseExpiresAt) && computer.ConnectionLeaseID != hello.LeaseID {
		return Computer{}, ErrLease
	}
	computer.Status, computer.ConnectionLeaseID, computer.LeaseExpiresAt = Online, hello.LeaseID, now.Add(ttl)
	if computer.CurrentCapability != digest {
		computer.Capabilities = append(computer.Capabilities, CapabilitySnapshot{
			Revision: int64(len(computer.Capabilities) + 1), Digest: digest, OS: hello.OS, Arch: hello.Arch,
			Runtime: hello.Runtime, Tools: cloneMap(hello.Tools), Sandboxes: append([]string(nil), hello.Sandboxes...), CreatedAt: now,
		})
		computer.CurrentCapability = digest
	}
	computer.Revision++
	computer.UpdatedAt = now
	state.Computers[computer.ID] = computer
	appendOperation(&state, internalOperationID("connect", hello.LeaseID, fmt.Sprint(computer.Revision)), "connect", digest, computer.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Computer{}, err
	}
	return computer, nil
}

func (s *Store) Heartbeat(ctx context.Context, computerID, leaseID string, ttl time.Duration) (Computer, error) {
	if err := ctx.Err(); err != nil {
		return Computer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Computer{}, err
	}
	computer, found := state.Computers[computerID]
	now := s.now()
	if !found || computer.ConnectionLeaseID != leaseID || computer.Status != Online || !now.Before(computer.LeaseExpiresAt) || ttl <= 0 || ttl > 10*time.Minute {
		return Computer{}, ErrLease
	}
	computer.LeaseExpiresAt, computer.UpdatedAt = now.Add(ttl), now
	computer.Revision++
	state.Computers[computer.ID] = computer
	fingerprint, _ := semanticFingerprint("heartbeat", struct{ Computer, Lease string }{computerID, leaseID})
	appendOperation(&state, internalOperationID("heartbeat", leaseID, fmt.Sprint(computer.Revision)), "heartbeat", fingerprint, computer.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Computer{}, err
	}
	return computer, nil
}

func (s *Store) Disconnect(ctx context.Context, computerID, leaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return err
	}
	computer, found := state.Computers[computerID]
	if !found || computer.ConnectionLeaseID != leaseID {
		return ErrLease
	}
	now := s.now()
	computer.Status, computer.ConnectionLeaseID, computer.LeaseExpiresAt = Offline, "", time.Time{}
	computer.Revision++
	computer.UpdatedAt = now
	state.Computers[computer.ID] = computer
	fingerprint, _ := semanticFingerprint("disconnect", struct{ Computer, Lease string }{computerID, leaseID})
	appendOperation(&state, internalOperationID("disconnect", leaseID, fmt.Sprint(computer.Revision)), "disconnect", fingerprint, computer.ID, now)
	return s.writeUnlocked(state)
}

func (s *Store) Rebind(ctx context.Context, input RebindInput) (Binding, *Command, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, nil, err
	}
	input.OperationID, input.AgentID, input.TargetComputerID, input.ActorID = strings.TrimSpace(input.OperationID), strings.TrimSpace(input.AgentID), strings.TrimSpace(input.TargetComputerID), strings.TrimSpace(input.ActorID)
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.AgentID) || !safeID.MatchString(input.TargetComputerID) || !safeID.MatchString(input.ActorID) || input.ExpectedBindingRevision < 0 {
		return Binding{}, nil, errors.New("operation, agent, target computer, actor, and non-negative expected revision are required")
	}
	fingerprint, _ := semanticFingerprint("rebind", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Binding{}, nil, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return Binding{}, nil, ErrConflict
		}
		binding := state.Bindings[input.AgentID]
		return binding, commandForOperation(state, operation.EntityID), nil
	}
	now := s.now()
	target, found := state.Computers[input.TargetComputerID]
	if !found || target.Status != Online || !now.Before(target.LeaseExpiresAt) {
		return Binding{}, nil, ErrOffline
	}
	current, exists := state.Bindings[input.AgentID]
	if !exists && input.ExpectedBindingRevision != 0 || exists && current.Revision != input.ExpectedBindingRevision {
		return Binding{}, nil, ErrConflict
	}
	for runID, lease := range state.RunLeases {
		if !now.Before(lease.ExpiresAt) {
			delete(state.RunLeases, runID)
			continue
		}
		if lease.AgentID == input.AgentID {
			return Binding{}, nil, ErrActiveRun
		}
	}
	binding := Binding{AgentID: input.AgentID, ComputerID: input.TargetComputerID, Revision: input.ExpectedBindingRevision + 1, BoundBy: input.ActorID, BoundAt: now}
	state.Bindings[input.AgentID] = binding
	var cleanup *Command
	entityID := binding.AgentID
	if exists && current.ComputerID != input.TargetComputerID {
		command := Command{ID: internalOperationID("cleanup", input.OperationID), ComputerID: current.ComputerID, Kind: "cleanup_agent", AgentID: input.AgentID, State: CommandQueued, CreatedAt: now, UpdatedAt: now}
		state.Commands[command.ID] = command
		cleanup, entityID = &command, command.ID
	}
	appendOperation(&state, input.OperationID, "rebind", fingerprint, entityID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Binding{}, nil, err
	}
	return binding, cleanup, nil
}

func (s *Store) AcquireRun(ctx context.Context, input RunLeaseInput) (RunLease, error) {
	if err := ctx.Err(); err != nil {
		return RunLease{}, err
	}
	if !safeID.MatchString(input.OperationID) || !safeID.MatchString(input.RunID) || !safeID.MatchString(input.AgentID) || !safeID.MatchString(input.ComputerID) || !safeID.MatchString(input.LeaseID) || input.TTLSeconds <= 0 || input.TTLSeconds > 86400 {
		return RunLease{}, errors.New("valid run lease fields are required")
	}
	fingerprint, _ := semanticFingerprint("acquire_run", input)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return RunLease{}, err
	}
	if operation, found := findOperation(state, input.OperationID); found {
		if operation.Fingerprint != fingerprint {
			return RunLease{}, ErrConflict
		}
		return state.RunLeases[input.RunID], nil
	}
	binding, found := state.Bindings[input.AgentID]
	if !found || binding.ComputerID != input.ComputerID {
		return RunLease{}, ErrConflict
	}
	now := s.now()
	if current, found := state.RunLeases[input.RunID]; found && now.Before(current.ExpiresAt) {
		return RunLease{}, ErrLease
	}
	lease := RunLease{RunID: input.RunID, AgentID: input.AgentID, ComputerID: input.ComputerID, LeaseID: input.LeaseID, ExpiresAt: now.Add(time.Duration(input.TTLSeconds) * time.Second)}
	state.RunLeases[lease.RunID] = lease
	appendOperation(&state, input.OperationID, "acquire_run", fingerprint, lease.RunID, now)
	if err := s.writeUnlocked(state); err != nil {
		return RunLease{}, err
	}
	return lease, nil
}

func (s *Store) ReleaseRun(ctx context.Context, runID, operationID, leaseID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	semantic := struct{ Run, Lease string }{runID, leaseID}
	fingerprint, _ := semanticFingerprint("release_run", semantic)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return err
	}
	if operation, found := findOperation(state, operationID); found {
		if operation.Fingerprint != fingerprint {
			return ErrConflict
		}
		return nil
	}
	lease, found := state.RunLeases[runID]
	if !found || lease.LeaseID != leaseID {
		return ErrLease
	}
	delete(state.RunLeases, runID)
	now := s.now()
	appendOperation(&state, operationID, "release_run", fingerprint, runID, now)
	return s.writeUnlocked(state)
}

func (s *Store) PollCommand(ctx context.Context, computerID, leaseID string) (Command, error) {
	if err := ctx.Err(); err != nil {
		return Command{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Command{}, err
	}
	now := s.now()
	computer, found := state.Computers[computerID]
	if !found || computer.ConnectionLeaseID != leaseID || computer.Status != Online || !now.Before(computer.LeaseExpiresAt) {
		return Command{}, ErrLease
	}
	commands := make([]Command, 0)
	for _, command := range state.Commands {
		if command.ComputerID == computerID && (command.State == CommandQueued || command.State == CommandSent) {
			commands = append(commands, command)
		}
	}
	if len(commands) == 0 {
		return Command{}, ErrNoCommand
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].CreatedAt.Before(commands[j].CreatedAt) })
	command := commands[0]
	command.State, command.Attempt, command.UpdatedAt = CommandSent, command.Attempt+1, now
	state.Commands[command.ID] = command
	fingerprint, _ := semanticFingerprint("poll_command", struct{ Computer, Command string }{computerID, command.ID})
	appendOperation(&state, internalOperationID("poll", command.ID, fmt.Sprint(command.Attempt)), "poll_command", fingerprint, command.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Command{}, err
	}
	return command, nil
}

func (s *Store) AckCommand(ctx context.Context, computerID, leaseID string, ack CommandAck) (Command, error) {
	if err := ctx.Err(); err != nil {
		return Command{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.readUnlocked()
	if err != nil {
		return Command{}, err
	}
	computer, found := state.Computers[computerID]
	now := s.now()
	if !found || computer.ConnectionLeaseID != leaseID || !now.Before(computer.LeaseExpiresAt) {
		return Command{}, ErrLease
	}
	command, found := state.Commands[ack.CommandID]
	if !found || command.ComputerID != computerID || command.State != CommandSent {
		return Command{}, ErrConflict
	}
	command.Error = strings.TrimSpace(ack.Error)
	if ack.Success {
		command.State = CommandAcked
	} else {
		command.State = CommandFailed
		if command.Error == "" {
			command.Error = "daemon reported failure"
		}
	}
	command.UpdatedAt = now
	state.Commands[command.ID] = command
	fingerprint, _ := semanticFingerprint("ack_command", ack)
	appendOperation(&state, internalOperationID("ack", command.ID, fmt.Sprint(command.Attempt)), "ack_command", fingerprint, command.ID, now)
	if err := s.writeUnlocked(state); err != nil {
		return Command{}, err
	}
	return command, nil
}

func (s *Store) GetState(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readUnlocked()
}

func emptyState(now time.Time) State {
	return State{SchemaVersion: SchemaVersion, Computers: map[string]Computer{}, Bindings: map[string]Binding{}, RunLeases: map[string]RunLease{}, Commands: map[string]Command{}, UpdatedAt: now}
}

func appendOperation(state *State, id, action, fingerprint, entityID string, now time.Time) {
	state.Revision++
	state.UpdatedAt = now
	state.Operations = append(state.Operations, Operation{ID: id, Action: action, Fingerprint: fingerprint, EntityID: entityID, Revision: state.Revision, CreatedAt: now})
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
		return State{}, errors.New("computer state has trailing data")
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
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".computers-*.tmp")
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
		return errors.New("computer state read-back verification failed")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != SchemaVersion || state.Revision < 0 || state.Computers == nil || state.Bindings == nil || state.RunLeases == nil || state.Commands == nil {
		return errors.New("invalid canonical computer state")
	}
	for id, computer := range state.Computers {
		if id != computer.ID || !safeID.MatchString(id) || !safeID.MatchString(computer.OwnerID) || computer.Name == "" || computer.Revision <= 0 || computer.CreatedAt.IsZero() || computer.UpdatedAt.IsZero() {
			return errors.New("invalid canonical computer")
		}
		if computer.Status != Online && computer.Status != Offline {
			return errors.New("invalid computer status")
		}
		if computer.Status == Online && (!safeID.MatchString(computer.ConnectionLeaseID) || computer.LeaseExpiresAt.IsZero()) {
			return errors.New("online computer lacks connection lease")
		}
		if computer.Status == Offline && (computer.ConnectionLeaseID != "" || !computer.LeaseExpiresAt.IsZero()) {
			return errors.New("offline computer carries connection lease")
		}
		for index, snapshot := range computer.Capabilities {
			if snapshot.Revision != int64(index+1) || !hexDigest.MatchString(snapshot.Digest) || snapshot.OS == "" || snapshot.Arch == "" || snapshot.Runtime == "" || snapshot.CreatedAt.IsZero() {
				return errors.New("invalid capability snapshot")
			}
		}
		if len(computer.Capabilities) == 0 && computer.CurrentCapability != "" || len(computer.Capabilities) > 0 && computer.CurrentCapability != computer.Capabilities[len(computer.Capabilities)-1].Digest {
			return errors.New("current capability does not identify latest snapshot")
		}
	}
	for agentID, binding := range state.Bindings {
		if agentID != binding.AgentID || !safeID.MatchString(agentID) || state.Computers[binding.ComputerID].ID == "" || binding.Revision <= 0 || !safeID.MatchString(binding.BoundBy) || binding.BoundAt.IsZero() {
			return errors.New("invalid agent computer binding")
		}
	}
	for runID, lease := range state.RunLeases {
		binding, bound := state.Bindings[lease.AgentID]
		if runID != lease.RunID || !safeID.MatchString(runID) || !safeID.MatchString(lease.AgentID) || !safeID.MatchString(lease.ComputerID) || !safeID.MatchString(lease.LeaseID) || lease.ExpiresAt.IsZero() || !bound || binding.ComputerID != lease.ComputerID {
			return errors.New("invalid agent run lease")
		}
	}
	for id, command := range state.Commands {
		if id != command.ID || !safeID.MatchString(id) || state.Computers[command.ComputerID].ID == "" || !safeID.MatchString(command.AgentID) || command.Kind != "cleanup_agent" || command.Attempt < 0 || command.CreatedAt.IsZero() || command.UpdatedAt.IsZero() {
			return errors.New("invalid daemon command")
		}
		if command.State != CommandQueued && command.State != CommandSent && command.State != CommandAcked && command.State != CommandFailed {
			return errors.New("invalid daemon command state")
		}
	}
	seenOperations := map[string]bool{}
	for index, operation := range state.Operations {
		if operation.Revision != int64(index+1) || !safeID.MatchString(operation.ID) || seenOperations[operation.ID] || operation.Action == "" || !hexDigest.MatchString(operation.Fingerprint) || operation.CreatedAt.IsZero() {
			return errors.New("invalid computer operation ledger")
		}
		seenOperations[operation.ID] = true
	}
	if int64(len(state.Operations)) != state.Revision {
		return errors.New("computer operation ledger does not reach revision")
	}
	return nil
}

func normalizeHello(hello Hello) Hello {
	hello.ComputerID, hello.LeaseID = strings.TrimSpace(hello.ComputerID), strings.TrimSpace(hello.LeaseID)
	hello.OS, hello.Arch, hello.Runtime = strings.TrimSpace(hello.OS), strings.TrimSpace(hello.Arch), strings.TrimSpace(hello.Runtime)
	hello.Sandboxes = normalizeStrings(hello.Sandboxes)
	if hello.Tools == nil {
		hello.Tools = map[string]string{}
	}
	return hello
}

func capabilityDigest(hello Hello) (string, error) {
	encoded, err := json.Marshal(struct {
		OS, Arch, Runtime string
		Tools             map[string]string
		Sandboxes         []string
	}{hello.OS, hello.Arch, hello.Runtime, hello.Tools, hello.Sandboxes})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeStrings(items []string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	sort.Strings(result)
	return result
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
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

func findOperation(state State, id string) (Operation, bool) {
	for _, operation := range state.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return Operation{}, false
}

func commandForOperation(state State, id string) *Command {
	command, found := state.Commands[id]
	if !found {
		return nil
	}
	return &command
}

func internalOperationID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}
