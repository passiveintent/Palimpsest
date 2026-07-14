# ADR-016 — Gate G9: Final verdict on the compressed-sensing layer

- Status: PRE-REGISTERED — results pending
- Date: 2026-07-14
- Protocol: pre-registered confirmatory experiment. The "Pre-registered
  predictions" and "Kill criteria" sections below were copied verbatim from
  the gate prompt BEFORE any code was changed, and may never be edited.
  Calibration (Phase 2) touches only `c_gate` and `kappa`, only on the
  calibration set. Confirmatory scenarios run once, with locked parameters,
  on seeds {41,42,43}. Failing or unrunnable measurements are reported as
  INCONCLUSIVE, never substituted or dropped.

> Naming note: `docs/adr/ADR-016-kafka-transport.md` already holds the
> ADR-016 number in the `docs/adr/` sequence. This document lives at
> `docs/ADR-016-cs-verdict.md` because the gate protocol names that exact
> path as the deliverable; the path, not the number, is normative here.

---

## KILL CRITERIA (verbatim, pre-registered, evaluate in order; first match decides)

K1 If S3 detectability is non-monotone post-fix (any higher magnitude
   detected with lower probability), verdict = KILL (decoder pathology
   survives its principled fix).
K2 Unique-detection set: incidents detected by the CS layer that
   substrate (c)+F3 misses or detects >60 s later. If this set is empty
   across S1–S4, verdict = KILL.
K3 If unique detections exist but with >5 spurious series per true event
   or quiet-month events >1/deployment-day, verdict = KILL (unusable
   precision).
K4 Dumb-money test: for each unique-detection class, compare Palimpsest's
   post-fix total bytes against the cheapest classical config (from the
   Phase 3 baselines) achieving >=90% of the same recall on that class.
   If a classical config wins bytes for EVERY unique class, verdict =
   KILL (the value exists but classical buys it cheaper).
K5 Otherwise verdict = KEEP-NARROW: CS ships as an optional sidecar
   scoped to the named class(es) it uniquely and economically wins,
   with the operational significance of that class honestly assessed.

## PRE-REGISTERED PREDICTIONS (verbatim, from the adversarial reviewer)

P1 S3 monotonicity is restored by F2 (donut hole dies). Confidence: high.
P2 S5 spurious support drops to single digits under F1. Confidence:
   medium-high.
P3 S1 detection floor improves by less than 2x and the scenario remains
   in the dead zone — pooled-noise physics, not decoder error.
   Confidence: medium-high.
P4 S4 is detected by CS (native turf). Confidence: medium.
P5 K4 goes against CS: a 15–30 s keyframe cadence or routed-exact config
   matches S4 recall for fewer bytes. Confidence: medium.
   Net expected verdict: KILL, with KEEP-NARROW possible only via S4
   surviving K4.

---

## Locked definitions (declared before any run; not tunable in Phase 2)

The fix specification leaves a small number of implementation constants
open. They are fixed here, before baseline or calibration runs, and are
NOT calibration parameters:

- **Event** (for the quiet budget and K3): one emitted deviation event =
  one non-keyframe window whose solve produced non-empty support and
  passed the residual gate. "Deployment-day" = one simulated day of one
  deployment (single decode pipeline).
- **sigma_hat (decode side, F1/F2)**: per-series noise scale estimated
  from the sketch itself: `sigma_bucket = MAD(y)/0.6745` over the m
  bucket values of a quiet window, converted to per-series units by
  `sigma_series = sigma_bucket * sqrt(m/n)` (columns of Phi are
  unit-norm: d entries of ±1/sqrt(d), so n series of per-series jitter
  sigma give bucket std sigma*sqrt(n/m)). A rolling window of the last
  30 quiet windows' estimates is kept — quiet = no storm fired, no
  deviation event emitted, and no series currently frozen under F3 (an
  active freeze means an incident is in progress and the sketch carries
  its sustained residual) — and sigma_hat is the median of that buffer.
  Until the buffer has 5 entries, detectors treat the window as warm-up
  and emit nothing.
- **F1 gate**: `gate = c_gate * sqrt(n) * sigma_hat`, n = active
  dictionary size at solve time. (Expected quiet solve-residual L2 norm is
  sigma*sqrt(n) under the unit-norm-column convention; c_gate absorbs the
  remaining O(1) factors and is calibrated in Phase 2.)
- **F2 lambda**: `lambda_abs = kappa * sigma_hat * sqrt(2 ln N)`, N =
  active dictionary size. kappa absorbs the design factor sqrt(n/m) at
  the calibrated fleet shape and is calibrated in Phase 2. This replaces
  `lambda = 0.05 * max|Phi^T y|` and removes the penalty's dependence on
  anomaly magnitude. Stage-2 refit note: the production solver already
  refits the selected support by ridge least squares (ridge 1e-6,
  effectively unpenalized) and computes the gate residual from the refit
  reconstruction (pkg/recover: debias.go, fista.go); F2's stage 2 is
  therefore already present, and the fix's substance is the lambda law.
  This is recorded as-found, not improvised.
- **F3 band**: a series is flagged when any detector fires on it (an
  emitted deviation event names it; substrate (c) drift exceeds its
  threshold; it appears in a FALLBACK top-K during a storm window).
  While flagged, keyframe re-basing of that series' baseline is frozen —
  in the Hold predictor (CS residual baseline) and in substrate (c)'s
  previous-keyframe reference. "Within band" = |current value − frozen
  baseline| ≤ max(3 * sigma_hat_enc, 0.3), where sigma_hat_enc is the
  encoder-side rolling MAD estimate of that series' own residuals and 0.3
  is the existing FISTA support floor. Three consecutive in-band windows
  unfreeze the series.
- **K1 operationalization (S3)**: detection rates are pooled across the
  three confirmatory seeds (≥90 trials per magnitude). Non-monotone means
  a higher magnitude's pooled detection rate falls below any lower
  magnitude's by more than 0.05 (≈1σ of binomial sampling noise at n=90).
  The full raw table is published either way, so a reader can apply the
  strict zero-tolerance reading themselves.
- **Detection-eligibility window**: all detectors (CS and classical) are
  scored only after a shared warm-up (storm median + sigma buffer, 36
  windows) so pre-fix and post-fix configs see identical eligible spans.
- **Substrate (c) threshold** stays at its production default 10.0
  (absolute keyframe-to-keyframe delta). F1 normalizes the CS gate only;
  the classical competitor gets F3 (as specified) but no new threshold.
- **Confirmatory harness fleet (`plsim --g9`, new mode)**: 900 background
  logical series (per-series baseline 50–150, daily seasonality amplitude
  0.25, per-window Gaussian jitter sigma = 0.2 absolute residual units)
  plus one reserved low-volume target series (baseline 0.02, same jitter),
  m=2000, d=6, bits=8, 10 s flush, keyframes every 6 windows (golden every
  10th), single emitter, storm 25× over 30 windows, FALLBACK top-K 100,
  FISTA iters 350 / threshold 0.3, no churn, snapshots disabled. This
  matches docs/DEADZONE.md's measured configuration and the motivating S1
  regime. Because kappa absorbs the design factor sqrt(n/m), the
  calibration set runs on this same fleet shape ("quiet-month traffic" =
  this generator with no anomalies, seasonality on), seeds {100,101}.
- **Calibration selection rule (declared before calibration)**: on the
  calibration set, choose the smallest kappa (most sensitive) from the
  declared grid whose quiet-window event rate, with c_gate at the smallest
  grid value meeting the budget, is ≤ 0.5 events/deployment-day (2×
  safety margin under the ≤1/day budget); c_gate is then the smallest
  grid value meeting that same ≤0.5/day target at the chosen kappa.
  Calibration solves sample every 2nd window; rates are normalized to
  events/day. Calibration anomaly check magnitudes: {3, 7} (disjoint from
  every confirmatory magnitude).
- **Byte-ledger baselines (K4)**: (i) Gorilla at 1.37 B/sample on the raw
  series count; (ii) DUMB-MONEY: exact keyframes at 30 s and at 15 s
  cadence (real wire.BuildKeyframe KDELTA/golden sizes measured on the
  same fleet, cadence-scaled), each paired with the substrate (c)+F3
  drift rule evaluated on that cadence's sample grid; (iii)
  "interestingness-routed 10 s exact on the top 5% of series": 60 s
  keyframes for the fleet plus exact 10 s samples (priced at Gorilla
  1.37 B/sample) for the top 5% of series ranked by trailing residual
  variance, with full-resolution threshold detection on routed series
  only.
- **Runtime-budget deviation, declared up front**: the Phase 0 PRE-FIX
  `--month` snapshot uses `--month-days 6` (existing flag; incidents and
  all other knobs at defaults, scaled proportionally by the generator
  itself) because three full 30-day runs alone exceed the gate's ~30 min
  wall-clock budget on this machine. Any post-fix month comparison uses
  the identical configuration and seeds.

## Phase records

(Appended as each phase completes. The pre-registration sections above
are frozen.)

### Phase 0 — PRE-FIX baseline snapshot (seeds {41,42,43})

Archived in `results/g9/prefix/` with exact replay commands in
`results/g9/prefix/COMMANDS.md`. Repo state before fixes: commit 93fe626.

`plsim --deadzone` boundary tables reproduce docs/DEADZONE.md across all
three seeds (seed-level wobble only between 0.4/0.8 at σ∈{0.05,0.1}):
σ=0.2 column has no reliable recovery floor, detection floor 51.2 (storm),
dead zone up to 25.6; σ≥0.8 columns are entirely dead to 102.4. The σ=0.2
band-pass (donut) is present in every seed's CSV (e.g. seed 41:
recovery_rate 0.96/0.88 at ε=0.8/1.6 falling to 0.04→0.00 at ε≥3.2 —
this is the K1 pathology, pre-fix).

`plsim --month --month-days 6 --codec zstd` (declared runtime-budget
deviation): amortized compression 12.0× vs snappy on all seeds; substrate
(a) emitted nothing in any run — 100% of quiet solves gated at the fixed
gate 5.0 (mean solve residual 19.85/19.99/19.91 vs gate 5, mean spurious
support 196).

### Phase 1 — fixes implemented (all opt-in, off by default)

- F1/F2 plumbing: `pkg/recover` gained `Options.LambdaAbs` (absolute,
  magnitude-independent L1 penalty; 0 = pre-existing ADR-014 scaling) and
  `MADSigma`/`QuietSigma` (robust noise estimation per the locked
  definitions). All three lambda call sites route through
  `effectiveLambda`.
- F2 stage-2 note confirmed in code: `Recover` already refits the
  selected support by ridge-LS (`debias`, ridge 1e-6) and computes the
  gate residual from the refit reconstruction; no change needed there.
- F3: plsim's pipeline gained `SetFrozen` — frozen series keep their Hold
  baseline at keyframes (`cmd/plsim/encoder.go`); the harness applies the
  same freeze to substrate (c)'s reference and to the classical cadence
  samplers' references, per the fix spec. Pinned by
  `TestPipelineFreezeBlocksRebasing`.
- New `plsim --g9` harness (`cmd/plsim/g9.go`, `g9_calib.go`,
  `g9_sweep.go`): scenario suite, calibration mode, post-fix dead-zone
  sweep. `--deadzone`/`--month` generators untouched; full test suite
  passes.
- Engineering smoke runs used seed 999 only (never 41/42/43/100/101);
  their outputs are in `results/g9/smoke/` for audit and were used solely
  to verify the harness executes, not to pick parameters. One honest
  observation recorded before confirmation: pre-fix CS "detections" at
  this fleet are partly chance membership in ~170-500-series spurious
  support sets; the spurious-per-event metric is the qualifier.
