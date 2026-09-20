//go:build linux && !amd64 && !arm64

package sandbox

const (
	auditArchitecture            = 0
	seccompArchitectureSupported = false
)
