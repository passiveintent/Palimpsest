#!/usr/bin/env bash
# Gate G9 Phase 3: confirmatory scenarios, run ONCE with locked parameters.
# Usage: scripts/g9-confirm.sh <c_gate> <kappa>
set -euo pipefail
CG="$1"; KAPPA="$2"
OUT=results/g9/confirm
mkdir -p "$OUT"
POST=(--g9-fix-f1 --g9-fix-f2 --g9-fix-f3 --g9-c-gate "$CG" --g9-kappa "$KAPPA" --g9-label postfix)
PRE=(--g9-label prefix)
for seed in 41 42 43; do
  for sc in s1 s2 s3 s4 s6; do
    go run ./cmd/plsim --g9 --g9-scenario "$sc" --seed "$seed" --g9-out "$OUT" "${PRE[@]}"  >> "$OUT/run.log" 2>&1
    go run ./cmd/plsim --g9 --g9-scenario "$sc" --seed "$seed" --g9-out "$OUT" "${POST[@]}" >> "$OUT/run.log" 2>&1
  done
  go run ./cmd/plsim --g9 --g9-scenario s5 --g9-days 2 --seed "$seed" --g9-out "$OUT" "${PRE[@]}"  >> "$OUT/run.log" 2>&1
  go run ./cmd/plsim --g9 --g9-scenario s5 --g9-days 2 --seed "$seed" --g9-out "$OUT" "${POST[@]}" >> "$OUT/run.log" 2>&1
  # Post-fix dead-zone map (DEADZONE.md deliverable)
  go run ./cmd/plsim --g9 --g9-scenario sweep --seed "$seed" --g9-trials 15 --g9-out "$OUT" \
    --g9-fix-f1 --g9-fix-f2 --g9-c-gate "$CG" --g9-kappa "$KAPPA" --g9-label postfix >> "$OUT/run.log" 2>&1
done
# Calibration-magnitude sanity check (Phase 2 record; non-confirmatory magnitudes {3,7})
go run ./cmd/plsim --g9 --g9-scenario calib-anom --g9-trials 15 --seed 100 --g9-out results/g9/calib "${POST[@]}" >> "$OUT/run.log" 2>&1
echo "g9-confirm complete: c_gate=$CG kappa=$KAPPA"
