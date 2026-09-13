package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFilePagesContent(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("abcdefghij"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}

	output, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"hello.txt","offset":2,"limit":4}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output, "cdef") {
		t.Fatalf("output = %q, want cdef prefix", output)
	}
	if !strings.Contains(output, "next_offset=6") {
		t.Fatalf("output = %q, want pagination marker", output)
	}
}

func TestReadFileDoesNotSplitUTF8AtPageBoundary(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "unicode.txt"), []byte("a你b"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"unicode.txt","limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output, "a\n\n") || !strings.Contains(output, "next_offset=1") {
		t.Fatalf("output = %q, want valid UTF-8 page ending at byte 1", output)
	}
}

func TestReadFileRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"../secret.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("error = %v, want workspace escape", err)
	}
}

func TestReadFileRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(workDir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	tool, err := NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}

	_, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"link.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Fatalf("error = %v, want symlink escape", err)
	}
}
