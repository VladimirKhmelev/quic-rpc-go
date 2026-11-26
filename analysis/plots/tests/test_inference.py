"""Verdicts on pre-registered families, including boundaries and missing runs."""

from itertools import product

import numpy as np
import pandas as pd
import pytest

from qrpcplots import inference as inf
from qrpcplots.main_data import CPU
from qrpcplots.main_design import Cell, H2, LIMITED_PAIR, LOSS_NAMES, MATRICES, PAIR, POOL, QUIC, TCP_DEFAULT


def observations(cell, variant, latency=100, overloaded=False, cpu=2, goodput=3):
    return [{"series": cell.series, "network": cell.network, "load": cell.load, "kind": cell.kind, "variant": variant,
             "repeat": r, "role": "", "p50": latency, "p99": latency, "overloaded": overloaded, "goodput_mbps": goodput,
             **{key: cpu for key in CPU}} for r in range(1, MATRICES[cell.series].repeats + 1)]


@pytest.mark.parametrize("lo,hi,p,equivalence,result", [
    (.71, .79, .049, False, inf.SUPPORT), (.71, .8, .049, False, inf.UNKNOWN),
    (.8, .81, .049, False, inf.UNKNOWN), (.801, .82, .9, False, inf.OPPOSE),
    (.71, .79, .05, False, inf.UNKNOWN), (.95, 1.05, 1, True, inf.SUPPORT),
    (.9, .949, .049, True, inf.OPPOSE), (1.051, 1.1, .049, True, inf.OPPOSE),
    (.9, .95, .049, True, inf.UNKNOWN), (.9, .949, .05, True, inf.UNKNOWN),
])
def test_latency_boundaries(lo, hi, p, equivalence, result):
    assert inf.latency_decision(inf.Pair(ratio_lo=lo, ratio_hi=hi), p, "steady", equivalence)[0] == result


@pytest.mark.parametrize("regime,advantage,equivalence", [
    ("missing", inf.UNKNOWN, inf.UNKNOWN), ("both-overloaded", inf.EXCLUDED, inf.EXCLUDED),
    ("other-overloaded", inf.SUPPORT, inf.OPPOSE), ("quic-overloaded", inf.OPPOSE, inf.OPPOSE),
])
def test_latency_regime(regime, advantage, equivalence):
    assert inf.latency_decision(inf.Pair(), 1, regime)[0] == advantage
    assert inf.latency_decision(inf.Pair(), 1, regime, True)[0] == equivalence


def test_missing_comparisons_keep_holm_denominator(monkeypatch):
    h1 = [s for s in inf.pair_families() if s.family == "H1"]
    cell = h1[0].cell
    samples = inf.Samples(pd.DataFrame(observations(cell, QUIC) + observations(cell, H2)))
    monkeypatch.setattr(inf, "compare", lambda *_: inf.Pair(ratio_lo=.7, ratio_hi=.75, p=.01))
    rows = inf.evaluate_pairs(samples, h1)
    assert rows[0]["p_holm"] == pytest.approx(.06) and rows[0]["outcome"] == inf.UNKNOWN
    assert all(r["p"] == 1 and r["reason"] == "missing-repeats" for r in rows[1:])


def test_overloaded_comparison_remains_in_holm_family(monkeypatch):
    source = [s for s in inf.pair_families() if s.family == "H1"][:2]
    rows = []
    for case, overloaded in zip(source, (False, True), strict=True):
        rows.extend(observations(case.cell, QUIC) + observations(case.cell, H2, overloaded=overloaded))
    monkeypatch.setattr(inf, "compare", lambda *_: inf.Pair(ratio_lo=.7, ratio_hi=.75, p=.03))
    a, b = inf.evaluate_pairs(inf.Samples(pd.DataFrame(rows)), source)
    assert a["p_holm"] == pytest.approx(.06) and a["outcome"] == inf.UNKNOWN
    assert b["p_holm"] == pytest.approx(.06) and b["outcome"] == inf.SUPPORT


def test_bootstrap_resamples_joint_triples_and_handles_halving_boundary():
    q = np.arange(1.0, 26.0) * 64
    cell = Cell("loss-axis", "loss-1", "small")
    rows = np.column_stack([q, q + 20, q + 10])
    intervals = inf.paired_intervals(rows, cell, "H3")
    assert list(intervals["G"].values()) == [20] * 5
    assert list(intervals["S"].values()) == [0] * 5
    assert inf.triple_decision("H3", (False, False, False), intervals)[0] == inf.SUPPORT
    assert inf.paired_intervals(rows, cell, "H3") == intervals


def test_h5_uses_configured_family_and_requires_strictly_smaller(monkeypatch):
    monkeypatch.setattr(inf, "PAIRED_DRAWS", 2000)
    cell = Cell("loss-axis", "loss-1", "small")
    scale = np.arange(1.0, 26.0)
    equal = np.column_stack([scale, 2 * scale, 3 * scale])
    intervals = inf.paired_intervals(equal, cell, "H5")
    assert list(intervals["D"].values()) == [0] * 5
    assert inf.triple_decision("H5", (False, False, False), intervals)[0] == inf.OPPOSE
    mirrored = np.column_stack([3 * scale, 5 * scale, 7 * scale])
    assert list(inf.paired_intervals(mirrored, cell, "H5")["D"].values()) == [0] * 5
    mixed = np.column_stack([scale, 2 * scale, scale * np.linspace(1.1, 2.99, len(scale))])
    d = inf.paired_intervals(mixed, cell, "H5")["D"]
    assert d["adjusted_lo"] <= d["lo"] < d["hi"] <= d["adjusted_hi"]
    assert (d["adjusted_lo"], d["adjusted_hi"]) != (d["lo"], d["hi"])


@pytest.mark.parametrize("flags,want", [
    ((True, True, True), inf.EXCLUDED), ((False, True, True), inf.SUPPORT),
    ((False, True, False), inf.OPPOSE), ((True, False, True), inf.OPPOSE),
    ((True, False, False), inf.UNKNOWN),
])
def test_h5_overload_flags(flags, want):
    assert inf.triple_decision("H5", flags, {})[0] == want
    assert inf.triple_decision("H3", flags, {})[0] == inf.EXCLUDED


def test_all_hypotheses_and_sweep_scope(monkeypatch):
    monkeypatch.setattr(inf, "PAIRED_DRAWS", 1000)
    pairs = inf.pair_families()
    cells = {(s.cell, variant) for s in pairs for variant in s.variants}
    cells |= {(Cell("loss-axis", n, l), v) for n, l, v in product(LOSS_NAMES[1:], ("small", "small-heavy"), (POOL, TCP_DEFAULT))}
    observations_all = []
    for cell, variant in sorted(cells, key=str):
        if variant == QUIC or variant == LIMITED_PAIR[0]:
            value = 100 if cell.network in ("base", "datacenter", "rtt-20", "rtt-150") and cell.load in ("small", "small-50", "small-1000") else 60
        else:
            value = 79 if variant == POOL else 102 if variant == TCP_DEFAULT else 100
        observations_all.extend(observations(cell, variant, value, cpu=3 if variant == QUIC else 2))
    frame = pd.DataFrame(observations_all).sample(frac=1, random_state=2)
    out = inf.hypotheses(inf.Samples(frame))
    assert set(out["hypotheses"]["verdict"]) == {"accepted"}
    assert out["hypothesis_cells"]["family"].value_counts().to_dict() == {
        "H1": 6, "H2": 5, "H3": 8, "H4": 4, "H5": 6, "H6a": 6, "H6b": 12, "H6c": 3,
    }
    assert len(out["hypothesis_contrasts"]) == 22
    assert len(out["hypothesis_cpu"]) == 8
    drop = (frame["series"] == "sweep-intensity") & (frame["load"] == "small") & (frame["variant"] == QUIC) & (frame["repeat"] == 1)
    changed = inf.hypotheses(inf.Samples(frame.assign(role=np.where(drop, "drift-start", ""))))["hypotheses"].set_index("hypothesis")
    assert changed.loc["H6a", "verdict"] == changed.loc["H6", "verdict"] == "insufficient"
    assert changed.loc["H1", "verdict"] == "accepted"


def test_cpu_cap_and_unmeasured_zero():
    q = pd.DataFrame([{key: value for key, value in zip(CPU, (3, 3.1, 0, 0), strict=True)}])
    h = pd.DataFrame([{key: value for key, value in zip(CPU, (2, 2, 0, 1), strict=True)}])
    assert [r["outcome"] for r in inf.cpu_checks((q, h), True)] == [inf.SUPPORT, inf.OPPOSE, inf.UNKNOWN, inf.SUPPORT]


def test_recommendations_inconclusive_equivalent_and_overload(monkeypatch):
    cell = Cell("datacenter", "datacenter", "small")
    sample = inf.Samples(pd.DataFrame(observations(cell, QUIC) + observations(cell, H2)))
    got = inf.recommendations(sample)
    assert len(got) == 20
    base = got[got["network"].eq("datacenter") & got["load"].eq("small")].iloc[0].to_dict()
    assert (base["decision"], base["preferred"]) == ("no-difference", H2)
    monkeypatch.setattr(inf, "compare", lambda *_: inf.Pair(ratio_lo=.75, ratio_hi=1.01, p=.01))
    row = inf.recommendations(sample).iloc[0]
    assert row["decision"] == "insufficient" and row["preferred"] == ""
    assert inf.recommendation(dict(base, cpu=inf.OPPOSE))[0] == H2
    both = dict(base, regime="both-overloaded", goodput_median_quic=12, goodput_median_other=10, goodput_p_holm=.01)
    assert inf.recommendation(both)[0] == "no-difference"
    assert inf.recommendation(dict(both, goodput_median_quic=13))[0] == QUIC
    assert inf.recommendation(dict(both, goodput_median_other=20))[0] == H2
    assert inf.recommendation(dict(both, goodput_median_quic=13, goodput_p_holm=.05))[0] == "no-difference"


def test_verdict_precedence():
    assert inf.verdict([inf.EXCLUDED] * 3) == "insufficient"
    assert inf.verdict([inf.EXCLUDED, inf.SUPPORT]) == "accepted"
    assert inf.verdict([inf.SUPPORT, inf.UNKNOWN]) == "insufficient"
    assert inf.verdict([inf.UNKNOWN, inf.OPPOSE]) == "rejected"
