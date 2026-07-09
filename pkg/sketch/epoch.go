/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import (
	"encoding/binary"
	"time"

	"github.com/cespare/xxhash/v2"
)

// EpochJitter returns emitterID's deterministic offset into [0, jitterWindow)
// (ADR-013 §Addendum "herd jitter"): every emitter sharing the same epoch
// rotate interval would otherwise cross each epoch boundary at the exact
// same wall-clock instant, so a decoder-side fleet's golden-keyframe /
// fresh-Accumulator cost all lands in the same second. Both the encoder
// (deciding when to rotate) and anyone reasoning about the fleet after the
// fact can recompute this from emitterID alone — it never needs to travel
// on the wire.
//
// A non-positive jitterWindow disables jitter (returns 0): every emitter
// rotates at the unshifted boundary, matching pre-jitter behavior.
func EpochJitter(emitterID uint64, jitterWindow time.Duration) time.Duration {
	if jitterWindow <= 0 {
		return 0
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], emitterID)
	h := xxhash.Sum64(buf[:])
	return time.Duration(h % uint64(jitterWindow))
}

// EpochIndex returns the ADR-013 epoch index for wall-clock t under
// rotate-width buckets, delaying emitterID's own transition into each new
// bucket by its EpochJitter offset: the rotation instant for emitter e is
// epoch_boundary + EpochJitter(e, jitterWindow), spreading a fleet's
// rotations across jitterWindow instead of concentrating them at one
// instant. jitterWindow should be <= rotate (a jitter wider than the epoch
// itself would delay a rotation past the next one); a non-positive rotate
// always returns 0, matching the unjittered epochIndex convention it
// replaces.
func EpochIndex(t time.Time, rotate, jitterWindow time.Duration, emitterID uint64) uint64 {
	if rotate <= 0 {
		return 0
	}
	shifted := t.Add(-EpochJitter(emitterID, jitterWindow))
	return uint64(shifted.UnixNano() / rotate.Nanoseconds())
}
