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
