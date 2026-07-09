/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import "github.com/passiveintent/Palimpsest/pkg/recover"

// mergedMinSignalRatioDefault is the harness-derived default MinSignalRatio
// (ADR-015): (k/N)*A*(SigmaEstimate)^2 must reach this for a merged-tier
// result to be trusted. A MergedPolicy with a zero MinSignalRatio uses this
// default rather than trivially passing every window (a zero threshold
// would make the guardrail meaningless).
const mergedMinSignalRatioDefault = 3.0

// MergedPolicy configures the ADR-015 scaling-law trust guardrail for one
// merged-tier view: weak-signal fusion across many emitters is only
// trustworthy once (k/N)*A*(SigmaEstimate)^2 reaches MinSignalRatio, where
// k (recovered support size), N (dictionary size), and A (emitter
// coverage) are all observed live from the completed Recover call and its
// view — see evaluate and docs/adr/ADR-015-merged-tier.md.
type MergedPolicy struct {
	// MinSignalRatio is the guardrail threshold. <= 0 uses the
	// harness-derived default (3.0).
	MinSignalRatio float64

	// SigmaEstimate is delta/sigma: a config estimate of per-agent
	// predictor quality (anomaly amplitude over per-agent noise stddev).
	// This is the one guardrail input the decoder cannot measure live: a
	// merged sketch only ever exposes the sum of true signal and per-agent
	// noise, never the two separately.
	SigmaEstimate float64
}

// evaluate computes the ADR-015 scaling-law ratio (k/N)*A*(SigmaEstimate)^2
// from this window's live k (recovered support size), N (dictionary size),
// and A (emitter coverage), and returns the MergedTrust verdict alongside
// the ratio itself (returned for logging/testing; callers stamping
// Result.MergedTrust only need the verdict).
//
// n == 0 (an empty dictionary) returns MergedTrustUnproven with ratio 0
// rather than dividing by zero: an empty dictionary can't justify trusting
// anything fused into it.
func (p MergedPolicy) evaluate(k, n, a int) (ratio float64, trust string) {
	if n == 0 {
		return 0, recover.MergedTrustUnproven
	}
	threshold := p.MinSignalRatio
	if threshold <= 0 {
		threshold = mergedMinSignalRatioDefault
	}
	ratio = (float64(k) / float64(n)) * float64(a) * (p.SigmaEstimate * p.SigmaEstimate)
	if ratio >= threshold {
		return ratio, recover.MergedTrustProven
	}
	return ratio, recover.MergedTrustUnproven
}
