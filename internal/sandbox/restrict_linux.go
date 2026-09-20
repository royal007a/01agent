//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// RestrictLinux applies a strict Landlock V4 filesystem policy and a seccomp
// network syscall deny-list to the current process before exec. ABI V4 is the
// newest baseline available on Linux 6.8, which is used by the deployment.
func RestrictLinux(workDir string) error {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("sandbox workspace must be a directory")
	}
	readOnly := existingPaths("/bin", "/usr", "/lib", "/lib64")
	rules := []landlock.Rule{
		landlock.RODirs(readOnly...),
		landlock.RWDirs(abs),
	}
	for _, path := range existingPaths(
		"/etc/ld.so.cache", "/etc/localtime", "/etc/passwd", "/etc/group",
		"/etc/ld-musl-aarch64.path", "/etc/ld-musl-x86_64.path",
	) {
		rules = append(rules, landlock.ROFiles(path))
	}
	for _, path := range existingPaths("/dev/null", "/dev/random", "/dev/urandom") {
		rules = append(rules, landlock.RWFiles(path))
	}
	if err := landlock.V4.Restrict(rules...); err != nil {
		return fmt.Errorf("apply Landlock V4 policy: %w", err)
	}
	if err := restrictNetworkSyscalls(); err != nil {
		return fmt.Errorf("apply seccomp network policy: %w", err)
	}
	return nil
}

func existingPaths(paths ...string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			result = append(result, path)
		}
	}
	return result
}
