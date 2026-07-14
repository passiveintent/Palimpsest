# Gate G9 results (docs/ADR-016-cs-verdict.md)

Pre-registered confirmatory experiment on the compressed-sensing layer.
Verdict: **KILL** (at K2, unique value). All seeds and exact commands below.

## Layout
- `prefix/`  — Phase 0 PRE-FIX baseline: `plsim --deadzone` and `--month`
  at seeds {41,42,43} on repo state 93fe626. Replay: `prefix/COMMANDS.md`.
- `calib/`   — Phase 2 calibration curves (seeds {100,101}) + the
  calibration-magnitude anomaly check (`calib-anom-*`, magnitudes {3,7}).
- `confirm/` — Phase 3 confirmatory runs (seeds {41,42,43}), one JSON per
  scenario × {prefix,postfix} × seed, plus the post-fix dead-zone sweep
  (`sweep-postfix-seed*`). Consolidated tables: `confirm/SUMMARY.md`.
- `smoke/`   — engineering smoke runs (seed 999 only), harness sanity, not
  used to pick parameters.

## Replay (from repo root)
    # Phase 0 baseline (pre-fix tree)
    #   see prefix/COMMANDS.md
    # Phase 2 calibration
    go run ./cmd/plsim --g9 --g9-scenario calib --g9-days 1 --seed 100 --g9-out results/g9/calib
    go run ./cmd/plsim --g9 --g9-scenario calib --g9-days 1 --seed 101 --g9-out results/g9/calib
    # Phase 3 confirmatory (locked kappa=0.7 c_gate=1.0), all seeds + sweep:
    scripts/g9-confirm.sh 1.0 0.7
    # Analysis / K-gate evaluation:
    python3 scripts/g9_analyze.py results/g9/confirm

## Locked parameters
kappa = 0.7, c_gate = 1.0 (Phase 2, seeds {100,101}; selection rule in ADR-016).
