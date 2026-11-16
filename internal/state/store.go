package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store is the state file and its in-memory copy. Every Update is written to
// disk before it becomes visible to Get.
type Store struct {
	path string

	mu    sync.Mutex
	state State

	// rename is os.Rename; tests replace it to simulate a failed write.
	rename func(oldpath, newpath string) error
}

// Open loads the state file at path. A missing file is not an error: it gives
// an empty state, and nothing is created until the first Update, so a tool
// that has never learned anything leaves no trace.
//
// A file that cannot be parsed, or that a newer cpms wrote, is an error and is
// never overwritten: it may hold the only copy of the counterparty's token.
func Open(path string) (*Store, error) {
	s := &Store{path: path, state: empty(), rename: os.Rename}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file %s: %w", path, err)
	}

	st, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("state file %s %w; cpms will not overwrite it. "+
			"Move it aside to start with empty state", path, err)
	}
	s.state = st
	return s, nil
}

// parse decodes a state file. Its errors complete the sentence "state file
// <path> ...".
func parse(data []byte) (State, error) {
	// The version is read on its own first: a newer schema may not decode into
	// this one's types, and "written by a newer cpms" is more useful than
	// whatever type mismatch that would produce.
	var header struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return State{}, jsonProblem(data, err)
	}
	switch {
	case header.Version == nil:
		return State{}, errors.New("has no version field")
	case *header.Version > Version:
		return State{}, fmt.Errorf("has version %d and was written by a newer cpms (this one reads version %d)",
			*header.Version, Version)
	case *header.Version < 1:
		return State{}, fmt.Errorf("has unsupported version %d (this cpms reads versions 1 to %d)",
			*header.Version, Version)
	}
	// Version 1 lacks only fields that decode as their zero value (see
	// Version), and normalize stamps the current version, so the next write
	// migrates the file.

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, jsonProblem(data, err)
	}
	st.normalize()
	return st, nil
}

// jsonProblem describes a decode failure, with a line number when the decoder
// gives an offset: the operator is going to open the file in an editor.
func jsonProblem(data []byte, err error) error {
	var offset int64 = -1
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		offset = syntaxErr.Offset
	case errors.As(err, &typeErr):
		offset = typeErr.Offset
	}
	if offset < 0 {
		return fmt.Errorf("is not valid JSON: %w", err)
	}
	line := 1 + bytes.Count(data[:min(int(offset), len(data))], []byte("\n"))
	return fmt.Errorf("is not valid JSON (line %d): %w", line, err)
}

// Path is the file this store reads and writes.
func (s *Store) Path() string { return s.path }

// Get returns a deep copy of the current state; changing it does not change
// the store.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.clone()
}

// Update applies fn to a copy of the state and persists the result. If fn
// returns an error, or the write fails, neither the file nor the in-memory
// state changes and the error is returned. fn must not keep the pointer it is
// given.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.state.clone()
	if err := fn(&next); err != nil {
		return err
	}
	next.normalize()

	if err := s.write(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

// write replaces the state file atomically: a reader, or a restart after a
// crash, sees either the old file or the new one, never a torn mix.
func (s *Store) write(st State) (err error) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	// People will cat this file.
	data = append(data, '\n')

	// Same directory, because a rename across filesystems is not atomic.
	// CreateTemp opens the file 0600, which is what we want for a file
	// holding a bearer token.
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing state file %s: %w", s.path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("writing state file %s: %w", s.path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("writing state file %s: %w", s.path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("writing state file %s: %w", s.path, err)
	}
	if err = s.rename(tmp, s.path); err != nil {
		return fmt.Errorf("replacing state file %s: %w", s.path, err)
	}

	// The rename is the commit point: the new file is in place and is what a
	// restart will read. Syncing the directory only makes that survive a power
	// cut, so a failure here is not reported as a failed update, which would
	// leave memory disagreeing with disk.
	_ = syncDir(dir)
	return nil
}
