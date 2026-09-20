//go:build darwin

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinSandboxConfinesFilesAndNetwork(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := Auto()
	if err != nil {
		t.Fatal(err)
	}
	run := func(script string) ([]byte, error) {
		command, err := backend.Command(context.Background(), Request{
			Executable: "/bin/bash", Arguments: []string{"--noprofile", "--norc", "-c", script},
			WorkDir: workDir, Env: []string{"PATH=/usr/bin:/bin", "TMPDIR=" + workDir},
		})
		if err != nil {
			return nil, err
		}
		return command.CombinedOutput()
	}
	if output, err := run("printf ok > allowed.txt"); err != nil {
		t.Fatalf("workspace write failed: %v: %s", err, output)
	}
	if _, err := run("cat " + secret); err == nil {
		t.Fatal("sandbox read a file outside the workspace")
	}
	if _, err := run("curl --max-time 1 http://127.0.0.1:9 >/dev/null 2>&1"); err == nil {
		t.Fatal("sandbox opened a network connection")
	}
}
