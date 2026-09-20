package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentsandbox "github.com/royal007a/01agent/internal/sandbox"
	"github.com/royal007a/01agent/internal/schema"
)

func TestWriteAndEditRequireApprovalAndStayInWorkspace(t *testing.T) {
	workDir := t.TempDir()
	writeTool, err := NewWriteFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := NewEditFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(WithPermissionPolicy(ApprovalPolicy{}))
	if err := registry.Register(writeTool); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(editTool); err != nil {
		t.Fatal(err)
	}
	writeCall := schema.ToolCall{ID: "w", Name: "write_file", Arguments: json.RawMessage(`{"path":"nested/file.txt","content":"hello"}`)}
	denied := registry.Execute(context.Background(), writeCall)
	if denied.ErrorCode != "approval_required" || !denied.Fatal {
		t.Fatalf("denied = %#v", denied)
	}
	if _, err := os.Stat(filepath.Join(workDir, "nested", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("file exists before approval: %v", err)
	}
	approved := WithApprovedTools(context.Background(), []string{"write_file", "edit_file"})
	if result := registry.Execute(approved, writeCall); result.IsError {
		t.Fatalf("write result = %#v", result)
	}
	editCall := schema.ToolCall{ID: "e", Name: "edit_file", Arguments: json.RawMessage(`{"path":"nested/file.txt","old_text":"hello","new_text":"world"}`)}
	if result := registry.Execute(approved, editCall); result.IsError {
		t.Fatalf("edit result = %#v", result)
	}
	content, err := os.ReadFile(filepath.Join(workDir, "nested", "file.txt"))
	if err != nil || string(content) != "world" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	escape := schema.ToolCall{ID: "escape", Name: "write_file", Arguments: json.RawMessage(`{"path":"../escape","content":"bad"}`)}
	if result := registry.Execute(approved, escape); !result.IsError {
		t.Fatalf("escape result = %#v", result)
	}
}

func TestEditRejectsAmbiguousMatch(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "x.txt"), []byte("x x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"x.txt","old_text":"x","new_text":"y"}`))
	if err == nil || !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("error = %v", err)
	}
}

func TestEditFuzzyMatchingLevelsPreserveFileConventions(t *testing.T) {
	tests := []struct {
		name     string
		original string
		oldText  string
		newText  string
		want     string
		level    string
	}{
		{
			name: "newline", original: "first\r\nsecond\r\n", oldText: "first\nsecond", newText: "one\ntwo",
			want: "one\r\ntwo\r\n", level: "newline",
		},
		{
			name: "outer blank lines", original: "before\nvalue\nafter\n", oldText: "\n\nvalue\n\n", newText: "changed",
			want: "before\nchanged\nafter\n", level: "outer_blank_lines",
		},
		{
			name: "indentation", original: "func run() {\n        if ready {\n                start()\n        }\n}\n",
			oldText: "if ready {\nstart()\n}", newText: "if ready {\n    stop()\n}",
			want: "func run() {\n        if ready {\n            stop()\n        }\n}\n", level: "indentation",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workDir := t.TempDir()
			path := filepath.Join(workDir, "target.txt")
			if err := os.WriteFile(path, []byte(test.original), 0o600); err != nil {
				t.Fatal(err)
			}
			tool, err := NewEditFileTool(workDir)
			if err != nil {
				t.Fatal(err)
			}
			arguments, _ := json.Marshal(editFileArgs{Path: "target.txt", OldText: test.oldText, NewText: test.newText})
			result, err := tool.Execute(context.Background(), arguments)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result, fmt.Sprintf(`"match_level":%q`, test.level)) {
				t.Fatalf("result=%s", result)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != test.want {
				t.Fatalf("content=%q want=%q err=%v", content, test.want, err)
			}
		})
	}
}

func TestEditFuzzyMatchMustRemainUnique(t *testing.T) {
	workDir := t.TempDir()
	original := "  if ready {\n    same()\n  }\n\n    if ready {\n      same()\n    }\n"
	if err := os.WriteFile(filepath.Join(workDir, "x.txt"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"x.txt","old_text":"if ready {\nsame()\n}","new_text":"changed()"}`))
	var toolErr *Error
	if !errors.As(err, &toolErr) || toolErr.Code != "ambiguous_match" || !strings.Contains(toolErr.Message, "indentation") {
		t.Fatalf("error=%#v", err)
	}
	content, readErr := os.ReadFile(filepath.Join(workDir, "x.txt"))
	if readErr != nil || string(content) != original {
		t.Fatalf("ambiguous edit changed file: content=%q err=%v", content, readErr)
	}
}

func TestEditReplaceAllDoesNotUseFuzzyMatching(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "x.txt"), []byte("  value\n  next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"x.txt","old_text":"value\nnext","new_text":"changed","replace_all":true}`))
	var toolErr *Error
	if !errors.As(err, &toolErr) || toolErr.Code != "no_match" {
		t.Fatalf("error=%#v", err)
	}
}

func TestEditRejectsStaleExpectedDigest(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "x.txt")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	stale := sha256.Sum256([]byte("stale"))
	arguments, _ := json.Marshal(editFileArgs{
		Path: "x.txt", OldText: "current", NewText: "updated", ExpectedSHA256: fmt.Sprintf("%x", stale),
	})
	_, err = tool.Execute(context.Background(), arguments)
	var toolErr *Error
	if !errors.As(err, &toolErr) || toolErr.Code != "edit_conflict" || !toolErr.Retryable {
		t.Fatalf("error=%#v", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "current" {
		t.Fatalf("stale edit changed file: content=%q err=%v", content, readErr)
	}
}

func TestBashRequiresApproval(t *testing.T) {
	tool, err := NewBashToolWithSandbox(t.TempDir(), directTestSandbox{})
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	registry := NewRegistry(WithPermissionPolicy(ApprovalPolicy{}))
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	call := schema.ToolCall{ID: "b", Name: "bash", Arguments: json.RawMessage(`{"command":"printf ok"}`)}
	if result := registry.Execute(context.Background(), call); result.ErrorCode != "approval_required" {
		t.Fatalf("unapproved result = %#v", result)
	}
	result := registry.Execute(WithApprovedTools(context.Background(), []string{"bash"}), call)
	if result.IsError || !strings.Contains(result.Output, "ok") {
		t.Fatalf("approved result = %#v", result)
	}
}

func TestBashFailsClosedWhenPlatformSandboxCannotInitialize(t *testing.T) {
	tool, err := NewBashToolWithSandbox(t.TempDir(), unavailableTestSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"printf should-not-run"}`))
	var toolErr *Error
	if !errors.As(err, &toolErr) || toolErr.Code != "sandbox_unavailable" || !toolErr.Fatal {
		t.Fatalf("error=%#v", err)
	}
}

type directTestSandbox struct{}

func (directTestSandbox) Name() string { return "test" }

func (directTestSandbox) Command(ctx context.Context, request agentsandbox.Request) (*exec.Cmd, error) {
	command := exec.CommandContext(ctx, request.Executable, request.Arguments...)
	command.Dir = request.WorkDir
	command.Env = request.Env
	return command, nil
}

type unavailableTestSandbox struct{}

func (unavailableTestSandbox) Name() string { return "unavailable-test" }

func (unavailableTestSandbox) Command(ctx context.Context, _ agentsandbox.Request) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", "printf '01agent-sandbox: kernel support missing' >&2; exit 126"), nil
}
