#!/usr/bin/env python3
# Gate G9 analysis: pool the confirmatory JSON outputs across seeds {41,42,43}
# and evaluate the pre-registered kill criteria K1-K5 in order.
# Usage: python3 scripts/g9_analyze.py results/g9/confirm
import json
import glob
import sys
import os
from collections import defaultdict

CONFIRM = sys.argv[1] if len(sys.argv) > 1 else "results/g9/confirm"
SEEDS = [41, 42, 43]


def load(scenario, label, mag=None):
    """Load per-seed run results for a scenario/label (optionally one s3 magnitude)."""
    out = []
    for seed in SEEDS:
        tag = f"-m{mag}" if mag is not None else ""
        path = os.path.join(CONFIRM, f"{scenario}{tag}-{label}-seed{seed}.json")
        if os.path.exists(path):
            out.append(json.load(open(path)))
    return out


def burst_detect_rate(runs, detector):
    """Fraction of bursts across all seeds where `detector` fired, and mean lag."""
    hit = tot = 0
    lags = []
    for r in runs:
        for b in r["bursts"]:
            tot += 1
            d = b["det"].get(detector, {"detected": False})
            if d["detected"]:
                hit += 1
                lags.append(d["lag_sec"])
    rate = hit / tot if tot else 0.0
    lag = sum(lags) / len(lags) if lags else float("nan")
    return rate, lag, hit, tot


def section(title):
    print("\n" + "=" * 72)
    print(title)
    print("=" * 72)


# ---------------------------------------------------------------------------
section("Per-scenario detector detection rates (pooled over seeds)")
DETS = ["cs", "c", "storm", "k30", "k15", "routed"]
scen_rows = {}
for sc in ["s1", "s2", "s4", "s6"]:
    pre = load(sc, "prefix")
    post = load(sc, "postfix")
    print(f"\n{sc}: {len(post)} seeds, {sum(len(r['bursts']) for r in post)} bursts total")
    row = {}
    for det in DETS:
        rpre = burst_detect_rate(pre, det)
        rpost = burst_detect_rate(post, det)
        row[det] = rpost
        print(f"  {det:7} pre={rpre[0]:.2f} (lag {rpre[1]:6.0f}s)   post={rpost[0]:.2f} (lag {rpost[1]:6.0f}s)  [{rpost[2]}/{rpost[3]}]")
    scen_rows[sc] = {"pre": pre, "post": post}

# ---------------------------------------------------------------------------
section("K1 — S3 monotonicity (post-fix CS detection vs magnitude)")
s3mags = ["1", "2", "5", "10", "20"]
k1_rates = {}
print("mag   cs_post   cs_pre    (pooled detection rate over seeds×bursts)")
for mag in s3mags:
    post = load("s3", "postfix", mag)
    pre = load("s3", "prefix", mag)
    rp = burst_detect_rate(post, "cs")
    rq = burst_detect_rate(pre, "cs")
    k1_rates[mag] = rp[0]
    print(f"{mag:>3}   {rp[0]:.3f}    {rq[0]:.3f}     [{rp[2]}/{rp[3]}]")
# Monotonicity: higher magnitude must not detect with lower probability
# (tolerance 0.05, per locked K1 operationalization).
mono = True
viol = []
order = [float(m) for m in s3mags]
for i in range(len(s3mags)):
    for j in range(i + 1, len(s3mags)):
        if k1_rates[s3mags[j]] < k1_rates[s3mags[i]] - 0.05:
            mono = False
            viol.append((s3mags[i], s3mags[j], k1_rates[s3mags[i]], k1_rates[s3mags[j]]))
print(f"\nK1 monotone (tol 0.05): {mono}")
for v in viol:
    print(f"  VIOLATION: mag {v[0]} rate {v[2]:.2f} > mag {v[1]} rate {v[3]:.2f}")

# ---------------------------------------------------------------------------
section("K2 — unique-detection set (CS wins vs substrate(c)+F3 across S1-S4)")
# For each scenario/burst, CS uniquely detects iff cs detected AND
# (c NOT detected OR c lag > cs lag + 60s).
unique = defaultdict(list)
for sc in ["s1", "s2", "s3", "s4"]:
    if sc == "s3":
        runs = []
        for mag in s3mags:
            runs += load("s3", "postfix", mag)
    else:
        runs = load(sc, "postfix")
    for r in runs:
        for b in r["bursts"]:
            cs = b["det"].get("cs", {"detected": False, "lag_sec": 0})
            c = b["det"].get("c", {"detected": False, "lag_sec": 0})
            if not cs["detected"]:
                continue
            cwins = c["detected"] and c["lag_sec"] <= cs["lag_sec"] + 60
            if not cwins:
                unique[sc].append((r.get("mag_label", ""), b["magnitude"], cs["lag_sec"],
                                   c["detected"], c["lag_sec"], b.get("cs_spurious", 0)))
n_unique = sum(len(v) for v in unique.values())
print(f"Unique CS detections (CS detects, substrate(c)+F3 misses or is >60s slower): {n_unique}")
for sc, lst in unique.items():
    print(f"  {sc}: {len(lst)} unique burst-detections")
    for u in lst[:6]:
        print(f"     mag={u[1]} cs_lag={u[2]:.0f}s c_detected={u[3]} c_lag={u[4]:.0f}s cs_spurious={u[5]}")
print(f"\nK2: unique set {'EMPTY -> KILL' if n_unique == 0 else 'non-empty -> continue'}")

# ---------------------------------------------------------------------------
section("K3 — precision of unique detections + quiet-month events/day")
# Spurious per true event on unique classes; quiet-month from s5.
s5post = load("s5", "postfix")
s5pre = load("s5", "prefix")
for label, runs in [("post", s5post), ("pre", s5pre)]:
    if not runs:
        continue
    epd = [r["quiet_events_per_day"] for r in runs]
    sup = [r["quiet_mean_support"] for r in runs]
    print(f"s5 {label}: quiet events/day {[round(x,2) for x in epd]} "
          f"(mean {sum(epd)/len(epd):.2f}); mean support {[round(x,1) for x in sup]}")
if n_unique:
    spur = [u[5] for lst in unique.values() for u in lst]
    print(f"Unique-class CS spurious series per true event: "
          f"min {min(spur)} max {max(spur)} mean {sum(spur)/len(spur):.1f}")

# ---------------------------------------------------------------------------
section("K4 — dumb-money byte ledger (bytes/day, pooled mean over seeds)")
def bytes_row(sc, label):
    runs = load(sc, label)
    if not runs:
        return None
    agg = defaultdict(float)
    for r in runs:
        for k, v in r["bytes_per_day"].items():
            agg[k] += v / len(runs)
    return agg
for sc in ["s1", "s2", "s4", "s6", "s5"]:
    b = bytes_row(sc, "postfix")
    if not b:
        continue
    print(f"\n{sc} (KiB/day): pal_total={b['pal_total']/1024:.1f}  "
          f"gorilla_10s={b['gorilla_10s']/1024:.1f}  "
          f"dumb_k30={b['dumb_k30']/1024:.1f}  dumb_k15={b['dumb_k15']/1024:.1f}  "
          f"dumb_routed={b['dumb_routed']/1024:.1f}")

# ---------------------------------------------------------------------------
section("S4 detail — the one class CS might uniquely own (sub-keyframe)")
for label in ["prefix", "postfix"]:
    runs = load("s4", label)
    print(f"\ns4 {label}:")
    for det in ["cs", "c", "k30", "k15", "routed", "storm"]:
        r = burst_detect_rate(runs, det)
        print(f"  {det:7} rate={r[0]:.2f} lag={r[1]:6.0f}s [{r[2]}/{r[3]}]")
if s5post:
    print("\n(routed set contains a burst target in any s4 post run:",
          any(r.get("routed_has_target") for r in load("s4", "postfix")), ")")
