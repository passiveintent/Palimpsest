/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package sketch

import "testing"

func TestStormDetectorNoTriggerBeforeHistory(t *testing.T) {
	sd := NewStormDetector(5, 3.0)
	if sd.Trigger(1e9) {
		t.Fatal("Trigger on the very first reading (no history yet) should not fire")
	}
}

func TestStormDetectorTriggersOnSpike(t *testing.T) {
	sd := NewStormDetector(5, 3.0)
	baseline := []float64{10, 11, 9, 10, 10}
	for _, e := range baseline {
		if sd.Trigger(e) {
			t.Fatalf("unexpected trigger during baseline fill at energy=%v", e)
		}
	}
	if !sd.Trigger(1000) {
		t.Fatal("expected a 100x spike over the baseline median to trigger")
	}
}

func TestStormDetectorNoTriggerWithinMultiplier(t *testing.T) {
	sd := NewStormDetector(5, 3.0)
	for _, e := range []float64{10, 10, 10, 10, 10} {
		sd.Trigger(e)
	}
	if sd.Trigger(20) { // 2x median, under the 3x multiplier
		t.Fatal("2x median should not trigger a 3x-multiplier detector")
	}
}

func TestStormDetectorWindowIsBounded(t *testing.T) {
	sd := NewStormDetector(3, 2.0)
	// Fill with a high baseline, then roll it out with a longer run of a
	// low baseline; only the last `windowSize` readings should count
	// toward the rolling median.
	for _, e := range []float64{1000, 1000, 1000} {
		sd.Trigger(e)
	}
	for _, e := range []float64{10, 10, 10} {
		sd.Trigger(e)
	}
	// Median of the last 3 readings (10,10,10) is 10; 25 is 2.5x that,
	// over the 2.0 multiplier, so it should trigger. If the old 1000s were
	// still in the window this would not trigger.
	if !sd.Trigger(25) {
		t.Fatal("expected old high-energy readings to have rolled out of the window")
	}
}
