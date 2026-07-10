# Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
# SPDX-License-Identifier: BUSL-1.1
#
# This source code is licensed under the BUSL-1.1 license found in the
# LICENSE file in the root directory of this source tree.

"""Plot helpers for eyeballing each synth generator's output.

Not part of the statistical core (I/O at the edges) — these write PNGs to
`docs/fixtures/` purely for human inspection during development. Colors
follow the project's validated reference palette (categorical hues
assigned in fixed order, sequential blue for ordered magnitude, the
`critical` status color reserved for exceedances/incidents).
"""

from __future__ import annotations

from pathlib import Path

import matplotlib

matplotlib.use("Agg")

import matplotlib.pyplot as plt  # noqa: E402 (backend must be set first)
import numpy as np  # noqa: E402
from matplotlib.axes import Axes  # noqa: E402
from matplotlib.figure import Figure  # noqa: E402

from pconformal.synth.bimodal_fleet import bimodal_fleet  # noqa: E402
from pconformal.synth.heavy_tail import heavy_tail  # noqa: E402
from pconformal.synth.heteroscedastic import heteroscedastic_pair  # noqa: E402
from pconformal.synth.incidents import inject_incidents  # noqa: E402
from pconformal.synth.regime_shift import Kind, Target, regime_shift  # noqa: E402
from pconformal.synth.seasonal import seasonal_latency  # noqa: E402
from pconformal.synth.slow_burn import slow_burn  # noqa: E402

FIXTURES_DIR = Path(__file__).resolve().parents[3] / "docs" / "fixtures"

_SURFACE = "#fcfcfb"
_INK = "#0b0b0b"
_MUTED = "#898781"
_GRID = "#e1e0d9"
_BLUE = "#2a78d6"  # categorical slot 1
_AQUA = "#1baf7a"  # categorical slot 2
_ORANGE = "#eb6834"  # categorical slot 8
_CRITICAL = "#d03b3b"  # status: critical, reserved for exceedances/incidents
_SEQ_LIGHT = "#86b6ef"  # sequential step 250
_SEQ_MID = "#3987e5"  # sequential step 400
_SEQ_DARK = "#184f95"  # sequential step 600


def _new_axes(figsize: tuple[float, float] = (9.0, 4.5)) -> tuple[Figure, Axes]:
    fig, ax = plt.subplots(figsize=figsize, facecolor=_SURFACE)
    ax.set_facecolor(_SURFACE)
    ax.grid(True, color=_GRID, linewidth=0.8)
    ax.set_axisbelow(True)
    ax.spines["top"].set_visible(False)
    ax.spines["right"].set_visible(False)
    ax.spines["left"].set_color(_MUTED)
    ax.spines["bottom"].set_color(_MUTED)
    ax.tick_params(colors=_MUTED)
    ax.xaxis.label.set_color(_INK)
    ax.yaxis.label.set_color(_INK)
    ax.title.set_color(_INK)
    return fig, ax


def _save(fig: Figure, name: str, out_dir: Path) -> Path:
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / f"{name}.png"
    fig.tight_layout()
    fig.savefig(path, dpi=150, facecolor=_SURFACE)
    plt.close(fig)
    return path


def plot_seasonal_latency(seed: int = 0, out_dir: Path = FIXTURES_DIR, **kwargs: object) -> Path:
    frame, _gt = seasonal_latency(seed, **kwargs)  # type: ignore[arg-type]
    fig, ax = _new_axes()
    ax.plot(frame.t, frame.p50, color=_SEQ_LIGHT, linewidth=1.5, label="p50")
    ax.plot(frame.t, frame.p95, color=_SEQ_MID, linewidth=1.5, label="p95")
    ax.plot(frame.t, frame.p99, color=_SEQ_DARK, linewidth=1.5, label="p99")
    ax.set_xlabel("window (hour)")
    ax.set_ylabel("latency")
    ax.set_title("seasonal_latency: hour-of-week-seasonal window stats")
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "seasonal_latency", out_dir)


def plot_heteroscedastic_pair(
    seed: int = 0, out_dir: Path = FIXTURES_DIR, **kwargs: object
) -> Path:
    frame, gt = heteroscedastic_pair(seed, **kwargs)  # type: ignore[arg-type]
    is_a = frame.entity == "service_a"
    fig, ax = _new_axes()
    ax.plot(frame.t[is_a], frame.p50[is_a], color=_BLUE, linewidth=1.2, label="service_a p50")
    ax.plot(
        frame.t[~is_a], frame.p50[~is_a], color=_AQUA, linewidth=1.2, label="service_b p50"
    )
    ax.set_xlabel("window")
    ax.set_ylabel("latency")
    ax.set_title(
        f"heteroscedastic_pair: variance_ratio={gt.variance_ratio:g}"
        f" (sigma_a={gt.sigma_a:.2f}, sigma_b={gt.sigma_b:.2f})"
    )
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "heteroscedastic_pair", out_dir)


def plot_heavy_tail(seed: int = 0, out_dir: Path = FIXTURES_DIR, **kwargs: object) -> Path:
    frame, gt = heavy_tail(seed, **kwargs)  # type: ignore[arg-type]
    del frame
    fig, ax = _new_axes()
    ax.hist(gt.raw[~gt.is_exceedance], bins=40, color=_BLUE, alpha=0.8, label="body")
    ax.hist(gt.raw[gt.is_exceedance], bins=20, color=_CRITICAL, alpha=0.9, label="exceedances")
    ax.axvline(gt.u, color=_MUTED, linestyle="--", linewidth=1.2, label=f"u={gt.u:g}")
    ax.set_yscale("log")
    ax.set_xlabel("value")
    ax.set_ylabel("count (log)")
    ax.set_title(f"heavy_tail: xi={gt.xi:g}, sigma={gt.sigma:g}, u={gt.u:g}")
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "heavy_tail", out_dir)


def plot_regime_shift(
    seed: int = 0,
    t0: int = 200,
    kind: Kind = "step",
    target: Target = "inputs",
    out_dir: Path = FIXTURES_DIR,
    **kwargs: object,
) -> Path:
    frame, gt = regime_shift(seed, t0, kind, target, **kwargs)  # type: ignore[arg-type]
    fig, (ax_score, ax_feat) = plt.subplots(
        2, 1, figsize=(9.0, 6.0), sharex=True, facecolor=_SURFACE
    )
    for ax in (ax_score, ax_feat):
        ax.set_facecolor(_SURFACE)
        ax.grid(True, color=_GRID, linewidth=0.8)
        ax.set_axisbelow(True)
        ax.spines["top"].set_visible(False)
        ax.spines["right"].set_visible(False)
        ax.spines["left"].set_color(_MUTED)
        ax.spines["bottom"].set_color(_MUTED)
        ax.tick_params(colors=_MUTED)
        ax.xaxis.label.set_color(_INK)
        ax.yaxis.label.set_color(_INK)
        ax.title.set_color(_INK)
        ax.axvline(t0, color=_MUTED, linestyle="--", linewidth=1.0)

    assert frame.score is not None
    assert frame.input_features is not None
    ax_score.plot(frame.t, frame.score, color=_BLUE, linewidth=1.2, label="score")
    ax_score.set_ylabel("score")
    ax_score.set_title(f"regime_shift: target={target}, kind={kind}, t0={t0}")
    ax_score.legend(frameon=False, labelcolor=_INK)

    palette = [_BLUE, _AQUA, _ORANGE]
    for i in range(frame.input_features.shape[1]):
        ax_feat.plot(
            frame.t,
            frame.input_features[:, i],
            color=palette[i % len(palette)],
            linewidth=1.2,
            label=f"feature_{i}",
        )
    ax_feat.set_xlabel("window")
    ax_feat.set_ylabel("input feature mix")
    ax_feat.legend(frameon=False, labelcolor=_INK)

    del gt
    return _save(fig, "regime_shift", out_dir)


def plot_slow_burn(
    seed: int = 0, rate: float = 0.02, out_dir: Path = FIXTURES_DIR, **kwargs: object
) -> Path:
    frame, gt = slow_burn(seed, rate, **kwargs)  # type: ignore[arg-type]
    fig, ax = _new_axes()
    assert frame.score is not None
    ax.plot(frame.t, frame.score, color=_BLUE, linewidth=1.0, alpha=0.6, label="score")
    ax.plot(frame.t, gt.true_score_mean, color=_INK, linewidth=1.5, label="true mean")
    ax.set_xlabel("window")
    ax.set_ylabel("score")
    ax.set_title(f"slow_burn: rate={rate:g} (stationary inputs)")
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "slow_burn", out_dir)


def plot_bimodal_fleet(
    seed: int = 0,
    hosts: int = 24,
    mix: float = 0.25,
    out_dir: Path = FIXTURES_DIR,
    **kwargs: object,
) -> Path:
    frame, gt = bimodal_fleet(seed, hosts, mix, **kwargs)  # type: ignore[arg-type]
    order = np.argsort(frame.p95)
    colors = [_ORANGE if m == "slow" else _BLUE for m in gt.host_mode[order]]

    fig, ax = _new_axes()
    ax.bar(range(hosts), frame.p95[order], color=colors)
    ax.axhline(
        gt.naive_avg_p95,
        color=_MUTED,
        linestyle="--",
        linewidth=1.2,
        label=f"naive avg p95 = {gt.naive_avg_p95:.1f}",
    )
    ax.axhline(
        gt.true_pooled_p95,
        color=_INK,
        linewidth=1.5,
        label=f"true pooled p95 = {gt.true_pooled_p95:.1f}",
    )
    ax.set_xlabel("host (sorted by p95; orange = slow mode)")
    ax.set_ylabel("p95 latency")
    ax.set_title(f"bimodal_fleet: mix={mix:g}, relative_error={gt.relative_error:.0%}")
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "bimodal_fleet", out_dir)


def plot_inject_incidents(
    seed: int = 0, k: int = 3, out_dir: Path = FIXTURES_DIR, **kwargs: object
) -> Path:
    base_frame, _base_gt = seasonal_latency(seed, weeks=4)
    frame, gt = inject_incidents(base_frame, seed, k, **kwargs)  # type: ignore[arg-type]

    fig, ax = _new_axes()
    ax.plot(frame.t, frame.p50, color=_BLUE, linewidth=1.2, label="p50 (with incidents)")
    for s in gt.starts:
        ax.axvspan(s, s + gt.incident_len, color=_CRITICAL, alpha=0.25)
    ax.set_xlabel("window")
    ax.set_ylabel("latency")
    ax.set_title(f"inject_incidents: k={k}, incident_len={gt.incident_len}")
    ax.legend(frameon=False, labelcolor=_INK)
    return _save(fig, "inject_incidents", out_dir)


ALL_PLOTS = (
    plot_seasonal_latency,
    plot_heteroscedastic_pair,
    plot_heavy_tail,
    plot_regime_shift,
    plot_slow_burn,
    plot_bimodal_fleet,
    plot_inject_incidents,
)


__all__ = [
    "FIXTURES_DIR",
    "ALL_PLOTS",
    "plot_seasonal_latency",
    "plot_heteroscedastic_pair",
    "plot_heavy_tail",
    "plot_regime_shift",
    "plot_slow_burn",
    "plot_bimodal_fleet",
    "plot_inject_incidents",
]
