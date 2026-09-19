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

	"github.com/royal007a/01agent/internal/engine"
)

var safeRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type FileStore struct {
	dir string
	mu  sync.Mutex
}

type record struct {
	Kind   string            `json:"kind"`
	Event  *engine.Event     `json:"event,omitempty"`
	Result *engine.RunResult `json:"result,omitempty"`
}

type Trace struct {
	Events []engine.Event   `json:"events"`
	Result engine.RunResult `json:"result"`
}

type ReplaySummary struct {
	RunID       string `json:"run_id"`
	Events      int    `json:"events"`
	ToolCalls   int    `json:"tool_calls"`
	ToolResults int    `json:"tool_results"`
	FinalReason string `json:"final_reason"`
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

func (s *FileStore) SaveCheckpoint(_ context.Context, checkpoint engine.Checkpoint) error {
	if err := validateID(checkpoint.RunID); err != nil {
		return err
	}
	return s.writeAtomic(s.checkpointPath(checkpoint.RunID), checkpoint)
}

func (s *FileStore) LoadCheckpoint(_ context.Context, runID string) (engine.Checkpoint, error) {
	if err := validateID(runID); err != nil {
		return engine.Checkpoint{}, err
	}
	var checkpoint engine.Checkpoint
	if err := readJSON(s.checkpointPath(runID), &checkpoint); err != nil {
		return engine.Checkpoint{}, fmt.Errorf("load checkpoint %q: %w", runID, err)
	}
	return checkpoint, nil
}

func (s *FileStore) LoadTrace(runID string) (Trace, error) {
	if err := validateID(runID); err != nil {
		return Trace{}, err
	}
	return LoadTrace(s.tracePath(runID))
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
	for _, event := range trace.Events {
		if event.RunID != trace.Result.RunID {
			return ReplaySummary{}, fmt.Errorf("event run id %q does not match result %q", event.RunID, trace.Result.RunID)
		}
		if event.Sequence <= lastSequence {
			return ReplaySummary{}, fmt.Errorf("event sequence %d is not greater than %d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		switch event.Type {
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
	for callID, count := range pending {
		if count != 0 && trace.Result.Reason == "completed" {
			return ReplaySummary{}, fmt.Errorf("completed trace has %d unmatched call(s) for %q", count, callID)
		}
	}
	return summary, nil
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
	return os.Rename(temporaryName, path)
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
func (s *FileStore) checkpointPath(runID string) string {
	return filepath.Join(s.dir, runID+".checkpoint.json")
}
