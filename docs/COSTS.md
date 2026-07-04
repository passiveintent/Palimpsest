# Cost Model

Cost decomposition per ADR-011: per-series pricing, placement arbitrage, and
an honest anti-claim about when this project does *not* save money.

> This is a first-pass compilation for the pre-alpha scaffold, drawn from
> `v3_quick_reference.md` and expanded with the decision recorded in
> [docs/adr/ADR-011-keyframe-economics.md](adr/ADR-011-keyframe-economics.md),
> which is normative.

## Per-series cost

Per-series-billed SaaS (Datadog, CloudWatch, Azure Monitor) charge per active
series. Palimpsest routes only the 1-5% of series flagged interesting to
that expensive tier; the rest live in the sketch and keyframe layers.
Typical savings: **30-100x** versus $100k+/mo at typical per-series rates.

## Placement arbitrage

Keyframes are byte-parity with raw metrics — compression alone doesn't create
savings there. The savings come from *where* each layer is allowed to live:

- **Layer 1 (exact keyframes, 60s)**: bulk, byte-parity data. Routes to cheap
  storage — self-hosted Prometheus, S3/Parquet — not the per-series SaaS.
- **Layer 2 (sketch, 10s)**: kilobytes per flush. Cheap enough to send
  anywhere.
- **Layer 3 (instance ring buffer + snapshots)**: only flagged series get
  drilldown detail persisted; everything else stays in a 5-15 min ring
  buffer and is discarded.

Only the small slice that actually needs per-series billing (anomalies,
drilldown) touches the expensive tier.

## Cost table

| Line | v1 (raw 15s remote-write) | Palimpsest | Savings |
| --- | --- | --- | --- |
| Wire bytes | 86-170 GB/day (10M series) | 15-20 GB/day | ~4-6x |
| Per-series billing | $100k+/mo at typical rates | 1-5% of series sent to expensive store | 30-100x |
| Storage (any tier) | 86-170 GB/day | Keyframe (6 GB/day) + sketch (0.1 GB/day) + anomaly detail (3-10 GB/day) | ~6x |
| Retention | Downsampling burns detail | Keep everything at 60s; anomalies at 10s | retained |
| Blended | n/a | n/a | 10-50x |

## Honest anti-claim

If you're already self-hosted Prometheus at a 60s scrape interval, you see
roughly **1x** cost — you're not paying per-series SaaS pricing today, so
there's nothing to arbitrage. This project's wedge is per-series-billed SaaS
(Datadog, CloudWatch, Azure Monitor), not self-hosted TSDBs.
