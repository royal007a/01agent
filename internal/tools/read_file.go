package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/royal007a/01agent/internal/schema"
)

const (
	defaultReadLimit = int64(8_000)
	maximumReadLimit = int64(65_536)
)

type ReadFileTool struct {
	workDir string
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
}

func NewReadFileTool(workDir string) (*ReadFileTool, error) {
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workdir %q is not a directory", workDir)
	}
	return &ReadFileTool{workDir: filepath.Clean(resolved)}, nil
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name:        t.Name(),
		Description: "Read a byte range from a UTF-8 text file inside the workspace. Use offset and limit to page through large files.",
		InputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"minLength":   1,
					"description": "Path relative to the workspace, for example internal/engine/engine.go",
				},
				"offset": map[string]any{
					"type":        "integer",
					"minimum":     0,
					"description": "Zero-based byte offset; defaults to 0",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     maximumReadLimit,
					"description": "Maximum bytes to return; defaults to 8000",
				},
			},
			"required": []string{"path"},
		},
		Risk:         schema.RiskRead,
		ParallelSafe: true,
	}
}

func (t *ReadFileTool) Validate(arguments json.RawMessage) error {
	var input readFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if strings.TrimSpace(input.Path) == "" {
		return errorsForArgument("path", "must not be empty")
	}
	if filepath.IsAbs(input.Path) {
		return errorsForArgument("path", "must be relative to the workspace")
	}
	if input.Offset < 0 {
		return errorsForArgument("offset", "must be non-negative")
	}
	if input.Limit < 0 || input.Limit > maximumReadLimit {
		return errorsForArgument("limit", fmt.Sprintf("must be between 1 and %d when set", maximumReadLimit))
	}
	return nil
}

func (t *ReadFileTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input readFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", &Error{Code: "invalid_arguments", Message: err.Error(), Retryable: true}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	path, err := t.confinedPath(input.Path)
	if err != nil {
		return "", &Error{Code: "path_outside_workspace", Message: err.Error(), Retryable: true}
	}
	file, err := os.Open(path)
	if err != nil {
		return "", &Error{Code: "open_file", Message: err.Error(), Retryable: true}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", &Error{Code: "stat_file", Message: err.Error(), Retryable: true}
	}
	if !info.Mode().IsRegular() {
		return "", &Error{Code: "not_regular_file", Message: fmt.Sprintf("%q is not a regular file", input.Path), Retryable: true}
	}
	if input.Offset > info.Size() {
		return "", &Error{Code: "offset_out_of_range", Message: fmt.Sprintf("offset %d exceeds file size %d", input.Offset, info.Size()), Retryable: true}
	}

	limit := input.Limit
	if limit == 0 {
		limit = defaultReadLimit
	}
	if _, err := file.Seek(input.Offset, io.SeekStart); err != nil {
		return "", &Error{Code: "seek_file", Message: err.Error(), Retryable: true}
	}

	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", &Error{Code: "read_file", Message: err.Error(), Retryable: true}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	truncated := int64(len(content)) > limit
	if truncated {
		content = content[:limit]
		for len(content) > 0 && !utf8.Valid(content) {
			content = content[:len(content)-1]
		}
	}
	text := strings.ToValidUTF8(string(content), "�")
	if truncated || input.Offset+int64(len(content)) < info.Size() {
		next := input.Offset + int64(len(content))
		text += fmt.Sprintf("\n\n...[truncated: file_size=%d bytes, next_offset=%d]...", info.Size(), next)
	}
	return text, nil
}

func (t *ReadFileTool) confinedPath(input string) (string, error) {
	candidate := filepath.Clean(filepath.Join(t.workDir, input))
	if !inside(t.workDir, candidate) {
		return "", fmt.Errorf("path %q escapes workspace", input)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !inside(t.workDir, resolved) {
		return "", fmt.Errorf("path %q resolves outside workspace", input)
	}
	return resolved, nil
}

func inside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func errorsForArgument(name, message string) error {
	return fmt.Errorf("%s %s", name, message)
}
