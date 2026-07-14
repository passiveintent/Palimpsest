/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

// groupomp.go implements the Group-OMP escalation path (ADR-014 kill criterion,
// Prompt 11c). group-FISTA was abandoned — see ADR-014 §Negative result.
//
// Algorithm (detection-then-refit):
//
//  1. Compute the debiased residual r from the capped plain-LASSO support.
//  2. For each declared group g with |g| ≥ √m, compute
//       score_g = ‖Φ_gᵀ r‖₂ / √|g|
//     and compare against the H0-calibrated size-aware threshold
//       τ(g) = σ̂ · (1 + C_GROUP / √(2·|g|))
//     where σ̂ = ‖r‖₂ / √m  and  C_GROUP = 6.0 (6-sigma above null mean).
//     Derivation: under H0 (no group signal), score_g/σ̂ ~ √(χ²(|g|)/|g|)
//     with mean 1 and std 1/√(2·|g|).  C_GROUP=6 controls the false-alarm
//     rate per group; τ(g) → σ̂ as |g| → ∞.
//
//  3. Iterative activation (≤ maxOmpRounds = 3): activate the highest-scoring
//     group above its τ, refit on the union support, recompute r, retest.
//
//  4. Final LS-refit on (activated group rows ∪ capped plain rows).
//
// The √|g| normalisation lets large groups pool coherent signal across many
// series; the size-aware denominator penalises singletons (which have large
// raw cross-talk from the 500-member AZ group) because their τ(1) = σ̂·(1+6/√2)
// ≈ 5.24·σ̂ >> cross-talk ≈ 4.0·σ̂.

package recover

import (
	"math"
	"sort"

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// CGroupDefault is the H0-calibrated threshold constant for Group-OMP
// (6 standard deviations above the null-score mean under chi-squared
// concentration; see package-level comment). Exported for documentation;
// callers should not need to override it.
const CGroupDefault = 6.0

// groupEntry holds the group ID and the CSR row indices belonging to that
// group (rows sorted ascending for deterministic iteration).
type groupEntry struct {
	id   uint64
	rows []int
}

// maxOmpRounds is the maximum number of Group-OMP activation rounds.
const maxOmpRounds = 3

// RecoverGroup is the public API for group-sparse recovery (ADR-014).
// It runs Group-OMP: build the plain-LASSO solution, compute the debiased
// residual, activate groups by the H0-calibrated score test, LS-refit.
// Compared to the abandoned group-FISTA path (see ADR-014 §Negative result),
// Group-OMP is immune to the block-threshold momentum latch, 10–50× faster,
// and provably detects the AZ-outage group when its H0 z-score ≫ C_GROUP.
func RecoverGroup(y []float64, dict *Dictionary, p sketch.Params, grouper Grouper, o Options) (Result, error) {
	if grouper == nil {
		grouper = DefaultGrouper()
	}

	present, total := dict.Coverage(o.EmittersExpected)
	if o.EmittersExpected > 0 && present == 0 {
		return Result{
			Confidence:    ConfidenceFallback,
			Coverage:      present,
			CoverageTotal: total,
			Revision:      o.Revision,
			Residual:      l2norm(y),
		}, nil
	}

	csr := dict.BuildCSR(p.Seed, p.M, p.D)
	n := csr.NRows

	if n == 0 {
		return Result{
			Confidence:    ConfidenceFallback,
			Coverage:      present,
			CoverageTotal: total,
			Revision:      o.Revision,
			Residual:      l2norm(y),
		}, nil
	}

	// ompContextCap (M/5): plain-support context for Group-OMP. Larger than
	// escalationCap so cross-talk-boosted AZ members do not displace scattered
	// singletons from the working set (see fista.go for the rationale comment).
	const ompContextCap = 5

	// pool parallelizes this call's matvecs (fista's iteration loop plus,
	// below, Group-OMP's own residual scan reuses the same workers —
	// Addendum A3) across GOMAXPROCS goroutines.
	pool := newMatvecPool(csr)
	defer pool.Close()

	// Run plain FISTA to get the initial context support for Group-OMP.
	// Use M/ompContextCap (not M/escalationCap) so scattered singletons are not
	// displaced by AZ-group members whose FISTA amplitudes are cross-talk boosted.
	// The M/escalationCap escalation trigger in Recover() is separate from this cap.
	lambdaAbs := effectiveLambda(csr, y, o, n)
	x, xRestarts, xIters := fista(pool, y, lambdaAbs, o.Iters, o.PowerIters, o.ObjectiveCheckEvery)
	plainRows := cappedSupport(x, o.Threshold, p.M/ompContextCap)

	return recoverGroupOMP(pool, csr, y, dict, p, grouper, x, plainRows, xRestarts, xIters, o)
}

// recoverGroupOMP is the Group-OMP core (ADR-014 kill criterion, Prompt 11c):
// build the debiased residual from the plain-LASSO support, activate groups
// by the H0-calibrated score test, LS-refit. Compared to the abandoned
// group-FISTA path (see ADR-014 §Negative result), Group-OMP is immune to
// the block-threshold momentum latch and 10-50x faster. pool/csr must be
// built from the same CSR (pool wraps csr); callers that already hold both
// from their own FISTA solve pass them through here to avoid rebuilding.
func recoverGroupOMP(
	pool *matvecPool,
	csr *CSR,
	y []float64,
	dict *Dictionary,
	p sketch.Params,
	grouper Grouper,
	plainX []float64,
	plainRows []int,
	plainRestarts int,
	plainIters int,
	o Options,
) (Result, error) {
	if grouper == nil {
		grouper = DefaultGrouper()
	}

	present, total := dict.Coverage(o.EmittersExpected)
	ids := dict.ActiveIDs()
	n := csr.NRows

	// Build group partition from the active dictionary.
	groupMap := make(map[uint64]*groupEntry, n/8+1)
	for i, id := range ids {
		gid := grouper(id, dict)
		g, ok := groupMap[gid]
		if !ok {
			g = &groupEntry{id: gid}
			groupMap[gid] = g
		}
		g.rows = append(g.rows, i)
	}
	entries := make([]*groupEntry, 0, len(groupMap))
	for _, g := range groupMap {
		entries = append(entries, g)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	// minGroupSize: in round 0, only groups with |g| ≥ √m are checked.
	// (In subsequent rounds the threshold drops to 1; see the OMP loop.)
	minGroupSize := int(math.Sqrt(float64(p.M)))
	if minGroupSize < 2 {
		minGroupSize = 2
	}

	// ---------------------------------------------------------------
	// Iterative Group-OMP (≤ maxOmpRounds activation rounds).
	// ---------------------------------------------------------------

	// Working sets: activated group IDs + union of rows (plain + activated).
	var activatedGroupIDs []uint64
	unionRowSet := make(map[int]struct{}, len(plainRows)+500)
	for _, r := range plainRows {
		unionRowSet[r] = struct{}{}
	}

	// Initial debias on the plain support.
	unionRows := sortedKeys(unionRowSet)
	unionValues := debias(csr, y, unionRows, debiasRidge)

	// Compute residual r = y - Phi * debiasedValues.
	r := make([]float64, p.M)
	reconstructInto(csr, unionRows, unionValues, r)
	for i := range r {
		r[i] = y[i] - r[i]
	}

	// rowScores[i] = Phi_i^T r, i.e. exactly one row of MulInto(r, ·).
	// Addendum A3: the per-group score loop below needs this dot product
	// for every candidate row across all groups — which, summed over the
	// whole partition, is one full Phi^T r pass. Computing it once via the
	// pool's parallel row-range gather (the same workers fista's gradient
	// step uses) replaces what used to be a serial per-group inline loop
	// with a single reused parallel matvec.
	rowScores := make([]float64, n)

	for round := 0; round < maxOmpRounds; round++ {
		rNorm := l2norm(r)
		sigmaHat := rNorm / math.Sqrt(float64(p.M))
		if sigmaHat == 0 {
			break // residual is zero; perfect fit
		}

		pool.MulInto(r, rowScores)

		// In round 0, only check large groups (|g| >= minGroupSize) to avoid
		// false alarms from the AZ-group cross-talk noise floor swamping the
		// background singletons. In rounds 1+, the AZ group has been activated
		// and the residual is dominated by remaining singletons, so we lower
		// the size threshold to 1 and let the size-aware tau discriminate.
		// After the large-group refit, sigma_hat drops significantly, so
		// tau(1) = sigma_hat*(1+C/sqrt(2)) is still above the background
		// singleton cross-talk level.
		sizeThreshold := minGroupSize
		if round > 0 {
			sizeThreshold = 1
		}

		// Find the group with the highest score above its H0-calibrated τ(g).
		var bestGroup *groupEntry
		var bestExcess float64 // score - tau(g), highest wins

		for _, g := range entries {
			// Skip groups whose rows are all already in the union.
			var anyNew bool
			for _, i := range g.rows {
				if _, inUnion := unionRowSet[i]; !inUnion {
					anyNew = true
					break
				}
			}
			if !anyNew {
				continue
			}

			if len(g.rows) < sizeThreshold {
				continue
			}

			// Compute ‖Φ_gᵀ r‖₂: only over NEW (not-yet-in-union) rows so
			// already-explained rows don't contaminate the score. rowScores
			// was precomputed once for all rows this round (see above).
			var normSq float64
			var newRows int
			for _, i := range g.rows {
				if _, inUnion := unionRowSet[i]; inUnion {
					continue
				}
				normSq += rowScores[i] * rowScores[i]
				newRows++
			}
			if newRows == 0 {
				continue
			}
			score := math.Sqrt(normSq) / math.Sqrt(float64(newRows))

			// H0-calibrated size-aware threshold over the NEW rows only.
			tauG := sigmaHat * (1 + CGroupDefault/math.Sqrt(2*float64(newRows)))

			excess := score - tauG
			if excess > bestExcess {
				bestExcess = excess
				bestGroup = g
			}
		}

		if bestGroup == nil {
			break // no group exceeds its threshold
		}

		// Activate the best group.
		activatedGroupIDs = append(activatedGroupIDs, bestGroup.id)
		for _, i := range bestGroup.rows {
			unionRowSet[i] = struct{}{}
		}

		// Refit on the updated union support.
		unionRows = sortedKeys(unionRowSet)
		unionValues = debias(csr, y, unionRows, debiasRidge)

		// Recompute residual.
		reconstructInto(csr, unionRows, unionValues, r)
		for i := range r {
			r[i] = y[i] - r[i]
		}
	}

	// ---------------------------------------------------------------
	// Final result from the last LS-refit.
	// ---------------------------------------------------------------
	var sumSq float64
	for _, ri := range r {
		sumSq += ri * ri
	}

	supportIDs := make([]uint64, len(unionRows))
	for i, row := range unionRows {
		supportIDs[i] = ids[row]
	}

	confidence := ConfidenceRecoveredGroup
	if len(activatedGroupIDs) == 0 {
		confidence = ConfidenceRecovered
	}

	return Result{
		SupportIDs:    supportIDs,
		GroupIDs:      activatedGroupIDs,
		Values:        unionValues,
		Residual:      math.Sqrt(sumSq),
		Confidence:    confidence,
		Coverage:      present,
		CoverageTotal: total,
		Revision:      o.Revision,
		Iters:         plainIters,
		Restarts:      plainRestarts,
		RawSupport:    len(unionRows),
	}, nil
}

// sortedKeys returns the keys of a map[int]struct{} sorted ascending.
func sortedKeys(m map[int]struct{}) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// effectiveLambda resolves a call's absolute L1 penalty: Options.LambdaAbs
// when set (G9 fix F2, magnitude-independent), else the ADR-014
// signal-scaled default via scaledLambda.
func effectiveLambda(csr *CSR, y []float64, o Options, n int) float64 {
	if o.LambdaAbs > 0 {
		return o.LambdaAbs
	}
	return scaledLambda(csr, y, o.Lambda, n)
}

// scaledLambda computes lambdaAbs = lambdaMul * max|Φᵀy| for lambda conformance
// (ADR-014): the LASSO penalty scales with the signal magnitude so lamOverL
// lies in [0.01, 0.05] at typical problem scales (SPEC §Recovery).
func scaledLambda(csr *CSR, y []float64, lambdaMul float64, n int) float64 {
	phiTy := make([]float64, n)
	csr.MulInto(y, phiTy) // phiTy = Phi^T @ y
	var maxV float64
	for _, v := range phiTy {
		if av := math.Abs(v); av > maxV {
			maxV = av
		}
	}
	if maxV == 0 {
		maxV = 1
	}
	return lambdaMul * maxV
}

// cappedSupport returns the row indices of elements with |x[i]| > threshold,
// capped to capSize by keeping the capSize largest by absolute value.
// Used to limit the plain-LASSO support to M/10 before debiasing (ADR-014 §4).
func cappedSupport(x []float64, threshold float64, capSize int) []int {
	support := make([]int, 0, capSize+1)
	for i, v := range x {
		if math.Abs(v) > threshold {
			support = append(support, i)
		}
	}
	if len(support) <= capSize {
		return support
	}
	ranked := make([]supportEntry, len(support))
	for i, row := range support {
		ranked[i] = supportEntry{row, math.Abs(x[row])}
	}
	partialSort(ranked, capSize)
	capped := make([]int, capSize)
	for i := range capped {
		capped[i] = ranked[i].row
	}
	sortInts(capped)
	return capped
}

// GroupZScore computes groupID's H0-calibrated z-score (ADR-014/Prompt 11c
// acceptance criterion (c): "how many standard deviations above the
// null-hypothesis mean does this group's residual-correlation score sit")
// against y's plain-path diagnostic residual — the same M/escalationCap
// context Group-OMP's own round-0 score test uses. Exported for
// observability: a caller that already escalated via Recover can log this
// alongside Result.GroupIDs to report detection confidence, without
// re-running FISTA/debias itself. Returns 0 if the dictionary is empty,
// groupID has no members, or the diagnostic residual is exactly zero
// (a perfect fit has no meaningful z-score).
func GroupZScore(y []float64, dict *Dictionary, p sketch.Params, grouper Grouper, groupID uint64, o Options) float64 {
	if grouper == nil {
		grouper = DefaultGrouper()
	}
	csr := dict.BuildCSR(p.Seed, p.M, p.D)
	n := csr.NRows
	if n == 0 {
		return 0
	}
	ids := dict.ActiveIDs()

	lambdaAbs := effectiveLambda(csr, y, o, n)
	pool := newMatvecPool(csr)
	defer pool.Close()
	x, _, _ := fista(pool, y, lambdaAbs, o.Iters, o.PowerIters, o.ObjectiveCheckEvery)

	// ompContextCap (M/5): the same diagnostic context recoverGroupOMP's
	// round-0 test scans (wider than the plain-path M/10 cap so
	// cross-talk-boosted group members don't crowd out the residual this
	// score is measured against).
	const ompContextCap = 5
	diagRows := cappedSupport(x, o.Threshold, p.M/ompContextCap)
	diagVals := debias(csr, y, diagRows, debiasRidge)
	r := make([]float64, p.M)
	reconstructInto(csr, diagRows, diagVals, r)
	for i := range r {
		r[i] = y[i] - r[i]
	}
	rNorm := l2norm(r)
	sigmaHat := rNorm / math.Sqrt(float64(p.M))
	if sigmaHat == 0 {
		return 0
	}

	var rows []int
	for i, id := range ids {
		if grouper(id, dict) == groupID {
			rows = append(rows, i)
		}
	}
	if len(rows) == 0 {
		return 0
	}

	var normSq float64
	for _, i := range rows {
		var dot float64
		for k := csr.RowPtr[i]; k < csr.RowPtr[i+1]; k++ {
			dot += float64(csr.Vals[k]) * r[csr.ColIdx[k]]
		}
		normSq += dot * dot
	}
	score := math.Sqrt(normSq) / math.Sqrt(float64(len(rows)))
	std := sigmaHat / math.Sqrt(2*float64(len(rows)))
	if std == 0 {
		return 0
	}
	return (score - sigmaHat) / std
}
