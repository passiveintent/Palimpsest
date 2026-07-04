# Palimpsest

Palimpsest is a Go library and toolchain for compressed-sensing telemetry.
Instrumented agents fold metric samples into residuals against a predicted
baseline, scatter those residuals into a small number of linear measurements
(a "sketch"), and ship the sketch instead of every raw series. A backend
recovers which exact series misbehaved via sparse recovery, reconstructing
anomalies from a fraction of the bytes a full remote-write stream would cost.

Observability incumbents force a choice between paying full per-series price
or losing fidelity to downsampling. Palimpsest keeps three layers instead: an
exact keyframe layer forever (PromQL-queryable), a sketched layer for anomaly
recovery, and a short instance-level ring buffer for drilldown — while only
routing the small slice of genuinely interesting series to expensive
per-series-billed stores. See [docs/COSTS.md](docs/COSTS.md) for the full
cost decomposition (including an honest anti-claim about when this doesn't
save money), and [docs/adr/](docs/adr/) for the architecture decision records
the design is built from.

**Status: pre-alpha.** This repository currently contains the design
([docs/SPEC.md](docs/SPEC.md), [docs/adr/ADR-001](docs/adr/) through
ADR-013) and a package skeleton (`doc.go` stubs, no implementation yet).
Nothing here is ready for use.

## License

Palimpsest is licensed under the GNU Affero General Public License,
version 3 only (`AGPL-3.0-only`). See [LICENSE](LICENSE).
