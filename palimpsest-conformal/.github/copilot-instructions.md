# Palimpsest Conformal — Copilot Instructions

## What this project is
A calibrated statistical alerting engine extending Palimpsest (sketch-native
telemetry). Thesis: alerting as calibrated statistical inference. Conformalized
quantile regression + DtACI produce adaptive bands; EVT (GPD) owns the extreme
tail; Sliced-Wasserstein drift on INPUT distributions feeds recalibration.
PID framing: conformal quantile = P, adaptive miscoverage = I, SW drift = D.

## Non-negotiable invariants — never write code that violates these
1. SW drift watches input/context distributions ONLY — never nonconformity
   scores. Recalibrating on score drift teaches the system to hide slow-burn
   incidents (the boiling-frog failure).
2. Human labels never touch sketches, calibration sets, or live alpha. Labels
   are logged immutably; a weekly batch may PROPOSE alpha changes
   (human-approved, floored at alpha_max).
3. EVT-zone violations are unsuppressible by any rule, label, or alpha change.
4. Confirmed-incident windows are MASKED OUT of calibration (tightens bands,
   fails noisy). Never include incident windows to widen bands. The structural
   guarantee is non-influence: masked buckets contribute zero weight, so
   incident scores can never widen a band through inclusion.
5. Recalibration = weight operations on the time-bucketed sketch ring buffer.
   Calibration data is never deleted. Reversibility is by snapshot restore
   (pure ops preserve their pre-image bit-identically), not algebraic inverse.
6. Every band carries a calibration-provenance tag:
   bucket-local | service-pooled | fleet-pooled — and the decay in force
   (decay=1 is finite-sample guaranteed; decay<1 is adaptively weighted).
7. Decoder narrates, conformal decides. Any future decoder/retrieval layer
   (HMM incident narration, Wasserstein incident recall) is a READ-ONLY
   consumer of the violation log and fingerprint corpus. It may never touch
   alpha, bands, suppression, or routing. A mis-decoded "Recovering" state
   must be structurally incapable of silencing a page.
8. Fingerprint corpus channels are dimensionless by construction
   (calibrated p-values, drift scores, EVT flags) so fingerprints transfer
   across services and customers. Never persist raw-unit metrics into the
   corpus.

## Engineering discipline
- Every statistical claim ships with a test that would falsify it.
- Every module adds >= 1 control experiment (zero-skill, shuffled, or
  adversarial).
- All stochastic tests use paired seeds from tests/seeds.py. A/B comparisons
  share seeds. Equal-budget: compared policies see identical windows.
- Coverage is the invariant; band width is the score. Ablations hold coverage
  fixed and compare width.
- Null results are results: modules need explicit refuse-to-fit /
  degrade-visibly paths (return REFUSE, never a silently bad fit).
- ADRs in docs/adr/ are binding. Read the relevant ADR before implementing.
  If code must deviate, stop and flag — never silently diverge.
- NO "definition of done" / "gate green" claim without a CI run link. Gates
  exist to remove trust in the author's self-report; a green claimed by the
  same agent that wrote the code is worth nothing. A DoD assertion must cite
  the `conformal-ci` run (ruff + mypy --strict + pytest + `pytest -m gate`)
  that proves it. Note the footgun: local `uv run mypy` uses the packages
  config (src only) while CI runs `mypy .` over src AND tests — always verify
  with the CI-exact commands (`uv run ruff check .`, `uv run mypy .`,
  `uv run pytest -m "not gate"`, `uv run pytest -m gate`) before claiming done.

## Stack & style
Python 3.12, uv, numpy/scipy/statsmodels/polars, pytest + hypothesis,
ruff + mypy --strict. Pure functions in the statistical core; I/O at edges.
No pandas in hot paths. Tests first or alongside, never after.
