"""Descriptive tables of the main experiment, with series kept in every key."""

from dataclasses import asdict

import numpy as np
import pandas as pd

from . import bulk, inference, stats
from .main_data import CPU, MainData
from .main_design import Cell, LOSSES, MATRICES, PAIR, RTTS, grid_cell

CELL = ["series", "network", "load", "kind", "variant"]


def estimate(values: pd.Series, key: tuple) -> dict:
    sample = values.dropna().to_numpy(dtype=float)
    if len(sample) >= 2:
        median, lo, hi = stats.bootstrap_median(np.sort(sample), stats.rng_for("main-table", *key))
    else:
        median, lo, hi = (float(sample[0]) if len(sample) else np.nan), np.nan, np.nan
    return {"median": median, "lo": lo, "hi": hi, "samples": len(sample)}


def latency_rows(summaries: pd.DataFrame) -> pd.DataFrame:
    columns = [*CELL, "metric", "runs", "expected", "overloaded", "overloaded_runs", "errors", "error_pct", "median", "lo", "hi", "samples", "rtt_ms"]
    rows = []
    if not summaries.empty:
        for key, g in stats.measured_runs(summaries).groupby(CELL, observed=True):
            row = {**dict(zip(CELL, key, strict=True)), "runs": len(g), "expected": MATRICES[key[0]].repeats,
                   "overloaded": stats.overloaded(g), "overloaded_runs": int(g["overloaded"].sum()), "errors": int(g["errors"].sum()),
                   "error_pct": float(g["error_pct"].mean()), "rtt_ms": g["rtt_ms"].iloc[0]}
            for metric in stats.QUANTILES:
                rows.append({**row, "metric": metric, **estimate(g[metric], (*key, metric))})
    return pd.DataFrame(rows, columns=columns)


def cpu_rows(summaries: pd.DataFrame) -> pd.DataFrame:
    columns = ["series", "network", "load", "variant", "metric", "runs", "expected", "median", "lo", "hi", "samples"]
    rows = []
    if not summaries.empty:
        keys = ["series", "network", "load", "variant"]
        measured = stats.measured_runs(summaries).drop_duplicates(["series", "run_id"])
        for key, group in measured.groupby(keys, observed=True):
            for metric in CPU:
                rows.append({**dict(zip(keys, key, strict=True)), "metric": metric, "runs": len(group),
                             "expected": MATRICES[key[0]].repeats, **estimate(group[metric], (*key, metric))})
    return pd.DataFrame(rows, columns=columns)


def ratio_row(samples: inference.Samples, cell: Cell) -> dict:
    groups = samples.cell(cell, PAIR)
    repeats = MATRICES[cell.series].repeats
    complete = inference.full(groups, repeats)
    regime = stats.REGIMES[tuple(stats.overloaded(g) for g in groups)] if complete else "missing"
    pair = inference.compare(groups, "p99", (cell, "p99", PAIR)) if regime == "steady" else inference.Pair()
    return {**asdict(cell), "runs": min(map(len, groups)), "expected": repeats, "regime": regime,
            "median": pair.ratio, "lo": pair.ratio_lo, "hi": pair.ratio_hi}


def tables(main: MainData) -> dict[str, pd.DataFrame]:
    samples = inference.Samples(main.summaries)
    latency = latency_rows(main.summaries)
    connect = latency[latency["kind"] == "connect"].copy()
    for col in ("median", "lo", "hi"):
        connect[col + "_rtt"] = connect[col] / connect["rtt_ms"].where(connect["rtt_ms"] > 0)
    grid = [{"rtt_ms": rtt, "loss_pct": loss, **ratio_row(samples, grid_cell(rtt, i, load))}
            for rtt in RTTS for i, loss in enumerate(LOSSES) for load in ("small", "small-heavy")]
    reorder = [{"reorder_pct": p, **ratio_row(samples, Cell("one-factor" if p else "loss-axis", f"reorder-{p}" if p else "base", load))}
               for p in (0, 1, 5, 10) for load in ("small", "small-heavy")]
    sections = {"coverage": main.coverage, "missing_runs": main.missing, "runs": main.runs, "run_metrics": main.summaries,
                "latency": latency, "cpu": cpu_rows(main.summaries), "connections": connect,
                "grid": pd.DataFrame(grid), "reorder": pd.DataFrame(reorder),
                "transport_recommendations": inference.recommendations(samples), **inference.hypotheses(samples)}
    if not main.summaries.empty:
        sections["warmup"] = main.summaries[[*CELL, "run_id", "role", "head_ratio"]]
        sections["drift"] = pd.concat([stats.drift(g).assign(series=series) for series, g in main.summaries.groupby("series")],
                                      ignore_index=True)
    if not main.runs.empty and (main.runs["bulk_total_bytes"] > 0).any():
        sections["bulk_runs"], sections["bulk"] = bulk.tables(main.runs)
    return sections
