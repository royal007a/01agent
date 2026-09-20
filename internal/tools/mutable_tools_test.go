package tools

import (
	"context"
	"encoding/json"
	"errors"
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
