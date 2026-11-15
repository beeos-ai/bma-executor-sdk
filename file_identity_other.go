//go:build !linux

package bmaexecutor

import (
	"errors"
)

// File identity changes are supported on Linux only.
func withFileIdentity(*RunAs, func() error) error {
	return errors.New("a separate workload user is supported on linux only")
}
