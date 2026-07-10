# ADR-002: v1 nonconformity score — CQR on window summary statistics

## Status

Accepted

## Context

The nonconformity score is the single number every downstream layer
(calibration, DtACI, drift-triggered recalibration, EVT hand-off,
fingerprint corpus) is built on top of, so it has to be cheap to compute
from data every TSDB already stores, and it has to be the *same*
computation in backtest and in the live product — a score that only
exists in an offline notebook is not a falsifiable product claim per
ADR-001.

Windowed p50/p95/p99 are already emitted by essentially every metrics
backend, so scoring against those (rather than raw high-frequency samples)
means backtest and product share one input schema and one scoring path,
which is itself a falsifiable claim (see falsification hooks below), not
an assumption.

We also need to decouple "what predicts the next window's summary stats"
from "how we calibrate around that prediction," because the forecasting
model is the most likely thing to be swapped, upgraded, or eventually sold
as a bring-your-own-model product line, while the calibration mechanics
(this ADR, plus ADR-003/004) should not have to change when that happens.

## Decision

- **Score.** `score = max(q_lo - y, y - q_hi)`, where `q_lo`/`q_hi` are the
  forecaster's predicted quantile bounds for the window and `y` is the
  observed window summary statistic. This is standard conformalized
  quantile regression (CQR) nonconformity, studentized by a rolling MAD
  (median absolute deviation) so that the score is scale-normalized across
  regimes with different volatility instead of comparing raw residuals
  across heteroscedastic strata.
- **Inputs.** Windowed p50/p95/p99 — the summary statistics every TSDB
  already stores — not raw per-request samples. Backtest and the live
  product both score against this same schema.
- **Forecaster.** A Holt-Winters-class model sits behind a `Forecaster`
  protocol (bring-your-own-model, we calibrate it, is a deliberate future
  product line, not built now). The conformal core only ever depends on
  the protocol's predicted quantiles, never on the concrete forecaster
  implementation.

## Consequences

- Swapping the forecaster (Holt-Winters-class today, something else later,
  or a customer's own model under the future BYO-model line) must never
  require touching the score, calibration, or drift code — that isolation
  is the point of the protocol boundary, and it is testable, not just
  aspirational.
- Studentizing by rolling MAD means the score is already scale-normalized
  before it reaches calibration/stratification (ADR-003) and drift
  (ADR-004) — those layers do not need to re-normalize per stratum.
  Nonconformity scores still must never be read by SW drift (invariant 1);
  this ADR produces the exact quantity that invariant forbids feeding to
  the drift detector.
- Restricting inputs to windowed p50/p95/p99 is a deliberate ceiling: it
  means we cannot score request-level exceedances at v1 fidelity (that is
  the Tier 2 / EVT upsell in ADR-006), in exchange for a scoring path any
  TSDB integration can feed from day one.

## Falsification hooks

- **G1** (`docs/GATES.md`) — the studentized CQR score achieves nominal
  marginal *and* per-stratum coverage on backtest windows.
- **G2** — the same coverage claim holds with `ConstantForecaster`
  substituted for Holt-Winters-class behind the `Forecaster` protocol
  (proves the score path is forecaster-agnostic), while Holt-Winters-class
  still earns its complexity via a >= 20% width reduction.
- Rolling-MAD studentization's conditional-coverage lift over the
  unstudentized score, and backtest/live scoring-path parity, are ordinary
  falsifiable claims but not yet named gates — cover them as regular
  (non-`gate`) tests in `tests/score/` and `tests/forecast/`.
