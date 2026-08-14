#!/usr/bin/env python3
"""Render a comparison plot from one ablation run.

Input:  a dated output dir produced by run_ablation.sh, e.g.
          data/20260525-153000-NVIDIA_A100-SXM4-40GB/
        containing one subdir per variant (each with samples.csv + meta.json).

Output: ``compare.png`` saved into that same dir, plus a stats table to stdout.

The plot has four panels:
  (1) Time series overlay of SM utilization per variant, with the target line
  (2) Per-variant histogram of util samples
  (3) MAE bar chart (mean absolute |util - target|)
  (4) Per-variant text stats annotation

Methodology notes printed in the stats table:
  - Samples before the first one >5% util are treated as warm-up and dropped
    (collect.sh has a 0.5s pre-sleep to leave a visible idle baseline).
  - Samples after meta.wall_ms_actual are post-workload tail; also dropped.
  - MAE / P50 / P95 / P99 are computed over the surviving window.

Dependencies: matplotlib, numpy. Both are standard scientific-python.
"""

from __future__ import annotations

import json
import os
import sys
from dataclasses import dataclass
from pathlib import Path

import numpy as np

try:
    import matplotlib
    matplotlib.use("Agg")  # headless / CI-friendly
    import matplotlib.pyplot as plt
except ImportError as exc:
    print(f"error: matplotlib is required ({exc})", file=sys.stderr)
    print("       install with: pip3 install matplotlib", file=sys.stderr)
    sys.exit(2)


@dataclass
class Variant:
    name: str
    target_pct: float
    util: np.ndarray          # active-window samples, percent
    elapsed_ms: np.ndarray    # matching elapsed_ms (relative to workload start)
    meta: dict

    @property
    def mae(self) -> float:
        return float(np.mean(np.abs(self.util - self.target_pct)))

    @property
    def mean_util(self) -> float:
        return float(np.mean(self.util))

    def percentiles(self):
        return {p: float(np.percentile(np.abs(self.util - self.target_pct), p))
                for p in (50, 95, 99)}


def load_variant(var_dir: Path) -> Variant | None:
    """Read one variant subdir; return None if it has no usable data."""
    samples_path = var_dir / "samples.csv"
    meta_path = var_dir / "meta.json"
    if not samples_path.exists() or not meta_path.exists():
        print(f"warn: {var_dir} missing samples.csv or meta.json -- skipped",
              file=sys.stderr)
        return None

    with meta_path.open() as f:
        meta = json.load(f)

    # samples.csv: elapsed_ms,sm_util_pct  (header included). genfromtxt with
    # invalid_raise=False silently skips any non-numeric line (e.g. an
    # nvidia-smi error message that snuck into the stream).
    try:
        data = np.genfromtxt(samples_path, delimiter=",", skip_header=1,
                             invalid_raise=False)
    except (ValueError, OSError) as exc:
        print(f"warn: {var_dir} samples.csv unreadable ({exc}) -- skipped",
              file=sys.stderr)
        return None
    if data.size == 0:
        print(f"warn: {var_dir} has no samples -- skipped", file=sys.stderr)
        return None
    if data.ndim == 1:  # exactly one data row -> shape (2,), reshape to (1, 2)
        data = data.reshape(1, -1)

    elapsed_ms = data[:, 0]
    util = data[:, 1]
    wall_ms = float(meta.get("wall_ms_actual", elapsed_ms[-1]))

    # Trim warm-up: drop samples up to the first one above 5% util.
    above_idle = np.where(util > 5.0)[0]
    if len(above_idle) == 0:
        print(f"warn: {var_dir} util never exceeded 5% -- workload probably "
              f"failed (check workload.log)", file=sys.stderr)
        return None
    start_idx = int(above_idle[0])

    # Trim post-workload tail: drop samples beyond meta.wall_ms (relative to
    # workload-start, not sample-start). We anchor at start_idx to keep this
    # robust against varying warm-up time.
    elapsed_rel = elapsed_ms - elapsed_ms[start_idx]
    in_window = elapsed_rel <= wall_ms
    mask = np.zeros_like(util, dtype=bool)
    mask[start_idx:] = True
    mask &= in_window
    if mask.sum() < 5:
        print(f"warn: {var_dir} active window has only {mask.sum()} samples "
              f"-- result may be noisy", file=sys.stderr)

    return Variant(
        name=meta.get("variant", var_dir.name),
        target_pct=float(meta.get("target_sm_pct", 0)),
        util=util[mask],
        elapsed_ms=elapsed_rel[mask],
        meta=meta,
    )


def render(out_dir: Path, variants: list[Variant]) -> Path:
    """Build the 4-panel comparison figure; return the output png path."""
    fig, axes = plt.subplots(2, 2, figsize=(14, 9))
    fig.suptitle(
        f"vgpu-manager SM throttle ablation -- "
        f"{variants[0].meta.get('gpu_name', 'unknown GPU')} "
        f"(target {variants[0].target_pct:.0f}%)",
        fontsize=13,
    )

    colors = plt.rcParams["axes.prop_cycle"].by_key()["color"]
    target = variants[0].target_pct  # assume same target across variants

    # -- (1) time series overlay -------------------------------------------
    ax = axes[0, 0]
    for v, c in zip(variants, colors):
        ax.plot(v.elapsed_ms / 1000.0, v.util, label=v.name, color=c,
                linewidth=0.9, alpha=0.85)
    ax.axhline(target, color="black", linestyle="--", linewidth=1, label=f"target {target:.0f}%")
    ax.set_xlabel("elapsed since workload start (s)")
    ax.set_ylabel("SM utilization (%)")
    ax.set_ylim(0, 105)
    ax.set_title("Time series")
    ax.legend(loc="upper right", fontsize=9)
    ax.grid(alpha=0.3)

    # -- (2) histogram overlay ---------------------------------------------
    ax = axes[0, 1]
    bins = np.linspace(0, 100, 51)
    for v, c in zip(variants, colors):
        ax.hist(v.util, bins=bins, label=v.name, color=c, alpha=0.5,
                density=True)
    ax.axvline(target, color="black", linestyle="--", linewidth=1)
    ax.set_xlabel("SM utilization (%)")
    ax.set_ylabel("density")
    ax.set_title("Sample distribution")
    ax.legend(loc="upper left", fontsize=9)
    ax.grid(alpha=0.3)

    # -- (3) MAE bar chart -------------------------------------------------
    ax = axes[1, 0]
    names = [v.name for v in variants]
    maes = [v.mae for v in variants]
    bars = ax.bar(names, maes, color=colors[: len(variants)])
    for bar, mae in zip(bars, maes):
        ax.text(bar.get_x() + bar.get_width() / 2, mae + 0.2,
                f"{mae:.2f}%", ha="center", va="bottom", fontsize=10)
    ax.set_ylabel("MAE vs target (%)")
    ax.set_title("Mean absolute error (lower is better)")
    ax.grid(alpha=0.3, axis="y")

    # -- (4) stats text panel ----------------------------------------------
    ax = axes[1, 1]
    ax.axis("off")
    lines = []
    for v in variants:
        p = v.percentiles()
        meta = v.meta
        lines.append(f"[{v.name}]  controller={meta.get('controller')}")
        lines.append(f"  samples={len(v.util)}  mean_util={v.mean_util:.2f}%")
        lines.append(f"  MAE={v.mae:.2f}%  P50={p[50]:.2f}%  P95={p[95]:.2f}%  P99={p[99]:.2f}%")
        if meta.get("controller") == "aimd":
            lines.append(f"  AIMD: md={meta.get('aimd_md_divisor')} "
                         f"eff={meta.get('aimd_eff_ratio')} "
                         f"ai_div={meta.get('aimd_ai_base_div')}")
        result = meta.get("workload_result", "")
        if result:
            lines.append(f"  workload: {result}")
        lines.append("")
    ax.text(0.0, 1.0, "\n".join(lines), family="monospace", fontsize=9,
            va="top", ha="left", transform=ax.transAxes)

    plt.tight_layout(rect=(0, 0, 1, 0.96))
    out_png = out_dir / "compare.png"
    fig.savefig(out_png, dpi=120)
    plt.close(fig)
    return out_png


def print_stats(variants: list[Variant]) -> None:
    """Stdout summary table for log scraping."""
    print()
    print(f"{'variant':<16} {'samples':>8} {'mean':>8} {'MAE':>8} "
          f"{'P50':>8} {'P95':>8} {'P99':>8}")
    print("-" * 72)
    for v in variants:
        p = v.percentiles()
        print(f"{v.name:<16} {len(v.util):>8d} {v.mean_util:>7.2f}% "
              f"{v.mae:>7.2f}% {p[50]:>7.2f}% {p[95]:>7.2f}% {p[99]:>7.2f}%")
    print()


def main(argv: list[str]) -> int:
    if len(argv) != 2 or argv[1] in ("-h", "--help"):
        print(f"usage: {argv[0]} <dated_output_dir>", file=sys.stderr)
        print("       e.g. data/20260525-153000-NVIDIA_A100-SXM4-40GB/",
              file=sys.stderr)
        return 2

    out_dir = Path(argv[1]).resolve()
    if not out_dir.is_dir():
        print(f"error: {out_dir} is not a directory", file=sys.stderr)
        return 2

    variant_dirs = sorted(p for p in out_dir.iterdir() if p.is_dir())
    variants = [v for v in (load_variant(d) for d in variant_dirs) if v]
    if not variants:
        print("error: no usable variants found", file=sys.stderr)
        return 1

    # Sanity: all variants should share the same target (otherwise the
    # comparison is meaningless).
    targets = {v.target_pct for v in variants}
    if len(targets) > 1:
        print(f"warn: variants have different targets {targets} -- comparison "
              f"may mislead", file=sys.stderr)

    print_stats(variants)
    out_png = render(out_dir, variants)
    print(f"wrote {out_png}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
