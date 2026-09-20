package runstore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/royal007a/01agent/internal/engine"
)

var safeRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var safeFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)

var ErrHistoryConflict = errors.New("canonical history revision conflict")

type FileStore struct {
	dir string
	mu  sync.Mutex
}

type record struct {
	Kind   string            `json:"kind"`
	Event  *engine.Event     `json:"event,omitempty"`
	Result *engine.RunResult `json:"result,omitempty"`
}

type canonicalHistory struct {
	Acknowledgement engine.HistoryAck `json:"acknowledgement"`
	Checkpoint      engine.Checkpoint `json:"checkpoint"`
}

type operationRecord struct {
	OperationID string    `json:"operation_id"`
	Fingerprint string    `json:"fingerprint"`
	Revision    int64     `json:"revision"`
	CommittedAt time.Time `json:"committed_at"`
}

type Trace struct {
	Events []engine.Event   `json:"events"`
	Result engine.RunResult `json:"result"`
}

type ReplaySummary struct {
	RunID                 string `json:"run_id"`
	Events                int    `json:"events"`
	ToolCalls             int    `json:"tool_calls"`
	ToolResults           int    `json:"tool_results"`
	CapabilitySnapshots   int    `json:"capability_snapshots"`
	CapabilityDigest      string `json:"capability_digest,omitempty"`
	ToolDigest            string `json:"tool_digest,omitempty"`
	PromptDigest          string `json:"prompt_digest,omitempty"`
	AgentsDigest          string `json:"agents_digest,omitempty"`
	SkillsDigest          string `json:"skills_digest,omitempty"`
	HistoryCommits        int    `json:"history_commits"`
	LastHistoryRevision   int64  `json:"last_history_revision"`
	InputClaims           int    `json:"input_claims"`
	InputAcknowledgements int    `json:"input_acknowledgements"`
	PendingInputClaims    int    `json:"pending_input_claims"`
	FinalReason           string `json:"final_reason"`
}

func New(dir string) (*FileStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve run directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	return &FileStore{dir: abs}, nil
}

func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) Record(ctx context.Context, event engine.Event) error {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return s.append(event.RunID, record{Kind: "event", Event: &event})
}

func (s *FileStore) Complete(_ context.Context, result engine.RunResult) error {
	if err := s.append(result.RunID, record{Kind: "result", Result: &result}); err != nil {
		return err
	}
	return s.writeAtomic(s.resultPath(result.RunID), result)
}

func (s *FileStore) LoadResult(_ context.Context, runID string) (engine.RunResult, error) {
	if err := validateID(runID); err != nil {
		return engine.RunResult{}, err
	}
	var result engine.RunResult
	if err := readJSON(s.resultPath(runID), &result); err != nil {
		return engine.RunResult{}, fmt.Errorf("load result %q: %w", runID, err)
	}
	if result.RunID != runID || result.Reason == "" || result.CompletedAt.IsZero() {
		return engine.RunResult{}, fmt.Errorf("load result %q: invalid result identity", runID)
	}
	return result, nil
}

func (s *FileStore) LoadCheckpoint(_ context.Context, runID string) (engine.Checkpoint, error) {
	if err := validateID(runID); err != nil {
		return engine.Checkpoint{}, err
	}
	var history canonicalHistory
	if err := readJSON(s.historyPath(runID), &history); err != nil {
		return engine.Checkpoint{}, fmt.Errorf("load checkpoint %q: %w", runID, err)
	}
	if err := validateCanonicalHistory(runID, history); err != nil {
		return engine.Checkpoint{}, fmt.Errorf("load checkpoint %q: %w", runID, err)
	}
	return history.Checkpoint, nil
}

// CommitHistory is the canonical mutation lane for one run. It provides
// optimistic revision checks, operation identity, semantic conflict detection,
// durable replace, read-back verification, and only then acknowledgement.
func (s *FileStore) CommitHistory(_ context.Context, commit engine.HistoryCommit) (engine.HistoryAck, error) {
	if err := validateID(commit.RunID); err != nil {
		return engine.HistoryAck{}, err
	}
	if commit.Checkpoint.RunID != commit.RunID || commit.OperationID == "" || len(commit.OperationID) > 256 || !safeFingerprint.MatchString(commit.Fingerprint) {
		return engine.HistoryAck{}, errors.New("invalid canonical history commit")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var current canonicalHistory
	err := readJSON(s.historyPath(commit.RunID), &current)
	historyExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return engine.HistoryAck{}, fmt.Errorf("read canonical history: %w", err)
	}
	if historyExists {
		if err := validateCanonicalHistory(commit.RunID, current); err != nil {
			return engine.HistoryAck{}, fmt.Errorf("validate canonical history: %w", err)
		}
	}
	operations, err := loadOperations(s.operationsPath(commit.RunID))
	if err != nil {
		return engine.HistoryAck{}, err
	}
	if !historyExists && len(operations) > 0 {
		return engine.HistoryAck{}, errors.New("operation ledger exists without canonical history")
	}
	if previous, ok := operations[commit.OperationID]; ok {
		if previous.Revision > current.Acknowledgement.Revision {
			return engine.HistoryAck{}, errors.New("operation ledger revision exceeds canonical history")
		}
		if previous.Fingerprint != commit.Fingerprint {
			return engine.HistoryAck{}, fmt.Errorf("operation %q reused with different semantics: %w", commit.OperationID, ErrHistoryConflict)
		}
		return engine.HistoryAck{
			RunID: commit.RunID, OperationID: previous.OperationID, Fingerprint: previous.Fingerprint,
			Revision: previous.Revision, CommittedAt: previous.CommittedAt,
		}, nil
	}
	if current.Acknowledgement.OperationID == commit.OperationID {
		if current.Acknowledgement.Fingerprint != commit.Fingerprint {
			return engine.HistoryAck{}, fmt.Errorf("operation %q reused with different semantics: %w", commit.OperationID, ErrHistoryConflict)
		}
		if err := s.appendOperationUnlocked(commit.RunID, operationRecord{
			OperationID: commit.OperationID, Fingerprint: commit.Fingerprint,
			Revision: current.Acknowledgement.Revision, CommittedAt: current.Acknowledgement.CommittedAt,
		}); err != nil {
			return engine.HistoryAck{}, err
		}
		return current.Acknowledgement, nil
	}
	currentRevision := current.Acknowledgement.Revision
	if commit.ExpectedRevision != currentRevision {
		return engine.HistoryAck{}, fmt.Errorf("expected revision %d, current revision %d: %w", commit.ExpectedRevision, currentRevision, ErrHistoryConflict)
	}

	ack := engine.HistoryAck{
		RunID: commit.RunID, OperationID: commit.OperationID, Fingerprint: commit.Fingerprint,
		Revision: currentRevision + 1, CommittedAt: time.Now().UTC(),
	}
	checkpoint := commit.Checkpoint
	checkpoint.HistoryRevision = ack.Revision
	checkpoint.LastOperationID = ack.OperationID
	checkpoint.LastFingerprint = ack.Fingerprint
	checkpoint.UpdatedAt = ack.CommittedAt
	next := canonicalHistory{Acknowledgement: ack, Checkpoint: checkpoint}
	if err := s.writeAtomicUnlocked(s.historyPath(commit.RunID), next); err != nil {
		return engine.HistoryAck{}, fmt.Errorf("write canonical history: %w", err)
	}
	var verified canonicalHistory
	if err := readJSON(s.historyPath(commit.RunID), &verified); err != nil {
		return engine.HistoryAck{}, fmt.Errorf("read back canonical history: %w", err)
	}
	if verified.Acknowledgement != ack || verified.Checkpoint.HistoryRevision != ack.Revision || verified.Checkpoint.LastFingerprint != ack.Fingerprint {
		return engine.HistoryAck{}, errors.New("canonical history read-back verification failed")
	}
	if err := s.appendOperationUnlocked(commit.RunID, operationRecord{
		OperationID: ack.OperationID, Fingerprint: ack.Fingerprint, Revision: ack.Revision, CommittedAt: ack.CommittedAt,
	}); err != nil {
		return engine.HistoryAck{}, fmt.Errorf("append operation acknowledgement: %w", err)
	}
	return ack, nil
}

func (s *FileStore) LoadTrace(runID string) (Trace, error) {
	if err := validateID(runID); err != nil {
		return Trace{}, err
	}
	return LoadTrace(s.tracePath(runID))
}

func (s *FileStore) LastEventSequence(_ context.Context, runID string) (int, error) {
	if err := validateID(runID); err != nil {
		return 0, err
	}
	file, err := os.Open(s.tracePath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	last := 0
	for {
		var item record
		if err := decoder.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				return last, nil
			}
			return 0, fmt.Errorf("decode trace sequence: %w", err)
		}
		if item.Kind != "event" {
			continue
		}
		if item.Event == nil || item.Event.RunID != runID || item.Event.Sequence <= last {
			return 0, errors.New("trace event sequence is corrupt")
		}
		last = item.Event.Sequence
	}
}

func LoadTrace(path string) (Trace, error) {
	file, err := os.Open(path)
	if err != nil {
		return Trace{}, err
	}
	defer file.Close()

	var trace Trace
	decoder := json.NewDecoder(bufio.NewReader(file))
	for {
		var item record
		if err := decoder.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Trace{}, fmt.Errorf("decode trace: %w", err)
		}
		switch item.Kind {
		case "event":
			if item.Event == nil {
				return Trace{}, errors.New("trace event record has no event")
			}
			trace.Events = append(trace.Events, *item.Event)
		case "result":
			if item.Result == nil {
				return Trace{}, errors.New("trace result record has no result")
			}
			trace.Result = *item.Result
		default:
			return Trace{}, fmt.Errorf("unknown trace record kind %q", item.Kind)
		}
	}
	if trace.Result.RunID == "" {
		return Trace{}, errors.New("trace has no completed result")
	}
	return trace, nil
}

func Replay(trace Trace) (ReplaySummary, error) {
	summary := ReplaySummary{RunID: trace.Result.RunID, Events: len(trace.Events), FinalReason: string(trace.Result.Reason)}
	lastSequence := 0
	pending := make(map[string]int)
	operations := make(map[string]string)
	inputClaims := make(map[string]bool)
	for _, event := range trace.Events {
		if event.RunID != trace.Result.RunID {
			return ReplaySummary{}, fmt.Errorf("event run id %q does not match result %q", event.RunID, trace.Result.RunID)
		}
		if event.Sequence <= lastSequence {
			return ReplaySummary{}, fmt.Errorf("event sequence %d is not greater than %d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		switch event.Type {
		case engine.EventCapability:
			digest, ok := metadataString(event.Metadata, "digest")
			if !ok || !safeFingerprint.MatchString(digest) {
				return ReplaySummary{}, errors.New("capability snapshot has invalid digest")
			}
			if summary.CapabilityDigest != "" && summary.CapabilityDigest != digest {
				return ReplaySummary{}, errors.New("capability revision changed inside one run")
			}
			summary.CapabilityDigest = digest
			summary.CapabilitySnapshots++
			toolDigest, hasTool := metadataString(event.Metadata, "tool_digest")
			promptDigest, hasPrompt := metadataString(event.Metadata, "prompt_digest")
			agentsDigest, hasAgents := metadataString(event.Metadata, "agents_digest")
			skillsDigest, hasSkills := metadataString(event.Metadata, "skills_digest")
			if hasTool || hasPrompt || hasAgents || hasSkills {
				if !hasTool || !safeFingerprint.MatchString(toolDigest) || !hasPrompt || !safeFingerprint.MatchString(promptDigest) || !hasSkills || !safeFingerprint.MatchString(skillsDigest) || (agentsDigest != "" && !safeFingerprint.MatchString(agentsDigest)) {
					return ReplaySummary{}, errors.New("capability snapshot has invalid component digests")
				}
				summary.ToolDigest = toolDigest
				summary.PromptDigest = promptDigest
				summary.AgentsDigest = agentsDigest
				summary.SkillsDigest = skillsDigest
			}
		case engine.EventCommitted:
			operationID, operationOK := metadataString(event.Metadata, "operation_id")
			fingerprint, fingerprintOK := metadataString(event.Metadata, "fingerprint")
			revision, revisionOK := metadataInt64(event.Metadata, "revision")
			if !operationOK || operationID == "" || !fingerprintOK || !safeFingerprint.MatchString(fingerprint) || !revisionOK {
				return ReplaySummary{}, errors.New("history_committed event has invalid commit identity")
			}
			if existing, exists := operations[operationID]; exists {
				if existing != fingerprint {
					return ReplaySummary{}, fmt.Errorf("operation %q has conflicting fingerprints", operationID)
				}
				return ReplaySummary{}, fmt.Errorf("operation %q was acknowledged more than once", operationID)
			}
			if revision != summary.LastHistoryRevision+1 {
				return ReplaySummary{}, fmt.Errorf("history revision %d does not follow %d", revision, summary.LastHistoryRevision)
			}
			operations[operationID] = fingerprint
			summary.HistoryCommits++
			summary.LastHistoryRevision = revision
		case engine.EventInputClaimed:
			claimID, ok := metadataString(event.Metadata, "claim_id")
			if !ok || claimID == "" {
				return ReplaySummary{}, errors.New("input_claimed event has empty claim id")
			}
			if _, exists := inputClaims[claimID]; exists {
				return ReplaySummary{}, fmt.Errorf("input claim %q appears more than once", claimID)
			}
			inputClaims[claimID] = false
			summary.InputClaims++
		case engine.EventInputAcked:
			claimID, ok := metadataString(event.Metadata, "claim_id")
			revision, revisionOK := metadataInt64(event.Metadata, "history_revision")
			acked, exists := inputClaims[claimID]
			if !ok || claimID == "" || !exists {
				return ReplaySummary{}, fmt.Errorf("input acknowledgement %q has no preceding claim", claimID)
			}
			if acked {
				return ReplaySummary{}, fmt.Errorf("input claim %q was acknowledged more than once", claimID)
			}
			if !revisionOK || revision <= 0 || revision > summary.LastHistoryRevision {
				return ReplaySummary{}, fmt.Errorf("input claim %q has invalid committed revision %d", claimID, revision)
			}
			inputClaims[claimID] = true
			summary.InputAcknowledgements++
		case engine.EventToolStarted:
			if event.ToolCall.ID == "" {
				return ReplaySummary{}, errors.New("tool_started event has empty call id")
			}
			pending[event.ToolCall.ID]++
			summary.ToolCalls++
		case engine.EventToolResult:
			if pending[event.ToolResult.ToolCallID] == 0 {
				return ReplaySummary{}, fmt.Errorf("tool result %q has no preceding call", event.ToolResult.ToolCallID)
			}
			pending[event.ToolResult.ToolCallID]--
			summary.ToolResults++
		}
	}
	if summary.CapabilityDigest != "" && trace.Result.Capability.Digest != summary.CapabilityDigest {
		return ReplaySummary{}, errors.New("result capability does not match trace snapshot")
	}
	if summary.ToolDigest != "" {
		capability := trace.Result.Capability
		if capability.ToolDigest != summary.ToolDigest || capability.PromptDigest != summary.PromptDigest || capability.AgentsDigest != summary.AgentsDigest || capability.SkillsDigest != summary.SkillsDigest {
			return ReplaySummary{}, errors.New("result capability components do not match trace snapshot")
		}
	}
	if summary.HistoryCommits > 0 && trace.Result.HistoryRevision != summary.LastHistoryRevision {
		return ReplaySummary{}, fmt.Errorf("result history revision %d does not match trace revision %d", trace.Result.HistoryRevision, summary.LastHistoryRevision)
	}
	for _, acknowledged := range inputClaims {
		if !acknowledged {
			summary.PendingInputClaims++
		}
	}
	for callID, count := range pending {
		if count != 0 && trace.Result.Reason == "completed" {
			return ReplaySummary{}, fmt.Errorf("completed trace has %d unmatched call(s) for %q", count, callID)
		}
	}
	return summary, nil
}

func metadataString(metadata map[string]any, key string) (string, bool) {
	value, ok := metadata[key].(string)
	return value, ok
}

func metadataInt64(metadata map[string]any, key string) (int64, bool) {
	switch value := metadata[key].(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		converted := int64(value)
		return converted, float64(converted) == value
	case json.Number:
		converted, err := value.Int64()
		return converted, err == nil
	default:
		return 0, false
	}
}

func (s *FileStore) append(runID string, item record) error {
	if err := validateID(runID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.OpenFile(s.tracePath(runID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encodeErr := encoder.Encode(item)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(encodeErr, syncErr, closeErr)
}

func (s *FileStore) writeAtomic(path string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeAtomicUnlocked(path, value)
}

func (s *FileStore) writeAtomicUnlocked(path string, value any) error {
	temporary, err := os.CreateTemp(s.dir, ".01agent-*.tmp")
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
	if err := encoder.Encode(value); err != nil {
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
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func (s *FileStore) appendOperationUnlocked(runID string, operation operationRecord) error {
	file, err := os.OpenFile(s.operationsPath(runID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(operation)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(encodeErr, syncErr, closeErr)
}

func loadOperations(path string) (map[string]operationRecord, error) {
	operations := make(map[string]operationRecord)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return operations, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	for {
		var operation operationRecord
		if err := decoder.Decode(&operation); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode operation log: %w", err)
		}
		if previous, exists := operations[operation.OperationID]; exists && previous != operation {
			return nil, fmt.Errorf("conflicting operation log entry %q", operation.OperationID)
		}
		if operation.OperationID == "" || !safeFingerprint.MatchString(operation.Fingerprint) || operation.Revision <= 0 || operation.CommittedAt.IsZero() {
			return nil, errors.New("operation log contains an invalid record")
		}
		operations[operation.OperationID] = operation
	}
	return operations, nil
}

func validateCanonicalHistory(runID string, history canonicalHistory) error {
	ack := history.Acknowledgement
	checkpoint := history.Checkpoint
	if ack.RunID != runID || checkpoint.RunID != runID || checkpoint.Version != 2 {
		return errors.New("canonical history identity mismatch")
	}
	if ack.Revision <= 0 || ack.OperationID == "" || !safeFingerprint.MatchString(ack.Fingerprint) || ack.CommittedAt.IsZero() {
		return errors.New("canonical history acknowledgement is invalid")
	}
	if checkpoint.HistoryRevision != ack.Revision || checkpoint.LastOperationID != ack.OperationID || checkpoint.LastFingerprint != ack.Fingerprint {
		return errors.New("canonical history acknowledgement mismatch")
	}
	return nil
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

func validateID(runID string) error {
	if !safeRunID.MatchString(runID) {
		return fmt.Errorf("invalid run id %q", runID)
	}
	return nil
}

func (s *FileStore) tracePath(runID string) string { return filepath.Join(s.dir, runID+".trace.jsonl") }
func (s *FileStore) resultPath(runID string) string {
	return filepath.Join(s.dir, runID+".result.json")
}
func (s *FileStore) historyPath(runID string) string {
	return filepath.Join(s.dir, runID+".history.json")
}
func (s *FileStore) operationsPath(runID string) string {
	return filepath.Join(s.dir, runID+".operations.jsonl")
}
