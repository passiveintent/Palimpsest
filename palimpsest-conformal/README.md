# palimpsest-conformal

Calibrated statistical alerting engine extending Palimpsest (sketch-native
telemetry). Conformalized quantile regression + DtACI produce adaptive
bands; EVT (GPD) owns the extreme tail; Sliced-Wasserstein drift on input
distributions feeds recalibration.

See `.github/copilot-instructions.md` (repo root) for the binding invariants
and engineering discipline this module must follow, and `docs/adr/` for
design decisions.

## Development

```sh
uv sync
uv run pytest
uv run pytest -m gate   # statistical acceptance gates, see docs/GATES.md
uv run ruff check .
uv run mypy .
```
