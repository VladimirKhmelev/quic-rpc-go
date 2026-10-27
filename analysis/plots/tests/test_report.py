import pandas as pd
from qrpcplots import __main__ as cli
from qrpcplots import data, figures, report


def test_loss_axis(series):
    runs = data.read_series(series).runs
    assert figures.loss_axis(runs) == [("base", 0.0), ("loss-1", 1.0), ("loss-2", 2.0)]


def test_loss_axis_keeps_the_base_point_and_its_losses():
    base = {"rtt_ms": 50.0, "jitter_ms": 0.0, "reorder_pct": 0.0, "rate_bps": 1e8, "queue_bdp": 1.0, "loss_pct": 0.0, "loss_burst": 0.0}
    nets = {
        "base": base,
        "loss-5": {**base, "loss_pct": 5.0},
        "burst-2": {**base, "loss_pct": 2.0, "loss_burst": 4.0},
        "rtt-150": {**base, "rtt_ms": 150.0, "queue_bdp": 0.333333},
        "jitter-10": {**base, "jitter_ms": 10.0},
        "interregion": {**base, "rtt_ms": 150.0, "loss_pct": 0.5, "jitter_ms": 10.0},
    }
    runs = pd.DataFrame([{"network": n, **p} for n, p in nets.items()])
    assert figures.loss_axis(runs) == [("base", 0.0), ("loss-5", 5.0)]
    assert figures.loss_axis(runs[runs["network"] != "base"]) == []


def test_report(series, tmp_path):
    s = data.read_series(series)
    out = tmp_path / "out"
    written = {p.name for p in report.report(s.runs, s.calls, out)}
    assert written == {
        "summary.csv", "cells.csv", "comparisons.csv", "spread.csv", "cpu.csv", "warmup.csv", "drift.csv",
        "fig01-loss-small.png", "fig02-loss-small-heavy.png", "fig04-ecdf-loss-2.png", "fig13-profiles.png", "report.md",
    }
    cells = pd.read_csv(out / "cells.csv")
    assert (cells["runs"] == 3).all() and len(cells) == 4 * 2 * 3
    # gRPC falls behind at 2% of losses only.
    behind = (cells["network"] == "loss-2") & (cells["variant"] != "quic")
    assert cells.loc[behind, "overloaded"].all() and (cells.loc[behind, "overloaded_runs"] == 3).all()
    assert not cells.loc[~behind, "overloaded"].any() and (cells.loc[~behind, "overloaded_runs"] == 0).all()
    assert (cells.loc[behind, "growth"] > 1.5).all() and (cells.loc[~behind, "growth"] < 1.5).all()
    assert (cells["goodput_mbps"] == (1000 + 900) * 8 / 1e6).all()
    comparisons = pd.read_csv(out / "comparisons.csv")
    lossy = comparisons[(comparisons["network"] == "loss-2") & (comparisons["metric"] == "p99")]
    assert (lossy["effect_pct"] < -50).all() and (lossy["regime"] == "other-overloaded").all()
    assert (comparisons.loc[comparisons["network"] != "loss-2", "regime"] == "steady").all()
    text = (out / "report.md").read_text(encoding="utf-8")
    assert "Прогонов: 84, из них прогонов дрейфа: 12." in text and "![fig13-profiles](fig13-profiles.png)" in text
    assert "## Перегрузка" in text and "(перегрузка 3 из 3)" in text and "| перегружен другой |" in text
    assert "## CPU на вызов, мкс" in text and "| сервер, ядра |" in text

    # The same data give the same tables.
    again = tmp_path / "again"
    report.report(s.runs, s.calls, again)
    for name in ("cells.csv", "comparisons.csv", "spread.csv"):
        assert (out / name).read_bytes() == (again / name).read_bytes()


def test_command_line(series, tmp_path):
    out = tmp_path / "analysis"
    assert cli.main(["ingest", str(series), "-o", str(out)]) == 0
    assert (out / "runs.csv").exists() and (out / "calls.parquet").exists()
    assert cli.main(["report", str(out)]) == 0
    assert (out / "report.md").exists() and (out / "fig01-loss-small.png").exists()
