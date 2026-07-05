/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

// windowFold accumulates, for one pipeline's current window, the latest
// per-instance value observed for each logical group (pre-aggregate
// identity: metric + logical/view key values, no "|agg=" suffix and no
// instance labels — ADR-008's two-level identity). At flush time, fold
// folds each group's instances into the configured sum/count/max
// aggregates (aggregate.go's stated responsibility): these are what enter
// the Tracker/Accumulator, one logical series per configured aggregate.
//
// A window may see more than one sample from the same instance (e.g. the
// upstream scrape interval is shorter than flush_interval); only the last
// value observed this window is folded, matching a gauge-style "current
// state of the world at flush time" semantic rather than summing repeats.
//
// windowFold is NOT safe for concurrent use; the caller (pipeline) owns
// its own locking.
type windowFold struct {
	groups map[string]map[string]float64 // groupKey -> instanceKey -> latest value
}

func newWindowFold() *windowFold {
	return &windowFold{groups: make(map[string]map[string]float64)}
}

// observe records value as instanceKey's latest sample this window under
// groupKey.
func (w *windowFold) observe(groupKey, instanceKey string, value float64) {
	m, ok := w.groups[groupKey]
	if !ok {
		m = make(map[string]float64)
		w.groups[groupKey] = m
	}
	m[instanceKey] = value
}

// foldResult is one group's folded aggregates for the window just ended,
// plus the raw per-instance values they were folded from (so callers can
// attribute a hot instance, e.g. for snapshot.go's dashcam blobs).
type foldResult struct {
	sum       float64
	count     float64
	max       float64
	instances map[string]float64 // instanceKey -> value
}

// fold computes sum/count/max per group from every instance observed this
// window. It does not reset the fold; call reset separately once the
// result has been consumed (mirrors sketch.Accumulator.Flush/Tracker's
// split between reading and clearing transient window state).
func (w *windowFold) fold() map[string]foldResult {
	out := make(map[string]foldResult, len(w.groups))
	for g, instances := range w.groups {
		var sum, max float64
		for _, v := range instances {
			sum += v
			if v > max {
				max = v
			}
		}
		out[g] = foldResult{sum: sum, count: float64(len(instances)), max: max, instances: instances}
	}
	return out
}

// reset clears the window's accumulated samples, ready for the next
// window (ADR-013: transient per-window state is explicitly reset at
// window boundaries, mirroring sketch.Tracker.ResetWindow).
func (w *windowFold) reset() {
	w.groups = make(map[string]map[string]float64)
}

// aggregateValue extracts the configured aggregate's value from a folded
// result. agg must be one of "sum", "count", "max" (validated by
// Config.Validate).
func aggregateValue(fr foldResult, agg string) float64 {
	switch agg {
	case "sum":
		return fr.sum
	case "count":
		return fr.count
	case "max":
		return fr.max
	default:
		return 0
	}
}
