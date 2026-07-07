# ADR-017: Merkle Dict Root + Bidirectional Resync — Evidence-Gate Decision

**Status:** Accepted (evidence-gated; backlog item stays OPEN — see Decision)

## Problem

ADR-008's `dict_root` is a flat digest — `xxh64(sorted active ids, seed=0)`
(`pkg/wire/dictroot.go`) — with exactly one resync mechanism: a golden
(Full, non-KDELTA) keyframe re-announces every active ID via
`Tracker.FullDict()`, and a decoder whose dictionary has drifted heals only
when the *next* such golden keyframe verifies. A backlog item proposed
replacing this with a true Merkle tree over the active-ID set plus a
bidirectional resync channel (request/respond over specific subtree hashes
that differ, rather than re-sending the whole dictionary). Building it is
real engineering cost; this ADR is the evidence gate Prompt 16 (Part A)
specified before spending it.

## Decision rule (specified in advance)

Build the Merkle root + resync channel **only if**, measured at realistic
churn:

1. full-dict-keyframe bytes exceed 10% of total wire bytes, **or**
2. time-to-heal (mismatch detected → next verified golden keyframe) exceeds
   2x `keyframe_full_dict_every` (expressed as the golden-keyframe cadence
   in wall-clock time).

Otherwise: mark the backlog item `CLOSED-NOT-NEEDED` with the numbers. A
negative result closes it.

## Measurement

Full methodology, exact commands/test names, and the complete tables are in
`docs/PERF.md`'s "Merkle dict_root / resync evidence gate" section
(`internal/core.TestDictRootEvidenceGateMeasurement` and
`.TestDictRootHealTimeMeasurement`). Summary:

| Condition | Threshold | Measured | Result |
| --- | --- | --- | --- |
| Full-dict-keyframe bytes / total wire bytes, churn=10/min | 10% | **34.7%** | exceeds |
| Full-dict-keyframe bytes / total wire bytes, churn=60/min | 10% | **28.5%** | exceeds |
| Full-dict-keyframe bytes / total wire bytes, churn=300/min | 10% | **18.1%** | exceeds |
| Time-to-heal (isolated, single dropped birth, no other confound) | 20 min (2x a 10 min golden cycle) | 9 min | within bound |

All three churn rates tested (10, 60, 300 events/min, each run for a
1h-equivalent 360 windows at plsim's own defaults: 10s flush interval, 300
initial logical series, `m=2000 d=6 bits=8`, `keyframe-every=6
golden-every=10`) clear condition 1 by a wide margin — including the
*lowest* churn rate, where the golden keyframe's relative share is largest
precisely because there is less competing per-window churn traffic to
dilute it. Condition 2, measured in isolation from the defect below (long
`series_ttl`, one deliberately dropped frame, no other churn), stays inside
its bound: the designed mechanism heals within one golden cycle, as
intended.

## Decision

**Condition 1 is met at every tested churn rate. The backlog item is not
`CLOSED-NOT-NEEDED`.** A Merkle dict root + bidirectional resync channel is
justified by the evidence and is proposed as a v3 protocol change (see
`docs/rfc/palimpsest-wire-v2.md`'s "v3 proposal appendix (informative)") —
per this prompt's guardrails, it is *proposed*, not implemented, here: this
prompt documents wire v2 as shipped and performs no protocol changes.

The core economic argument condition 1 confirms: `ADR-011`'s "golden
keyframes carry no savings, only correctness" already flagged golden
keyframes as the expensive case (byte parity with raw remote-write); this
measurement shows the *dictionary* half of that payload (the full re-list
of every active ID's name string, not the values) is, on its own, over a
quarter of total shard traffic at realistic churn. A Merkle root would let
a decoder verify "my dictionary still matches" in O(1) (one hash compare)
and, on a mismatch, resync only the differing subtrees — instead of paying
the full O(N) name-list cost on every golden cadence regardless of whether
anything actually diverged.

## Discovered defect (not fixed here)

The lossless (zero dropped/duplicated/reordered frames) condition-1 runs
were expected to show `dict_root_mismatches = 0`. They did not: all three
showed exactly one mismatch, and the shard was still DEGRADED at the end of
the run. Root cause, present identically in `cmd/plsim/encoder.go`,
`otel/processor/csresidual/processor.go`, and `internal/core`'s test
encoder: on a golden keyframe window, `tracker.Expire()`'s tombstones are
applied to the encoder's own state but then discarded from the wire —
`f.DictDeltas` is set to `tracker.FullDict()` (adds only) instead of the
usual `dictDeltas` (`births ++ tombstones`) whenever `golden == true`. A
decoder that already held a since-expired ID is never told to drop it, and
never will be (the encoder does not re-tombstone an ID it has already
expired once) — a **permanent**, not bounded, divergence. Over a realistic
1h run this is close to inevitable: tombstones fire roughly every 10-15
windows against a 60-window golden cycle, so a same-window collision is a
matter of when, not if.

This is a real, evidence-backed bug and independently strengthens the case
against `CLOSED-NOT-NEEDED` — a resync channel would detect and repair
exactly this kind of divergence automatically, which the current
"heal-on-next-golden" mechanism structurally cannot when the golden itself
is what drops the tombstone. It is also fixable on its own, without any
wire-format change (a golden keyframe's `DictDeltas` should carry that
window's tombstones *alongside* `FullDict()`'s adds, not instead of them —
`dict_delta` entries already support mixed birth/tombstone lists in any
frame type). Per this prompt's guardrails ("no protocol changes... discovered
defects become a v3 proposal appendix, not silent edits"), it is documented
here and in the RFC's v3 appendix, not patched in this change.
