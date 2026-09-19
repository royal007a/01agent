//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import "syscall"

const nonblockFlag = syscall.O_NONBLOCK
