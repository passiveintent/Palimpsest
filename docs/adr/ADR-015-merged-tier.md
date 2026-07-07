# ADR-015: Merged Tier — Trust Policy for Cross-Emitter Fusion

**Status:** Accepted (implemented)

## Context

ADR-013's watermark/repair engine already sums frames per (tenant, shard,
epoch, window, view) across every emitter that contributes to it
(`WindowState.merge`, `internal/core/engine.go`) — sketch additivity makes
this arithmetically free. `pkg/tier` has recognized a third tier name,
`"merged"`, since ADR-005, alongside `exact`/`sketched`, but nothing has ever
given it distinct behavior: a metric routed to the `merged` tier is today
sketched exactly like `sketched` tier at the agent (`otel/processor/csresidual`
already treats them identically — see `consumeMetric`).

The fusion **plumbing** exists and always has. What doesn't exist is the
**policy** that makes deliberately relying on it — routing a metric whose
per-agent signal is individually too weak to recover, and trusting the
*merged* result once enough emitters have contributed — a safe product
feature rather than an accident of how sketches happen to add. Two things
are missing:

1. A way to know, per window, whether the fleet-scale fusion that just
   happened was *statistically justified* — enough emitters, enough signal
   quality — to trust the result, as opposed to a coincidental low-residual
   solve on a window with too few contributors.
2. A recovery path that can actually resolve the sparsity structure a
   correlated fleet-wide incident produces once fusion pulls it above the
   noise floor: ADR-014's Group-OMP escalation, which this tier leans on
   directly rather than reinventing.

This ADR adds that policy layer. It changes no wire bytes, no agent
behavior, and no encoder config surface: agents already sketch every tier
the same way (`otel/processor/csresidual`'s `consumeMetric` has no
tier-specific encode path beyond routing `exact` around the sketch
entirely). Everything here is decoder-side.

## Latency class

Merged-tier windows are **not** the easy, well-separated, high-SNR case
`BenchmarkRecover`'s headline 127ms number describes (`recovery_case1`:
k=50 of N=50k, amplitude 2–10, objective flat by iteration ~100). A
per-agent SNR of ~1.0 fused across `A` emitters gives a merged-sketch SNR of
only `~sqrt(A)` (independent per-agent noise partially cancels on summation,
correlated true signal adds coherently — see the arithmetic in
`internal/core/merged_test.go`). That is enough for Group-OMP to detect and
recover the correlated group (its own H0 test only needs a several-sigma
margin, comfortably met once `A` is in the hundreds-to-thousands range this
tier targets), but it is *not* enough for FISTA's early-exit to fire the way
it does on `recovery_case1` — early-exit requires the objective to go
machine-precision flat, and a low-SNR objective keeps grinding. Cliff cases
(ADR-004) and AZ-scale escalations (ADR-014) are the same regime.

Capacity planning for this tier must use the **worst-case 350-iteration
cost (~718ms, post-Prompt-12 matvec improvements) plus Group-OMP's own
refit**, not the 127ms typical-case number — see `docs/PERF.md`'s
"Iterations-used distribution by case class" table. The acceptance E2E
below logs `Result.Iters` per solve for exactly this reason: the
iteration count actually used is the observable that lets a future
per-tier p99 histogram (PERF.md's own suggested follow-up) replace this
ADR's worst-case assumption with a measured one. The absolute decode
budget this tier is held to is ADR-014's existing one: **≤1.5s**
(`testGroupCaseWallTime`'s budget, itself derived from the flush-interval
wall-clock budget a decode has to clear before the next window's frames
start arriving) — not a new number invented for this tier.

## Decision

### 1. Merged tier is intra-tenant only

Restating ADR-012: sketches never sum across tenants. Nothing in this ADR
changes that boundary or adds a new one — `internal/core.Engine` is
provisioned with exactly one tenant's key material (`Config.TenantKey` or
`Config.KeyRing`, both singular per Engine instance) and `pkg/recover`'s
`Dictionary`/`ViewState` have no notion of tenant at all, by design: a
tenant boundary that could be checked *inside* the merge path would require
tenant identity to be a value the decoder can read off a frame, which
ADR-012 deliberately avoided putting on the wire ("seeds derive via
HKDF-SHA256(tenant_key, …), not public convention"). The boundary is
enforced by construction instead: one Engine (equivalently, one
`otel/processor/csresidual` `Config`, which requires exactly one
`tenant_id`/key set — see `Config.Validate()`) serves exactly one tenant,
and a frame's residual payload is only ever meaningful decoded against the
seed its *own* tenant's key derives. `internal/core/merged_test.go` adds an
explicit regression: a frame produced under a genuinely different tenant's
key, delivered into another tenant's Engine (misroute scenario), never
produces a trusted low-residual "recovered" event — it either fails the
existing high-residual/DEGRADED gate (`Config.MaxResidual`,
`solveWindow`'s existing gate) before any anomaly is emitted, or (if its
`key_version` doesn't resolve in the receiving tenant's `KeyRing`) is gated
pre-merge and counted via the existing `keyring_miss` metric
(ADR-012 §Addendum) — either way, zero trusted summation reaches a
consumer.

### 2. Scaling-law guardrail

Weak-signal fusion is only trustworthy once enough independent looks (`A`)
and signal quality (`delta/sigma`) have accumulated relative to how rare the
anomalous set is expected to be (`k/N`). The guardrail (a feasibility-harness
law in the same spirit as ADR-014's H0-calibrated `C_GROUP` constant):

```text
(k/N) * A * (delta/sigma)^2  >=  MinSignalRatio   (harness-derived default: 3.0)
```

| Symbol | Meaning | Source |
| --- | --- | --- |
| `k` | size of the recovered/flagged anomalous set this window | **live**: `Result.RawSupport` (post-recovery; the union support size for a Group-OMP result, the raw pre-cap density for a plain result) |
| `N` | total active series this view's dictionary currently tracks | **live**: `len(Dictionary.ActiveIDs())` at solve time |
| `A` | per-window emitter coverage | **live**: `Result.Coverage` (ADR-013's existing "emitters present" count — **not** `Config.EmittersExpected`/configured fleet size, which is a static ceiling, not what actually showed up this window) |
| `delta/sigma` | per-agent predictor-quality estimate (anomaly amplitude over per-agent noise stddev) | **config**: `MergedPolicy.SigmaEstimate` |
| `MinSignalRatio` | the threshold the ratio must clear | **config**: `MergedPolicy.MinSignalRatio`, harness-derived default 3.0 |

`k`, `N`, and `A` are all directly observable at decode time from a
completed `Recover` solve and the dictionary it ran against — no config
knob is needed or invented for them. `delta/sigma` is the one quantity the
decoder fundamentally *cannot* measure: a merged sketch only ever exposes
the sum of true signal and per-agent noise, never the two separately, so
distinguishing "large true delta" from "large sigma" requires an out-of-band
estimate of predictor quality. That's the one number this policy asks an
operator to configure, and it's why `A` gets called out explicitly as
*not* being the configured fleet size — the one live quantity is also the
one earlier drafts of this guardrail were most tempted to get wrong.

The verdict is carried on its own `Result` field, never folded into
`Confidence`:

```go
// pkg/recover (fista.go)
const (
    MergedTrustProven   = "proven"
    MergedTrustUnproven = "unproven"
    MergedTrustNA       = "na" // not a merged-tier view; guardrail doesn't apply
)
```

`Confidence` keeps reporting *what recovery mode actually ran*
("recovered", "recovered-group", …) regardless of `MergedTrust` — a
merged-tier window that fails the guardrail is not thereby "less recovered"
in the FISTA/Group-OMP sense, and a window that passes the guardrail can
still fail to recover anything if there's genuinely no signal. These are
independent bits by design: one says "did the solver find something",
the other says "is fusing this many weak agents' signal together
statistically sound at all". Below threshold, merged results are still
emitted, labeled `MergedTrust="unproven"` — **never suppressed silently**.
An operator who wants suppression can filter on the label downstream; the
decoder's job is to label honestly, not to decide silently on the
operator's behalf that a window isn't worth seeing.

### 3. Merged recovery escalates like any other correlated incident

Merged-tier windows use the same `Recover()` trigger contract as every
other view (ADR-014 final, Prompt 11c): escalate to Group-OMP when
`Residual > EscalateThreshold` OR `rawSupportLen > M/10`. There is no
merged-tier-specific escalation default and no separate code path —
fleet-scale incidents are correlated by nature (that's what makes them
fleet-scale), so in practice these triggers fire routinely in this tier,
but the *mechanism* is unchanged ADR-014 machinery. This keeps the merged
tier from becoming a second, parallel recovery implementation to maintain.

## Code

### `internal/core`: `MergedPolicy` and `Result.MergedTrust` wiring

```go
// MergedPolicy configures the ADR-015 trust guardrail for one merged-tier
// view.
type MergedPolicy struct {
    MinSignalRatio float64 // threshold; harness-derived default 3.0
    SigmaEstimate  float64 // delta/sigma: config estimate of per-agent predictor quality
}
```

`Config.MergedViews map[uint16]MergedPolicy` declares, by `ViewID`, which
views are merged tier. A `ViewID` absent from this map is not merged tier:
its `Result`s always carry `MergedTrust = recover.MergedTrustNA` and the
guardrail is never evaluated for it (`na` is not a failure state — it's "this
question doesn't apply here"). `solveWindow` evaluates the guardrail
immediately after `Recover()` returns (using that call's own `RawSupport`/
`Coverage` and the view's live dictionary size) and stamps the result before
the existing residual/DEGRADED gate runs — the two gates are independent
and both apply, in either order, without interacting. `merged_windows{trust}`
(a labeled counter on `internal/metrics.Metrics`, keyed by the `MergedTrust`
value) is incremented once per merged-tier window solve, so the
proven/unproven mix is directly observable in production without needing to
scrape per-event labels.

`ports.AnomalyEvent` gains a `MergedTrust` field, copied straight from
`Result.MergedTrust` by `Matcher.EvalDeviation`, so the label survives all
the way to whatever consumes `AnomalySink`.

### otel config: no agent changes, decoder-only wiring

`pkg/tier`'s `Merged` tier value has been valid config syntax since
ADR-005 (`tier.Compile` already accepts it; `otel/processor/csresidual`'s
`compileTierRules` delegates straight through with no allow-list of its
own). At the agent, a metric routed to `merged` tier is sketched exactly
like `sketched` tier — `consumeMetric` has no branch for it, and none is
added: "agents don't change" per this ADR's charter. What an operator adds
is a dedicated `views` entry (ADR-010) whose `ViewID` (its index in
`Config.Views`) is then given a `MergedPolicy` on the decode side, at the
same index. The correlation between an encoder's `views[i]` and a
decoder's `ViewID=i` is the same implicit, position-based contract every
other declared view already relies on; this ADR does not add a new one.

**Fleet config-consistency requirement:** merged tier requires every
emitter contributing to a given (shard, view) to agree on `(m, d, view,
key_version)` — a differing `m`/`d` builds a different `Phi`, and a
differing `key_version` derives a different seed, either of which makes
"summing" two emitters' contributions meaningless (not merely
lower-quality). `epoch`/seed agreement across the fleet is already
guaranteed structurally: every emitter derives its seed from
`HKDF-SHA256(tenant_key, shard_id, epoch_index, view_id)`
(ADR-002/ADR-012), and `epoch_index` is a deterministic function of
wall-clock time and the shared `epoch_rotate` config
(`otel/processor/csresidual`'s `epochIndex`), not a per-emitter choice —
so as long as the fleet shares one collector config (the normal
deployment shape), epoch and seed agreement need no separate check. `m`,
`d`, and `key_version` agreement is an operational requirement on that
shared config, not something the decoder can validate at the frame level
(there is nothing to check against — a differing `m` on an incoming frame
is already rejected by `wire.Dequantize`'s length check, which is a
correctness bound that predates this ADR, not a merged-tier-specific one).

### `plsim`: simulating a merged-tier fleet

`--merged-emitters N` (0 disables) simulates `N` weak-observer agents
sharing a correlated-group anomaly at per-agent SNR ~1.0, fused into the
same (shard, epoch, view, window) coordinate the way a real fleet-scale
incident would be. Full per-agent `Tracker`+`Accumulator`+`Predictor`
pipelines (`cmd/plsim`'s normal per-emitter machinery) are unnecessary
overhead at `N` in the thousands — a weak observer's only job is to
contribute one `sketch.Accumulator` window's worth of signal for a fixed,
already-known set of series names, not to track open-loop prediction state
across many windows. Reusing the **aggregate-equivalence trick**: sketch
additivity means `Σ_i Φx_i = Φ(Σ_i x_i)`, so for `N` large the simulator
draws the *equivalent combined noise* directly (the sum of `N` i.i.d.
per-agent noise draws is itself normally distributed with variance `N`
times one agent's — one draw from that distribution is exact in
distribution, not an approximation) rather than paying for `N` independent
draws and a summation. `N <= 50` still generates one frame per emitter
(distinct `EmitterID`s), because at that scale per-emitter fidelity is
cheap and it keeps `--merged-emitters 10`-style small runs useful for
manually inspecting individual frames.

## Acceptance Criteria

* **E2E, proven:** correlated 500-member group across 2000 simulated
  emitters, per-agent SNR ~1.0 on a shared anomaly support. Merged sketch
  escalates via the standard `Recover()` trigger contract (ADR-014 final);
  Group-OMP activates the group; group recall >= 0.90; `MergedTrust ==
  "proven"` (guardrail arithmetic documented in the test); `Result.Iters`
  logged per solve; escalated solve wall time fits the ≤1.5s decode budget
  (ADR-014's existing `testGroupCaseWallTime` budget); H0 z-score of the
  activated group logged (ADR-014/Prompt 11c criterion (c)).
* **E2E, unproven:** same scenario, 100 emitters. `MergedTrust ==
  "unproven"`. `Confidence` reports whatever recovery mode actually
  occurred (not asserted either way) — the test gates on the guardrail
  firing, not on recovery quality, per this ADR's explicit separation of
  the two signals.
* **Regression (ADR-012):** cross-tenant frames injected into the wrong
  tenant's Engine are never trusted-summed: either gated pre-merge
  (`keyring_miss`, unknown `key_version`) or gated post-merge by the
  existing high-residual/DEGRADED check, in both cases with a metric
  incrementing and zero anomalies emitted from the contaminated window.

## Guardrails

* No wire format changes.
* Guardrail thresholds (`MinSignalRatio`, `SigmaEstimate`) are config, with
  the harness-derived `MinSignalRatio` default documented above (3.0), not
  hardcoded.
* Merged results are never silently dropped for failing the guardrail —
  only labeled `MergedTrust="unproven"`.

## Consequences

* `pkg/recover`: `Result` gains `MergedTrust string`
  (`""`/`na`/`unproven`/`proven` — `Recover`/`RecoverGroup` themselves
  never set it, since they have no notion of "which view is merged tier";
  it is zero-valued until a caller (`internal/core.Engine`) stamps it).
* `internal/core`: new `MergedPolicy` type and `Config.MergedViews`;
  `solveWindow` stamps `Result.MergedTrust` and increments
  `merged_windows{trust}`. `Config` also gains `EscalateThreshold`/
  `Grouper`, previously entirely absent from `recoverOptions` — Group-OMP
  escalation was unreachable through the real decode path before this
  change (`Options.EscalateThreshold` always defaulted to 0/disabled
  regardless of what an operator configured, since nothing threaded it
  through). This is the "standard `Recover()` trigger contract" merged-tier
  escalation depends on per this ADR's §3 — without it, no view (merged or
  otherwise) could ever escalate outside of `pkg/recover`'s own direct-call
  tests.
* `internal/ports`: `AnomalyEvent` gains `MergedTrust string`.
* `internal/metrics`: new labeled counter `merged_windows` (key: trust
  value).
* `otel/processor/csresidual`: no code changes; documentation only
  (this ADR + config comments on the operational requirement to keep
  `m`/`d`/`key_version` consistent fleet-wide for any merged-tier view).
* `cmd/plsim`: `--merged-emitters`/`--merged-group-size` flags and a new
  weak-observer simulation path (`cmd/plsim/merged.go`), independent of
  the existing `--emitters` (disjoint-group sharding demo) flag.

*"The fusion was always free; trusting it wasn't — that's what a threshold
buys you: not detection, permission to believe the detection."*
