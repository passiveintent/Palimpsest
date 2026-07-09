/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package csresidual

import (
	"time"

	"go.uber.org/zap"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// Alert is a churn-breaker trip event (ADR-009: "Churn circuit breaker
// turns crash loops into detected events"). metricsProcessor keeps a
// bounded log of recent alerts (in addition to logging them through the
// collector's own logger) so tests and operators can inspect what tripped
// without parsing logs.
type Alert struct {
	Time   time.Time
	Shard  uint64
	View   uint16
	Reason string
}

// maxAlertsRetained bounds the in-memory alert log so a persistently
// tripped breaker cannot grow it without bound.
const maxAlertsRetained = 256

// newBreaker builds the sketch.Breaker a pipeline's Tracker rate-limits
// dictionary births through (ADR-009 churn circuit breaker): more than
// cfg.MaxBirthsPerLogicalPerMin births within a minute trips it.
func newBreaker(cfg ChurnConfig) *sketch.Breaker {
	return sketch.NewBreaker(cfg.MaxBirthsPerLogicalPerMin, time.Minute)
}

// checkBreaker inspects pl's Tracker after a flush's Observe calls and
// reacts to a *new* trip (the false->true transition): it fires exactly
// one Alert per trip (not one per subsequent window while still tripped)
// and marks the pipeline degraded, which the flush path reads to skip
// ring-buffer instance recording for every group in this pipeline
// ("degrade to aggregate-only buffering (no instances recorded for that
// series)") until an operator acknowledges the event (resetBreaker).
// Aggregate sum/count/max folding and the Tracker/Accumulator path are
// unaffected: only per-instance ring-buffer detail is shed.
//
// Locking contract: the caller must hold pl.mu but must NOT hold p.mu;
// checkBreaker acquires p.mu itself (only to append to the shared alert
// log) and releases it before returning.
func (p *metricsProcessor) checkBreaker(pl *pipeline, now time.Time) {
	if !pl.tracker.BreakerTripped() {
		return
	}
	pl.degraded = true
	if pl.breakerAlerted {
		return
	}
	pl.breakerAlerted = true

	alert := Alert{
		Time:   now,
		Shard:  pl.shardID,
		View:   pl.viewID,
		Reason: "churn circuit breaker tripped: birth rate exceeded churn.max_births_per_logical_per_min",
	}
	p.logger.Warn(alert.Reason,
		zap.Uint64("shard_id", alert.Shard),
		zap.Uint16("view_id", alert.View),
	)

	p.mu.Lock()
	p.alerts = append(p.alerts, alert)
	if len(p.alerts) > maxAlertsRetained {
		p.alerts = p.alerts[len(p.alerts)-maxAlertsRetained:]
	}
	p.mu.Unlock()
}

// Alerts returns a copy of the churn-breaker alerts fired so far.
func (p *metricsProcessor) Alerts() []Alert {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Alert, len(p.alerts))
	copy(out, p.alerts)
	return out
}

// resetBreaker clears pl's tripped/degraded/alerted state, e.g. once an
// operator has acknowledged the churn event (ADR-009). Not wired to any
// automatic trigger: recovery from a crash loop is an operational
// decision, not a timeout. Caller must hold pl.mu.
func (p *metricsProcessor) resetBreaker(pl *pipeline) {
	pl.breaker.Reset()
	pl.degraded = false
	pl.breakerAlerted = false
}
