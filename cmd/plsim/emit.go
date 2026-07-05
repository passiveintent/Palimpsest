/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/purushpsm147/palimpsest/pkg/wire"
)

// channel delivers one emitter's frames to --frames-out, simulating
// network conditions: a fixed per-emitter clock skew plus random
// per-frame latency jitter (both realized as an actual delay before the
// file lands, so palimpsestd's wall-clock-based clock-skew tracking sees
// real skew), a probabilistic duplicate-rate resend, and an optional
// partition (withhold-then-release-shuffled) window.
//
// All randomness (delay amounts, duplicate decisions, shuffle order) is
// drawn from rnd synchronously inside Send/EndPartition — both of which
// must only ever be called from a single goroutine (plsim's main loop);
// the background goroutines Send spawns only sleep and write bytes to
// already-decided paths, touching no shared mutable state.
type channel struct {
	dir           string
	rnd           *rand.Rand
	skew          time.Duration
	jitter        time.Duration
	duplicateRate float64

	partitioned bool
	buffered    [][]byte

	fileSeq uint64 // monotonic, for unique duplicate-file suffixes
}

func newChannel(dir string, skew, jitter time.Duration, duplicateRate float64, rnd *rand.Rand) *channel {
	return &channel{dir: dir, rnd: rnd, skew: skew, jitter: jitter, duplicateRate: duplicateRate}
}

// Send marshals f and delivers it: buffered (if a partition is active) or
// written after this emitter's skew + a random jitter delay, with an
// independent duplicateRate chance of also writing an exact byte-for-byte
// second copy under a different filename.
func (c *channel) Send(f *wire.Frame) error {
	b, err := wire.Marshal(f)
	if err != nil {
		return fmt.Errorf("plsim: marshal frame: %w", err)
	}

	if c.partitioned {
		c.buffered = append(c.buffered, b)
		return nil
	}

	delay := c.nextDelay()
	dup := c.rnd.Float64() < c.duplicateRate
	c.deliver(b, f, delay, dup)
	return nil
}

// nextDelay computes this frame's total artificial latency: a fixed skew
// plus a uniform-random jitter component in [0, jitter].
func (c *channel) nextDelay() time.Duration {
	d := c.skew
	if c.jitter > 0 {
		d += time.Duration(c.rnd.Int63n(int64(c.jitter) + 1))
	}
	return d
}

// deliver writes b (and, if dup, a second identical copy under a
// different name, after its own independent delay) to c.dir after delay,
// in background goroutines so the caller's window loop never blocks on
// simulated network latency.
func (c *channel) deliver(b []byte, f *wire.Frame, delay time.Duration, dup bool) {
	name := frameFileName(f, atomic.AddUint64(&c.fileSeq, 1))
	go func() {
		time.Sleep(delay)
		writeFrameFileNow(c.dir, name, b)
	}()
	if !dup {
		return
	}
	// A real duplicate delivery (network retry) wouldn't consistently
	// arrive back-to-back with the original, so give it its own small
	// additional delay rather than reusing exactly `delay`. A fresh
	// disambiguator alone makes the name unique and .plmp-suffixed (must
	// match fswatch's glob, or the duplicate would silently never be
	// ingested at all).
	dupName := frameFileName(f, atomic.AddUint64(&c.fileSeq, 1))
	go func() {
		time.Sleep(delay + 15*time.Millisecond)
		writeFrameFileNow(c.dir, dupName, b)
	}()
}

// BeginPartition stops delivering this emitter's frames: subsequent Send
// calls buffer instead of writing, simulating a network outage.
func (c *channel) BeginPartition() {
	c.partitioned = true
}

// EndPartition releases every frame buffered during the partition, in
// shuffled order (simulating out-of-order redelivery once connectivity
// returns) with an independent duplicateRate chance per buffered frame of
// also writing a second copy — directly exercising ADR-013's
// reorder/duplicate/repair path. All buffered frames are written with a
// short, staggered delay so they don't hit the filesystem in one instant
// burst.
func (c *channel) EndPartition() {
	c.partitioned = false
	frames := c.buffered
	c.buffered = nil

	c.rnd.Shuffle(len(frames), func(i, j int) { frames[i], frames[j] = frames[j], frames[i] })

	for i, b := range frames {
		delay := time.Duration(i) * 20 * time.Millisecond
		name := fmt.Sprintf("partition-release-%016x.plmp", atomic.AddUint64(&c.fileSeq, 1))
		go func(b []byte, name string, delay time.Duration) {
			time.Sleep(delay)
			writeFrameFileNow(c.dir, name, b)
		}(b, name, delay)

		if c.rnd.Float64() < c.duplicateRate {
			// Must still end in .plmp (fswatch's glob), or the duplicate
			// would silently never be ingested at all.
			dupName := fmt.Sprintf("partition-release-%016x.plmp", atomic.AddUint64(&c.fileSeq, 1))
			go func(b []byte, name string, delay time.Duration) {
				time.Sleep(delay + 20*time.Millisecond)
				writeFrameFileNow(c.dir, name, b)
			}(b, dupName, delay)
		}
	}
}

// frameFileName mirrors otel/processor/csresidual's fileFrameSink naming
// convention, with a caller-supplied disambiguator appended so plsim's
// concurrent/duplicate writers never collide on the same path.
func frameFileName(f *wire.Frame, disambiguator uint64) string {
	return fmt.Sprintf("emitter%016x-shard%016x-epoch%016x-view%04x-seq%08x-%016x.plmp",
		f.EmitterID, f.ShardID, f.Epoch, f.ViewID, f.Seq, disambiguator)
}

// writeFrameFileNow writes b to dir/name via a temp file + rename, per
// internal/adapters/fswatch's documented requirement that producers never
// write directly into the watched directory (a partially-written file
// would otherwise race a poll).
func writeFrameFileNow(dir, name string, b []byte) {
	final := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, final)
}
