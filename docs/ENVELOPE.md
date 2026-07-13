# Operating envelope — the honest-loss spec sheet

Every observability vendor claims to "keep what matters and drop the
noise." This document is the opposite kind of claim: a quantified list of
exactly what Palimpsest loses, where the boundaries are, and the receipt —
the test, tool, or measurement — behind each number. If a number here has
no receipt, it doesn't belong here. When a shipped default changes, the
receipt changes with it or the row is wrong.

Numbers below are for the shipped defaults (`m=2000, d=6, bits=8`, flush
10s, keyframe 60s; otel/processor/csresidual factory defaults; palimpsestd
flag defaults) unless a row says otherwise. **Residual units** are
`|value − Hold baseline|` per flush window.

## The envelope at a glance

| # | Condition | What you lose | Receipt |
| --- | --- | --- | --- |
| 1 | Anomaly below ~0.4 residual units | Never appears in Layer-2 deviation events, at any noise level | [DEADZONE.md](DEADZONE.md), `plsim --deadzone` |
| 2 | Per-series background jitter σ | Recovery floor rises to ~4σ; σ ≳ 0.3 closes the max-residual gate — zero deviation events at *any* magnitude | [DEADZONE.md](DEADZONE.md) table, `deadzone.csv` `gated_rate` |
| 3 | Fleet noise σ ≥ 0.4 with the gate raised | Precision collapses: ~200 spurious support entries per window (the m/10 cap, saturated) around 1 real anomaly | `deadzone.csv` `mean_spurious_support`; `cmd/plsim/main.go` `--noise` note |
| 4 | Storm breaker trips (energy > 25× median) | Layer 2 degrades to exactly top-100 heavy hitters + global sum/count for the window; per-series recovery is suspended; anything outside the top-K is invisible regardless of its own magnitude | `TestFallbackOnStormTrigger`, ADR-004, [DEADZONE.md](DEADZONE.md) §Knobs |
| 5 | Simultaneous deviations approaching m/5 (~400 series) | Sparse recovery degrades before the breaker catches it; between the two, fidelity is not guaranteed | ADR-004, README Limitations |
| 6 | Root cause older than `ringbuffer.window` (15m) | Structurally absent from the dashcam snapshot — the T-45 problem, below | `pkg/sketch/snapshot.go`, ringbuffer caps in `otel/.../ringbuffer.go` |
| 7 | Cardinality burst hits `max_instances_per_logical` (1000) or `max_total_bytes` (100MB) | New instances silently stop being buffered (aggregates unaffected); drilldown narrows exactly when cardinality explodes | `instanceBuffers.push` drop path + its tests |
| 8 | >100 births/logical/min (churn breaker) | Pipeline goes degraded: **all** instance ring-buffer recording sheds until operator reset; one Alert fires | `breaker.go`, `TestChurnBreaker*` |
| 9 | Quantile/percentile/histogram-quantile metrics | No savings at all — forced to the exact tier, full price, by design | ADR-005, `pkg/tier` tests |
| 10 | Anomaly shorter than keyframe cadence straddling a keyframe | Apparent "reversal" artifact; precision on short frequent bursts is lower than on sustained ones | ADR-003, `TestE2E_SlowDriftKeyframe` doc |
| 11 | 1-2 emitters per shard | Merged-tier weak-signal fusion provides no benefit; guardrail reports `unproven` | ADR-015, `merged_e2e_test.go` |
| 12 | Already self-hosting Prometheus at 60s | ~1× cost — nothing to arbitrage | [COSTS.md](COSTS.md) anti-claim |

## 1-3. The small-anomaly dead zone (measured)

Mapped empirically by `plsim --deadzone` and documented with the full
methodology in [DEADZONE.md](DEADZONE.md). The shape, at defaults with 900
active series:

- **Absolute floor:** ε ≈ 0.4 residual units (the 0.3 post-FISTA support
  threshold plus shrinkage). Ratio-shaped series (an error *rate* going
  0.01%→0.5% is ε ≈ 0.005) never clear it. Count-shaped series usually do.
- **Noise scaling:** the reliable-recovery floor is ~4σ; at σ=0.2 the
  envelope becomes a band-pass (ε 0.8–1.6 emits, ε ≥ 3.2 is suppressed by
  the λ-coupled residual gate), and from σ ≈ 0.3 the gate closes the
  entire column.
- **Storm bar:** a lone anomaly trips the breaker only at
  ε ≳ σ·√(24·n) ≈ 147σ — two orders of magnitude above the recovery
  floor. The gap between those two lines **is** the dead zone.
- **Precision:** where noise is high and the gate is loosened, the
  support saturates at m/10 = 200 entries/window — 1 real anomaly among
  ~199 spurious ones.

## 4-5. Storm fidelity (what a Sev-1 actually gets)

When pre-quantization energy exceeds 25× the 30-window rolling median, the
encoder stops shipping RESIDUAL frames and ships FALLBACK: the top-100
series by |residual| (exact values) plus global sum/count. That is the
whole Layer-2 story for that window — recovery is suspended, and series
#101 does not exist at this layer no matter what it's doing. Wire bytes do
*not* blow up (a FALLBACK payload at top-K=100 is ~1.2KB — smaller than
the 2KB RESIDUAL it replaces); what drops is fidelity: from
"any-of-n recoverable" to "top-100 exactly". Below the breaker but above
~m/5 ≈ 400 simultaneous deviations, recovery degrades without the breaker
firing — that corridor is unguaranteed by construction.

Two facts the 30-day benchmark ([LEDGER.md](LEDGER.md), `plsim --month`)
measured about a *sustained* storm (a 30-minute AZ outage):

- **The fallback burst is keyframe-bounded, not storm-bounded.** A level
  shift only produces residual energy until the next keyframe re-bases it
  into every affected baseline (ADR-003) — at defaults, ≤60s. The breaker
  fired for the first ~30s of the 30-minute outage, RESIDUAL frames
  resumed against the re-based fleet, and the recovery transient at
  outage end fired it again. Anything you want from FALLBACK's exact
  top-K, you get in that first minute.
- **Per-emitter sharding is what keeps top-K coverage alive.** 30% of a
  10k-series fleet is ~600 affected sum-series, far past one top-100
  frame; sharded across 8 emitters it is ~75 per frame, and measured
  coverage was 99%. A single-emitter shard in the same incident would
  cover 100/600 ≈ 17%.

## 6-8. Dashcam forensics: the T-45 problem and the caps

The instance ring buffer is a **lookback from flag time, not root-cause
coverage**. A snapshot fires when a series' residual z-score crosses the
threshold at T-0 and carries the last `ringbuffer.window` (default 15m) of
instance samples. A memory leak that started at T-45min produces its
snapshot evidence starting at T-15min: the onset, the deploy that caused
it, and the first 30 minutes of climb are structurally absent. The only
earlier record is Layer-1 keyframes — exact, forever, but logical-level:
you can see *that* the aggregate climbed from T-45, not *which pod* led.

Memory is physically capped, and the caps have teeth:

- One buffered sample ≈ 20 wire bytes; at 10s flush a full 15m window is
  ~90 samples ≈ **1.8KB per instance**.
- `max_instances_per_logical` = 1000 → ≤ ~1.8MB per logical group.
- `max_total_bytes` = 100MB → ~55,000 instances fleet-wide per collector.
- When either cap binds, `push` **silently drops new instances** from
  buffering (their samples still fold into the sketched aggregates). The
  practical consequence: during a cardinality explosion — precisely when
  you want per-pod forensics — coverage narrows to the instances that
  were already being tracked.
- Separately, the churn breaker (>100 births/logical/min) sheds **all**
  instance recording for the pipeline until an operator resets it; that
  is a detected, alerted event, not a silent one.

Mitigations are honest but partial: widen `ringbuffer.window` (memory
scales linearly; 45m ≈ 3× the budget), lower the snapshot threshold
(earlier flag ⇒ lookback covers more of the cause; more false snapshots),
or accept that slow-burn forensics belong to Layer 1 + the exact tier.

## 9-11. Structural exclusions

- **Quantiles** don't compose through linear measurements. Histogram
  quantiles and summaries route to the exact tier at full price (ADR-005).
  This is a design boundary, not a roadmap item.
- **Keyframe reversal**: a spike still elevated at a keyframe boundary is
  baked into the new baseline; its recovery 1-2 windows later appears as
  an equal-and-opposite deviation. Correct, explainable, and it lowers
  precision on sub-keyframe-cadence anomaly bursts (ADR-003).
- **Thin fleets**: the ADR-015 merged tier needs multiple emitters per
  shard to fuse weak signals; with 1-2, the `proven` guardrail never
  clears and you get single-emitter behavior.

## Open questions (tracked, not yet quantified)

- **Conformal validity across mode switches.** `palimpsest-conformal`
  calibrates adaptive bands (CQR + DtACI) on Layer-2 observables. A regime
  switch (RESIDUAL → FALLBACK → RESIDUAL) changes the observable itself —
  per-series recovered values on one side of the boundary, top-K heavy
  hitters on the other — which breaks the exchangeability assumption at
  the switch points. DtACI is designed for distribution *shift*, not for
  the observable being redefined mid-stream. The 30-day benchmark
  ([LEDGER.md](LEDGER.md)) sharpens the question: the FALLBACK interlude
  itself is short (keyframe-bounded, ~30s measured), but the keyframe
  that ends it *re-bases every affected series*, so post-storm residuals
  are drawn against a different baseline than the calibration window —
  two distinct exchangeability breaks per storm, not one. Whether
  coverage guarantees hold across them, and how long re-calibration
  takes, needs a statistical gate in that repo (`docs/GATES.md` there)
  before any coverage claim spans a storm.
- **Detection-lag distribution vs the ring window.** The T-45 statement
  above is the deterministic bound, and the 30-day benchmark's slow-leak
  scenario measures one concrete point ([LEDGER.md](LEDGER.md)): a 6-hour
  ramp is first *flagged* only at its end-of-ramp collapse, when the 15m
  lookback covers 4.2% of the climb. A full sweep over ramp shapes (what
  fraction of realistic slow-burns exceed the lookback, as a distribution)
  hasn't been run.

## Re-measuring

The dead-zone rows: `go run ./cmd/plsim --deadzone` (~10s, deterministic;
see [DEADZONE.md](DEADZONE.md) for re-running under your own m/n/σ). The
cap math in §6-8 is arithmetic over shipped defaults — recompute it when
you change them. Cost-model context: [COSTS.md](COSTS.md), including the
anti-claim about when Palimpsest saves nothing.
