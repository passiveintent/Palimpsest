/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import "time"

// Breaker bounds the rate of dictionary births a Tracker will accept: if a
// deployment crash-loops and mints a flood of distinct logical series
// names, the breaker trips rather than letting the dictionary — and the
// implicit column cache it drives — grow unbounded. A tripped Breaker
// turns a crash loop into a detected event (ADR-009) instead of silent
// cardinality blowup.
//
// Breaker is NOT safe for concurrent use.
type Breaker struct {
	maxBirths int
	window    time.Duration
	births    []time.Time // birth timestamps currently inside the rolling window, ascending
	tripped   bool
}

// NewBreaker returns a Breaker that trips once more than maxBirths births
// are observed within any window-length rolling interval.
func NewBreaker(maxBirths int, window time.Duration) *Breaker {
	return &Breaker{maxBirths: maxBirths, window: window}
}

// ObserveBirth records a birth attempt for id at now and reports whether it
// is accepted. Once the rolling birth count would exceed maxBirths, it
// returns false (and latches Tripped) instead of admitting the birth: the
// caller must fall back to aggregate-only handling for this sample rather
// than growing the dictionary further.
//
// id is accepted for symmetry with the rest of the lifecycle API and for
// future diagnostics; the rate limit itself does not depend on it since
// every birth is by definition a distinct ID.
func (b *Breaker) ObserveBirth(id uint64, now time.Time) bool {
	_ = id
	cutoff := now.Add(-b.window)
	i := 0
	for i < len(b.births) && b.births[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		b.births = append(b.births[:0], b.births[i:]...)
	}

	if len(b.births) >= b.maxBirths {
		b.tripped = true
		return false
	}
	b.births = append(b.births, now)
	return true
}

// Tripped reports whether the breaker has ever tripped since construction
// or the last Reset.
func (b *Breaker) Tripped() bool { return b.tripped }

// Reset clears the tripped latch and the rolling birth history, e.g. once
// an operator has acknowledged the crash-loop event.
func (b *Breaker) Reset() {
	b.tripped = false
	b.births = nil
}
