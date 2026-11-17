import json

import pandas as pd
import pytest
from conftest import make_series
from qrpcplots import bulk, data, report


def bulk_series(tmp_path):
    path = make_series(tmp_path / "s", ["base"], ["bulk"], 1)
    plan = json.loads((path / "series.json").read_text())
    config = {"total_bytes": 100 << 20, "chunk_bytes": 64 << 10, "timeout_ns": 60_000_000_000}
    plan["loads"] = [{"name": "bulk", "bulk": config}]
    (path / "series.json").write_text(json.dumps(plan))
    for r in plan["runs"]:
        meta_path = path / r["id"] / "meta.json"
        meta = json.loads(meta_path.read_text())
        meta.update(load={"name": "bulk", "bulk": config}, measure_ns=2_000_000_000)
        meta["window"]["duration_ns"] = 2_000_001_000
        meta["results"]["bulk"] = {
            "total_bytes": 100 << 20, "chunk_bytes": 64 << 10,
            "received_bytes": 100 << 20, "chunks": 1600, "duration_ns": meta["measure_ns"], "code": 0, "completed": True,
        }
        meta_path.write_text(json.dumps(meta))
        pd.DataFrame([{
            "seq": 0, "kind": "bulk", "scheduled_ns": meta["warmup_ns"], "latency_ns": meta["measure_ns"],
            "code": 0, "req_bytes": 0, "resp_bytes": 100 << 20, "conn": 0, "phase": "measure", "chunks": 1600,
        }]).to_csv(path / r["id"] / "calls.csv", index=False)
    return path, plan


def test_bulk_statistics_and_report(tmp_path):
    path, _ = bulk_series(tmp_path)
    s = data.read_series(path)
    attempted, summary = bulk.tables(s.runs)
    assert len(attempted) == 9 and len(summary) == 3
    assert set(attempted["observed_mbps"]) == {(100 << 20) * 8 / 1e6 / 2}
    assert set(attempted["server_cpu_us_per_mib"]) == {0.004}
    assert set(summary["runs"]) == {1}  # 1600 chunks are not 1600 samples.
    assert set(summary["completed"]) == {1} and set(summary["errors"]) == {0}
    assert summary["goodput_mbps_lo"].isna().all()
    assert summary["goodput_mbps_hi"].isna().all()
    data.write_tables(s, tmp_path / "tables")
    runs, calls = data.read_tables(tmp_path / "tables")
    pd.testing.assert_frame_equal(runs, s.runs, check_dtype=False)
    pd.testing.assert_frame_equal(calls, s.calls, check_categorical=False)
    out = tmp_path / "report"
    written = report.report(runs, calls, out)
    assert {p.name for p in written} == {"bulk_runs.csv", "bulk.csv", "report.md"}
    text = (out / "report.md").read_text()
    assert "Передач: 9, завершено: 9" in text and "блоки не являются независимыми" in text


def test_bulk_partial_transfer(tmp_path):
    path, plan = bulk_series(tmp_path)
    run = next(r for r in plan["runs"] if not r.get("role"))
    meta_path = path / run["id"] / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta["results"]["bulk"].update(code=4, completed=False, received_bytes=32 << 20, chunks=512)
    meta_path.write_text(json.dumps(meta))
    csv_path = path / run["id"] / "calls.csv"
    calls = pd.read_csv(csv_path)
    calls.loc[0, ["code", "resp_bytes", "chunks"]] = [4, 32 << 20, 512]
    calls.to_csv(csv_path, index=False)
    s = data.read_series(path)
    attempted, summary = bulk.tables(s.runs)
    row = attempted.set_index("run_id").loc[run["id"]]
    assert row["observed_mbps"] > 0 and pd.isna(row["goodput_mbps"])
    aggregate = summary[summary["variant"] == run["variant"]].iloc[0]
    assert aggregate["errors"] == 1 and aggregate["completed"] == 0 and pd.isna(aggregate["goodput_mbps"])


@pytest.mark.parametrize("corruption", ["chunks", "duration_ns", "completed", "total_bytes"])
def test_bulk_metadata_corruption(tmp_path, corruption):
    path, plan = bulk_series(tmp_path)
    meta_path = path / plan["runs"][0]["id"] / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta["results"]["bulk"][corruption] = False if corruption == "completed" else 1
    meta_path.write_text(json.dumps(meta))
    with pytest.raises(ValueError, match="bulk"):
        data.read_series(path)


def test_bulk_raw_and_tables_corruption(tmp_path):
    path, plan = bulk_series(tmp_path)
    s = data.read_series(path)
    data.write_tables(s, tmp_path / "tables")
    parquet = tmp_path / "tables" / data.CALLS_FILE
    calls = pd.read_parquet(parquet)
    calls.loc[0, "chunks"] = 1
    calls.to_parquet(parquet)
    with pytest.raises(ValueError, match="differs from metadata"):
        data.read_tables(tmp_path / "tables")
    csv_path = path / plan["runs"][0]["id"] / "calls.csv"
    calls = pd.read_csv(csv_path)
    calls.loc[0, "resp_bytes"] -= 1
    calls.to_csv(csv_path, index=False)
    with pytest.raises(ValueError, match="differs from metadata"):
        data.read_series(path)


def test_bulk_configuration_must_match_plan(tmp_path):
    path, plan = bulk_series(tmp_path)
    plan["loads"][0]["bulk"]["timeout_ns"] = 1
    (path / "series.json").write_text(json.dumps(plan))
    with pytest.raises(ValueError, match="differs from plan"):
        data.read_series(path)


def test_mixed_report_keeps_workloads_separate(tmp_path):
    path, _ = bulk_series(tmp_path / "bulk")
    transfer = data.read_series(path)
    ordinary = data.read_series(make_series(tmp_path / "ordinary", ["base"], ["small"], 1))
    # Prefix ids because the synthetic series independently number their runs.
    transfer.runs["run_id"] = "bulk-" + transfer.runs["run_id"].astype(str)
    transfer.calls["run_id"] = "bulk-" + transfer.calls["run_id"].astype(str)
    runs = pd.concat([ordinary.runs, transfer.runs], ignore_index=True)
    calls = pd.concat([ordinary.calls, transfer.calls], ignore_index=True)
    out = tmp_path / "out"
    report.report(runs, calls, out)
    text = (out / "report.md").read_text()
    assert "Всего прогонов: 18" in text and "Передач: 9" in text
    assert set(pd.read_csv(out / "summary.csv")["kind"]) == {"small"}
    assert len(pd.read_csv(out / "bulk_runs.csv")) == 9


def test_bulk_intervals_require_independent_runs(tmp_path):
    path, _ = bulk_series(tmp_path)
    runs = data.read_series(path).runs
    original = runs[runs["role"] == ""].iloc[[0]].copy()
    second = original.copy()
    second["run_id"] += "-second"
    second["bulk_duration_ns"] *= 2
    _, summary = bulk.tables(pd.concat([original, second], ignore_index=True))
    row = summary.iloc[0]
    assert row["runs"] == row["completed"] == 2
    assert row["goodput_mbps_lo"] < row["goodput_mbps"] < row["goodput_mbps_hi"]
    _, again = bulk.tables(pd.concat([original, second], ignore_index=True))
    pd.testing.assert_frame_equal(summary, again)
