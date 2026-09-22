package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/royal007a/01agent/internal/computer"
	"github.com/royal007a/01agent/internal/engine"
	"github.com/royal007a/01agent/internal/schema"
)

const maxRuntimeOutput = 2 << 20

type Executor interface {
	Execute(context.Context, string, computer.RunRequest) (computer.RunResult, error)
}

type ProcessExecutor struct {
	Binary       string
	AllowedTools map[string]bool
}

func (e ProcessExecutor) Execute(ctx context.Context, root string, request computer.RunRequest) (computer.RunResult, error) {
	binary := strings.TrimSpace(e.Binary)
	if binary == "" {
		binary = "/usr/local/bin/01agent"
	}
	agentRoot := filepath.Join(root, "agents", request.AgentID)
	workspace := filepath.Join(agentRoot, "workspace")
	runDir := filepath.Join(agentRoot, "runs")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return computer.RunResult{}, err
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return computer.RunResult{}, err
	}
	if result, err := loadRuntimeResult(filepath.Join(runDir, request.RunID+".result.json"), request); err == nil {
		return result, nil
	}
	args := []string{"--workdir", workspace, "--run-dir", runDir, "--json", "--max-turns", fmt.Sprint(request.MaxTurns), "--timeout", time.Duration(request.TimeoutSeconds).String()}
	if _, err := os.Stat(filepath.Join(runDir, request.RunID+".history.json")); err == nil {
		args = append(args, "--resume", request.RunID)
	} else {
		args = append(args, "--run-id", request.RunID)
	}
	var approved []string
	for _, tool := range request.ApprovedTools {
		if e.AllowedTools[tool] {
			approved = append(approved, tool)
		}
	}
	if len(approved) > 0 {
		args = append(args, "--enable-dangerous-tools")
		for _, tool := range approved {
			args = append(args, "--approve-tool", tool)
		}
	}
	if !contains(args, "--resume") {
		args = append(args, request.Prompt)
	}
	command := exec.CommandContext(ctx, binary, args...)
	stdout, stderr := &limitedBuffer{limit: maxRuntimeOutput}, &limitedBuffer{limit: maxRuntimeOutput}
	command.Stdout, command.Stderr = stdout, stderr
	processErr := command.Run()
	var runtimeResult engine.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &runtimeResult); err != nil {
		if processErr != nil {
			return computer.RunResult{}, fmt.Errorf("runtime failed: %w: %s", processErr, strings.TrimSpace(stderr.String()))
		}
		return computer.RunResult{}, fmt.Errorf("decode runtime result: %w", err)
	}
	if runtimeResult.RunID != request.RunID || runtimeResult.CompletedAt.IsZero() {
		return computer.RunResult{}, errors.New("runtime returned a mismatched or incomplete result")
	}
	artifactPath := filepath.Join(runDir, request.RunID+".result.json")
	artifactBytes, err := os.ReadFile(artifactPath)
	if err != nil {
		artifactBytes = stdout.Bytes()
		if writeErr := writeAtomic(artifactPath, artifactBytes); writeErr != nil {
			return computer.RunResult{}, writeErr
		}
	}
	digest := sha256.Sum256(artifactBytes)
	result := computer.RunResult{
		RunID: request.RunID, TaskID: request.TaskID, Success: runtimeResult.Reason == schema.TerminalCompleted,
		Terminal: string(runtimeResult.Reason), Summary: strings.TrimSpace(runtimeResult.FinalMessage.Content),
		Evidence:    []string{"runtime result " + request.RunID, fmt.Sprintf("turns=%d tokens=%d", runtimeResult.Turns, runtimeResult.Usage.TotalTokens())},
		ArtifactURI: "file://" + artifactPath, ArtifactHash: hex.EncodeToString(digest[:]), StartedAt: runtimeResult.StartedAt, CompletedAt: runtimeResult.CompletedAt,
	}
	if result.Summary == "" {
		result.Summary = "runtime ended with " + result.Terminal
	}
	if request.Mode == computer.RunReview {
		result.Decision, result.Reason = parseReview(runtimeResult.FinalMessage.Content, result.Success)
	}
	return result, nil
}

func loadRuntimeResult(file string, request computer.RunRequest) (computer.RunResult, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return computer.RunResult{}, err
	}
	var runtimeResult engine.RunResult
	if err := json.Unmarshal(content, &runtimeResult); err != nil {
		return computer.RunResult{}, err
	}
	if runtimeResult.RunID != request.RunID || runtimeResult.CompletedAt.IsZero() {
		return computer.RunResult{}, errors.New("cached runtime result identity mismatch")
	}
	digest := sha256.Sum256(content)
	result := computer.RunResult{
		RunID: request.RunID, TaskID: request.TaskID, Success: runtimeResult.Reason == schema.TerminalCompleted,
		Terminal: string(runtimeResult.Reason), Summary: strings.TrimSpace(runtimeResult.FinalMessage.Content),
		Evidence: []string{"recovered durable runtime result " + request.RunID}, ArtifactURI: "file://" + file,
		ArtifactHash: hex.EncodeToString(digest[:]), StartedAt: runtimeResult.StartedAt, CompletedAt: runtimeResult.CompletedAt,
	}
	if result.Summary == "" {
		result.Summary = "runtime ended with " + result.Terminal
	}
	if request.Mode == computer.RunReview {
		result.Decision, result.Reason = parseReview(runtimeResult.FinalMessage.Content, result.Success)
	}
	return result, nil
}

func parseReview(content string, completed bool) (string, string) {
	var review struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(content)), &review) == nil {
		switch review.Decision {
		case "pass", "reject", "needs_human":
			if strings.TrimSpace(review.Reason) != "" {
				return review.Decision, strings.TrimSpace(review.Reason)
			}
		}
	}
	if completed {
		return "needs_human", "reviewer did not return the required structured decision"
	}
	return "needs_human", "review runtime did not complete"
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := w.limit - w.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = w.buffer.Write(value)
	}
	return original, nil
}

func (w *limitedBuffer) Bytes() []byte  { return w.buffer.Bytes() }
func (w *limitedBuffer) String() string { return w.buffer.String() }

func writeAtomic(name string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), ".runtime-result-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}

func contains(items []string, expected string) bool {
	for _, item := range items {
		if item == expected {
			return true
		}
	}
	return false
}
