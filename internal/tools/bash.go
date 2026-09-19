package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/royal007a/01agent/internal/schema"
)

const maximumCommandOutput = 64 << 10

type BashTool struct {
	workDir string
	shell   string
}

type bashArgs struct {
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

func NewBashTool(workDir string) (*BashTool, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("bash workspace must be a directory")
	}
	shell, err := exec.LookPath("bash")
	if err != nil {
		return nil, fmt.Errorf("bash executable is required: %w", err)
	}
	return &BashTool{workDir: abs, shell: shell}, nil
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Run a Bash command in the workspace. This tool is dangerous and requires explicit per-run approval.",
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
	command := exec.CommandContext(commandCtx, t.shell, "--noprofile", "--norc", "-c", input.Command)
	command.Dir = t.workDir
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TMPDIR=" + os.TempDir()}
	output := &cappedBuffer{limit: maximumCommandOutput}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	result := fmt.Sprintf("exit_code=%d\n%s", command.ProcessState.ExitCode(), output.String())
	if output.truncated {
		result += "\n...[output truncated]..."
	}
	if err != nil {
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
