//go:build !darwin && !linux

package sandbox

import "errors"

func Auto() (Backend, error) {
	return nil, errors.New("no supported OS sandbox backend is available")
}
