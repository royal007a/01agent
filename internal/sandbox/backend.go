package sandbox

import (
	"context"
	"os/exec"
)

type Request struct {
	Executable string
	Arguments  []string
	WorkDir    string
	Env        []string
}

type Backend interface {
	Name() string
	Command(context.Context, Request) (*exec.Cmd, error)
}
