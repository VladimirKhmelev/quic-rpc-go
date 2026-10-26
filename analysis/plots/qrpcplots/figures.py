"""The figures of a series (ANL-2): each shows its conditions, units,
intervals and number of runs. The numbers are those of the list of figures
in the requirements."""

from __future__ import annotations

from pathlib import Path

import matplotlib

matplotlib.use("Agg")

import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402
from matplotlib.ticker import FuncFormatter  # noqa: E402

BASE = "base"
# The fields of a network that the axis of losses keeps at the base point.
_AXIS_FIXED = ["rtt_ms", "jitter_ms", "reorder_pct", "rate_bps", "queue_bdp"]
DPI = 150


def networks(runs: pd.DataFrame) -> pd.DataFrame:
    """Returns a row per network of the runs with its profile."""
    return runs.drop_duplicates("network").set_index("network")[_AXIS_FIXED + ["loss_pct", "loss_burst"]]


def loss_axis(runs: pd.DataFrame) -> list[tuple[str, float]]:
    """Returns the networks of the base point and of uniform losses around
    it, by loss: those that differ from the base point in the loss alone."""
    nets = networks(runs)
    if BASE not in nets.index:
        return []
    base = nets.loc[BASE]
    axis = [(name, float(p["loss_pct"])) for name, p in nets.iterrows()
            if (p[_AXIS_FIXED] == base[_AXIS_FIXED]).all() and p["loss_burst"] in (0, 1)]
    return sorted(axis, key=lambda a: a[1])


def plain_log(axis) -> None:
    """Puts an axis on a log scale with its ticks as plain numbers."""
    axis.axes.set_xscale("log") if axis is axis.axes.xaxis else axis.axes.set_yscale("log")
    plain = FuncFormatter(lambda v, _: f"{v:g}")
    axis.set_major_formatter(plain)
    axis.set_minor_formatter(FuncFormatter(lambda v, _: f"{v:g}" if f"{v:.1g}"[0] in "25" else ""))


def colors(variants: list[str]) -> dict[str, str]:
    """Returns a color per variant, the same in every figure."""
    cycle = plt.rcParams["axes.prop_cycle"].by_key()["color"]
    return {v: cycle[i % len(cycle)] for i, v in enumerate(sorted(variants))}


def describe(runs: pd.DataFrame, network: str) -> str:
    p = networks(runs).loc[network]
    parts = [f"RTT {p['rtt_ms']:g} мс"]
    if p["jitter_ms"]:
        parts.append(f"джиттер {p['jitter_ms']:g} мс")
    if p["reorder_pct"]:
        parts.append(f"перестановки {p['reorder_pct']:g}%")
    parts.append(f"полоса {p['rate_bps'] / 1e6:g} Мбит/с" if p["rate_bps"] else "полоса не ограничена")
    return ", ".join(parts)


def runs_note(table: pd.DataFrame) -> str:
    lo, hi = int(table["runs"].min()), int(table["runs"].max())
    n = f"{lo}" if lo == hi else f"{lo}–{hi}"
    return f"медиана по {n} прогонам, интервалы — 95% bootstrap"


def fig_loss(cells: pd.DataFrame, runs: pd.DataFrame, load: str, kind: str, number: int, path: Path) -> bool:
    """Draws p99 of the calls of kind under load against uniform losses, a
    line per variant (fig. 1 and 2). Returns False without the data."""
    axis = loss_axis(runs)
    table = cells[(cells["load"] == load) & (cells["kind"] == kind) & cells["network"].isin([n for n, _ in axis])]
    if len(axis) < 2 or table.empty:
        return False
    loss = dict(axis)
    palette = colors(list(cells["variant"].unique()))
    fig, ax = plt.subplots(figsize=(7, 4.5))
    for variant, group in table.groupby("variant"):
        group = group.assign(loss=group["network"].map(loss)).sort_values("loss")
        err = [group["p99"] - group["p99_lo"], group["p99_hi"] - group["p99"]]
        ax.errorbar(group["loss"], group["p99"], yerr=err, marker="o", capsize=3, label=variant, color=palette[variant])
    plain_log(ax.yaxis)
    ax.set_xlabel("потери в каждую сторону, %")
    ax.set_ylabel(f"p99 вызова {kind}, мс")
    ax.set_title(f"Рис. {number}. p99 вызовов {kind} при нагрузке {load} от равномерных потерь", fontsize=10)
    fig.text(0.5, 0.01, f"{describe(runs, BASE)}; {runs_note(table)}", ha="center", fontsize=8)
    ax.grid(True, which="both", alpha=0.3)
    ax.legend(fontsize=8)
    fig.tight_layout(rect=(0, 0.04, 1, 1))
    fig.savefig(path, dpi=DPI)
    plt.close(fig)
    return True


def fig_ecdf(calls: pd.DataFrame, runs: pd.DataFrame, network: str, load: str, kind: str, path: Path) -> bool:
    """Draws the distribution of the latencies of the calls of kind in a
    network, pooled over the runs of each variant, with its tail (fig. 4)."""
    chosen = runs[(runs["network"] == network) & (runs["load"] == load) & (runs["role"] == "")]
    if chosen.empty:
        return False
    measured = calls[(calls["phase"] == "measure") & (calls["kind"] == kind)]
    palette = colors(list(runs["variant"].unique()))
    fig, (cdf, tail) = plt.subplots(1, 2, figsize=(10, 4.5))
    counts = []
    for variant, group in chosen.groupby("variant"):
        latency = np.sort(measured.loc[measured["run_id"].isin(group["run_id"]), "latency_ns"].to_numpy() / 1e6)
        if len(latency) == 0:
            continue
        counts.append(len(group))
        share = np.arange(1, len(latency) + 1) / len(latency)
        cdf.plot(latency, share, label=variant, color=palette[variant])
        tail.plot(latency, 1 - share + 1 / len(latency), label=variant, color=palette[variant])
    for ax in (cdf, tail):
        plain_log(ax.xaxis)
        ax.set_xlabel(f"задержка вызова {kind}, мс")
        ax.grid(True, which="both", alpha=0.3)
    cdf.set_ylabel("доля вызовов не дольше")
    tail.set_yscale("log")
    tail.set_ylabel("доля вызовов дольше (хвост)")
    tail.legend(fontsize=8)
    loss = float(networks(runs).loc[network, "loss_pct"])
    fig.suptitle(f"Рис. 4. Распределение задержек вызовов {kind} при нагрузке {load}, потери {loss:g}%", fontsize=10)
    n = f"{min(counts)}" if min(counts) == max(counts) else f"{min(counts)}–{max(counts)}"
    fig.text(0.5, 0.01, f"{describe(runs, network)}; все вызовы измерения {n} прогонов каждого варианта", ha="center", fontsize=8)
    fig.tight_layout(rect=(0, 0.04, 1, 0.95))
    fig.savefig(path, dpi=DPI)
    plt.close(fig)
    return True


def fig_profiles(cells: pd.DataFrame, runs: pd.DataFrame, kind: str, path: Path) -> bool:
    """Draws p50, p95 and p99 of the calls of kind for every variant on the
    networks off the axis of losses, a panel per network and load
    (fig. 13)."""
    on_axis = {n for n, _ in loss_axis(runs)}
    table = cells[(cells["kind"] == kind) & ~cells["network"].isin(on_axis)]
    if table.empty:
        return False
    panels = table[["network", "load"]].drop_duplicates().sort_values(["network", "load"]).to_numpy()
    palette = colors(list(cells["variant"].unique()))
    variants = sorted(table["variant"].unique())
    fig, axes = plt.subplots(1, len(panels), figsize=(4.5 * len(panels), 4.5), squeeze=False)
    width = 0.8 / len(variants)
    for ax, (network, load) in zip(axes[0], panels, strict=True):
        group = table[(table["network"] == network) & (table["load"] == load)].set_index("variant")
        for i, variant in enumerate(variants):
            if variant not in group.index:
                continue
            row = group.loc[variant]
            x = np.arange(3) + (i - (len(variants) - 1) / 2) * width
            values = [row[m] for m in ("p50", "p95", "p99")]
            err = [[row[m] - row[m + "_lo"] for m in ("p50", "p95", "p99")], [row[m + "_hi"] - row[m] for m in ("p50", "p95", "p99")]]
            ax.bar(x, values, width, yerr=err, capsize=2, label=variant, color=palette[variant])
        ax.set_xticks(range(3), ["p50", "p95", "p99"])
        plain_log(ax.yaxis)
        ax.set_ylabel(f"задержка вызова {kind}, мс")
        ax.set_title(f"{network}, нагрузка {load}\n{describe(runs, network)}", fontsize=8)
        ax.grid(True, axis="y", which="both", alpha=0.3)
    axes[0][0].legend(fontsize=8)
    fig.suptitle(f"Рис. 13. Перцентили вызовов {kind} на профилях сети", fontsize=10)
    fig.text(0.5, 0.01, runs_note(table), ha="center", fontsize=8)
    fig.tight_layout(rect=(0, 0.04, 1, 0.95))
    fig.savefig(path, dpi=DPI)
    plt.close(fig)
    return True
