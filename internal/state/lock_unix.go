//go:build !windows

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another cpms holds the state file.
var ErrLocked = errors.New("state file is in use by another cpms")

// Lock is an exclusive claim on a state file, held until Release or until the
// process exits. The kernel drops it with the process, so a crash never
// leaves a stale lock behind.
type Lock struct {
	f    *os.File
	path string
}

// LockPath claims the state file at path for this process. It locks a sibling
// "<path>.lock" file, not the state file itself, because the state file is
// replaced by rename on every write and a lock on the old inode would guard
// nothing. The lock file is removed on Release so a run that learned nothing
// leaves no trace.
func LockPath(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	lockPath := path + ".lock"
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening state lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("%w: %s. Stop the other cpms, or point this one at a different state file", ErrLocked, path)
			}
			return nil, fmt.Errorf("locking %s: %w", lockPath, err)
		}
		// The previous holder unlinks the file on Release. If that happened
		// between our open and our flock, we hold a lock on a file nobody can
		// find any more; try again on the current one.
		held, err1 := f.Stat()
		current, err2 := os.Stat(lockPath)
		if err1 == nil && err2 == nil && os.SameFile(held, current) {
			return &Lock{f: f, path: lockPath}, nil
		}
		_ = f.Close()
	}
}

// Release drops the claim.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = os.Remove(l.path) // while still holding the lock, see LockPath
		_ = l.f.Close()       // closing the descriptor releases the flock
	}
}
