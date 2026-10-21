//go:build !windows

package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockPathIsExclusivePerStateFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")

	first, err := LockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockPath(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock err = %v, want ErrLocked", err)
	}

	first.Release()
	again, err := LockPath(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again.Release()
}
