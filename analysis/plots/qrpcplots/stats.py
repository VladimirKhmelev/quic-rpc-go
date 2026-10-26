"""The statistics of a series (EXP-7).

The calls within a run depend on each other: one loss delays a batch of
them. So the unit of analysis is the run: a run yields its percentiles, and
a cell (network, load, kind of call, variant) is the set of the percentiles
of its runs, described by their median with a bootstrap confidence interval
over the runs. The variants are compared on the runs with the Mann-Whitney
test, corrected with Holm's method within a family of tests.
"""

from __future__ import annotations

import math
import zlib

import numpy as np
import pandas as pd
from scipy import stats

QUANTILES = {"p50": 0.50, "p95": 0.95, "p99": 0.99}
CELL = ["network", "load", "kind", "variant"]
BOOTSTRAP = 10_000
LEVEL = 0.95
ALPHA = 0.05
POWER = 0.8
# The asymptotic efficiency of the Mann-Whitney test against the t-test
# under normality: it needs 1/0.955 times the runs.
MW_EFFICIENCY = 3 / math.pi
EFFECTS = (0.05, 0.10, 0.20)


def nearest_rank(values: np.ndarray, q: float) -> float:
    """Returns the q-quantile of values by the nearest rank, as the load
    generator does (loadgen.Percentile)."""
    return float(np.percentile(values, q * 100, method="inverted_cdf"))


def rng_for(*key: object) -> np.random.Generator:
    """Returns a generator seeded by key, so that every number comes out the
    same in any order of computation."""
    return np.random.default_rng(zlib.crc32(repr(key).encode()))


def run_summary(runs: pd.DataFrame, calls: pd.DataFrame) -> pd.DataFrame:
    """Returns a row per run and kind of call of the measurement: the count,
    the errors and the percentiles in milliseconds. A failed call counts
    with its latency, up to the deadline, as the open-loop load sees it."""
    measured = calls[calls["phase"] == "measure"]
    rows = []
    for (run_id, kind), group in measured.groupby(["run_id", "kind"], observed=True):
        latency = group["latency_ns"].to_numpy() / 1e6
        errors = int((group["code"] != 0).sum())
        row = {"run_id": run_id, "kind": kind, "count": len(group), "errors": errors,
               "error_pct": 100 * errors / len(group)}
        row.update({name: nearest_rank(latency, q) for name, q in QUANTILES.items()})
        rows.append(row)
    summary = pd.DataFrame(rows)
    return runs.merge(summary, on="run_id")


def bootstrap_median(values: np.ndarray, rng: np.random.Generator) -> tuple[float, float, float]:
    """Returns the median of values and its percentile bootstrap interval."""
    values = np.asarray(values, dtype=float)
    medians = np.median(rng.choice(values, size=(BOOTSTRAP, len(values))), axis=1)
    tail = (1 - LEVEL) / 2
    return float(np.median(values)), float(np.quantile(medians, tail)), float(np.quantile(medians, 1 - tail))


def measured_runs(summary: pd.DataFrame) -> pd.DataFrame:
    """Returns the runs of the matrix, without the drift runs."""
    return summary[summary["role"] == ""]


def cells(summary: pd.DataFrame) -> pd.DataFrame:
    """Returns a row per cell: the number of runs, and for every percentile
    the median over the runs with its interval, as NAME, NAME_lo, NAME_hi."""
    rows = []
    for key, group in measured_runs(summary).groupby(CELL, observed=True):
        row = dict(zip(CELL, key, strict=True))
        row["runs"] = len(group)
        row["error_pct"] = float(group["error_pct"].mean())
        for name in QUANTILES:
            row[name], row[name + "_lo"], row[name + "_hi"] = bootstrap_median(group[name].to_numpy(), rng_for(*key, name))
        rows.append(row)
    return pd.DataFrame(rows)


def holm(pvalues: np.ndarray) -> np.ndarray:
    """Returns the p-values adjusted by Holm's step-down method."""
    pvalues = np.asarray(pvalues, dtype=float)
    order = np.argsort(pvalues)
    adjusted = np.empty_like(pvalues)
    running = 0.0
    for rank, i in enumerate(order):
        running = max(running, (len(pvalues) - rank) * pvalues[i])
        adjusted[i] = min(running, 1.0)
    return adjusted


def ratio_interval(x: np.ndarray, y: np.ndarray, rng: np.random.Generator) -> tuple[float, float]:
    """Returns the bootstrap interval of median(x) / median(y), resampling
    the runs of either group on its own."""
    mx = np.median(rng.choice(x, size=(BOOTSTRAP, len(x))), axis=1)
    my = np.median(rng.choice(y, size=(BOOTSTRAP, len(y))), axis=1)
    tail = (1 - LEVEL) / 2
    ratios = mx / my
    return float(np.quantile(ratios, tail)), float(np.quantile(ratios, 1 - tail))


def comparisons(summary: pd.DataFrame, family: tuple[str, ...] = ("load", "kind", "other", "metric")) -> pd.DataFrame:
    """Compares the runs of every QUIC variant with those of every other
    variant in each cell and percentile: the effect, the median of QUIC
    against the other's, in percent with its interval (negative: QUIC is
    faster), the two-sided Mann-Whitney p-value and the p-value adjusted
    with Holm's method within each family of the given columns."""
    measured = measured_runs(summary)
    quic = sorted(measured.loc[measured["transport"] == "quic", "variant"].unique())
    others = sorted(measured.loc[measured["transport"] != "quic", "variant"].unique())
    rows = []
    for (network, load, kind), group in measured.groupby(["network", "load", "kind"], observed=True):
        for q in quic:
            for o in others:
                for metric in QUANTILES:
                    x = group.loc[group["variant"] == q, metric].to_numpy()
                    y = group.loc[group["variant"] == o, metric].to_numpy()
                    if len(x) == 0 or len(y) == 0:
                        continue
                    lo, hi = ratio_interval(x, y, rng_for(network, load, kind, q, o, metric))
                    test = stats.mannwhitneyu(x, y, alternative="two-sided")
                    rows.append({
                        "network": network, "load": load, "kind": kind, "quic": q, "other": o, "metric": metric,
                        "runs_quic": len(x), "runs_other": len(y),
                        "median_quic": float(np.median(x)), "median_other": float(np.median(y)),
                        "effect_pct": 100 * (np.median(x) / np.median(y) - 1),
                        "effect_lo_pct": 100 * (lo - 1), "effect_hi_pct": 100 * (hi - 1),
                        "u": float(test.statistic), "p": float(test.pvalue),
                    })
    table = pd.DataFrame(rows)
    if table.empty:
        return table
    table["p_holm"] = table.groupby(list(family))["p"].transform(lambda p: holm(p.to_numpy()))
    return table


def runs_needed(s_log: float, effect: float) -> int:
    """Returns how many runs per variant the Mann-Whitney test needs to tell
    apart two variants whose percentile differs by effect (0.1 is 10%), at
    ALPHA and POWER, if the logarithm of the percentile has the standard
    deviation s_log across runs: the normal approximation for two samples,
    divided by the efficiency of the test."""
    z = stats.norm.ppf(1 - ALPHA / 2) + stats.norm.ppf(POWER)
    return max(2, math.ceil(2 * (z * s_log / math.log1p(effect)) ** 2 / MW_EFFICIENCY))


def spread(summary: pd.DataFrame, metric: str = "p99") -> pd.DataFrame:
    """Returns, per cell, how much the percentile varies across runs, as the
    coefficient of variation and the standard deviation of its logarithm,
    and the runs needed to tell apart differences of EFFECTS."""
    rows = []
    for key, group in measured_runs(summary).groupby(CELL, observed=True):
        values = group[metric].to_numpy()
        several = len(values) > 1
        s_log = float(np.std(np.log(values), ddof=1)) if several else float("nan")
        cv = float(np.std(values, ddof=1) / np.mean(values)) if several else float("nan")
        row = dict(zip(CELL, key, strict=True))
        row.update({"metric": metric, "runs": len(values), "cv": cv, "s_log": s_log})
        for effect in EFFECTS:
            row[f"runs_for_{round(effect * 100)}pct"] = runs_needed(s_log, effect) if not math.isnan(s_log) else None
        rows.append(row)
    return pd.DataFrame(rows)


def warmup(runs: pd.DataFrame, calls: pd.DataFrame, head_s: float = 5.0) -> pd.DataFrame:
    """Returns, per run and kind, the median latency of the first head_s of
    the measurement against that of the rest, as a ratio: well above 1
    means the warm-up was too short."""
    start = runs.set_index("run_id")["warmup_ns"]
    measured = calls[calls["phase"] == "measure"]
    rows = []
    for (run_id, kind), group in measured.groupby(["run_id", "kind"], observed=True):
        since = group["scheduled_ns"].to_numpy() - start[run_id]
        latency = group["latency_ns"].to_numpy()
        head, rest = latency[since < head_s * 1e9], latency[since >= head_s * 1e9]
        if len(head) and len(rest):
            rows.append({"run_id": run_id, "kind": kind, "head_ratio": float(np.median(head) / np.median(rest))})
    return runs.merge(pd.DataFrame(rows), on="run_id")


def drift(summary: pd.DataFrame) -> pd.DataFrame:
    """Returns, per variant, load and kind, the percentiles of the drift runs
    at the start and the end of the series and their change in percent."""
    start = summary[summary["role"] == "drift-start"].set_index(["variant", "load", "kind"])
    end = summary[summary["role"] == "drift-end"].set_index(["variant", "load", "kind"])
    both = start.index.intersection(end.index)
    rows = []
    for key in both:
        row = dict(zip(["variant", "load", "kind"], key, strict=True))
        for name in QUANTILES:
            a, b = float(start.loc[key, name]), float(end.loc[key, name])
            row.update({f"{name}_start": a, f"{name}_end": b, f"{name}_change_pct": 100 * (b / a - 1)})
        rows.append(row)
    return pd.DataFrame(rows)
