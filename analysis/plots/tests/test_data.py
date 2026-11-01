import json

import pandas as pd
import pytest
from conftest import make_series
from qrpcplots import data


def test_parse_cpus():
    assert data.parse_cpus("2-3") == [2, 3]
    assert data.parse_cpus("0,4-6") == [0, 4, 5, 6]
    assert data.parse_cpus("") == []


def test_read_series(tmp_path):
    path = make_series(tmp_path / "s", ["base", "loss-2"], ["small"], 1, missing={"007-grpc-h2-loss-2-small-r1"})
    series = data.read_series(path)
    assert series.missing == ["007-grpc-h2-loss-2-small-r1"]
    assert len(series.runs) == 11
    run = series.runs.set_index("run_id").loc["006-quic-loss-2-small-r1"]
    assert run[["variant", "transport", "tcp", "network", "load", "repeat", "seed", "role"]].tolist() == \
        ["quic", "quic", "aligned", "loss-2", "small", 1, 1, ""]
    assert run["loss_pct"] == 2 and run["rtt_ms"] == 50
    assert run["server_cpu_ns"] == 400 and run["client_cpu_ns"] == 250
    # Not idle: user, nice, system, irq, softirq and steal of CPUs 2-3 and 4.
    assert run["server_cores_busy_ns"] == (20 + 3) + (30 + 3) and run["client_cores_busy_ns"] == 40 + 3
    assert run["tls"] == "TLS 1.3 / TLS_AES_128_GCM_SHA256 / X25519MLKEM768"
    drift = series.runs.set_index("run_id").loc["000-quic-base-small-drift-start"]
    assert drift["role"] == "drift-start" and drift["repeat"] == 0
    assert set(series.calls["run_id"].unique()) == set(series.runs["run_id"])
    assert len(series.calls) == 11 * 200


def test_tables_round_trip(tmp_path):
    series = data.read_series(make_series(tmp_path / "s", ["base"], ["small"], 1))
    data.write_tables(series, tmp_path / "out")
    runs, calls = data.read_tables(tmp_path / "out")
    pd.testing.assert_frame_equal(runs, series.runs, check_dtype=False)
    pd.testing.assert_frame_equal(calls, series.calls, check_categorical=False)


def test_read_series_without_runs(tmp_path):
    path = make_series(tmp_path / "s", ["base"], ["small"], 1)
    for run in path.iterdir():
        if run.is_dir():
            (run / "meta.json").unlink()
    with pytest.raises(ValueError, match="no finished runs"):
        data.read_series(path)


def pooled_series(tmp_path, size=4):
    path = make_series(tmp_path / "s", ["base"], ["small"], 1)
    plan_path = path / "series.json"
    plan = json.loads(plan_path.read_text())
    pooled = []
    for run in plan["runs"]:
        if run["variant"] != "grpc-h2":
            continue
        run.update(variant=f"grpc-h2-pool-{size}", transport="grpc-h2-pool", pool_size=size, pool_policy="round-robin")
        meta_path = path / run["id"] / "meta.json"
        meta = json.loads(meta_path.read_text())
        meta.update(transport=run["transport"], pool_size=size, pool_policy="round-robin")
        meta_path.write_text(json.dumps(meta))
        pooled.append(run)
    plan_path.write_text(json.dumps(plan))
    return path, plan, pooled


def test_pool_metadata_round_trip(tmp_path):
    path, _, _ = pooled_series(tmp_path, 8)
    series = data.read_series(path)
    pool = series.runs[series.runs["transport"] == "grpc-h2-pool"]
    assert pool["pool_size"].tolist() == [8, 8, 8]
    assert set(pool["pool_policy"]) == {"round-robin"}
    legacy = series.runs[series.runs["transport"] != "grpc-h2-pool"]
    assert set(legacy["pool_size"]) == {1} and set(legacy["pool_policy"]) == {"single"}
    data.write_tables(series, tmp_path / "out")
    runs, _ = data.read_tables(tmp_path / "out")
    pd.testing.assert_frame_equal(runs, series.runs, check_dtype=False)


def test_pool_plan_must_match_metadata(tmp_path):
    path, _, pooled = pooled_series(tmp_path)
    meta_path = path / pooled[0]["id"] / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta["pool_size"] = 2
    meta_path.write_text(json.dumps(meta))
    with pytest.raises(ValueError, match="differs from plan"):
        data.read_series(path)


def test_variant_cannot_mix_pool_sizes(tmp_path):
    path, plan, pooled = pooled_series(tmp_path)
    pooled[0]["pool_size"] = 2
    (path / "series.json").write_text(json.dumps(plan))
    meta_path = path / pooled[0]["id"] / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta["pool_size"] = 2
    meta_path.write_text(json.dumps(meta))
    with pytest.raises(ValueError, match="different connection setups"):
        data.read_series(path)


def test_read_legacy_tables(tmp_path):
    series = data.read_series(make_series(tmp_path / "s", ["base"], ["small"], 1))
    out = tmp_path / "out"
    data.write_tables(series, out)
    series.runs.drop(columns=["pool_size", "pool_policy"]).to_csv(out / data.RUNS_FILE, index=False)
    runs, _ = data.read_tables(out)
    assert set(runs["pool_size"]) == {1} and set(runs["pool_policy"]) == {"single"}


def test_pool_tables_require_explicit_setup(tmp_path):
    path, _, _ = pooled_series(tmp_path)
    series = data.read_series(path)
    out = tmp_path / "out"
    data.write_tables(series, out)
    series.runs.drop(columns=["pool_size", "pool_policy"]).to_csv(out / data.RUNS_FILE, index=False)
    with pytest.raises(ValueError, match="missing pool_size"):
        data.read_tables(out)


def test_negotiated_http_version(tmp_path):
    path = make_series(tmp_path / "s", ["base"], ["small"], 1)
    plan_path = path / "series.json"
    plan = json.loads(plan_path.read_text())
    run = plan["runs"][1]
    run.update(variant="grpc-h3", transport="grpc-h3")
    plan_path.write_text(json.dumps(plan))
    meta_path = path / run["id"] / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta["tls"][0]["alpn"] = "h3"
    meta_path.write_text(json.dumps(meta))
    rows = data.read_series(path).runs.set_index("run_id")
    assert rows.loc[run["id"], "alpn"] == "h3"
    meta["tls"][0]["alpn"] = "h2"
    meta_path.write_text(json.dumps(meta))
    with pytest.raises(ValueError, match="negotiated ALPN"):
        data.read_series(path)
