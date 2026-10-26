import math

import numpy as np
import pandas as pd
import pytest
from qrpcplots import stats


def go_percentile(values: list[float], q: float) -> float:
    """loadgen.Percentile of the load generator."""
    s = sorted(values)
    rank = math.ceil(q * len(s)) - 1
    return s[min(max(rank, 0), len(s) - 1)]


@pytest.mark.parametrize("values", [[5.0], [3.0, 1.0], list(range(1, 11)), list(np.random.default_rng(3).exponential(size=101))])
@pytest.mark.parametrize("q", [0.01, 0.5, 0.95, 0.99, 1.0])
def test_nearest_rank_is_that_of_the_generator(values, q):
    assert stats.nearest_rank(np.array(values, dtype=float), q) == go_percentile(values, q)


def test_bootstrap_median():
    values = np.array([10.0, 12, 11, 30, 9, 10, 11])
    a = stats.bootstrap_median(values, stats.rng_for("cell"))
    assert a == stats.bootstrap_median(values, stats.rng_for("cell"))
    median, lo, hi = a
    assert median == 11 and lo <= median <= hi and lo >= 9 and hi <= 30


def test_bootstrap_median_is_of_medians():
    # An outlier moves the mean of a resample, not its median.
    _, lo, hi = stats.bootstrap_median(np.array([10.0] * 9 + [1000.0]), stats.rng_for("outlier"))
    assert lo == hi == 10


def test_run_summary():
    runs = pd.DataFrame([{"run_id": "r", "variant": "quic"}])
    calls = pd.DataFrame({
        "run_id": "r", "kind": ["small"] * 5 + ["heavy"],
        "latency_ns": [1_000_000, 2_000_000, 3_000_000, 10_000_000_000, 99_000_000_000, 5_000_000],
        "code": [0, 0, 0, 4, 0, 0],
        "phase": ["measure", "measure", "measure", "measure", "warmup", "measure"],
    })
    summary = stats.run_summary(runs, calls).set_index("kind")
    small = summary.loc["small"]
    assert small["variant"] == "quic" and small["count"] == 4 and small["errors"] == 1 and small["error_pct"] == 25
    # The warm-up call is out; the failed one counts with its latency.
    assert small["p50"] == 2 and small["p99"] == 10_000
    assert summary.loc["heavy", "p99"] == 5


def test_holm():
    adjusted = stats.holm(np.array([0.01, 0.04, 0.03, 0.005]))
    assert np.allclose(adjusted, [0.03, 0.06, 0.06, 0.02])
    assert np.allclose(stats.holm(np.array([0.5, 0.9])), [1.0, 1.0])


def test_runs_needed():
    assert stats.runs_needed(0.1, 0.1) == 19
    assert stats.runs_needed(0.1, 0.05) > stats.runs_needed(0.1, 0.1) > stats.runs_needed(0.1, 0.2)
    assert stats.runs_needed(0.0001, 0.2) == 2


def summary_of(groups: dict[tuple[str, str, str], list[float]]) -> pd.DataFrame:
    """A run summary with a run per value: (variant, transport, network) to
    the p99 of its runs; p50 and p95 follow p99."""
    rows = []
    for (variant, transport, network), values in groups.items():
        for i, v in enumerate(values):
            rows.append({"run_id": f"{variant}-{network}-{i}", "variant": variant, "transport": transport, "network": network,
                         "load": "small", "kind": "small", "role": "", "p50": v / 2, "p95": v * 0.9, "p99": v, "error_pct": 0.0})
    return pd.DataFrame(rows)


def test_comparisons():
    summary = summary_of({
        ("quic", "quic", "loss-2"): [10, 11, 12, 10, 11],
        ("grpc", "grpc-h2", "loss-2"): [20, 22, 21, 23, 20],
        ("quic", "quic", "base"): [5, 5.1, 4.9, 5, 5],
        ("grpc", "grpc-h2", "base"): [5, 5.2, 4.8, 5.1, 4.9],
        ("quic", "quic", "rtt-150"): [20, 22, 21, 23, 20],
        ("grpc", "grpc-h2", "rtt-150"): [10, 11, 12, 10, 11],
    })
    table = stats.comparisons(summary).set_index(["network", "metric"])
    lossy = table.loc[("loss-2", "p99")]
    assert lossy["effect_pct"] == pytest.approx(100 * (11 / 21 - 1))
    assert lossy["effect_lo_pct"] < lossy["effect_pct"] < lossy["effect_hi_pct"] < 0
    assert lossy["p"] < 0.05 and lossy["p_holm"] >= lossy["p"]
    clean = table.loc[("base", "p99")]
    assert clean["p"] > 0.5
    # The test is two-sided: QUIC slower is as significant as faster.
    slower = table.loc[("rtt-150", "p99")]
    assert slower["effect_pct"] > 50 and slower["p"] == pytest.approx(lossy["p"])
    # Holm over the three networks of the family: the two equal smallest p
    # both become 3p, since the step-down keeps the order, and the largest
    # stays.
    assert sorted(table.xs("p99", level="metric")["p_holm"]) == pytest.approx([3 * lossy["p"], 3 * lossy["p"], clean["p"]])


def test_spread():
    summary = summary_of({("quic", "quic", "base"): [10, 12, 8, 10]})
    row = stats.spread(summary).iloc[0]
    assert row["runs"] == 4
    assert row["cv"] == pytest.approx(np.std([10, 12, 8, 10], ddof=1) / 10)
    assert row["s_log"] == pytest.approx(np.std(np.log([10, 12, 8, 10]), ddof=1))
    assert row["runs_for_10pct"] == stats.runs_needed(row["s_log"], 0.1)


def test_warmup():
    # Run slow is slow for the first 5 s of the measurement, run brief for
    # its first second only: the median of the first 5 s does not see it.
    runs = pd.DataFrame([{"run_id": "slow", "warmup_ns": 1_000_000_000}, {"run_id": "brief", "warmup_ns": 1_000_000_000}])
    scheduled = np.arange(0, 11_000_000_000, 100_000_000)
    frames = []
    for run, slow_until in (("slow", 6_000_000_000), ("brief", 2_000_000_000)):
        frames.append(pd.DataFrame({"run_id": run, "kind": "small", "scheduled_ns": scheduled,
                                    "latency_ns": np.where(scheduled < slow_until, 20_000_000, 10_000_000),
                                    "phase": np.where(scheduled < 1_000_000_000, "warmup", "measure")}))
    ratios = stats.warmup(runs, pd.concat(frames)).set_index("run_id")["head_ratio"]
    assert ratios["slow"] == pytest.approx(2.0) and ratios["brief"] == pytest.approx(1.0)


def test_drift():
    summary = pd.DataFrame([
        {"variant": "quic", "load": "small", "kind": "small", "role": role, "p50": p, "p95": p, "p99": 2 * p}
        for role, p in (("drift-start", 10.0), ("drift-end", 11.0), ("", 50.0))
    ])
    row = stats.drift(summary).iloc[0]
    assert row["p50_start"] == 10 and row["p50_end"] == 11
    assert row["p99_change_pct"] == pytest.approx(10.0)
