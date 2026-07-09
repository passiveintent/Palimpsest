/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package core

import (
	"math"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/recover"
)

// TestMergedPolicyEvaluate covers the ADR-015 scaling-law guardrail
// arithmetic in isolation, without needing a full Recover solve: ratio =
// (k/N)*A*(SigmaEstimate)^2, verdict is Proven iff ratio >= MinSignalRatio.
func TestMergedPolicyEvaluate(t *testing.T) {
	tests := []struct {
		name      string
		policy    MergedPolicy
		k, n, a   int
		wantRatio float64
		wantTrust string
	}{
		{
			// Documented ADR-015 "proven" arithmetic: k=520, N=10000,
			// A=2000, SigmaEstimate=1.0 -> ratio = (520/10000)*2000*1 = 104.
			name:   "proven, large A",
			policy: MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0},
			k:      520, n: 10000, a: 2000,
			wantRatio: 104.0,
			wantTrust: recover.MergedTrustProven,
		},
		{
			// Same k/N/SigmaEstimate, A=100 -> ratio = (520/10000)*100*1 = 5.2,
			// still above the default 3.0 threshold: A alone is not always
			// enough to fail the guardrail (this test exists to prove the
			// arithmetic, not the E2E scenario's exact parameters).
			name:   "proven, smaller A still clears default threshold",
			policy: MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0},
			k:      520, n: 10000, a: 100,
			wantRatio: 5.2,
			wantTrust: recover.MergedTrustProven,
		},
		{
			// Sparser k/N (a smaller anomalous fraction of a larger dictionary)
			// at A=100 falls below threshold: ratio = (520/100000)*100*1 = 0.52.
			name:   "unproven, sparse k/N at moderate A",
			policy: MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0},
			k:      520, n: 100000, a: 100,
			wantRatio: 0.52,
			wantTrust: recover.MergedTrustUnproven,
		},
		{
			name:   "zero MinSignalRatio uses harness-derived default (3.0)",
			policy: MergedPolicy{SigmaEstimate: 1.0},
			k:      1, n: 10, a: 29, // ratio = 2.9, just under the 3.0 default
			wantRatio: 2.9,
			wantTrust: recover.MergedTrustUnproven,
		},
		{
			name:   "empty dictionary (N=0) is unproven, not a divide-by-zero",
			policy: MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 1.0},
			k:      0, n: 0, a: 2000,
			wantRatio: 0,
			wantTrust: recover.MergedTrustUnproven,
		},
		{
			name:   "zero SigmaEstimate zeroes the ratio regardless of A",
			policy: MergedPolicy{MinSignalRatio: 3.0, SigmaEstimate: 0},
			k:      500, n: 1000, a: 999999,
			wantRatio: 0,
			wantTrust: recover.MergedTrustUnproven,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ratio, trust := tt.policy.evaluate(tt.k, tt.n, tt.a)
			if math.Abs(ratio-tt.wantRatio) > 1e-9 {
				t.Fatalf("ratio = %v, want %v", ratio, tt.wantRatio)
			}
			if trust != tt.wantTrust {
				t.Fatalf("trust = %q, want %q", trust, tt.wantTrust)
			}
		})
	}
}
