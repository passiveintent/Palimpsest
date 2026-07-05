/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the AGPL-3.0-only license found in the
 * LICENSE file in the root directory of this source tree.
 */

package audit

import "time"

// SubstrateLatency summarizes detection-latency samples (wall-clock
// seconds from a truth event's predicted occurrence to first detection)
// for one substrate.
type SubstrateLatency struct {
	Count       int     `json:"count"`
	MeanSeconds float64 `json:"mean_seconds"`
	P95Seconds  float64 `json:"p95_seconds"`
}

// TypeRecall breaks recall down by ground-truth type ("anomaly" vs
// "drift"), since they're caught by different substrates on different
// timescales and blending them into one number would obscure both.
type TypeRecall struct {
	Truth    int      `json:"truth"`
	Recalled int      `json:"recalled"`
	Recall   *float64 `json:"recall,omitempty"`
}

// Report is internal/audit's final scoring output: written as JSON
// (out-dir/audit_report.json) and exposed as expvar. Rate/ratio fields are
// pointers so "no qualifying samples" (nil) is distinguishable from "the
// computed value happens to be exactly 0" — an empty report should read as
// "no data yet", not a misleadingly perfect or failing score.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`

	// TruthAnomalies is the count of "anomaly"+"drift" truth records
	// (lifecycle "birth"/"tombstone" records are excluded: they are cross
	// -referenced for LifecycleFalsePositives, not scored for recall).
	TruthAnomalies int `json:"truth_anomalies"`

	// Detections is the number of distinct (shard, view, series, window)
	// detection groups observed — one group even if a window was later
	// revised/repaired (ADR-013), since each repair re-emits an event for
	// the same underlying detection rather than a new one.
	Detections     int `json:"detections"`
	TruePositives  int `json:"true_positives"`
	FalsePositives int `json:"false_positives"`

	Precision *float64 `json:"precision,omitempty"`
	Recall    *float64 `json:"recall,omitempty"`

	RecallByType map[string]TypeRecall `json:"recall_by_type"`

	ValueMAPE        *float64 `json:"value_mape,omitempty"`
	ValueMAPESamples int      `json:"value_mape_samples"`

	// LifecycleFalsePositives counts "deviation"-substrate false
	// positives whose series had a birth/tombstone truth record within
	// +/-1 window — ADR-008's promise that lifecycle churn alone never
	// masquerades as a real deviation. Should be 0.
	LifecycleFalsePositives int `json:"lifecycle_false_positives"`

	DetectionLatencyBySubstrate map[string]SubstrateLatency `json:"detection_latency_by_substrate"`

	// RevisionAccuracy is, among detection groups that received at least
	// one ADR-013 watermark repair (Revision > 0), the fraction whose
	// final (highest-revision) value landed within tolerance of the
	// matched truth event's ExpectedValue — did repair converge on the
	// right answer.
	RevisionAccuracy        *float64 `json:"revision_accuracy,omitempty"`
	RevisionAccuracySamples int      `json:"revision_accuracy_samples"`
}
