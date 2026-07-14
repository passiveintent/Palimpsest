/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package recover

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
	"github.com/passiveintent/Palimpsest/pkg/wire"
)

// MADSigma should track a Gaussian sigma and shrug off sparse
// contamination (an anomaly touches only ~d of m buckets).
func TestMADSigmaRobustToSparseContamination(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const sigma = 0.5
	v := make([]float64, 2000)
	for i := range v {
		v[i] = rng.NormFloat64() * sigma
	}
	if got := MADSigma(v); math.Abs(got-sigma) > 0.05 {
		t.Fatalf("clean MADSigma = %v, want ~%v", got, sigma)
	}
	for i := 0; i < 6; i++ { // one d=6 anomaly worth of huge buckets
		v[i*137] += 1e6
	}
	if got := MADSigma(v); math.Abs(got-sigma) > 0.06 {
		t.Fatalf("contaminated MADSigma = %v, want ~%v", got, sigma)
	}
}

func TestQuietSigmaMedianAndReadiness(t *testing.T) {
	q := NewQuietSigma(5)
	if _, ok := q.Estimate(); ok {
		t.Fatal("empty QuietSigma reported an estimate")
	}
	for _, s := range []float64{1, 100, 2, 3, 2} {
		q.Observe(s)
	}
	if got, _ := q.Estimate(); got != 2 {
		t.Fatalf("median = %v, want 2 (robust to the 100 outlier)", got)
	}
	// Rolling: pushing 5 more replaces the window entirely.
	for i := 0; i < 5; i++ {
		q.Observe(7)
	}
	if got, _ := q.Estimate(); got != 7 {
		t.Fatalf("rolled median = %v, want 7", got)
	}
	if q.Count() != 5 {
		t.Fatalf("Count = %d, want 5 (saturated)", q.Count())
	}
}

// LambdaAbs must bypass the max|Phi^T y| coupling: scaling y by 10x with a
// fixed LambdaAbs must not scale the effective penalty (the recovered
// support of a lone spike stays a lone spike at both scales), and the
// baseline Lambda path is unchanged when LambdaAbs is 0.
func TestLambdaAbsDecouplesPenaltyFromMagnitude(t *testing.T) {
	p := sketch.Params{M: 256, D: 4, Seed: 42}
	dict := NewDictionary()
	acc := sketch.NewAccumulator(p)
	names := make([][]byte, 50)
	ids := make([]uint64, 50)
	for i := range names {
		names[i] = []byte(fmt.Sprintf("lambda_abs_%03d|agg=sum", i))
		acc.Update(names[i], 0)
		ids[i] = sketch.SeriesID(names[i])
		dict.ApplyDelta(wire.DictDelta{ID: ids[i], Name: names[i]})
	}
	acc.Flush()

	for _, scale := range []float64{1, 10} {
		acc.UpdateByID(ids[3], 5*scale)
		y, _ := acc.Flush()
		res, err := Recover(y, dict, p, Options{
			Iters: 200, Lambda: 0.05, PowerIters: 30, Threshold: 0.3,
			LambdaAbs: 0.5,
		})
		if err != nil {
			t.Fatalf("scale %v: %v", scale, err)
		}
		if len(res.SupportIDs) != 1 || res.SupportIDs[0] != ids[3] {
			t.Fatalf("scale %v: support = %v, want exactly [%d]", scale, res.SupportIDs, ids[3])
		}
		if math.Abs(res.Values[0]-5*scale) > 0.5*scale {
			t.Fatalf("scale %v: debiased value %v, want ~%v", scale, res.Values[0], 5*scale)
		}
	}
}
