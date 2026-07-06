/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package kafka

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// Producer is the publish operation SpoolingSink wraps. *Sink satisfies it;
// tests substitute a fake that fails on command to simulate a broker outage
// deterministically, without needing to actually stop a broker.
type Producer interface {
	Send(ctx context.Context, f *wire.Frame) error
	Close() error
}

// spoolFileSuffix marks a file under SpoolingSink's dir as a spooled frame
// awaiting redelivery.
const spoolFileSuffix = ".plmp.spool"

// SpoolingSink wraps a Producer (normally a *Sink) with a bounded on-disk
// spool: a Send that fails (broker outage) is written to dir instead of
// being lost, and every subsequent Send first tries to drain any
// backlog — oldest frame first — before publishing the new one, so a
// reconnect flushes the backlog in original order per ADR-013's tolerance
// for late-but-ordered redelivery (the receiving engine's watermark/repair
// path already knows how to fold late frames back in, or drop them past
// RepairHorizon; SpoolingSink's only job is to eventually redeliver
// at-least-once, not to reason about lateness itself).
//
// Bounded disk: once the spool's total size would exceed maxBytes, further
// frames are dropped (not spooled) and Send returns an error — an operator
// deploying this in a lossy-telemetry pipeline is assumed to prefer a
// bounded, observable data gap over unbounded disk growth during a
// sustained outage.
//
// SpoolingSink is safe for concurrent use.
type SpoolingSink struct {
	producer Producer
	dir      string
	maxBytes int64

	mu           sync.Mutex
	spooledBytes int64
}

// NewSpoolingSink returns a SpoolingSink writing to dir (created if it
// doesn't exist) when producer.Send fails, bounded to maxBytes total. Any
// files already spooled in dir (e.g. from a prior process that crashed
// before draining) are counted against maxBytes and are the first thing a
// subsequent Send attempts to drain.
func NewSpoolingSink(producer Producer, dir string, maxBytes int64) (*SpoolingSink, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("kafka: creating spool dir %q: %w", dir, err)
	}
	entries, err := spoolFiles(dir)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return &SpoolingSink{producer: producer, dir: dir, maxBytes: maxBytes, spooledBytes: total}, nil
}

// Send implements ports.FrameSink. It first makes one best-effort attempt to
// drain any existing backlog (stopping at the first still-failing frame,
// left in place for the next attempt), then publishes f directly if the
// backlog is now empty, or spools f behind the backlog otherwise so ordering
// versus already-spooled frames is preserved.
func (s *SpoolingSink) Send(ctx context.Context, f *wire.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	drained, err := s.drainLocked(ctx)
	if err != nil {
		return s.spoolLocked(f)
	}
	if !drained {
		// Backlog remains (still down, or maxBytes already reached): keep
		// order by spooling behind it rather than racing ahead with f.
		return s.spoolLocked(f)
	}
	if err := s.producer.Send(ctx, f); err != nil {
		return s.spoolLocked(f)
	}
	return nil
}

// DrainOnce makes one best-effort pass over the backlog without publishing
// a new frame; the otel processor's start() runs this on a ticker so a
// quiet (shard, view) pipeline's backlog still drains promptly after a
// reconnect even if it has nothing new to flush. Caller must not hold any
// lock SpoolingSink itself needs (it takes its own).
func (s *SpoolingSink) DrainOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.drainLocked(ctx)
	return err
}

// drainLocked attempts to publish every spooled file, oldest first,
// removing each on success and stopping at the first failure (left spooled
// for the next attempt). Caller must hold s.mu. Returns drained=true only if
// the backlog is now fully empty.
func (s *SpoolingSink) drainLocked(ctx context.Context) (drained bool, err error) {
	if s.spooledBytes == 0 {
		// Nothing spooled (spoolLocked/removeLocked keep this exact,
		// seeded from a real directory scan at construction): skip the
		// directory listing on the common healthy-path Send.
		return true, nil
	}
	entries, err := spoolFiles(s.dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		path := filepath.Join(s.dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue // e.g. concurrently removed; not fatal, try the next
		}
		f, err := wire.Unmarshal(b)
		if err != nil {
			// Corrupt spool file: never retryable. Drop it rather than
			// wedging the whole backlog behind it forever.
			s.removeLocked(path, int64(len(b)))
			continue
		}
		if sendErr := s.producer.Send(ctx, f); sendErr != nil {
			return false, sendErr
		}
		s.removeLocked(path, int64(len(b)))
	}
	return true, nil
}

func (s *SpoolingSink) removeLocked(path string, size int64) {
	if err := os.Remove(path); err != nil {
		return
	}
	s.spooledBytes -= size
	if s.spooledBytes < 0 {
		s.spooledBytes = 0
	}
}

// spoolLocked writes f to dir, or returns an error without writing if doing
// so would exceed maxBytes. Caller must hold s.mu.
func (s *SpoolingSink) spoolLocked(f *wire.Frame) error {
	b, err := wire.Marshal(f)
	if err != nil {
		return fmt.Errorf("kafka: marshaling frame to spool: %w", err)
	}
	if s.maxBytes > 0 && s.spooledBytes+int64(len(b)) > s.maxBytes {
		return fmt.Errorf("kafka: spool budget (%d bytes) exceeded, dropping frame emitter=%016x shard=%016x view=%04x seq=%08x",
			s.maxBytes, f.EmitterID, f.ShardID, f.ViewID, f.Seq)
	}
	name := fmt.Sprintf("%020d-emitter%016x-shard%016x-epoch%016x-view%04x-seq%08x%s",
		time.Now().UnixNano(), f.EmitterID, f.ShardID, f.Epoch, f.ViewID, f.Seq, spoolFileSuffix)
	if err := os.WriteFile(filepath.Join(s.dir, name), b, 0o644); err != nil {
		return fmt.Errorf("kafka: writing spool file: %w", err)
	}
	s.spooledBytes += int64(len(b))
	return nil
}

// closeDrainTimeout bounds Close's own best-effort final drain attempt.
// Close takes no context (it mirrors io.Closer/ports.FrameSink.Close), so if
// the producer is still unreachable this is what keeps shutdown from
// hanging: a caller-supplied producer whose Send doesn't itself respect
// context cancellation (see Producer's doc) would otherwise block Close
// forever on a down broker.
const closeDrainTimeout = 5 * time.Second

// Close drains one final time (best-effort, bounded by closeDrainTimeout)
// and closes the underlying producer.
func (s *SpoolingSink) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeDrainTimeout)
	defer cancel()
	_ = s.DrainOnce(ctx)
	return s.producer.Close()
}

func spoolFiles(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("kafka: reading spool dir %q: %w", dir, err)
	}
	out := entries[:0]
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".spool" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() }) // filename-embedded nanosecond timestamp prefix sorts oldest first
	return out, nil
}
