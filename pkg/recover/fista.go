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

	"github.com/passiveintent/Palimpsest/pkg/sketch"
)

// Confidence values a Result may carry (ADR-009's five recovery
// substrates). Recover (substrate a, "deviation via recovered support")
// only ever produces ConfidenceRecovered or degrades to ConfidenceFallback
// when there isn't enough verified data to trust a solve; PointQuery
// (substrate b) reports ConfidencePointQuery, and a caller's own
// keyframe-scan path (substrate c) reports ConfidenceKeyframe.
const (
	ConfidenceRecovered      = "recovered"
	ConfidenceRecoveredGroup = "recovered-group" // ADR-014: group-lasso escalation path
	ConfidencePointQuery     = "pointquery"
	ConfidenceKeyframe       = "keyframe"
	ConfidenceFallback       = "fallback"
)

// MergedTrust values a Result may carry (ADR-015): whether the ADR-015
// scaling-law guardrail found this window's cross-emitter fusion
// statistically justified. This is independent of Confidence, which keeps
// reporting which recovery mode actually ran regardless of MergedTrust —
// Recover/RecoverGroup never set this field themselves (they have no
// notion of which view is merged tier); it is left zero-valued until a
// caller (internal/core.Engine) stamps it after the guardrail is
// evaluated against that window's live coverage/support/dictionary size.
const (
	MergedTrustProven   = "proven"   // guardrail ratio >= MinSignalRatio
	MergedTrustUnproven = "unproven" // guardrail ratio < MinSignalRatio; result still emitted, not suppressed
	MergedTrustNA       = "na"       // not a merged-tier view; guardrail does not apply
)

// fistaMaxIters mirrors the Python conformance oracle's FISTA_MAX_ITERS
// cap (oracle/palimpsest_ref.py: "FISTA Iters cap is 350 per SPEC").
const fistaMaxIters = 350

// powerIterSeed is a fixed seed for the deterministic power-iteration
// starting vector (guardrail: deterministic recovery — the Lipschitz
// estimate must not depend on wall-clock or process-specific randomness).
const powerIterSeed = 1

// debiasRidge is debias's ridge regularization weight, matching the
// conformance oracle's default.
const debiasRidge = 1e-6

// Options configures a Recover call.
type Options struct {
	Iters      int     // FISTA iteration count (1..350)
	Lambda     float64 // L1 penalty weight in the FISTA objective
	PowerIters int     // power-iteration count for the Lipschitz estimate

	// Threshold is the post-FISTA support cutoff: a candidate is recovered
	// when |x_i| > Threshold. This is distinct from Lambda (the
	// optimization's L1 weight) — see oracle/palimpsest_ref.py's recover().
	Threshold float64

	// EmittersExpected, if > 0, enables the ADR-013 coverage gate: Recover
	// degrades to ConfidenceFallback whenever dict reports zero present
	// emitters (see Dictionary.ObserveEmitter/Coverage). 0 disables the
	// gate.
	EmittersExpected int

	// Revision is carried straight through to Result.Revision. ADR-013's
	// watermark/repair revision counter belongs to the caller's window
	// bookkeeping (which frames have merged so far, when a late frame
	// triggered a repair re-solve) rather than to a single Recover call.
	Revision int

	// EscalateThreshold, when > 0, enables the ADR-014 group-lasso escalation
	// path: if plain FISTA's Residual exceeds this value OR the recovered
	// support is denser than M/10 (operational ADR-004 cliff), Recover
	// re-runs as group-lasso. 0 disables escalation entirely.
	// Recommended production value: 0.3.
	EscalateThreshold float64

	// Grouper, if non-nil, overrides the default label-hierarchy grouper used
	// during escalation. nil uses DefaultGrouper(). Ignored when
	// EscalateThreshold is 0.
	Grouper Grouper

	// ObjectiveCheckEvery controls how often (every N iterations) fista
	// pays for the extra MulTransposeIntoAdd + F(x_new) evaluation that
	// both the best-seen restart criterion (ADR-014) and the early-exit
	// criterion (Prompt 12 step 5) need — one matvec, shared by both
	// features, on checked iterations only (Addendum A2). <= 0 uses the
	// default of 5, matching the early-exit streak window
	// (defaultObjectiveCheckEvery).
	ObjectiveCheckEvery int
}

// Result is a single Recover call's outcome.
type Result struct {
	SupportIDs    []uint64
	GroupIDs      []uint64 // flagged groups (ADR-014); non-nil only on the group-lasso path
	Values        []float64
	Residual      float64
	Confidence    string // "recovered" | "recovered-group" | "pointquery" | "keyframe" | "fallback"
	Coverage      int    // emitters present (ADR-013)
	CoverageTotal int    // emitters expected (ADR-013)
	Revision      int    // 0 for initial, incremented on repair (ADR-013)
	Iters         int
	Restarts      int    // function-value restarts fired (ADR-014)
	RawSupport    int    // len(support) before M/10 cap; used for escalation-size check
	MergedTrust   string // "" | "na" | "unproven" | "proven" (ADR-015); stamped by the caller, not by Recover itself
}

// Recover runs FISTA basis-pursuit-denoising recovery
// (min_x 0.5||y-Phi x||^2 + lambda||x||_1) over dict's currently active
// series, then debiases the recovered support via ridge least squares
// (debias.go). This implements ADR-002/ADR-009's substrate (a):
// "Deviation (via recovered support)".
//
// A zero-coverage window (Options.EmittersExpected > 0 and no emitter yet
// observed via Dictionary.ObserveEmitter) or an empty dictionary short
// circuits to a ConfidenceFallback Result without running FISTA: there is
// no verified data to solve against.
func Recover(y []float64, dict *Dictionary, p sketch.Params, o Options) (Result, error) {
	if len(y) != p.M {
		return Result{}, fmt.Errorf("recover: len(y)=%d != p.M=%d", len(y), p.M)
	}
	if o.Iters <= 0 || o.Iters > fistaMaxIters {
		return Result{}, fmt.Errorf("recover: iters %d out of range (1..%d)", o.Iters, fistaMaxIters)
	}
	if o.PowerIters <= 0 {
		return Result{}, fmt.Errorf("recover: power_iters must be positive, got %d", o.PowerIters)
	}
	if o.Threshold <= 0 {
		return Result{}, fmt.Errorf("recover: threshold must be positive, got %v", o.Threshold)
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
	ids := dict.ActiveIDs()
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

	// Lambda conformance (ADR-014): Options.Lambda is a multiplier applied to
	// max|Phi^T y| so the penalty scales with the problem's signal magnitude.
	// lamOverL ∈ [0.01, 0.05] at typical problem scales (SPEC §Recovery).
	lambdaAbs := scaledLambda(csr, y, o.Lambda, n)

	// pool parallelizes every MulInto/MulTransposeInto this solve makes
	// (FISTA's iteration loop, the Lipschitz power iteration, and — if
	// escalation fires below — Group-OMP's residual scan) across
	// GOMAXPROCS goroutines. Built once per solve and reused throughout.
	pool := newMatvecPool(csr)
	defer pool.Close()

	x, xRestarts, xIters := fista(pool, y, lambdaAbs, o.Iters, o.PowerIters, o.ObjectiveCheckEvery)

	// ompContextCap (M/5): plain-support context passed to Group-OMP. Larger
	// than the plain-path cap so cross-talk-boosted AZ members do not displace
	// scattered singletons from the working set (ADR-014 §Algorithm, step 1).
	const ompContextCap = 5
	// escalationCap (M/10): plain-path result cap AND the support-density
	// escalation trigger. Two functions, same constant: any plain support denser
	// than M/10 signals the phase-transition regime (ADR-004/ADR-014).
	const escalationCap = 10

	// Full (uncapped) support — used for the escalation support-density check.
	rawSupportLen := 0
	for _, v := range x {
		if math.Abs(v) > o.Threshold {
			rawSupportLen++
		}
	}

	// Cap support to M/escalationCap, keeping the highest-|x_i| candidates.
	support := cappedSupport(x, o.Threshold, p.M/escalationCap)

	supportIDs := make([]uint64, len(support))
	for i, row := range support {
		supportIDs[i] = ids[row]
	}

	values := debias(csr, y, support, debiasRidge)

	recon := make([]float64, p.M)
	reconstructInto(csr, support, values, recon)
	var sumSq float64
	for i := range y {
		diff := y[i] - recon[i]
		sumSq += diff * diff
	}
	plainResidual := math.Sqrt(sumSq)

	// ADR-014 escalation (Group-OMP kill path): escalate when plain FISTA
	// either (a) has high residual (poor fit) OR (b) recovers a dense uncapped
	// raw support (>M/10 — the ADR-004 cliff): dense regimes smear support.
	// Uses Group-OMP (groupomp.go) — not group-FISTA — because the OMP path
	// is 10–100× faster and immune to the block-threshold momentum latch.
	// Winner selection: escalation result returned only if its debiased
	// residual is strictly lower than the plain path's.
	if o.EscalateThreshold > 0 && (plainResidual > o.EscalateThreshold || rawSupportLen > p.M/escalationCap) {
		grouper := o.Grouper
		if grouper == nil {
			grouper = DefaultGrouper()
		}
		// Use ompContextCap (M/5) as OMP working set — wider than the plain-path
		// cap so cross-talk-boosted AZ members don't crowd out scattered singletons.
		ompRows := cappedSupport(x, o.Threshold, p.M/ompContextCap)
		// Reuse this call's pool/csr (Group-OMP's residual scan is one more
		// MulInto — Addendum A3); calling recoverGroupOMP directly avoids
		// rebuilding the CSR and matvec pool from scratch.
		gr, gerr := recoverGroupOMP(pool, csr, y, dict, p, grouper, x, ompRows, xRestarts, xIters, o)
		if gerr == nil && gr.Residual < plainResidual {
			return gr, nil
		}
	}

	return Result{
		SupportIDs:    supportIDs,
		Values:        values,
		Residual:      plainResidual,
		Confidence:    ConfidenceRecovered,
		Coverage:      present,
		CoverageTotal: total,
		Revision:      o.Revision,
		Iters:         xIters,
		Restarts:      xRestarts,
		RawSupport:    rawSupportLen,
	}, nil
}

// restartRelTol is the relative tolerance for the best-seen function-value
// restart (ADR-014 §3, Prompt 11c): restart fires iff
// F(x_new) > F_best * (1 + restartRelTol). On fire, F_best is RESET to
// F(x_new) (not to the pre-restart F_best), which prevents cascade restarts
// — the next restart tests against the current regressed level, not the
// old minimum.  The 1e-6 relative tolerance absorbs floating-point noise
// while catching genuine objective regressions from momentum overshoot.
const restartRelTol = 1e-6

// defaultObjectiveCheckEvery is Options.ObjectiveCheckEvery's default.
const defaultObjectiveCheckEvery = 5

// earlyExitStreak is K in "stop when relative objective change < 1e-6 for
// K iters" (Prompt 12 step 5): the number of consecutive CHECKED
// iterations (ObjectiveCheckEvery apart, not consecutive raw iterations)
// that must show a small change before fista stops early. Requiring a
// streak (not just one small reading) guards against a single
// coincidentally-flat checked iteration mid-convergence.
const earlyExitStreak = 5

// earlyExitRelTol is the relative objective-change threshold for early
// exit (Prompt 12 step 5).
const earlyExitRelTol = 1e-6

// fista runs the FISTA iteration for min_x 0.5||y-Phi x||^2 + lambdaAbs||x||_1
// over pool (wrapping Phi^T) with best-seen prox-iterate restart (ADR-014
// §3) and early-exit (Prompt 12 step 5). F(x_new) is expensive (one extra
// MulTransposeIntoAdd), so it is evaluated only every objectiveCheckEvery
// iterations (<=0 uses defaultObjectiveCheckEvery), serving both features
// from that single evaluation (Addendum A2):
//
//   - Best-seen restart: fires iff F(x_new) > F_best*(1+restartRelTol) on
//     a checked iteration. On fire: reset t=1, z=x_new, F_best=F(x_new).
//     Otherwise F_best=min(F_best, F(x_new)).
//   - Early exit: stops once earlyExitStreak consecutive checked
//     iterations each show |relative change| < earlyExitRelTol versus the
//     previous checked value.
//
// Unchecked iterations run the plain FISTA momentum update (no restart
// decision is possible without F(x_new) that iteration). Returns the
// recovered x, the restart count, and the number of iterations actually
// run (<= iters; Result.Iters reports this).
// Guardrail: no allocation inside the iteration loop.
func fista(pool *matvecPool, y []float64, lambdaAbs float64, iters, powerIters, objectiveCheckEvery int) ([]float64, int, int) {
	n, m := pool.csr.NRows, pool.csr.NCols
	L := estimateLipschitz(pool, powerIters)
	lamOverL := lambdaAbs / L

	if objectiveCheckEvery <= 0 {
		objectiveCheckEvery = defaultObjectiveCheckEvery
	}

	x := make([]float64, n)
	z := make([]float64, n)
	xNew := make([]float64, n)
	grad := make([]float64, n)
	resid := make([]float64, m) // fused: seeded with -y, then scatter-added into (Prompt 12 step 4)
	dNew := make([]float64, m)  // same fusion, for the shared objective check

	// negY seeds resid/dNew each iteration so MulTransposeIntoAdd's
	// scatter accumulates directly into Phi@v - y, fusing what would
	// otherwise be a separate O(m) subtraction pass over a freshly
	// zeroed-then-scattered buffer into one O(m) copy + scatter-add.
	negY := make([]float64, m)
	for i, yi := range y {
		negY[i] = -yi
	}

	// F(0) = 0.5*||y||^2 (x=0 initial).
	var fBest float64
	for _, yi := range y {
		fBest += yi * yi
	}
	fBest *= 0.5
	fLastChecked := fBest
	var smallStreak int

	t := 1.0
	var restarts, used int
	for iter := 0; iter < iters; iter++ {
		used = iter + 1
		copy(resid, negY)
		pool.MulTransposeIntoAdd(z, resid) // resid = Phi@z - y
		pool.MulInto(resid, grad)          // grad = Phi^T @ resid

		for i := 0; i < n; i++ {
			xNew[i] = softThreshold(z[i]-grad[i]/L, lamOverL)
		}

		// Only checked iterations pay for F(x_new); the last iteration is
		// always checked so a run that hits the iters cap still reports
		// an up-to-date restart/objective state.
		checked := used%objectiveCheckEvery == 0 || iter == iters-1
		if !checked {
			tNew := (1 + math.Sqrt(1+4*t*t)) / 2
			coeff := (t - 1) / tNew
			for i := 0; i < n; i++ {
				zi := xNew[i] + coeff*(xNew[i]-x[i])
				x[i] = xNew[i]
				z[i] = zi
			}
			t = tNew
			continue
		}

		// Compute F(x_new) = 0.5*||y-Phi x_new||^2 + lambda*||x_new||_1.
		copy(dNew, negY)
		pool.MulTransposeIntoAdd(xNew, dNew) // dNew = Phi@x_new - y
		var fNew float64
		for _, d := range dNew {
			fNew += d * d
		}
		fNew *= 0.5
		for _, xi := range xNew {
			if xi > 0 {
				fNew += lambdaAbs * xi
			} else if xi < 0 {
				fNew -= lambdaAbs * xi
			}
		}

		// Best-seen restart: fire iff F(x_new) > F_best*(1+restartRelTol).
		// On fire: reset t=1, z=x_new, AND F_best=F(x_new) (prevents cascades).
		// On normal: F_best = min(F_best, F(x_new)).
		if fNew > fBest*(1+restartRelTol) {
			t = 1.0
			for i := 0; i < n; i++ {
				x[i] = xNew[i]
				z[i] = xNew[i]
			}
			fBest = fNew // reset — next restart fires vs THIS regressed level
			restarts++
		} else {
			tNew := (1 + math.Sqrt(1+4*t*t)) / 2
			coeff := (t - 1) / tNew
			for i := 0; i < n; i++ {
				zi := xNew[i] + coeff*(xNew[i]-x[i])
				x[i] = xNew[i]
				z[i] = zi
			}
			t = tNew
			if fNew < fBest {
				fBest = fNew
			}
		}

		// Early exit: earlyExitStreak consecutive checked iterations with
		// small relative change versus the previous checked value.
		denom := math.Abs(fLastChecked)
		if denom == 0 {
			denom = 1
		}
		if math.Abs(fLastChecked-fNew)/denom < earlyExitRelTol {
			smallStreak++
			if smallStreak >= earlyExitStreak {
				fLastChecked = fNew
				break
			}
		} else {
			smallStreak = 0
		}
		fLastChecked = fNew
	}
	return x, restarts, used
}

// softThreshold is the proximal operator of the L1 norm.
func softThreshold(v, thresh float64) float64 {
	switch {
	case v > thresh:
		return v - thresh
	case v < -thresh:
		return v + thresh
	default:
		return 0
	}
}

// estimateLipschitz estimates the Lipschitz constant of the FISTA gradient
// (the largest eigenvalue of Phi^T Phi) via power iteration, matching
// oracle/palimpsest_ref.py's _estimate_lipschitz. The starting vector is
// deterministic (powerIterSeed): power iteration converges to the top
// eigenvector from any generic starting point given enough iterations, so
// determinism here costs nothing in practice.
func estimateLipschitz(pool *matvecPool, powerIters int) float64 {
	n, m := pool.csr.NRows, pool.csr.NCols
	rng := rand.New(rand.NewSource(powerIterSeed))
	v := make([]float64, n)
	for i := range v {
		v[i] = rng.NormFloat64()
	}
	tmp := make([]float64, m)
	av := make([]float64, n)

	norm := l2norm(v)
	if norm == 0 {
		return 1
	}
	for i := range v {
		v[i] /= norm
	}

	for iter := 0; iter < powerIters; iter++ {
		pool.MulTransposeInto(v, tmp) // tmp = Phi @ v
		pool.MulInto(tmp, av)         // av = Phi^T Phi v
		norm = l2norm(av)
		if norm == 0 {
			return 1
		}
		for i := range v {
			v[i] = av[i] / norm
		}
	}
	pool.MulTransposeInto(v, tmp)
	pool.MulInto(tmp, av)
	var rayleigh float64
	for i := range v {
		rayleigh += v[i] * av[i]
	}
	if rayleigh <= 0 {
		return 1
	}
	return rayleigh
}

// reconstructInto writes Phi @ x (x sparse, given as support row indices
// with parallel values) into out, which must have length csr.NCols. out is
// zeroed first.
func reconstructInto(csr *CSR, support []int, values []float64, out []float64) {
	for i := range out {
		out[i] = 0
	}
	for si, row := range support {
		v := values[si]
		if v == 0 {
			continue
		}
		for k := csr.RowPtr[row]; k < csr.RowPtr[row+1]; k++ {
			out[csr.ColIdx[k]] += float64(csr.Vals[k]) * v
		}
	}
}

func l2norm(v []float64) float64 {
	var sumSq float64
	for _, x := range v {
		sumSq += x * x
	}
	return math.Sqrt(sumSq)
}

// partialSort rearranges entries so the k largest (by abs field) occupy
// entries[0..k-1] in arbitrary order (Floyd-Rivest quickselect, O(n) average).
// Used to cap the support to M/10 without a full O(n log n) sort.
type supportEntry struct {
	row int
	abs float64
}

func partialSort(entries []supportEntry, k int) {
	if k >= len(entries) {
		return
	}
	lo, hi := 0, len(entries)-1
	for lo < hi {
		pivot := entries[(lo+hi)/2].abs
		i, j := lo, hi
		for i <= j {
			for entries[i].abs > pivot {
				i++
			}
			for entries[j].abs < pivot {
				j--
			}
			if i <= j {
				entries[i], entries[j] = entries[j], entries[i]
				i++
				j--
			}
		}
		if j < k {
			lo = i
		}
		if i > k {
			hi = j
		}
		if j < k && i > k {
			break
		}
	}
}

// sortInts sorts a small slice of ints ascending (used for capped support).
func sortInts(a []int) {
	// insertion sort: the capped support is at most M/10 ≈ 200 elements.
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}
