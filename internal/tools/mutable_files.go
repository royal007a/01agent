package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/royal007a/01agent/internal/schema"
)

const maximumMutableFileBytes = 1 << 20

type WriteFileTool struct{ workspaceRoot }
type EditFileTool struct{ workspaceRoot }

type workspaceRoot struct {
	workDir string
	root    *os.Root
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func NewWriteFileTool(workDir string) (*WriteFileTool, error) {
	root, err := newWorkspaceRoot(workDir)
	if err != nil {
		return nil, err
	}
	return &WriteFileTool{workspaceRoot: root}, nil
}

func NewEditFileTool(workDir string) (*EditFileTool, error) {
	root, err := newWorkspaceRoot(workDir)
	if err != nil {
		return nil, err
	}
	return &EditFileTool{workspaceRoot: root}, nil
}

func newWorkspaceRoot(workDir string) (workspaceRoot, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return workspaceRoot{}, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return workspaceRoot{}, err
	}
	return workspaceRoot{workDir: filepath.Clean(abs), root: root}, nil
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Definition() schema.ToolDefinition {
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Atomically create or replace a UTF-8 text file inside the workspace. Requires explicit approval.",
		InputSchema: mutableFileSchema(false), Risk: schema.RiskWrite,
	}
}

func (t *WriteFileTool) Validate(arguments json.RawMessage) error {
	var input writeFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if _, err := confinedRelative(input.Path); err != nil {
		return err
	}
	if len(input.Content) > maximumMutableFileBytes {
		return fmt.Errorf("content exceeds %d bytes", maximumMutableFileBytes)
	}
	return nil
}

func (t *WriteFileTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input writeFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := confinedRelative(input.Path)
	if err != nil {
		return "", &Error{Code: "path_outside_workspace", Message: err.Error(), Fatal: true}
	}
	if err := t.root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", &Error{Code: "create_parent", Message: err.Error(), Retryable: true}
	}
	if err := atomicWrite(t.root, path, []byte(input.Content), 0o600); err != nil {
		return "", &Error{Code: "write_file", Message: err.Error(), Retryable: true}
	}
	return fmt.Sprintf(`{"path":%q,"bytes":%d}`, path, len(input.Content)), nil
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Definition() schema.ToolDefinition {
	schemaDef := mutableFileSchema(true)
	return schema.ToolDefinition{
		Name: t.Name(), Description: "Atomically replace an exact text fragment in a workspace file. Requires explicit approval.",
		InputSchema: schemaDef, Risk: schema.RiskWrite,
	}
}

func (t *EditFileTool) Validate(arguments json.RawMessage) error {
	var input editFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return err
	}
	if _, err := confinedRelative(input.Path); err != nil {
		return err
	}
	if input.OldText == "" {
		return fmt.Errorf("old_text must not be empty")
	}
	if len(input.NewText) > maximumMutableFileBytes {
		return fmt.Errorf("new_text exceeds %d bytes", maximumMutableFileBytes)
	}
	return nil
}

func (t *EditFileTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var input editFileArgs
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	path, err := confinedRelative(input.Path)
	if err != nil {
		return "", &Error{Code: "path_outside_workspace", Message: err.Error(), Fatal: true}
	}
	file, err := t.root.OpenFile(path, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return "", &Error{Code: "open_file", Message: err.Error(), Retryable: true}
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > maximumMutableFileBytes {
		file.Close()
		return "", &Error{Code: "invalid_file", Message: "target must be a regular file no larger than 1 MiB", Fatal: true}
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maximumMutableFileBytes+1))
	closeErr := file.Close()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if readErr != nil || closeErr != nil {
		return "", &Error{Code: "read_file", Message: errorsJoin(readErr, closeErr), Retryable: true}
	}
	count := strings.Count(string(content), input.OldText)
	if count == 0 {
		return "", &Error{Code: "no_match", Message: "old_text was not found", Retryable: true}
	}
	if count > 1 && !input.ReplaceAll {
		return "", &Error{Code: "ambiguous_match", Message: fmt.Sprintf("old_text matched %d times; provide more context or set replace_all", count), Retryable: true}
	}
	limit := 1
	if input.ReplaceAll {
		limit = -1
	}
	updated := strings.Replace(string(content), input.OldText, input.NewText, limit)
	if len(updated) > maximumMutableFileBytes {
		return "", &Error{Code: "file_too_large", Message: "edited file exceeds 1 MiB", Fatal: true}
	}
	if err := atomicWrite(t.root, path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", &Error{Code: "write_file", Message: err.Error(), Retryable: true}
	}
	replacements := 1
	if input.ReplaceAll {
		replacements = count
	}
	return fmt.Sprintf(`{"path":%q,"replacements":%d,"bytes":%d}`, path, replacements, len(updated)), nil
}

func mutableFileSchema(edit bool) map[string]any {
	properties := map[string]any{
		"path": map[string]any{"type": "string", "minLength": 1},
	}
	required := []string{"path"}
	if edit {
		properties["old_text"] = map[string]any{"type": "string", "minLength": 1}
		properties["new_text"] = map[string]any{"type": "string"}
		properties["replace_all"] = map[string]any{"type": "boolean"}
		required = append(required, "old_text", "new_text")
	} else {
		properties["content"] = map[string]any{"type": "string"}
		required = append(required, "content")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func confinedRelative(path string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be non-empty and relative")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace", path)
	}
	return clean, nil
}

func atomicWrite(root *os.Root, path string, content []byte, mode os.FileMode) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(path), ".01agent-"+hex.EncodeToString(suffix[:])+".tmp")
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errorsJoinError(writeErr, syncErr, closeErr); err != nil {
		_ = root.Remove(temporary)
		return err
	}
	if err := root.Rename(temporary, path); err != nil {
		_ = root.Remove(temporary)
		return err
	}
	return nil
}

func errorsJoin(items ...error) string {
	if err := errorsJoinError(items...); err != nil {
		return err.Error()
	}
	return ""
}

func errorsJoinError(items ...error) error {
	var messages []string
	for _, item := range items {
		if item != nil {
			messages = append(messages, item.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}
