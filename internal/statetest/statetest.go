// Package statetest provides a real state.Store in a temporary directory whose
// writes can be made to fail on demand, for testing what happens when
// state.json cannot be written.
package statetest

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/state"
)

// ErrInjected is the error a failing Store returns from Update.
var ErrInjected = errors.New("statetest: injected write failure")

// Store is a state.Store whose Update can be switched to fail.
type Store struct {
	*state.Store
	fail atomic.Bool
}

// Open opens a store at path.
func Open(tb testing.TB, path string) *Store {
	tb.Helper()

	st, err := state.Open(path)
	if err != nil {
		tb.Fatalf("opening state store: %v", err)
	}
	return &Store{Store: st}
}

// New opens a store on a fresh file in a temporary directory.
func New(tb testing.TB) *Store {
	tb.Helper()
	return Open(tb, filepath.Join(tb.TempDir(), "state.json"))
}

// Fail makes every later Update fail with ErrInjected (true), or succeed
// again (false).
func (s *Store) Fail(fail bool) { s.fail.Store(fail) }

// Update is state.Store.Update, unless failures are switched on.
func (s *Store) Update(fn func(*state.State) error) error {
	if s.fail.Load() {
		return ErrInjected
	}
	return s.Store.Update(fn)
}
