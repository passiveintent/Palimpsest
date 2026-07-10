# Statistical acceptance gates

Eight named `@pytest.mark.gate` tests — the statistical acceptance criteria
that must pass before a module is considered production-ready, as distinct
from the unit/property suite that runs on every commit
(`pytest -m "not gate"`). Every ADR's "Falsification hooks" section cites
gates from this document by ID (`G1`-`G8`); this document is the only place
a gate's pass/fail criteria are defined.

Run gate tests explicitly:

```sh
uv run pytest -m gate
```

## Conventions

- **Fixtures are synthetic and seeded.** Every gate that involves a
  stochastic comparison (A/B, control-vs-treatment) draws from
  `tests/seeds.py::paired_seeds` so the comparison is equal-budget: both
  arms see identical windows/draws, per the repo's engineering discipline.
- **A gate records its observed statistic even when it passes.** "Assert
  X >= 20%" is a pass/fail bound; the actual delta is always logged/
  returned alongside the boolean so a marginal pass (20.1%) and a
  comfortable one (45%) are distinguishable in gate output.
- **A gate can FAIL two different ways**: the system underperforms a bound
  (ordinary gate failure), or the system violates a non-negotiable
  invariant from `.github/copilot-instructions.md` (e.g. G3's SW-triggers
  case). The second kind is not "needs tuning" — it blocks the module
  regardless of any other metric.
- **Numeric bounds marked "(default)" below are placeholders** to be
  pinned to concrete values in the implementing test, informed by the
  actual fixtures once built — they are declared here so the gate has a
  falsifiable shape today, not because the number is already validated.
- Every gate currently has one `@pytest.mark.gate`-marked stub in the
  matching `tests/<module>/` directory, `pytest.skip`-ed with a pointer
  back to its section here. `pytest -m gate` therefore collects and skips
  (not errors on) all eight until each is implemented.

---

## G1 — Coverage

**Claim.** Empirical coverage matches nominal `1 - alpha` within sampling
noise, both marginally and independently within every Mondrian stratum
(ADR-003) — not just in aggregate, since a stratum that's under-covering
can hide behind a marginal average that looks fine.

**Fixture.** Synthetic score-generating process with a known noise model,
run for enough windows per stratum that a binomial CI is informative;
paired seeds.

**Procedure.** Count band-violation events per stratum and overall over
the fixture's run. Compute a binomial confidence interval (e.g.
Clopper-Pearson) around the observed violation rate at a fixed confidence
level.

**Pass.** `1 - alpha` falls inside the CI, for the marginal rate AND for
every individual stratum's rate.

**Fail.** Any single stratum's CI excludes `1 - alpha` — a marginal-only
check is not sufficient to pass this gate.

**Relates to.** ADR-001 (core coverage promise), ADR-002 (CQR score
coverage), ADR-003 (per-stratum coverage / provenance tiers).

**Test location.** `tests/score/test_g1_coverage.py`.

---

## G2 — Zero-skill control

**Claim.** The coverage guarantee is a property of the conformal wrapper,
not of forecaster skill (model-agnosticism) — but a skilled forecaster
should still earn its complexity by producing narrower bands at equal
coverage.

**Fixture.** A seasonal fixture (real daily/weekly structure) run through
two forecasters behind the same `Forecaster` protocol: the production
Holt-Winters-class model, and a trivial `ConstantForecaster` baseline
(zero-skill control). Paired seeds — both forecasters see identical
windows.

**Procedure.** Run G1 independently for each forecaster on the same
fixture. Independently, compare median band width between the two.

**Pass.**

1. Both `ConstantForecaster` and Holt-Winters-class independently pass G1
   (proves coverage doesn't depend on forecaster skill).
2. Holt-Winters-class median band width is `>= 20%` narrower than
   `ConstantForecaster`'s on the seasonal fixture.
3. The actual width-reduction delta is recorded regardless of outcome.

**Fail.** Either forecaster fails G1 (the conformal wrapper itself is
coverage-dependent on the forecaster — a bug), or the width reduction is
`< 20%` (Holt-Winters isn't earning its complexity over the zero-skill
baseline on a fixture designed to reward skill).

**Relates to.** ADR-002 (`Forecaster` protocol, forecaster-agnostic
scoring), ADR-001 (width is the score once coverage is fixed).

**Test location.** `tests/forecast/test_g2_zero_skill_control.py`.

---

## G3 — Boiling frog

**Claim.** SW drift, which watches input/context distributions only
(invariant 1), does not fire on pure nonconformity-score drift under a
stationary input distribution — and separately, the ordinary band
mechanism still catches the incident before it can hide indefinitely.

**Fixture.** Synthetic score series with slow, monotonically increasing
drift injected into scores (mean and/or scale); input/context feature
distribution held exactly stationary throughout. Paired seeds.

**Procedure.** Inject drift of increasing cumulative magnitude over time.
Monitor (a) the SW drift signal, and (b) ordinary CQR band violations,
across the same run.

**Pass.**

1. SW drift's signal stays below its recalibration trigger threshold for
   the entire run (no recalibration event fires from score drift alone).
2. A band violation fires before cumulative drift magnitude reaches a
   declared bound `D_max` (default: 3x the pre-drift rolling MAD) — i.e.
   detection has a bounded delay, it does not silently absorb the drift
   forever.

**Fail — and this is an invariant breach, not a tunable miss.** SW
triggers a recalibration event at any point during the run. This is
reported and treated separately from an ordinary gate failure: it means
invariant 1 has been violated in code, which blocks the module regardless
of any other metric.

**Fail — ordinary.** No violation fires before drift exceeds `D_max`
(detection is too slow; the frog boils).

**Relates to.** ADR-004 (drift separation), invariant 1.

**Test location.** `tests/drift/test_g3_boiling_frog.py`.

---

## G4 — Input shift

**Claim.** A genuine regime shift in input/context features — with scores
that remain benign, i.e. not an incident — triggers SW-driven
recalibration that completes promptly and without flooding on-call during
the transient.

**Fixture.** Synthetic input-feature changepoint (e.g. a traffic-mix
shift); scores generated from a process that is correctly specified
*relative to the new regime* (this is a legitimate shift the system needs
to catch up to, not an incident it should alert on). Paired seeds.

**Procedure.** Inject the changepoint. Measure windows-to-recalibration-
completion (coverage returns to within G1's tolerance) and count band
violations during the recovery transient.

**Pass.**

1. Recalibration completes within `N` windows (default: `N = 10`
   calibration windows).
2. False pages during the recovery transient stay under a declared budget
   (default: no more than the marginal alpha-implied expected violation
   count for the transient window, rounded up).

**Fail.** Either bound is exceeded — recalibration is too slow, or the
zero-out/DtACI-gamma-boost mechanism (ADR-004) pages too much on the way
back to steady state.

**Relates to.** ADR-004 (changepoint recovery mechanics), invariant 5
(recalibration as reversible reweighting).

**Test location.** `tests/drift/test_g4_input_shift.py`.

---

## G5 — EVT recovery

**Claim.** GPD tail-fitting recovers known generating parameters within
tolerance when the fit is well-posed, and returns `REFUSE` — never a
silently bad fit — when it isn't; a `REFUSE` must propagate to the caller
falling back to the conformal core's outer band edge.

**Fixture.** Synthetic exceedances drawn from `GPD(xi, sigma)` for several
`(xi, sigma)` pairs spanning light- and heavy-tailed regimes, at multiple
sample sizes — including sample sizes deliberately too small to support a
stable fit. Paired seeds.

**Procedure.** Fit the GPD on each well-posed fixture and compare
`(xi_hat, sigma_hat)` to the true generating parameters within a declared
relative-error tolerance. Separately, run the ill-posed (too-small-sample,
non-convergent) fixtures and assert the module's response and the
caller's fallback path.

**Pass.**

1. Well-posed fixtures: recovered parameters within tolerance.
2. Ill-posed fixtures: the module returns `REFUSE` (not a fit with wide
   error bars presented as if trustworthy).
3. On `REFUSE`, the caller substitutes the conformal core's max-band —
   this substitution is itself asserted, not just the `REFUSE` return.

**Fail.** Parameter recovery outside tolerance on a well-posed fixture,
or a fit (rather than `REFUSE`) is returned on an ill-posed one, or
`REFUSE` fails to reach the max-band fallback.

**Relates to.** ADR-001 (EVT tail ownership), invariant 3 (EVT-zone
violations unsuppressible — meaningless if the fit underneath is wrong),
"null results are results" engineering discipline.

**Test location.** `tests/evt/test_g5_evt_recovery.py`.

---

## G6 — Merge algebra

**Claim.** Sketch merge is associative and commutative within a declared
rank-error tolerance, and ring-buffer weight operations (recalibration
reweighting) are reversible.

**Fixture.** `hypothesis`-generated sketches (bounded cardinality/size) for
the algebra property; `hypothesis`-generated weight-operation sequences
(e.g. changepoint zero-out) for the reversibility property.

**Procedure.**

- Associativity/commutativity: for generated sketch triples `A, B, C`,
  compare `merge(merge(A, B), C)`, `merge(A, merge(B, C))`, and
  `merge(merge(B, A), C)` within rank-error tolerance `epsilon`.
- Reversibility: apply a weight operation, then its declared inverse
  (anneal-back); compare recovered weights to the original within
  tolerance, and confirm no underlying bucket data was deleted at any
  point (invariant 5).

**Pass.** Property holds for all `hypothesis`-generated cases (no
shrunk counterexample survives); reversed weights match original within
tolerance; underlying data remains retrievable throughout.

**Fail.** Any `hypothesis`-found counterexample to associativity/
commutativity beyond tolerance, any reversibility mismatch beyond
tolerance, or any detected deletion of calibration data.

**Relates to.** ADR-003 (hierarchical shrinkage sketch-merge), ADR-004
(reversible recalibration), invariant 5.

**Test location.** `tests/sketch/test_g6_merge_algebra.py`.

---

## G7 — Counterfactual audit

**Claim.** Any alpha change proposed by the weekly batch (ADR-005) is
replayed against every confirmed-incident window before a human can
approve it; the gate fails if the proposed alpha would have suppressed a
confirmed incident that the current alpha correctly caught.

**Fixture.** The corpus of confirmed-incident windows (masked out of
*calibration* per invariant 4, but retained here specifically for this
audit), the current alpha, and a candidate alpha from the weekly batch.

**Procedure.** For each confirmed-incident window, reconstruct the band
under both the current alpha and the candidate alpha; check whether the
incident still produces at least one violation under the candidate.

**Pass.** Every confirmed incident that fired under the current alpha
still fires under the candidate alpha.

**Fail.** Any confirmed incident that fired under the current alpha would
not have fired under the candidate — the proposed alpha change is
rejected outright; it is never presented to the human approver as
approvable, per ADR-005.

**Relates to.** ADR-005 (weekly alpha proposals, human approval), and
invariants 2 and 4 — this gate is the machine-checkable backstop that
runs *before* a human ever sees a proposal.

**Test location.** `tests/flywheel/test_g7_counterfactual_audit.py`.

---

## G8 — Percentile-merge falsification

**Claim.** On a bimodal fleet, naively averaging per-host p95 values
diverges from the true fleet-wide p95 by a large, declared margin — this
is a control experiment proving a claim about the world (rollup artifacts
are unreliable), not a claim about our system's correctness.

**Fixture.** A synthetic bimodal fleet: two host populations with
distinct latency distributions (e.g. a "fast" mode and a "slow" mode) at a
controlled mixture proportion.

**Procedure.** Compute (a) the true fleet-wide p95 via correct
aggregation (native merge — summed histogram/DDSketch, then quantile),
and (b) the naive average of each host's locally computed p95 (the
ROLLUP_ARTIFACT computation, ADR-007). Compare the divergence.

**Pass.** Divergence between (a) and (b) exceeds a declared large margin
(default: `>= 25%` relative error on the bimodal fixture) — i.e. this
gate passes by demonstrating the failure mode is large and real.

**Fail.** The naive average tracks the true fleet p95 closely on this
fixture — which would undermine the premise that ROLLUP_ARTIFACT signals
need separate handling at all, and should trigger re-examination of
ADR-007, not just a retry.

**Doubles as.** The Tier-2 sales artifact (ADR-006): the same fixture and
computation that runs this gate produces the customer-facing "your
rollup metric is off by X%" figure.

**Relates to.** ADR-007 (native-mergeable vs. rollup-artifact taxonomy),
ADR-006 (Tier-2 fidelity lift / sales artifact).

**Test location.** `tests/triage/test_g8_percentile_merge_falsification.py`.
