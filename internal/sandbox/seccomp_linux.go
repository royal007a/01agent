//go:build linux

package sandbox

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

func restrictNetworkSyscalls() error {
	if !seccompArchitectureSupported {
		return errors.New("seccomp architecture is not supported")
	}
	filters := []unix.SockFilter{
		statement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, auditArchitecture, 1, 0),
		statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		statement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0),
	}
	blocked := []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_CONNECT, unix.SYS_ACCEPT,
		unix.SYS_ACCEPT4, unix.SYS_BIND, unix.SYS_LISTEN, unix.SYS_SENDTO,
		unix.SYS_RECVFROM, unix.SYS_SENDMSG, unix.SYS_RECVMSG, unix.SYS_SHUTDOWN,
		unix.SYS_GETSOCKNAME, unix.SYS_GETPEERNAME, unix.SYS_SETSOCKOPT, unix.SYS_GETSOCKOPT,
	}
	for _, number := range blocked {
		filters = append(filters,
			jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, number, 0, 1),
			statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		)
	}
	filters = append(filters, statement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0)
}

func statement(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func jump(code uint16, value uint32, yes, no uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: yes, Jf: no, K: value}
}
