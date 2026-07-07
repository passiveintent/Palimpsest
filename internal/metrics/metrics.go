/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

// Package metrics implements palimpsestd's expvar-published counters and
// gauges: frame throughput, recovery latency, gating/degradation,
// lifecycle churn, anomaly counts, snapshot forensics, and per-emitter
// clock skew.
//
// Metrics is safe for concurrent use. Construction (New) never touches the
// global expvar registry; call Publish exactly once (from cmd/palimpsestd)
// to expose it at /debug/vars. Tests read counters directly via the Get*
// methods instead.
/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package metrics

import (
	"expvar"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// labeled is a small mutex-guarded map of int64 counters/gauges keyed by a
// composite label string, reused for every labeled metric below.
type labeled struct {
	mu   sync.Mutex
	vals map[string]int64
}

func newLabeled() *labeled { return &labeled{vals: make(map[string]int64)} }

func (l *labeled) add(key string, delta int64) {
	l.mu.Lock()
	l.vals[key] += delta
	l.mu.Unlock()
}

func (l *labeled) set(key string, v int64) {
	l.mu.Lock()
	l.vals[key] = v
	l.mu.Unlock()
}

// setMax updates key to v only if v is greater than its current value,
// giving a running maximum (used for worst-case, not cumulative, gauges).
func (l *labeled) setMax(key string, v int64) {
	l.mu.Lock()
	if v > l.vals[key] {
		l.vals[key] = v
	}
	l.mu.Unlock()
}

func (l *labeled) get(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.vals[key]
}

func (l *labeled) snapshot() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int64, len(l.vals))
	for k, v := range l.vals {
		out[k] = v
	}
	return out
}

// Metrics holds every counter/gauge palimpsestd exposes. The zero value is
// not usable; construct with New.
type Metrics struct {
	framesTotal    *labeled // key: type|emitter|shard
	recoverSeconds *labeled // key: shard -> cumulative nanoseconds
	recoverCount   *labeled // key: shard -> count, for average

	gated             int64
	degradedShards    int64
	tombstonesApplied int64
	birthsApplied     int64

	anomaliesFired *labeled // key: substrate|confidence

	snapshotsCaptured       int64
	snapshotsLatencySeconds *labeled // key: "" -> cumulative nanoseconds
	snapshotsLatencyCount   int64

	breakerTrips           int64
	clockSkewMs            *labeled // key: emitter -> latest skew (gauge)
	lateFramesDropped      int64
	duplicateFramesDropped int64
	keyringMiss            int64 // frames gated because key_version not in KeyRing (ADR-012)
	unregisteredCodec      int64 // frame/blob named a Codec with no registered Compressor (ADR-006 §Addendum)

	mergedWindows *labeled // key: trust ("proven"|"unproven"|"na") -> count (ADR-015)

	// wireBytesTotal/fullDictKeyframeBytes back the ADR-008 dict_root
	// Merkle/resync evidence gate (docs/adr/ADR-017-merkle-decision.md): the
	// fraction of a shard's wire bytes spent on golden (Full, non-KDELTA)
	// keyframes, which is what a Merkle root + resync channel would need to
	// beat to be worth building.
	wireBytesTotal        *labeled // key: shard -> cumulative bytes
	fullDictKeyframeBytes *labeled // key: shard -> cumulative bytes (golden keyframes only)

	// dictRootMismatches/dictRootHeal* back the same evidence gate's second
	// condition: how often a decoder's dict_root diverges from the encoder's
	// (VerifyKeyframe failure, internal/core/engine.go's handleKeyframe) and
	// how long it takes to heal (mismatch detected -> next verified golden
	// keyframe).
	dictRootMismatches  *labeled // key: shard -> count
	dictRootHealSeconds *labeled // key: shard -> cumulative nanoseconds
	dictRootHealCount   *labeled // key: shard -> count, for average
	dictRootHealMaxNs   *labeled // key: shard -> worst observed heal duration (ns)

	scalarMu    sync.Mutex // guards the plain int64 scalar fields above
	publishOnce sync.Once
}

// New returns a fresh, unpublished Metrics.
func New() *Metrics {
	return &Metrics{
		framesTotal:             newLabeled(),
		recoverSeconds:          newLabeled(),
		recoverCount:            newLabeled(),
		anomaliesFired:          newLabeled(),
		snapshotsLatencySeconds: newLabeled(),
		clockSkewMs:             newLabeled(),
		mergedWindows:           newLabeled(),
		wireBytesTotal:          newLabeled(),
		fullDictKeyframeBytes:   newLabeled(),
		dictRootMismatches:      newLabeled(),
		dictRootHealSeconds:     newLabeled(),
		dictRootHealCount:       newLabeled(),
		dictRootHealMaxNs:       newLabeled(),
	}
}

// frameKey builds IncFramesTotal/FramesTotal's composite label. Called on
// every decoded frame (HandleFrame's first line), so this avoids
// fmt.Sprintf's reflection-driven verb parsing in favor of strconv.Append*
// into a stack buffer — the same "%s|%d|%d" shape, built without it.
func frameKey(frameType string, emitterID, shardID uint64) string {
	buf := make([]byte, 0, len(frameType)+1+20+1+20)
	buf = append(buf, frameType...)
	buf = append(buf, '|')
	buf = strconv.AppendUint(buf, emitterID, 10)
	buf = append(buf, '|')
	buf = strconv.AppendUint(buf, shardID, 10)
	return string(buf)
}

func pairKey(a, b string) string { return a + "|" + b }

// IncFramesTotal records one processed frame of frameType ("keyframe",
// "residual", "fallback") from emitterID/shardID.
func (m *Metrics) IncFramesTotal(frameType string, emitterID, shardID uint64) {
	m.framesTotal.add(frameKey(frameType, emitterID, shardID), 1)
}

// FramesTotal returns the current count for the given labels (test seam).
func (m *Metrics) FramesTotal(frameType string, emitterID, shardID uint64) int64 {
	return m.framesTotal.get(frameKey(frameType, emitterID, shardID))
}

// ObserveRecoverSeconds records one recover.Recover call's wall-clock
// duration for shardID.
func (m *Metrics) ObserveRecoverSeconds(shardID uint64, d time.Duration) {
	key := fmt.Sprintf("%d", shardID)
	m.recoverSeconds.add(key, d.Nanoseconds())
	m.recoverCount.add(key, 1)
}

// RecoverSecondsAvg returns the mean recover.Recover duration observed for
// shardID so far.
func (m *Metrics) RecoverSecondsAvg(shardID uint64) time.Duration {
	key := fmt.Sprintf("%d", shardID)
	n := m.recoverCount.get(key)
	if n == 0 {
		return 0
	}
	return time.Duration(m.recoverSeconds.get(key) / n)
}

// IncGatedFrames records a frame whose recovery was gated (either
// short-circuited by a sticky degraded stream, or a fresh high-residual
// gate event).
func (m *Metrics) IncGatedFrames() { m.addInt64(&m.gated, 1) }
func (m *Metrics) GatedFrames() int64 {
	return m.getInt64(&m.gated)
}

// SetDegradedShards sets the current count of degraded (shard, emitter,
// view) streams.
func (m *Metrics) IncDegradedShards() { m.addInt64(&m.degradedShards, 1) }
func (m *Metrics) DecDegradedShards() { m.addInt64(&m.degradedShards, -1) }
func (m *Metrics) DegradedShards() int64 {
	return m.getInt64(&m.degradedShards)
}

func (m *Metrics) IncTombstonesApplied(n int) { m.addInt64(&m.tombstonesApplied, int64(n)) }
func (m *Metrics) TombstonesApplied() int64   { return m.getInt64(&m.tombstonesApplied) }

func (m *Metrics) IncBirthsApplied(n int) { m.addInt64(&m.birthsApplied, int64(n)) }
func (m *Metrics) BirthsApplied() int64   { return m.getInt64(&m.birthsApplied) }

// IncAnomaliesFired records one AnomalyEvent emitted for (substrate,
// confidence).
func (m *Metrics) IncAnomaliesFired(substrate, confidence string) {
	m.anomaliesFired.add(pairKey(substrate, confidence), 1)
}
func (m *Metrics) AnomaliesFired(substrate, confidence string) int64 {
	return m.anomaliesFired.get(pairKey(substrate, confidence))
}

func (m *Metrics) IncSnapshotsCaptured(n int) { m.addInt64(&m.snapshotsCaptured, int64(n)) }
func (m *Metrics) SnapshotsCaptured() int64   { return m.getInt64(&m.snapshotsCaptured) }

func (m *Metrics) ObserveSnapshotsLatencySeconds(d time.Duration) {
	m.snapshotsLatencySeconds.add("", d.Nanoseconds())
	m.addInt64(&m.snapshotsLatencyCount, 1)
}
func (m *Metrics) SnapshotsLatencySecondsAvg() time.Duration {
	n := m.getInt64(&m.snapshotsLatencyCount)
	if n == 0 {
		return 0
	}
	return time.Duration(m.snapshotsLatencySeconds.get("") / n)
}

func (m *Metrics) IncBreakerTrips() { m.addInt64(&m.breakerTrips, 1) }
func (m *Metrics) BreakerTrips() int64 {
	return m.getInt64(&m.breakerTrips)
}

// SetClockSkewMs records emitterID's latest estimated clock skew, in
// milliseconds (signed: positive means that emitter's frames arrive later
// than its peers').
func (m *Metrics) SetClockSkewMs(emitterID uint64, ms int64) {
	m.clockSkewMs.set(fmt.Sprintf("%d", emitterID), ms)
}
func (m *Metrics) ClockSkewMs(emitterID uint64) int64 {
	return m.clockSkewMs.get(fmt.Sprintf("%d", emitterID))
}

func (m *Metrics) IncLateFramesDropped() { m.addInt64(&m.lateFramesDropped, 1) }
func (m *Metrics) LateFramesDropped() int64 {
	return m.getInt64(&m.lateFramesDropped)
}

// IncDuplicateFramesDropped records a RESIDUAL frame dropped because its
// (view, window, emitter) had already contributed (ADR-013 dedup: a
// repeated delivery of the same emitter's frame for a window it already
// merged, as opposed to a different emitter's late-but-first contribution,
// which is a repair, not a duplicate).
func (m *Metrics) IncDuplicateFramesDropped() { m.addInt64(&m.duplicateFramesDropped, 1) }
func (m *Metrics) DuplicateFramesDropped() int64 {
	return m.getInt64(&m.duplicateFramesDropped)
}

// IncKeyringMiss records a frame gated because its key_version was not
// found in the decoder's KeyRing (ADR-012 §Addendum). Frames are never
// silently dropped on a miss — they are held at low confidence and counted
// here.
func (m *Metrics) IncKeyringMiss() { m.addInt64(&m.keyringMiss, 1) }
func (m *Metrics) KeyringMisses() int64 {
	return m.getInt64(&m.keyringMiss)
}

// IncUnregisteredCodec records a frame or blob naming a Codec (Frame.Codec)
// with no Compressor registered for it (wire.Register / ErrUnregisteredCodec;
// ADR-006 §Addendum). Like IncKeyringMiss, this is never a silent drop: the
// caller gates the affected stream/window at low confidence and counts it
// here instead.
func (m *Metrics) IncUnregisteredCodec() { m.addInt64(&m.unregisteredCodec, 1) }
func (m *Metrics) UnregisteredCodec() int64 {
	return m.getInt64(&m.unregisteredCodec)
}

// IncMergedWindows records one merged-tier window solve's ADR-015 trust
// guardrail verdict (trust is recover.MergedTrustProven/Unproven; callers
// never record MergedTrustNA here since evaluateMergedTrust only calls this
// for views declared merged tier).
func (m *Metrics) IncMergedWindows(trust string) { m.mergedWindows.add(trust, 1) }
func (m *Metrics) MergedWindows(trust string) int64 {
	return m.mergedWindows.get(trust)
}

func shardKey(shardID uint64) string { return strconv.FormatUint(shardID, 10) }

// AddWireBytes records n more encoded wire bytes (any frame type) processed
// for shardID (ADR-017 evidence gate's denominator: total wire bytes).
func (m *Metrics) AddWireBytes(shardID uint64, n int64) {
	m.wireBytesTotal.add(shardKey(shardID), n)
}
func (m *Metrics) WireBytesTotal(shardID uint64) int64 {
	return m.wireBytesTotal.get(shardKey(shardID))
}

// AddFullDictKeyframeBytes records n more encoded wire bytes attributable to
// a golden (Full, non-KDELTA) KEYFRAME for shardID (ADR-017 evidence gate's
// numerator: the byte cost the current full-dict-re-announcement design
// pays every golden cadence, which a Merkle root + resync channel would aim
// to shrink).
func (m *Metrics) AddFullDictKeyframeBytes(shardID uint64, n int64) {
	m.fullDictKeyframeBytes.add(shardKey(shardID), n)
}
func (m *Metrics) FullDictKeyframeBytes(shardID uint64) int64 {
	return m.fullDictKeyframeBytes.get(shardKey(shardID))
}

// IncDictRootMismatches records one newly-detected dict_root divergence
// (recover.Dictionary.VerifyKeyframe returning false) for shardID. Callers
// (handleKeyframe) only call this once per mismatch episode, not once per
// still-degraded frame — see ObserveDictRootHealSeconds for the matching
// heal-side observation.
func (m *Metrics) IncDictRootMismatches(shardID uint64) {
	m.dictRootMismatches.add(shardKey(shardID), 1)
}
func (m *Metrics) DictRootMismatches(shardID uint64) int64 {
	return m.dictRootMismatches.get(shardKey(shardID))
}

// ObserveDictRootHealSeconds records one mismatch episode's time-to-heal for
// shardID: the wall-clock gap between a dict_root mismatch being detected
// and the next verified golden keyframe healing it (ADR-017 evidence gate).
func (m *Metrics) ObserveDictRootHealSeconds(shardID uint64, d time.Duration) {
	key := shardKey(shardID)
	m.dictRootHealSeconds.add(key, d.Nanoseconds())
	m.dictRootHealCount.add(key, 1)
	m.dictRootHealMaxNs.setMax(key, d.Nanoseconds())
}
func (m *Metrics) DictRootHealSecondsAvg(shardID uint64) time.Duration {
	key := shardKey(shardID)
	n := m.dictRootHealCount.get(key)
	if n == 0 {
		return 0
	}
	return time.Duration(m.dictRootHealSeconds.get(key) / n)
}
func (m *Metrics) DictRootHealSecondsMax(shardID uint64) time.Duration {
	return time.Duration(m.dictRootHealMaxNs.get(shardKey(shardID)))
}

// Publish registers m's counters under expvar at the given top-level name
// (e.g. "palimpsest"). Safe to call more than once; only the first call
// takes effect, matching expvar.Publish's "panics if name already
// registered" behavior being something callers should never need to think
// about.
func (m *Metrics) Publish(name string) {
	m.publishOnce.Do(func() {
		expvar.Publish(name, expvar.Func(func() any { return m.Snapshot() }))
	})
}

// Snapshot returns a point-in-time view of every metric, suitable for JSON
// encoding (used by Publish, and directly by tests/diagnostics).
func (m *Metrics) Snapshot() map[string]any {
	return map[string]any{
		"frames_total":             m.framesTotal.snapshot(),
		"recover_seconds_avg_ns":   m.avgMapNs(m.recoverSeconds, m.recoverCount),
		"gated_frames":             m.GatedFrames(),
		"degraded_shards":          m.DegradedShards(),
		"tombstones_applied":       m.TombstonesApplied(),
		"births_applied":           m.BirthsApplied(),
		"anomalies_fired":          m.anomaliesFired.snapshot(),
		"snapshots_captured":       m.SnapshotsCaptured(),
		"snapshots_latency_avg_ns": int64(m.SnapshotsLatencySecondsAvg()),
		"breaker_trips":            m.BreakerTrips(),
		"clock_skew_ms":            m.clockSkewMs.snapshot(),
		"late_frames_dropped":      m.LateFramesDropped(),
		"duplicate_frames_dropped": m.DuplicateFramesDropped(),
		"keyring_miss":             m.KeyringMisses(),
		"unregistered_codec":       m.UnregisteredCodec(),
		"merged_windows":           m.mergedWindows.snapshot(),
		"wire_bytes_total":         m.wireBytesTotal.snapshot(),
		"full_dict_keyframe_bytes": m.fullDictKeyframeBytes.snapshot(),
		"dict_root_mismatches":     m.dictRootMismatches.snapshot(),
		"dict_root_heal_avg_ns":    m.avgMapNs(m.dictRootHealSeconds, m.dictRootHealCount),
		"dict_root_heal_max_ns":    m.dictRootHealMaxNs.snapshot(),
	}
}

func (m *Metrics) avgMapNs(sum, count *labeled) map[string]int64 {
	sums := sum.snapshot()
	out := make(map[string]int64, len(sums))
	for k, s := range sums {
		if c := count.get(k); c > 0 {
			out[k] = s / c
		}
	}
	return out
}

func (m *Metrics) addInt64(p *int64, delta int64) {
	m.scalarMu.Lock()
	*p += delta
	m.scalarMu.Unlock()
}

func (m *Metrics) getInt64(p *int64) int64 {
	m.scalarMu.Lock()
	defer m.scalarMu.Unlock()
	return *p
}
