//go:build windows

package state

import "errors"

// ErrLocked means another cpms holds the state file.
var ErrLocked = errors.New("state file is in use by another cpms")

// Lock is not enforced on Windows; the control socket guard still applies.
type Lock struct{}

// LockPath is a no-op on Windows.
func LockPath(string) (*Lock, error) { return &Lock{}, nil }

// Release does nothing.
func (*Lock) Release() {}
