//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadFileRejectsFIFOWithoutBlocking(t *testing.T) {
	workDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workDir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool, err := NewReadFileTool(workDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, executeErr := tool.Execute(context.Background(), json.RawMessage(`{"path":"pipe"}`))
		done <- executeErr
	}()
	select {
	case executeErr := <-done:
		if executeErr == nil {
			t.Fatal("FIFO was accepted as a regular file")
		}
	case <-time.After(time.Second):
		t.Fatal("read_file blocked while opening a FIFO")
	}
}
