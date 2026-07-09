/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package fswatch implements a ports.FrameSource that polls a directory
// for wire-encoded frame files (the convention internal/adapters/jsonl's
// counterpart on the encode side — e.g. the otel/processor/csresidual
// fileFrameSink — writes: one file per frame). It never assumes inotify or
// any other OS-specific filesystem event API, matching ADR-007's stdlib-only
// guardrail.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package fswatch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// processedSubdir is where successfully-decoded frame files are moved after
// handling, so a restart never reprocesses them and an operator can still
// inspect what was consumed (a reversible move, not a delete).
const processedSubdir = ".processed"

// invalidSuffix marks a file that failed to decode as a wire frame: moved
// aside (once) rather than retried forever on every poll.
const invalidSuffix = ".invalid"

// Source polls Dir every PollInterval for new "*.plmp" files.
//
// Known limitation: Source assumes each frame file appears atomically
// (e.g. written to a temp name and renamed into Dir by the producer). A
// producer that writes directly into Dir risks Source reading a
// partially-written file; such a file will fail wire.Unmarshal and be
// moved aside as invalid rather than retried, so producers should write
// elsewhere and rename into Dir.
type Source struct {
	Dir          string
	PollInterval time.Duration

	// OnError, if non-nil, is called for every per-file error (a read
	// failure, a decode failure, a rename failure); Run itself never
	// returns a non-nil error for these, only for a directory it cannot
	// create/list at all.
	OnError func(path string, err error)
}

// New returns a Source polling dir every pollInterval (a non-positive
// interval falls back to 1s).
func New(dir string, pollInterval time.Duration) *Source {
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	return &Source{Dir: dir, PollInterval: pollInterval}
}

// Run implements ports.FrameSource. It polls immediately, then on every
// PollInterval tick, until ctx is canceled.
func (s *Source) Run(ctx context.Context, handle func(*wire.Frame)) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	processedDir := filepath.Join(s.Dir, processedSubdir)
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		return err
	}

	s.pollOnce(processedDir, handle)

	ticker := time.NewTicker(s.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.pollOnce(processedDir, handle)
		}
	}
}

func (s *Source) pollOnce(processedDir string, handle func(*wire.Frame)) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		s.onError(s.Dir, err)
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".plmp") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // best-effort deterministic processing order

	for _, name := range names {
		path := filepath.Join(s.Dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			s.onError(path, err) // transient (e.g. still being written); retried next poll
			continue
		}
		f, err := wire.Unmarshal(data)
		if err != nil {
			s.onError(path, err)
			_ = os.Rename(path, filepath.Join(processedDir, name+invalidSuffix))
			continue
		}
		handle(f)
		if err := os.Rename(path, filepath.Join(processedDir, name)); err != nil {
			s.onError(path, err)
		}
	}
}

func (s *Source) onError(path string, err error) {
	if s.OnError != nil {
		s.OnError(path, err)
	}
}
