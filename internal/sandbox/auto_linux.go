//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os/exec"
)

type linuxBackend struct{ helper string }

func Auto() (Backend, error) {
	path, err := exec.LookPath("01agent-sandbox")
	if err != nil {
		return nil, errors.New("01agent-sandbox helper is required for Bash on Linux")
	}
	return linuxBackend{helper: path}, nil
}

func (l linuxBackend) Name() string { return "landlock+seccomp" }

func (l linuxBackend) Command(ctx context.Context, request Request) (*exec.Cmd, error) {
	arguments := []string{"--workdir", request.WorkDir, "--", request.Executable}
	arguments = append(arguments, request.Arguments...)
	command := exec.CommandContext(ctx, l.helper, arguments...)
	command.Dir = request.WorkDir
	command.Env = append([]string(nil), request.Env...)
	return command, nil
}
