// Package statetest provides a real state.Store in a temporary directory whose
// writes can be made to fail on demand, for testing what happens when
// state.json cannot be written.
package statetest

import (
	"errors"
	"path/filepath"
	"sync"
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

	// block is the gate the next Update waits at, if BlockNext set one.
	mu    sync.Mutex
	block *gate
}

// gate holds one Update: entered is closed when it arrives, release lets it
// through.
type gate struct {
	entered chan struct{}
	release chan struct{}
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

// BlockNext makes the next Update wait before it writes, once. entered is
// closed when that Update has arrived; release lets it go on. It is for tests
// that need something to happen while a write is under way.
func (s *Store) BlockNext() (entered <-chan struct{}, release func()) {
	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	s.block = g
	s.mu.Unlock()
	var once sync.Once
	return g.entered, func() { once.Do(func() { close(g.release) }) }
}

// Update is state.Store.Update, unless failures are switched on or a block
// is set.
func (s *Store) Update(fn func(*state.State) error) error {
	s.mu.Lock()
	g := s.block
	s.block = nil
	s.mu.Unlock()
	if g != nil {
		close(g.entered)
		<-g.release
	}
	if s.fail.Load() {
		return ErrInjected
	}
	return s.Store.Update(fn)
}
