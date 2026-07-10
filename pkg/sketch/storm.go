/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import "sort"

// StormDetector watches the pre-quantization sketch energy ‖y‖²/m
// (Accumulator.Flush's energy return) across windows and flags spikes well
// before the recovery sparsity cliff (ADR-004): k blowing up during an
// incident corrupts recovery, but the energy spike that causes it is
// detectable first.
//
// A spike is defined relative to a rolling median of recent energy
// readings rather than a fixed threshold, since baseline energy varies by
// deployment/traffic shape: Trigger reports true when energy exceeds
// multiplier times the median of the readings seen so far (bounded to the
// last windowSize), before folding energy into that history.
//
// StormDetector is NOT safe for concurrent use.
type StormDetector struct {
	history    []float64
	cap        int
	pos        int
	multiplier float64
}

// NewStormDetector returns a StormDetector tracking a rolling median over
// the last windowSize energy readings, triggering when a new reading
// exceeds multiplier times that median. windowSize must be positive;
// multiplier should be > 1.
func NewStormDetector(windowSize int, multiplier float64) *StormDetector {
	return &StormDetector{
		history:    make([]float64, 0, windowSize),
		cap:        windowSize,
		multiplier: multiplier,
	}
}

// Trigger reports whether energy is a storm spike relative to the rolling
// median of prior readings, then folds energy into the rolling window.
// There is no spike detection (Trigger returns false) until at least one
// prior reading exists.
func (s *StormDetector) Trigger(energy float64) bool {
	med, ok := s.median()
	triggered := ok && energy > med*s.multiplier
	s.push(energy)
	return triggered
}

func (s *StormDetector) median() (float64, bool) {
	n := len(s.history)
	if n == 0 {
		return 0, false
	}
	sorted := append([]float64(nil), s.history...)
	sort.Float64s(sorted)
	if n%2 == 1 {
		return sorted[n/2], true
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2, true
}

func (s *StormDetector) push(energy float64) {
	if s.cap <= 0 {
		return
	}
	if len(s.history) < s.cap {
		s.history = append(s.history, energy)
		return
	}
	s.history[s.pos] = energy
	s.pos = (s.pos + 1) % s.cap
}
