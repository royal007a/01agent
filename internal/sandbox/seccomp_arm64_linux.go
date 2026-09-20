//go:build linux && arm64

package sandbox

const (
	auditArchitecture            = 0xc00000b7
	seccompArchitectureSupported = true
)
