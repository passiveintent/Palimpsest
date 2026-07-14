# PRE-FIX baseline snapshot (Phase 0)
# Repo state: commit 93fe626 (before G9 fixes). Replay:
go run ./cmd/plsim --deadzone --seed 41 --deadzone-out results/g9/prefix/deadzone-seed41
go run ./cmd/plsim --deadzone --seed 42 --deadzone-out results/g9/prefix/deadzone-seed42
go run ./cmd/plsim --deadzone --seed 43 --deadzone-out results/g9/prefix/deadzone-seed43
go run ./cmd/plsim --month --codec zstd --month-days 6 --seed 41 --month-out results/g9/prefix/month-seed41
go run ./cmd/plsim --month --codec zstd --month-days 6 --seed 42 --month-out results/g9/prefix/month-seed42
go run ./cmd/plsim --month --codec zstd --month-days 6 --seed 43 --month-out results/g9/prefix/month-seed43
# --month-days 6: declared runtime-budget deviation, see docs/ADR-016-cs-verdict.md.
