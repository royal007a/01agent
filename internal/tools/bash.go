package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	agentsandbox "github.com/royal007a/01agent/internal/sandbox"
	"github.com/royal007a/01agent/internal/schema"
)

const maximumCommandOutput = 64 << 10

type BashTool struct {
	workDir string
	tempDir string
	shell   string
	sandbox agentsandbox.Backend
}

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

func NewBashTool(workDir string) (*BashTool, error) {
	backend, err := agentsandbox.Auto()
	if err != nil {
		return nil, err
	}
	return NewBashToolWithSandbox(workDir, backend)
}

func NewBashToolWithSandbox(workDir string, backend agentsandbox.Backend) (*BashTool, error) {
	if backend == nil {
		return nil, fmt.Errorf("bash sandbox backend is required")
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("bash workspace must be a directory")
	}
	tempDir := filepath.Join(abs, ".01agent-tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox temporary directory: %w", err)
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("bash executable is required: %w", err)
	}
	return &BashTool{workDir: abs, tempDir: tempDir, shell: shell, sandbox: backend}, nil
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Run a Bash command in a platform sandbox confined to the workspace with networking denied. This tool is dangerous and requires explicit per-run approval.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "minLength": 1, "maxLength": 8192},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 120000},
			},
			"required": []string{"command"},
		},
		Risk: schema.RiskExecute,
	}
}

func (t *BashTool) Validate(arguments json.RawMessage) error {
	var input bashArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if input.Command == "" {
		return fmt.Errorf("command must not be empty")
	}
	return nil
}

func (t *BashTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input bashArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	timeout := 30 * time.Second
	if input.TimeoutMS > 0 {
		timeout = time.Duration(input.TimeoutMS) * time.Millisecond
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command, err := t.sandbox.Command(commandCtx, agentsandbox.Request{
		Executable: t.shell, Arguments: []string{"--noprofile", "--norc", "-c", input.Command},
		WorkDir: t.workDir, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TMPDIR=" + t.tempDir},
	})
	if err != nil {
		return "", &Error{Code: "sandbox_unavailable", Message: err.Error(), Fatal: true}
	}
	output := &cappedBuffer{limit: maximumCommandOutput}
	command.Stdout = output
	command.Stderr = output
	err = command.Run()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	result := fmt.Sprintf("exit_code=%d\n%s", command.ProcessState.ExitCode(), output.String())
	if output.truncated {
		result += "\n...[output truncated]..."
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 126 && strings.Contains(output.String(), "01agent-sandbox:") {
			return "", &Error{Code: "sandbox_unavailable", Message: strings.TrimSpace(output.String()), Fatal: true}
		}
		return result, &Error{Code: "command_failed", Message: result, Retryable: true}
	}
	return result, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(data)
	return original, nil
}
