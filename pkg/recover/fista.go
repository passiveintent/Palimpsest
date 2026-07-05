package recover

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/purushpsm147/palimpsest/pkg/sketch"
)

// Confidence values a Result may carry (ADR-009's five recovery
// substrates). Recover (substrate a, "deviation via recovered support")
// only ever produces ConfidenceRecovered or degrades to ConfidenceFallback
// when there isn't enough verified data to trust a solve; PointQuery
// (substrate b) reports ConfidencePointQuery, and a caller's own
// keyframe-scan path (substrate c) reports ConfidenceKeyframe.
const (
	ConfidenceRecovered  = "recovered"
	ConfidencePointQuery = "pointquery"
	ConfidenceKeyframe   = "keyframe"
	ConfidenceFallback   = "fallback"
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
}

// Result is a single Recover call's outcome.
type Result struct {
	SupportIDs    []uint64
	Values        []float64
	Residual      float64
	Confidence    string // "recovered" | "pointquery" | "keyframe" | "fallback"
	Coverage      int    // emitters present (ADR-013)
	CoverageTotal int    // emitters expected (ADR-013)
	Revision      int    // 0 for initial, incremented on repair (ADR-013)
	Iters         int
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

	x := fista(csr, y, o.Lambda, o.Iters, o.PowerIters)

	support := make([]int, 0)
	for i, v := range x {
		if math.Abs(v) > o.Threshold {
			support = append(support, i)
		}
	}

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

	return Result{
		SupportIDs:    supportIDs,
		Values:        values,
		Residual:      math.Sqrt(sumSq),
		Confidence:    ConfidenceRecovered,
		Coverage:      present,
		CoverageTotal: total,
		Revision:      o.Revision,
		Iters:         o.Iters,
	}, nil
}

// fista runs the FISTA iteration for min_x 0.5||y-Phi x||^2 + lambda||x||_1
// over csr (Phi^T). Guardrail: no allocation inside the iteration loop —
// every buffer below is allocated once, before it, and reused in place.
func fista(csr *CSR, y []float64, lambda float64, iters, powerIters int) []float64 {
	n, m := csr.NRows, csr.NCols
	L := estimateLipschitz(csr, powerIters)
	lamOverL := lambda / L

	x := make([]float64, n)
	z := make([]float64, n)
	xNew := make([]float64, n)
	grad := make([]float64, n)
	phiZ := make([]float64, m)
	resid := make([]float64, m)

	t := 1.0
	for iter := 0; iter < iters; iter++ {
		csr.MulTransposeInto(z, phiZ) // phiZ = Phi @ z
		for i := range resid {
			resid[i] = phiZ[i] - y[i]
		}
		csr.MulInto(resid, grad) // grad = Phi^T @ resid

		for i := 0; i < n; i++ {
			xNew[i] = softThreshold(z[i]-grad[i]/L, lamOverL)
		}

		tNew := (1 + math.Sqrt(1+4*t*t)) / 2
		coeff := (t - 1) / tNew
		for i := 0; i < n; i++ {
			zi := xNew[i] + coeff*(xNew[i]-x[i])
			x[i] = xNew[i]
			z[i] = zi
		}
		t = tNew
	}
	return x
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
func estimateLipschitz(csr *CSR, powerIters int) float64 {
	n, m := csr.NRows, csr.NCols
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
		csr.MulTransposeInto(v, tmp) // tmp = Phi @ v
		csr.MulInto(tmp, av)         // av = Phi^T Phi v
		norm = l2norm(av)
		if norm == 0 {
			return 1
		}
		for i := range v {
			v[i] = av[i] / norm
		}
	}
	csr.MulTransposeInto(v, tmp)
	csr.MulInto(tmp, av)
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
			out[csr.ColIdx[k]] += csr.Vals[k] * v
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
