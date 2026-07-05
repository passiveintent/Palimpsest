# Performance

Real numbers from this repository's benchmark suite (`make bench` /
`go test -bench=. -benchmem ./...`) and from short `cmd/plsim` runs whose
output frames were inspected directly. This is a Prompt 8 deliverable per
`palimpsest_copilot_prompts_v3_full.md`: no numbers here are estimated or
back-computed from the design — each one is either a `go test -bench`
result or a byte count measured off real encoded frames.

## Measurement environment

```
goos: windows
goarch: amd64
cpu: AMD Ryzen 7 5800H with Radeon Graphics
go version: go1.26.4
```

A single developer machine, not a dedicated benchmark host — treat these
as directionally representative, not SLA numbers. Reproduce with:

```
make bench
```

## Encode path (`pkg/sketch`)

| Benchmark | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `BenchmarkUpdateCached` (steady state: Phi columns already cached) | 28.14 | 0 | 0 |
| `BenchmarkUpdateUncached` (cold: hashes + cache insert every call) | 699.2 | 181 | 2 |
| `BenchmarkFlush` (drain + energy, m=4096) | 13,997 | 32,768 | 1 |
| `BenchmarkSnapshot` (`sketch.RingBuffer.Snapshot`, 1000 samples) | 49,628 | 109,304 | 19 |

**Honest note on the cached number**: `pkg/sketch/bench_test.go`'s own doc
comment sets a `< 20 ns/op` aspirational target for the cached path (not a
CI gate). On this machine it measures 28.14 ns/op — close, not under. Not
adjusted or re-run selectively; reported as measured.

## Wire codec (`pkg/wire`)

| Benchmark | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `BenchmarkMarshal` | 4,202 | 11,008 | 2 |
| `BenchmarkUnmarshal` | 2,856 | 4,368 | 6 |
| `BenchmarkQuantize` (m=2048) | 18,775 | 4,096 | 1 |
| `BenchmarkDequantize` (m=2048) | 10,488 | 16,384 | 1 |
| `BenchmarkEncode` (Quantize+Marshal, m=2000, bits=8) | 18,995 | 4,352 | 2 |

## Recovery (`pkg/recover`)

**Decode time (m=2000, N=50k)** — `BenchmarkRecover`, matching
`testdata/golden/recovery_case1.json`'s scale (N=50,000 tracked series,
m=2000 measurements, d=6 hashes/series, k=50 planted deviations, default
`Options{Iters:350, Lambda:0.05, PowerIters:50, Threshold:0.3}`):

```
BenchmarkRecover-16    1    3,095,950,200 ns/op   48,947,720 B/op   100,043 allocs/op
```

**~3.1 seconds per full solve** at this scale on a single core. This is the
dominant cost in the whole pipeline by four orders of magnitude — FISTA's
350-iteration cap and the per-iteration `CSR` matrix-vector products over
100k+ nonzero entries (N=50k dictionary × d=6) drive it, not allocation
(the 100,043 allocs/op is one per dictionary entry, from `BuildCSR`).
`go test`'s default benchtime stopped at b.N=1 (one iteration already
exceeded 1s). Practical implication: a shard's recovery cadence
(`flush_interval`) must stay comfortably above this at N=50k-series scale,
or reconstruction falls behind — see the "storm cliff" discussion in
[../README.md](../README.md)'s Limitations section for the related sparsity
ceiling.

## Watermark (`internal/core`)

| Benchmark | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| `BenchmarkWatermark` (`Watermark.OnFrameArrival`, monotonic windows) | 465.4 | 88 | 1 |

Negligible next to a recovery solve; the watermark/repair bookkeeping is
not a bottleneck at any realistic frame rate.

## Keyframe bytes/series: KDELTA vs full

Measured directly (not benchmarked) via `wire.EncodeKeyframe`, N series
with 2% changed since the last keyframe (a representative "most series are
quiet, a few moved" keyframe interval):

| N | Full (golden) | KDELTA | Ratio |
| --- | --- | --- | --- |
| 1,000 | 12,004 B (12.00 B/series) | 43 B (0.04 B/series) | 0.004x |
| 10,000 | 120,004 B (12.00 B/series) | 429 B (0.04 B/series) | 0.004x |

Full keyframes are exactly `12 B/series` regardless of N (`id u64 + value
f32`, ADR-011's byte-parity point: a full keyframe is *not* smaller than
raw remote-write). KDELTA's cost scales with *changed* series, not N — at
2% churn between keyframes, KDELTA is ~300x smaller than a full keyframe.
This is why `keyframe_full_dict_every`/`golden_keyframe_every` exists as a
separate, rarer cadence (ADR-011): full keyframes carry no savings, only
correctness (bootstrapping/resyncing); KDELTA carries the savings.

## Bytes/flush at bits=8

Measured via `wire.Quantize` + `wire.Marshal` for a representative m=2000
RESIDUAL window (no dict_deltas, no snapshot blob):

```
payload: 2,000 bytes (1 byte/measurement, bits=8)
full frame (magic/version/header/CRC32C etc.): 2,077 bytes
```

77 bytes of fixed per-frame overhead on top of the payload at this scale —
under 4% overhead, dominated by the payload itself as intended.

## Metadata overhead at churn-rate 60/min

Measured from a real `cmd/plsim` run (`--logical-series 300
--instances-per-logical 10 --churn-rate 60 --interval 200ms --windows 40`,
no anomalies/drift/hot-instance — isolating churn's cost alone), inspecting
every emitted frame directly:

```
frames: 40   total frame bytes: 125,400   dict_delta bytes: 45,147 (909 deltas)
36.0% of total frame bytes are dict_deltas; ~22.7 deltas/frame, ~1,129 B/frame
```

At 60 births+deaths/min against a 300-group (900-series) population and a
200ms flush interval, dictionary churn is over a third of wire bytes — a
meaningfully higher overhead than the steady-state case, and the honest
reason `series_ttl` and churn-rate tuning matter operationally: cardinality
churn is metadata-plane cost (ADR-008), not sparsity tax, but it is not
*free*.

## Snapshot size under hot load

Measured from a real `cmd/plsim` run (`--hot-instance 5
--snapshot-threshold 20`, no other scenarios active), inspecting every
frame's `SnapshotBlob` directly:

```
6 frames carried a snapshot blob: min 284 B, max 384 B, avg 334 B
```

Dashcam snapshots (ADR-009/ADR-012) stay small even under a hot-instance
spike, since each one carries a single flagged series' bounded ring-buffer
window (`--ring-window`, default 15 min, but only the samples actually
pushed since construction — these short test runs, not 15 minutes of
history), not the whole fleet's instance detail.
