//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"
	"syscall"

	"github.com/royal007a/01agent/internal/sandbox"
)

func main() {
	workDir := flag.String("workdir", "", "writable workspace boundary")
	flag.Parse()
	arguments := flag.Args()
	if *workDir == "" || len(arguments) == 0 {
		fmt.Fprintln(os.Stderr, "usage: 01agent-sandbox --workdir DIR -- COMMAND [ARGS...]")
		os.Exit(2)
	}
	if err := sandbox.RestrictLinux(*workDir); err != nil {
		fmt.Fprintln(os.Stderr, "01agent-sandbox:", err)
		os.Exit(126)
	}
	if err := syscall.Chdir(*workDir); err != nil {
		fmt.Fprintln(os.Stderr, "01agent-sandbox:", err)
		os.Exit(126)
	}
	if err := syscall.Exec(arguments[0], arguments, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "01agent-sandbox:", err)
		os.Exit(126)
	}
}
