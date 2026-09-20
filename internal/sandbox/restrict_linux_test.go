//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLinuxSandboxConfinesFilesAndAllSockets(t *testing.T) {
	if os.Getenv("GO_WANT_LINUX_SANDBOX_CHILD") == "1" {
		workDir := os.Getenv("SANDBOX_WORKDIR")
		secret := os.Getenv("SANDBOX_SECRET")
		if err := RestrictLinux(workDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(10)
		}
		if err := os.WriteFile(filepath.Join(workDir, "allowed.txt"), []byte("ok"), 0o600); err != nil {
			os.Exit(11)
		}
		command := exec.Command("/bin/sh", "-c", "printf child > child.txt")
		command.Dir = workDir
		if output, err := command.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "sandboxed child process failed: %v: %s\n", err, output)
			os.Exit(15)
		}
		if _, err := os.ReadFile(secret); err == nil {
			os.Exit(12)
		}
		for _, network := range []string{"tcp", "udp"} {
			connection, err := net.DialTimeout(network, "127.0.0.1:9", time.Second)
			if connection != nil {
				connection.Close()
				os.Exit(13)
			}
			if !errors.Is(err, syscall.EPERM) {
				os.Exit(14)
			}
		}
		os.Exit(0)
	}
	parent := t.TempDir()
	workDir := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestLinuxSandboxConfinesFilesAndAllSockets")
	command.Env = append(os.Environ(),
		"GO_WANT_LINUX_SANDBOX_CHILD=1", "SANDBOX_WORKDIR="+workDir, "SANDBOX_SECRET="+secret,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandbox subprocess failed: %v output=%s", err, output)
	}
}
