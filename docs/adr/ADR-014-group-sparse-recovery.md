# ADR-014: Group-Sparse Recovery

**Status:** Accepted (Prompt 11c — Group-OMP, negative result recorded)

## Context

### The ADR-004 Cliff

ADR-004 addresses k-blowup by falling back to FALLBACK frames when the
pre-quantisation energy `‖y‖²/m` spikes. This protects against cases where
_random_ anomalies push k above the plain-FISTA recovery limit (~m/5). However,
a correlated incident — an AZ outage, a bad deployment rolling across a fleet —
produces a very different sparsity structure:

* **Dense at the series level:** 500 series in the affected AZ all carry a
  moderate anomalous residual simultaneously. k_series = 520 (500 group + 20
  scattered), well above m/5 = 400 for m = 2000.
* **Sparse at the GROUP level:** every anomalous series in the AZ shares the
  same deployment label set. At group granularity there are only 21 anomalous
  groups (1 AZ + 20 scattered singletons), far below m/5.

ADR-010 promotes group-lasso from backlog to a hard prerequisite for the
declared sketch-cube view layer.

---

## Negative Result: Group-FISTA (Prompts 11a–11b)

Three session-days were spent on a FISTA-based group-lasso solver. The attempt
was definitively abandoned after the following failure mechanics were confirmed:

### (a) Lambda-resistant cross-talk floor — the phase-transition regime

With N=50 000, m=2 000, d=6, λ=0.05·max|Φᵀy|, the 500 AZ-group series create
a per-background-singleton cross-talk level of

    noise_floor ≈ √(500 · d/m · σ²) ≈ 6.1 per background series

where σ² is the per-series variance of the AZ signal. The conformant lambda
gives `lamOverL ≈ 0.025`. Since `noise_floor / L ≈ 0.177 >> lamOverL = 0.025`,
the LASSO soft-threshold cannot zero background singletons. The solution is
NOT sparse at the series level for any multiplier ≤ 0.35 — the problem is in
the **phase-transition regime** where individual-series sparsity is infeasible.
`λ_multiplier = 0.05` was not wrong; the problem simply cannot be solved with
scalar regularisation at k_series/m > 0.2.

### (b) Block-prox latch invisible to gradient restart

The block-soft-threshold for the 500-member group:

    shrink_g = max(0, 1 − λ·√500 / (L · ‖u_g‖₂))

latches the group to exactly zero when `‖u_g‖₂ < λ·√500/L = 0.031`. This
event is sign-invisible to gradient-restart: when the group transitions
from `x_g > 0` to `x_g = 0`, the per-group dot-product `(z_g − 0)·(0 − x_g)`
is **negative**, not positive. The restart criterion never fires on the exact
latch event; it fires (if at all) on background-series oscillations that are
unrelated to the group.

### (c) Function-value restart fires post-latch — not on the latch itself

Function-value restart `F(x_new) > F_best` was tried next. **Empirically confirmed
mechanism:** at the exact latch iteration where the group transitions from
`x_g > 0` to `x_g = 0`, the objective *decreases* — the L1 penalty drop
(removing all 500 non-zero entries) outweighs the data-fit increase from losing
the group's signal. This means F(x_new) < F_best at the latch, so the restart
criterion is blind to the latch event itself. Restart fires only later, on
unrelated non-monotone steps caused by Nesterov momentum overshoot on background
series. Diagnostic iteration table (oracle data, v=1 seed):

| iters | AZ above threshold | Scattered recall |
|-------|--------------------|-----------------|
|  50   | 331/500 ✓          | 11/20 (55%)     |
| 100   | 398/500 ✓          | 11/20 (55%)     |
| 200   | 354/500 ✓          | 11/20 (55%)     |
| 350   |   0/500 ✗ (latched)| 11/20 (55%)     |

The latch occurs between iter 200 and 350. At iter 350, ALL 500 AZ members
are zeroed. Restarting after the latch means `z_g = 0`; the next gradient
does revive the group (restoring force brings `‖u_g‖` back above 0.031), but
the accumulated Nesterov momentum re-latches it at a later iteration. A best-seen
prox-iterate restart fires 2–5 times for easy cases and does NOT prevent the
latch — it cannot, because the function value *drops* at the latch event itself.

**Conclusion:** group-FISTA failure at this problem scale is structural, not
tunable. The phase-transition landscape (k_series/m > 0.2) means there is no
scalar λ that simultaneously (i) zeros background singletons and (ii) keeps the
500-member group alive under momentum dynamics. The latch is invisible to both
gradient-based restart (sign argument in §b) and function-value restart
(objective decreases at latch, per §c).  The correct reframe: *we were asking
an optimiser to answer a detection question, and the ruler was wrong, not the
signal.*

---

## Decision: Detection-Then-Refit (Group-OMP)

The correct framing is **hypothesis testing**, not convex optimisation (ADR-009
detection framing). After the plain LASSO identifies the non-group scattered
singletons, the unrecovered group signal remains coherent in the residual.
Group-OMP detects it via a size-aware chi-squared test.

### Algorithm

`RecoverGroup` (= Group-OMP, implemented in `pkg/recover/groupomp.go`):

1. Run plain FISTA with lambda conformance (`λ_abs = 0.05·max|Φᵀy|`) and
   best-seen function-value restart (§3 below). Cap support to M/5 for Group-OMP
   context (M/10 for the plain-path result, to keep the escalation trigger clean).

2. Debias the M/5-capped support. Compute debiased residual `r`.

3. For each group `g` with `|g| ≥ √m` (round 0) or `|g| ≥ 1` (rounds 1+):
   compute the normalised residual-correlation over NEW (not yet union) rows:

       score_g = ‖Φ_gᵀ r‖₂ / √|g_new|

   and compare against the **H0-calibrated size-aware threshold**:

       τ(g) = σ̂ · (1 + C_GROUP / √(2·|g_new|)),  σ̂ = ‖r‖₂/√m,  C_GROUP = 6.0

   Derivation: under H0, score_g/σ̂ ~ √(χ²(|g|)/|g|) with mean 1 and std
   1/√(2|g|). C_GROUP=6 is a 6-sigma test per group (false-alarm rate ≈ 10⁻⁸
   per group). τ → σ̂ as |g| → ∞.

   In round 0, `|g| ≥ √m` filters out cross-talk-dominated singletons. After
   the large group is activated and the residual shrinks, subsequent rounds
   allow singleton activation with the now-smaller σ̂.

4. Iterative activation (≤ 3 rounds): activate highest-scoring group above τ,
   LS-refit on union, recompute r, retest.

5. Final LS-refit on `(activated groups ∪ M/5 capped plain support)`.
   `Result.GroupIDs` = activated group IDs. `Confidence = "recovered-group"`.

### Escalation Policy (unchanged trigger)

`Recover()` escalates to Group-OMP when:
- `Residual > EscalateThreshold` (recommended: 0.3), OR
- `rawSupportLen > M/10` (plain FISTA in phase-transition regime).

Winner selection by debiased residual: Group-OMP result returned only if its
residual is strictly lower than the plain path's.

### Restart (plain FISTA path, cheap insurance)

Best-seen function-value restart with relative tolerance 1e-6:

    fire iff F(x_new) > F_best · (1 + 1e-6)
    on fire: t=1, z=x_new, F_best=F(x_new)   [reset, not keep-best]
    on normal: F_best = min(F_best, F(x_new))

The reset-on-fire prevents cascade restarts. Fires 0–3 times on easy
convergent cases (k << m); functions as cheap insurance against objective
regressions without changing final quality.

---

## Acceptance Criteria

Scenario: N=50 000, m=2 000, d=6, λ_multiplier=0.05.
One AZ-outage group: 500 series, label `region=az-east-1,cluster=prod`.
20 scattered singletons. oracle: `testdata/golden/recovery_group_case.json`.

| Metric                                      | Plain FISTA | Group-OMP |
|---------------------------------------------|-------------|-----------|
| `Residual > 0.3` or `rawSupport > M/10`     | ✓ (cliff)   | —         |
| AZ group flagged in `Result.GroupIDs`        | —           | ✓         |
| Post-debias AZ member recall                 | —           | ≥ 0.90    |
| Scattered recall                             | —           | ≥ 0.90    |
| H0 z-score of AZ group                      | —           | > 10      |
| Wall time                                    | 1×          | ≤ 2×      |
| Seed sweep (5 different Φ matrices)          | —           | 5/5 ✓     |
| Null test (no group anomaly)                 | —           | 0 false alarms |

---

## Consequences

* `fistagroup.go` **deleted** (group-FISTA permanently abandoned).
* `groupomp.go` implements `RecoverGroup` and `RecoverGroupOMP`.
* `Result` gains `GroupIDs []uint64`, `RawSupport int`, `Restarts int`.
* `Options` gains `EscalateThreshold float64`, `Grouper Grouper`.
* `Options.InitX` removed (Group-OMP is stateless; no warm-start needed).
* Plain-path `SupportIDs` capped to M/10; OMP union uncapped (typically ~700).
* Escalation is decoder policy; encoder, wire, and agents are untouched.
* Wall-clock overhead: Group-OMP adds one plain FISTA solve + O(groups·|g|·d)
  correlation scan. At N=50 000 the ratio is ~1.2× (measured, see PERF.md §Group-OMP).

*"We asked an optimiser to answer a detection question, and the ruler was wrong, not the signal."*


**Status:** Accepted (implementation in progress)

## Context

### The ADR-004 Cliff

ADR-004 addresses k-blowup by falling back to FALLBACK frames when the
pre-quantisation energy `‖y‖²/m` spikes. This protects against cases where
_random_ anomalies push k above the plain-FISTA recovery limit (~m/5). However,
a correlated incident — an AZ outage, a bad deployment rolling across a fleet —
produces a very different sparsity structure:

* **Dense at the series level:** 500 series in the affected AZ all carry a
  moderate anomalous residual simultaneously. k_series = 520 (500 group + 20
  scattered), well above m/5 = 400 for m = 2000.
* **Sparse at the GROUP level:** every anomalous series in the AZ shares the
  same deployment label set. At group granularity there are only 21 anomalous
  groups (1 AZ + 20 scattered singletons), far below m/5.

Plain FISTA fails this case outright (measured Residual > 0.3 with the oracle's
reference scenario). The FALLBACK path would emit the AZ's aggregate but lose
individual series visibility — exactly the fine-grain view ADR-010 requires.

### ADR-010 Prerequisite

ADR-010 promotes group-lasso from backlog to a hard prerequisite for the
declared sketch-cube view layer: finer-grain dimensional slices depend on
recovering correlated-failure signatures, not just random individual anomalies.

## Decision

Implement a **block-soft-threshold FISTA** variant in `pkg/recover` (decoder
only; no wire change, no encoder change, no agent change).

### Groups

Groups are derived from the view's label hierarchy via a pluggable `Grouper`:

```
type Grouper func(seriesID uint64, dict *Dictionary) (groupID uint64)
```

The **default grouper** hashes the label segment of the series name — the
portion between the first and last `|` delimiter in Palimpsest's logical-name
format `metric|label1=v1,label2=v2|agg=sum`. All series sharing the same label
set (same AZ/deployment combination) are assigned the same group ID, making a
correlated outage 1-sparse at group level.

### Algorithm

`RecoverGroup(y, dict, params, grouper, opts)` builds groups from the active
dictionary, then runs FISTA with the **block-soft-threshold** proximal operator:

```
prox_g(u_g) = max(0, 1 − λ√|g| / (L ‖u_g‖₂)) · u_g
```

For singleton groups this reduces to the standard scalar soft-threshold.
After the solve, the union support is debiased via ridge least-squares
(same as the plain path). `Result.GroupIDs` carries the IDs of flagged
groups (those with at least one recovered member).

### Escalation Policy

`Recover()` escalates automatically when plain FISTA's residual exceeds the
configurable `Options.EscalateThreshold` (0 = disabled; recommended production
value: 0.3):

1. Run plain FISTA.
2. If `Residual > EscalateThreshold`, run group-lasso.
3. Return the result with the smaller residual; escalated results carry
   `Confidence = "recovered-group"`.

This is the ADR-004 storm-path upgrade: the decoder escalates before trusting
fallback-only visibility.

## Acceptance Criteria (oracle/testdata/golden/recovery_group_case.json)

Scenario: N = 50 000 series, m = 2 000, d = 6.
* One AZ-outage group: 500 series, label `region=az-east-1,cluster=prod`,
  moderate amplitudes (0.5–2.0).
* 20 scattered singleton anomalies, amplitudes 2.0–8.0.
* k_series / m = 520 / 2000 = 0.26 > 0.20.

| Metric                                        | Plain FISTA | RecoverGroup |
|-----------------------------------------------|-------------|--------------|
| `Result.Residual > 0.3`                       | ✓ (failure) | ✗ (success)  |
| AZ group flagged in `Result.GroupIDs`         | N/A         | ✓            |
| Recall on 20 scattered anomalies              | unconstrained | ≥ 0.90     |
| Wall time                                     | 1×          | ≤ 2×         |

Existing k ≤ 100 scattered cases (recovery_case1.json, recovery_watermark.json):
group path must not regress support (recall ≥ 0.95) when escalation does not
trigger.

## Alternatives Considered

### Plain Lasso Only

Rejected. Measured collapse at k ≥ m/5 in the oracle reference scenario: the
plain path cannot recover the 500-series AZ group because k_series/m = 0.26.
Group structure is the only lever that restores recovery without widening the
measurement budget.

### Overlapping Groups

Deferred. Overlapping groups (e.g. a series belonging to both a region group
and a tier group) require the overlap-group-LASSO proximal operator, which
introduces additional convergence complexity. The non-overlapping label-hierarchy
grouping covers the primary correlated-failure pattern. Overlapping groups are
reserved for a future ADR once the base case is validated in production.

## Consequences

* `pkg/recover` gains two new files (`grouping.go`, `fistagroup.go`) and zero
  new external dependencies (xxhash is the existing sole allowed dep).
* `Result` gains `GroupIDs []uint64` and `Restarts int`; `Options` gains
  `EscalateThreshold float64` and `Grouper Grouper`. All new fields are
  zero-valued by default, preserving backward compatibility for existing callers.
* Escalation is decoder policy: the encoder, wire format, and agents are
  untouched.
* Wall-clock overhead: RecoverGroup shares Lipschitz estimation and CSR with
  the plain path; in the non-escalating case (k < m/5) the group path is never
  invoked.

## Addendum — Adaptive Restart (Prompt 11a)

**Root cause confirmed:** FISTA rippling under Nesterov momentum + atomic
block-soft-threshold zeroing latches large groups. When the momentum
coefficient approaches 1 (late iterations), the look-ahead point `z` overshoots
the optimum for the group; the block norm `‖u_g‖₂` dips below the group
threshold `λ√|g|/L`, and the group is latched to exactly zero — permanently,
because subsequent momentum extrapolates away from the signal.

**Fix: O’Donoghue–Candès gradient restart** in both `fista()` and
`fistaGroupBlock()`. After computing `x_new`, if

    dot(z_prev − x_new, x_new − x_old) > 0

the momentum is counterproductive; reset `t = 1` and `z = x_new` (pay one
extra dot product; the momentum accumulator resets harmlessly). The criterion
detects over-shooting without requiring an objective evaluation.

*"FISTA trades monotonicity for speed; restart buys back monotonicity for
the price of a dot product — and block thresholds make monotonicity
non-optional."*

**Escalation rule upgrade:** escalate to group-lasso when
`Residual > EscalateThreshold` **OR** `len(SupportIDs) > M/10`.
Rationale: residual measures fit quality, not recovery correctness; dense
regimes can fit `y` well while smearing support across many false positives.
`M/10` is the ADR-004 cliff bound made operational as a support-density gate.

**Testing:** four new acceptance fixtures in `pkg/recover/group_test.go`:
1. *Group-latch*: 350 iters with restart → group flagged, ≥90% of 500
   members above threshold, scattered recall ≥90%, `Restarts ≥ 1`.
2. *Monotone*: restart fires ⇒ `Result.Residual` decreases relative to plain.
3. *Escalation-fires-via-support*: dense support triggers escalation even
   when `Residual < EscalateThreshold`.
4. *Regression*: existing k≤100 cases keep recall ≥0.95, `Restarts ≤ 2`.
