//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type darwinBackend struct{ executable string }

func Auto() (Backend, error) {
	path, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, errors.New("sandbox-exec is required for Bash on macOS")
	}
	return darwinBackend{executable: path}, nil
}

func (d darwinBackend) Name() string { return "sandbox-exec" }

func (d darwinBackend) Command(ctx context.Context, request Request) (*exec.Cmd, error) {
	workDir, err := filepath.Abs(request.WorkDir)
	if err != nil {
		return nil, err
	}
	// macOS exposes temporary directories through /var, which is a symlink to
	// /private/var.  Sandbox profile paths are matched after vnode resolution,
	// so use the canonical path for both the policy and the child cwd.
	workDir, err = filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	profile := darwinProfile(workDir)
	arguments := []string{"-p", profile, request.Executable}
	arguments = append(arguments, request.Arguments...)
	command := exec.CommandContext(ctx, d.executable, arguments...)
	command.Dir = workDir
	command.Env = append([]string(nil), request.Env...)
	return command, nil
}

func darwinProfile(workDir string) string {
	readOnly := []string{"/System", "/usr", "/bin", "/sbin", "/Library/Apple", "/private/etc"}
	var rules []string
	for _, path := range readOnly {
		rules = append(rules, fmt.Sprintf("(allow file-read* (subpath %s))", strconv.Quote(path)))
	}
	rules = append(rules,
		fmt.Sprintf("(allow file-read* file-write* (subpath %s))", strconv.Quote(workDir)),
		"(allow file-read* file-write* (literal \"/dev/null\"))",
		"(allow file-read* (literal \"/dev/random\") (literal \"/dev/urandom\"))",
	)
	return "(version 1)\n(deny default)\n(allow process*)\n(allow signal (target self))\n(allow sysctl-read)\n(allow mach-lookup)\n(allow file-read-metadata)\n(allow file-read-data (literal \"/\"))\n(deny network*)\n" + strings.Join(rules, "\n")
}
