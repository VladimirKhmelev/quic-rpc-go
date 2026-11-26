"""Decisions pre-registered in docs/hypotheses.md and the profile recommendation."""

from dataclasses import asdict, dataclass
from itertools import product
import math

import numpy as np
import pandas as pd
from scipy.stats import mannwhitneyu

from . import stats
from .main_data import CPU
from .main_design import (
    Cell, H2, INTENSITIES, LIMITED_PAIR, LOSS_NAMES, MATRICES, PAIR, POOL,
    QUIC, RTTS, SIZES, TCP_DEFAULT, grid_cell, profile_cells,
)

SUPPORT, OPPOSE, UNKNOWN, EXCLUDED = "support", "oppose", "inconclusive", "excluded"
PAIRED_DRAWS = 100_000
CPU_LIMIT = 1.5
ADVANTAGE_LIMIT = 0.8
EQUIVALENCE_LIMITS = (0.95, 1.05)


class Samples:
    """Keeps comparisons scoped to one series, cell and kind of operation."""

    def __init__(self, summaries: pd.DataFrame):
        self.groups = {} if summaries.empty else {
            (Cell(series, network, load, kind), variant): frame.sort_values("repeat")
            for (series, network, load, kind, variant), frame in stats.measured_runs(summaries).groupby(
                ["series", "network", "load", "kind", "variant"], observed=True
            )
        }

    def cell(self, cell: Cell, variants: tuple[str, ...]) -> tuple[pd.DataFrame, ...]:
        return tuple(self.groups.get((cell, v), pd.DataFrame()) for v in variants)


def full(groups: tuple[pd.DataFrame, ...], repeats: int) -> bool:
    return all(len(group) == repeats and np.array_equal(group["repeat"].to_numpy(), np.arange(1, repeats + 1))
               for group in groups)


def counts(groups: tuple[pd.DataFrame, ...], variants: tuple[str, ...]) -> str:
    return " / ".join(f"{variant}={len(group)}" for group, variant in zip(groups, variants, strict=True))


@dataclass(frozen=True)
class Pair:
    median_quic: float = math.nan
    median_other: float = math.nan
    ratio: float = math.nan
    ratio_lo: float = math.nan
    ratio_hi: float = math.nan
    p: float = 1.0


def compare(groups: tuple[pd.DataFrame, ...], metric: str, seed: tuple) -> Pair:
    """Comparisons of two variants draw runs independently within each variant."""
    x, y = (group[metric].to_numpy(dtype=float) for group in groups)
    if not np.isfinite(x).all() or not np.isfinite(y).all() or (x < 0).any() or (y < 0).any():
        return Pair()
    a, b = float(np.median(x)), float(np.median(y))
    ratio = a / b if b > 0 else math.nan
    lo, hi = stats.ratio_interval(x, y, stats.rng_for(*seed)) if (y > 0).all() else (math.nan, math.nan)
    p = float(mannwhitneyu(x, y, alternative="two-sided").pvalue)
    return Pair(a, b, ratio, lo, hi, p)


def latency_decision(pair: Pair, p_holm: float, regime: str, equivalence: bool = False) -> tuple[str, str]:
    if regime == "missing":
        return UNKNOWN, "missing-repeats"
    if regime == "both-overloaded":
        return EXCLUDED, regime
    if regime != "steady":
        return (SUPPORT if not equivalence and regime == "other-overloaded" else OPPOSE), regime
    lo, hi = pair.ratio_lo, pair.ratio_hi
    if not (math.isfinite(lo) and math.isfinite(hi)):
        return UNKNOWN, "ratio-unavailable"
    if equivalence:
        lower, upper = EQUIVALENCE_LIMITS
        if lo >= lower and hi <= upper:
            return SUPPORT, "equivalent"
        if p_holm < stats.ALPHA and (hi < lower or lo > upper):
            return OPPOSE, "different"
    else:
        if p_holm < stats.ALPHA and hi < ADVANTAGE_LIMIT:
            return SUPPORT, "advantage"
        if lo > ADVANTAGE_LIMIT:
            return OPPOSE, "no-practical-advantage"
    return UNKNOWN, "interval-or-significance"


@dataclass(frozen=True)
class Comparison:
    family: str
    cell: Cell
    metric: str = "p99"
    equivalence: bool = False
    variants: tuple[str, str] = PAIR


def pair_families() -> list[Comparison]:
    return [
        *(Comparison("H1", Cell("loss-axis", n, load)) for n, load in product(LOSS_NAMES[2:], ("small", "small-heavy"))),
        *(Comparison("H2", Cell("loss-axis", n, "small-heavy")) for n in LOSS_NAMES),
        *(Comparison("H4", c, metric, equivalence=True) for c, metric in product(
            (Cell("loss-axis", "base", "small"), Cell("datacenter", "datacenter", "small")), ("p50", "p99"))),
        *(Comparison("H6a", grid_cell(rtt, 0, "small"), equivalence=True) for rtt in RTTS),
        *(Comparison("H6a", Cell("sweep-intensity", "base", load), equivalence=True) for load in INTENSITIES),
        *(Comparison("H6b", grid_cell(rtt, i, "small")) for rtt, i in product(RTTS, range(1, len(LOSS_NAMES)))),
        *(Comparison("H6c", Cell("sweep-response-size", "base", load), variants=LIMITED_PAIR) for load in SIZES),
    ]


def evaluate_pairs(samples: Samples, specs: list[Comparison]) -> list[dict]:
    rows = []
    for spec in specs:
        cell = spec.cell
        groups = samples.cell(cell, spec.variants)
        repeats = MATRICES[cell.series].repeats
        complete = full(groups, repeats)
        regime = stats.REGIMES[tuple(stats.overloaded(g) for g in groups)] if complete else "missing"
        pair = compare(groups, spec.metric, (cell, spec.metric, spec.variants)) if complete else Pair()
        rows.append({"family": spec.family, **asdict(cell), "metric": spec.metric, "quic": spec.variants[0], "other": spec.variants[1],
                     "expected": repeats, "observed": counts(groups, spec.variants), "regime": regime, **asdict(pair)})
    for family in dict.fromkeys(spec.family for spec in specs):
        members = [i for i, spec in enumerate(specs) if spec.family == family]
        adjusted = stats.holm(np.asarray([rows[i]["p"] for i in members]))
        for i, p in zip(members, adjusted, strict=True):
            row = rows[i]
            row["p_holm"] = float(p)
            row["outcome"], row["reason"] = latency_decision(
                Pair(**{k: row[k] for k in Pair.__dataclass_fields__}), p, row["regime"], specs[i].equivalence
            )
    return rows


def contrasts(values: np.ndarray, family: str) -> dict[str, np.ndarray]:
    q, h, other = values.T
    if family == "H3":
        gap = h - q
        return {"G": gap, "S": gap / 2 - (other - q)}
    # Apply the common denominator after subtraction so equal distances on
    # opposite sides of H yield zero at the strict decision boundary.
    return {"D": (np.abs(q - h) - np.abs(other - h)) / h}


def paired_intervals(triples: np.ndarray, cell: Cell, family: str) -> dict:
    """Resamples joint rows of QUIC, aligned gRPC and the third variant."""
    rng = stats.rng_for("paired", family, cell)
    m = 16 if family == "H3" else 6
    draws = rng.integers(len(triples), size=(PAIRED_DRAWS, len(triples)))
    medians = np.median(triples[draws], axis=1)
    points = contrasts(np.median(triples, axis=0), family)
    out = {}
    for contrast, values in contrasts(medians, family).items():
        a, b, c, d = np.quantile(values, [0.025, 0.975, stats.ALPHA / (2 * m), 1 - stats.ALPHA / (2 * m)])
        out[contrast] = {"estimate": float(points[contrast]), "lo": a, "hi": b, "adjusted_lo": c, "adjusted_hi": d}
    return out


def triple_decision(family: str, overloaded: tuple[bool, bool, bool], intervals: dict) -> tuple[str, str]:
    q, h, other = overloaded
    if family == "H3" and any(overloaded):
        return EXCLUDED, "overload"
    if family == "H5" and any(overloaded):
        if all(overloaded):
            return EXCLUDED, "all-overloaded"
        if h != other:
            return OPPOSE, "tcp-settings-change-overload"
        if h and other:
            return SUPPORT, "both-grpc-overloaded"
        return UNKNOWN, "quic-overloaded"
    if not intervals:
        return UNKNOWN, "contrast-unavailable"
    if family == "H3":
        if intervals["G"]["adjusted_lo"] <= 0:
            return UNKNOWN, "initial-gap-unconfirmed"
        measurement = intervals["S"]
        if measurement["adjusted_lo"] >= 0:
            return SUPPORT, "gap-at-least-halved"
        if measurement["adjusted_hi"] < 0:
            return OPPOSE, "gap-not-halved"
    else:
        measurement = intervals["D"]
        if measurement["adjusted_lo"] > 0:
            return SUPPORT, "tcp-effect-smaller"
        if measurement["adjusted_hi"] <= 0:
            return OPPOSE, "tcp-effect-not-smaller"
    return UNKNOWN, "interval-crosses-threshold"


def evaluate_triples(samples: Samples) -> tuple[list[dict], list[dict]]:
    rows, intervals = [], []
    for family, networks, variants in (("H3", LOSS_NAMES[1:], (*PAIR, POOL)), ("H5", LOSS_NAMES[2:], (*PAIR, TCP_DEFAULT))):
        for network, load in product(networks, ("small", "small-heavy")):
            cell = Cell("loss-axis", network, load)
            groups = samples.cell(cell, variants)
            repeats = MATRICES[cell.series].repeats
            sampled = {}
            if full(groups, repeats):
                overload = tuple(stats.overloaded(g) for g in groups)
                if not any(overload):
                    values = np.column_stack([g["p99"].to_numpy(dtype=float) for g in groups])
                    if np.isfinite(values).all() and (values > 0).all():
                        sampled = paired_intervals(values, cell, family)
                outcome, reason = triple_decision(family, overload, sampled)
            else:
                overload = (None, None, None)
                outcome, reason = UNKNOWN, "missing-repeats"
            condition = {"family": family, **asdict(cell), "other_control": variants[2], "expected": repeats,
                         "observed": counts(groups, variants), "quic_overloaded": overload[0],
                         "h2_overloaded": overload[1], "control_overloaded": overload[2], "outcome": outcome, "reason": reason}
            rows.append({**condition, "metric": "p99"})
            for key in (("G", "S") if family == "H3" else ("D",)):
                intervals.append({**condition, "contrast": key, "unit": "ms" if family == "H3" else "ratio-difference",
                                  **sampled.get(key, dict.fromkeys(("estimate", "lo", "hi", "adjusted_lo", "adjusted_hi"), math.nan))})
    return rows, intervals


def cpu_checks(groups: tuple[pd.DataFrame, ...], complete: bool) -> list[dict]:
    rows = []
    for key in CPU:
        q, h = (float(group[key].median()) for group in groups) if complete else (math.nan, math.nan)
        if not (math.isfinite(q) and math.isfinite(h)) or q < 0 or h < 0 or (q == 0 and h == 0):
            outcome = UNKNOWN
        else:
            outcome = SUPPORT if q <= CPU_LIMIT * h else OPPOSE
        rows.append({"metric": key, "quic_us": q, "h2_us": h, "ratio": q / h if h > 0 else math.nan,
                     "outcome": outcome, "reason": {SUPPORT: "cpu-within-limit", OPPOSE: "cpu-over-limit", UNKNOWN: "cpu-unavailable"}[outcome]})
    return rows


def h4_cpu(samples: Samples) -> list[dict]:
    rows = []
    for cell in (Cell("loss-axis", "base", "small"), Cell("datacenter", "datacenter", "small")):
        groups = samples.cell(cell, PAIR)
        repeats = MATRICES[cell.series].repeats
        rows.extend({"family": "H4", **asdict(cell), "expected": repeats, "observed": counts(groups, PAIR), **row}
                    for row in cpu_checks(groups, full(groups, repeats)))
    return rows


def verdict(outcomes: list[str]) -> str:
    if OPPOSE in outcomes:
        return "rejected"
    if SUPPORT in outcomes and UNKNOWN not in outcomes:
        return "accepted"
    return "insufficient"


def hypotheses(samples: Samples) -> dict[str, pd.DataFrame]:
    pair_rows = evaluate_pairs(samples, pair_families())
    triple_rows, contrast_rows = evaluate_triples(samples)
    cpu_rows = h4_cpu(samples)
    all_rows = pair_rows + triple_rows + cpu_rows
    decisions = []
    for key in ("H1", "H2", "H3", "H4", "H5", "H6", "H6a", "H6b", "H6c"):
        outcomes = [row["outcome"] for row in all_rows if row["family"] == key or (key == "H6" and row["family"].startswith("H6"))]
        result = verdict(outcomes)
        if key == "H4" and result == "accepted" and not any(r["family"] == "H4" and r["outcome"] == SUPPORT for r in pair_rows):
            result = "insufficient"
        decisions.append({"hypothesis": key, "verdict": result, **{v: outcomes.count(v) for v in (SUPPORT, OPPOSE, UNKNOWN, EXCLUDED)}})
    return {"hypotheses": pd.DataFrame(decisions), "hypothesis_cells": pd.DataFrame(pair_rows + triple_rows),
            "hypothesis_contrasts": pd.DataFrame(contrast_rows), "hypothesis_cpu": pd.DataFrame(cpu_rows)}


def recommendation(row: dict) -> tuple[str, str]:
    if row["regime"] == "missing":
        return "insufficient", "missing-repeats"
    if row["regime"] == "both-overloaded":
        q, h = (row[f"goodput_median_{v}"] for v in ("quic", "other"))
        if not (math.isfinite(q) and math.isfinite(h)):
            return "insufficient", "goodput-unavailable"
        if row["goodput_p_holm"] < stats.ALPHA:
            if q > 1.2 * h:
                return QUIC, "higher-goodput"
            if h > 1.2 * q:
                return H2, "higher-goodput"
        return "no-difference", "goodput-rule-no-winner"
    if row["cpu"] == OPPOSE:
        return H2, "cpu-over-limit"
    if row["p50_equivalence"] == row["p99_equivalence"] == SUPPORT:
        return "no-difference", "latencies-equivalent"
    if row["p99_advantage"] == OPPOSE:
        return H2, "no-practical-advantage"
    if row["p99_advantage"] == row["cpu"] == SUPPORT:
        return QUIC, "advantage-within-cpu-limit"
    return "insufficient", "latency-or-cpu-inconclusive"


def recommendations(samples: Samples) -> pd.DataFrame:
    rows, comparisons = [], []
    metrics = ("p50", "p99", "goodput")
    for cell in profile_cells():
        groups = samples.cell(cell, PAIR)
        repeats = MATRICES[cell.series].repeats
        complete = full(groups, repeats)
        regime = stats.REGIMES[tuple(stats.overloaded(g) for g in groups)] if complete else "missing"
        cpu = cpu_checks(groups, complete)
        cpu_statuses = {item["outcome"] for item in cpu}
        cpu_status = OPPOSE if OPPOSE in cpu_statuses else SUPPORT if cpu_statuses == {SUPPORT} else UNKNOWN
        pairs = {m: compare(groups, "goodput_mbps" if m == "goodput" else m, (cell, m, PAIR)) if complete else Pair()
                 for m in metrics}
        comparisons.append(pairs)
        rows.append({**asdict(cell), "expected": repeats, "observed": counts(groups, PAIR), "regime": regime, "cpu": cpu_status,
                     **{f"cpu_ratio_{item['metric']}": item["ratio"] for item in cpu},
                     **{f"{m}_{k}": v for m in metrics for k, v in asdict(pairs[m]).items()}})
    for metric in metrics:
        adjusted = stats.holm(np.asarray([p[metric].p for p in comparisons]))
        for row, p in zip(rows, adjusted, strict=True):
            row[f"{metric}_p_holm"] = p
    for row, pairs in zip(rows, comparisons, strict=True):
        for metric in ("p50", "p99"):
            row[f"{metric}_equivalence"], _ = latency_decision(pairs[metric], row[f"{metric}_p_holm"], row["regime"], True)
        row["p99_advantage"], _ = latency_decision(pairs["p99"], row["p99_p_holm"], row["regime"])
        row["decision"], row["reason"] = recommendation(row)
        row["preferred"] = H2 if row["decision"] == "no-difference" else row["decision"] if row["decision"] in PAIR else ""
    return pd.DataFrame(rows)
