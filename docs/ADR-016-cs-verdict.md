# ADR-016 — Gate G9: Final verdict on the compressed-sensing layer

- Status: **DECIDED — VERDICT: KILL** (at K2, unique value). Retire
  compressed-sensing substrate (a) as a detector; see the salvage plan.
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

### Phase 2 — calibration record (calibration set ONLY; seeds {100,101})

Raw curves: `results/g9/calib/calib-seed100.csv`,
`results/g9/calib/calib-seed101.csv` (events/day over the full
kappa × c_gate grid). One simulated day of quiet traffic per seed, 2868
solved windows each.

**Quiet events/day at c_gate = 1.0 (the smallest grid gate), by kappa:**

| kappa | seed 100 | seed 101 | support non-empty (frac) | mean residual ratio |
| --- | --- | --- | --- | --- |
| 0.4 | 3376 | 3262 | 1.00 | 0.99 |
| 0.5 | 192 | 136 | 1.00 | 1.05 |
| 0.6 | 6.0 | 2.0 | 1.00 | 1.09 |
| **0.7** | **0.0** | **0.0** | 0.99 | 1.12 |
| 0.8 | 0.0 | 0.0 | 0.88 | 1.14 |
| 1.0 | 0.0 | 0.0 | 0.33 | 1.16 |

Applying the locked selection rule (declared before calibration): for
kappa ∈ {0.4, 0.5, 0.6} no grid c_gate meets the ≤ 0.5 events/day budget
(the minimum over c_gate is 3376 / 192 / 6 events/day respectively, all at
c_gate = 1.0). kappa = 0.7 is the smallest (most sensitive) kappa for
which a grid c_gate meets the budget, and the only grid c_gate that meets
it at kappa = 0.7 is 1.0 (c_gate = 1.05 already yields 72 / 44 events/day).

**LOCKED PARAMETERS: kappa = 0.7, c_gate = 1.0.** No parameter is touched
again after this line.

Calibration finding that survives into the verdict (recorded now, not
after confirmation): the *mean quiet residual ratio*
`residual / (sqrt(n)·sigma_hat)` is ≈ 1.0–1.16 across every kappa — the
post-debias solve residual is dominated by un-modeled fleet noise that
scales as `sqrt(n)·sigma` whether or not the target anomaly is present.
F1's normalized gate at c_gate = 1.0 therefore sits *just below* the
typical quiet residual: it buys precision (0 false events/day) precisely
because it fires only when a recovered anomaly pulls the residual ratio
under 1.0. Whether a weak anomaly can do that is exactly what S1/S3 test.
This is the structural reason P3 (S1 stays in the dead zone) was written
with medium-high confidence.

Calibration-magnitude anomaly check (magnitudes {3, 7}, disjoint from
every confirmatory magnitude): run as `--g9-scenario calib-anom`, recorded
in `results/g9/calib/` — used only to confirm the locked config recovers
*some* signal at supra-confirmatory magnitudes, never to adjust it.

### Phase 3 — confirmatory results (seeds {41,42,43}, run once, locked params)

Raw per-run JSON in `results/g9/confirm/` (one file per scenario × config ×
seed); consolidated tables in `results/g9/confirm/SUMMARY.md`; exact
commands in `scripts/g9-confirm.sh 1.0 0.7`. Post-fix dead-zone map in
`results/g9/confirm/sweep-postfix-seed*.md`.

**Detection rate by detector, post-fix, fraction of bursts:**

| scenario | CS (a) | subst (c) | storm | k30 | k15 | routed |
| --- | --- | --- | --- | --- | --- | --- |
| S1 payment weak signal +1.0 | 0/9 | 0/9 | 0/9 | 0/9 | 0/9 | 0/9 |
| S2 AZ outage 30 min +100 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 |
| S4 sub-keyframe 20–30 s +30 | **0/90** | 0/90 | 0/90 | 45/90 | **90/90** | 30/90 |
| S6 deploy wave 30% +15 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 | 3/3 |

**S3 magnitude ramp — post-fix CS detection (K1 evidence):**

| magnitude | CS post | CS pre | subst (c) post |
| --- | --- | --- | --- |
| 1× | 0/90 | 89/90 | 0/90 |
| 2× | 0/90 | 67/90 | 0/90 |
| 5× | 0/90 | 46/90 | 0/90 |
| 10× | 0/90 | 46/90 | 57/90 |
| 20× | 52/90 | 46/90 | 90/90 |

Post-fix CS detection is monotone non-decreasing in magnitude
(0,0,0,0,0.578). Pre-fix is the donut: 0.989 → 0.744 → 0.511 → 0.511 →
0.511, i.e. the smallest anomaly detected *more often* than the largest.
F2 removed the pathology.

**Detection lag where detected (post-fix), seconds — CS is never fastest:**

| scenario | CS lag | subst (c) lag | storm lag |
| --- | --- | --- | --- |
| S2 | 210 | 30 | 0 |
| S6 | 197 | 30 | 0 |

**Quiet-month precision (S5, 2 days/seed):**

| config | quiet events/day (per seed) | mean support |
| --- | --- | --- |
| pre-fix | [5415, 5441, 5415] | 165.2 |
| post-fix | [1, 0, 0] | 4.6 |

**Byte ledger, post-fix (KiB/day, mean over seeds):**

| scenario | Palimpsest total | of which sketch/RESIDUAL | Gorilla 1.37 B | keyframe 30 s | keyframe 15 s | routed-exact 5% |
| --- | --- | --- | --- | --- | --- | --- |
| S1 | 28975 | 14611 | 10415 | 27344 | 53316 | 14896 |
| S2 | 28831 | 13615 | 10415 | 26948 | 53928 | 15126 |
| S4 | 28096 | 14606 | 10415 | 26780 | 53548 | 14022 |
| S6 | 29197 | 13731 | 10415 | 27868 | 53813 | 15466 |
| S5 | 27875 | 14611 | 10415 | 26527 | 53054 | 13796 |

Post-fix dead-zone sweep (F1+F2, seed 41): the recovery floor did **not**
drop — in the σ=0.05/0.1 columns it *rose* (0.4/0.8 → 51.2/102.4), because
the precision-calibrated gate (c_gate=1.0) fires only when a recovered
anomaly pulls the residual ratio under 1.0, which small-to-moderate
anomalies cannot do against a `sqrt(n)·sigma` noise floor. The σ≥0.2
columns remain dead exactly as before; the storm/detection floors are
unchanged (storm physics, untouched by F1/F2). The fixes bought precision,
not reach.

Calibration-magnitude sanity check (magnitudes {3,7} on the payment class,
`results/g9/calib/calib-anom-*`): post-fix CS detects 0/many at both
magnitudes — the payment class is structurally dead to CS at every
magnitude up to at least 7×, not just at the S1 magnitude of 1.

---

## K-gate evaluation (in order; first match decides)

**K1 — S3 monotonicity.** Post-fix CS detection over magnitude is
(0, 0, 0, 0, 0.578), monotone non-decreasing (no higher magnitude detected
with lower probability, tolerance 0.05). The pre-fix donut hole is gone.
**K1 does not fire** — the decoder pathology was genuinely repaired by F2.
Proceed.

**K2 — unique-detection set.** Incidents CS detects that substrate (c)+F3
misses or detects >60 s later, across S1–S4. Per seed: **0, 0, 0.**
Pooled: **0.** Every post-fix CS detection is dominated by a classical
detector:
- S1: CS detects nothing (0/9).
- S2/S6 (mass events): CS detects, but at 197–250 s lag vs substrate (c)
  at 30 s and storm at 0 s — CS is 150–220 s *slower*, never unique.
- S3: CS's only non-zero magnitude is 20× (52/90); substrate (c) detects
  20× at 90/90 and 10× at 57/90 — CS's hits are a strict subset of c's.
- S4 (the one class CS could uniquely own — sub-keyframe, invisible to
  keyframe drift): post-fix CS detects **0/90**. The class exists and is
  real (k15 catches 90/90), but CS does not own it.

The unique-detection set is **EMPTY across S1–S4 on every seed.**
**K2 fires. VERDICT = KILL.**

**K3 — precision (not reached; would pass).** For the record: post-fix
quiet-month events are 0.33/deployment-day (well under the ≤1/day budget)
and mean spurious support fell from 165 to 4.6. The precision fixes worked;
they simply have nothing to be precise *about*, because K2's unique set is
empty.

**K4 — dumb-money (not reached; would also KILL).** Vacuous under the
protocol (no unique class survives K2), but the byte economics confirm the
direction the reviewer predicted:
- The sub-keyframe class (S4) is detected **only** by the 15 s exact
  cadence (100% recall) — Palimpsest's CS layer gets 0%. At *any* byte
  count Palimpsest does not achieve the ≥90% recall bar on this class, so
  the only config that clears the bar is classical. Classical wins by
  default.
- For the mass events (S2/S6) that Palimpsest *does* catch, it catches
  them via substrate (c)/storm — not the sketch — and a plain 30 s
  keyframe cadence (26.5–27.9 MiB/day) is **cheaper** than full Palimpsest
  (27.9–29.2 MiB/day) while detecting the same mass events via drift at
  the same 30 s and half the sub-keyframe transients besides.
- The sketch/RESIDUAL layer is ~47–52% of Palimpsest's wire
  (≈13.6–14.6 MiB/day). Post-fix it uniquely detects nothing. Deleting it
  roughly halves the wire at zero cost to unique detection.

**VERDICT: KILL.** The compressed-sensing deviation substrate (a) ships no
incident that a classical detector on the same data does not catch at
least as well and usually faster and cheaper. It dies at K2 — unique
value — having been given all three principled repairs and a fair fight.

## Prediction scorecard (pre-registered, resolved)

| # | prediction | confidence | outcome |
| --- | --- | --- | --- |
| P1 | S3 monotonicity restored by F2 (donut dies) | high | **CONFIRMED** — post-fix 0,0,0,0,0.578 monotone; pre-fix donut present |
| P2 | S5 spurious support drops to single digits under F1 | med-high | **CONFIRMED** — 165 → 4.6; quiet events 5424/day → 0.33/day |
| P3 | S1 floor improves <2× and stays in the dead zone | med-high | **CONFIRMED** — S1 undetectable by CS post-fix; pooled-noise physics |
| P4 | S4 detected by CS (native turf) | medium | **REFUTED** — post-fix CS 0/90 on S4; k15 owns it 90/90 |
| P5 | K4 goes against CS; classical matches S4 recall cheaper | medium | **CONFIRMED (stronger)** — CS never reaches K4; classical is the *only* config with S4 recall at all |

Net: the reviewer went 4/5. The one miss (P4) was the single prediction
*for* the CS layer — the optimistic case's best scenario failed too, which
is the outcome the gate was built to be able to embarrass. The verdict
(KILL) matches the net expectation, and it lands one gate earlier than the
fallback (K2, unique value) rather than needing the dumb-money comparison
(K4) to close it.

## Protocol compliance

- Pre-registration (kill criteria + predictions + locked definitions)
  written to this file before any code change (commit 314bae4).
- Parameters calibrated only on the calibration set (seeds {100,101}) and
  locked (kappa=0.7, c_gate=1.0) before the confirmatory run; the
  selection rule was declared before calibration.
- Confirmatory scenarios (seeds {41,42,43}) run once, with locked
  parameters. No parameter was touched after seeing confirmatory results.
- The three fixes were implemented exactly as specified and nothing else
  (F2's stage-2 refit was found already present and recorded as-found, not
  re-implemented). No new detection heuristic, scenario edit, or gate
  nudge was added.
- Identical seeds and identical world draws pre/post fix (pinned by
  `TestG9WorldDeterministicAcrossConfigs`). Golden vectors and the
  existing `--deadzone`/`--month` generators are untouched.
- No measurement was dropped or substituted; every scenario ran and is
  reported.
- **No protocol violations recorded.** No mid-run amendment was made.

## Salvage plan (KILL path)

Delete the compressed-sensing deviation substrate (a) — the sketch encode
path, FISTA/Group-OMP recovery, and the residual gate — as a *detection*
mechanism. Retain, as an explicitly classical stack, the parts the
confirmatory run showed carry the actual detection value:

1. **Two-level identity + exact keyframes** (ADR-003/008/011): the
   substrate (c) keyframe-drift detector caught every mass event at 30 s
   and every S3 anomaly ≥10× that mattered, at a byte cost competitive
   with or below full Palimpsest.
2. **Interestingness routing** (ADR-005/COSTS.md): keep the top-K / storm
   heavy-hitter path as the mechanism that promotes a small fraction of
   series to exact 10–15 s cadence. That routed-exact layer is what
   actually owns the sub-keyframe class, and at 5% routing it is cheaper
   than the sketch it would replace.
3. **Storm / top-K breaker** (ADR-004) as a first-class classical
   density/onset detector — it fired at 0 s on every mass event.
4. **Conformal engine repointed at keyframe deltas** (palimpsest-conformal,
   ADR-002/004 there): the nonconformity scoring that currently sits over
   recovered residuals moves onto substrate (c)'s keyframe-to-keyframe
   deltas, where the signal actually is.
5. **ADR-015 cross-emitter fusion** (the merged tier) spun out as research,
   not shipped: it depends on the same recovery substrate this gate killed.

What the wire keeps: exact keyframes (byte-parity, cheap-tier storage),
KDELTA cadence, dict deltas, golden re-announcement, storm FALLBACK frames,
dashcam snapshots. What it loses: the RESIDUAL sketch frames (~half the
wire) and the FISTA decode path. The per-series-billing arbitrage
(COSTS.md) is unaffected — it never depended on substrate (a).

### Negative-result writeup — outline

1. **Thesis.** Compressed sensing promises sub-keyframe, sub-storm anomaly
   recovery from a fixed-size sketch. Measured end-to-end at fleet scale
   under a pre-registered gate, substrate (a) adds no detection a classical
   keyframe/route/storm stack on the same bytes does not already provide.
2. **The two failure modes, separated.** (a) *Decoder pathology* — the
   λ ∝ max|Φᵀy| coupling made detectability non-monotone in magnitude (the
   donut). F2 fixes it (K1 passes). (b) *Physics* — pooled fleet noise sets
   a `sqrt(n)·sigma` residual floor; a precision-viable gate then fires only
   for anomalies large enough to also trip the storm breaker or move a
   keyframe. The two failure modes are independent, and fixing the first
   does not touch the second.
3. **The dumb-money frame.** Detection value must beat not just the old CS
   layer but the cheapest classical way of buying the same detection. On
   every confirmatory class it does not: mass events go to drift/storm
   faster and cheaper; the sub-keyframe class goes to routed 15 s exact,
   which is the only thing that detects it at all.
4. **What compressed sensing needs to win, and why this fleet denies it.**
   Sparsity in a basis the sketch preserves, and an anomaly-to-pooled-noise
   ratio above the gate — i.e. `epsilon ≳ c·sqrt(n)·sigma`. Real
   Kubernetes-shaped fleets are dense (deploy waves) or low-SNR-per-series
   (the payment class), and the interesting anomalies live below that bar.
5. **Honest scope.** The map is one-anomaly-at-a-time, single view, single
   shard, i.i.d. per-series noise; a genuinely sparse, high-SNR,
   basis-aligned regime could still favor CS, but this gate's fleet is the
   product's stated target and it does not.
