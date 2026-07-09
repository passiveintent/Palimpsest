/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package memstate implements ports.StateStore as an in-memory map with an
// optional gob-encoded snapshot file: Save updates memory immediately and
// (if a path is configured) persists the whole map via a write-to-temp,
// rename-into-place sequence so a crash mid-write never corrupts the
// on-disk snapshot; Load reads from memory only (call LoadFromDisk once at
// startup to repopulate it from a prior snapshot).
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package memstate

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"sync"
)

// Store is a ports.StateStore backed by an in-memory map, optionally
// mirrored to disk.
type Store struct {
	path string

	mu   sync.Mutex
	data map[string][]byte
}

// New returns a Store. If path is non-empty, Save additionally persists a
// gob snapshot of the whole key space to it after every call; call
// LoadFromDisk once at startup to restore from that snapshot.
func New(path string) *Store {
	return &Store{path: path, data: make(map[string][]byte)}
}

// Save implements ports.StateStore.
func (s *Store) Save(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	s.data[key] = append([]byte(nil), data...)
	snapshot := s.copyLocked()
	s.mu.Unlock()

	if s.path == "" {
		return nil
	}
	return persist(s.path, snapshot)
}

// Load implements ports.StateStore.
func (s *Store) Load(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// LoadFromDisk repopulates the Store's in-memory map from its gob snapshot
// file, if one exists. A missing file is not an error (fresh start).
func (s *Store) LoadFromDisk() error {
	if s.path == "" {
		return nil
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memstate: opening %s: %w", s.path, err)
	}
	defer f.Close()

	var m map[string][]byte
	if err := gob.NewDecoder(f).Decode(&m); err != nil {
		return fmt.Errorf("memstate: decoding %s: %w", s.path, err)
	}

	s.mu.Lock()
	s.data = m
	s.mu.Unlock()
	return nil
}

func (s *Store) copyLocked() map[string][]byte {
	out := make(map[string][]byte, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

func persist(path string, snapshot map[string][]byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("memstate: creating %s: %w", tmp, err)
	}
	if err := gob.NewEncoder(f).Encode(snapshot); err != nil {
		f.Close()
		return fmt.Errorf("memstate: encoding snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memstate: closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("memstate: renaming %s to %s: %w", tmp, path, err)
	}
	return nil
}
