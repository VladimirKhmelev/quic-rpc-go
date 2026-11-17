"""Checks collection completeness, TLS evidence and process-level statistics."""

import hashlib
import importlib.util
import itertools
import json
from pathlib import Path
import subprocess

import numpy as np
import pandas as pd
import pytest

from qrpcplots import micro
from qrpcplots.__main__ import main

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("microbench", ROOT / "scripts/microbench.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


def batch(family, cpu=1, scale=1, iterations=20):
    suffix = f"-{cpu}" if cpu != 1 else ""
    lines = []
    if family == "overhead":
        for backend, kind, size in itertools.product(micro.BACKENDS, micro.KINDS, micro.SIZES):
            lines.append(f"BenchmarkRPCOverhead/Backend={backend}/Kind={kind}/Bytes={size}{suffix} "
                         f"{iterations} {scale * 1000} ns/op 64 B/op 1 allocs/op")
    else:
        for rtt, mode in itertools.product(micro.RTTS, micro.MODES):
            ms = scale * (1 + rtt * (1 if mode == "early" else 2))
            normalized = f" {ms / rtt:.4g} RTT/reply" if rtt else ""
            lines.append(f"BenchmarkEarlyReconnect/RTT={rtt}ms/Mode={mode}{suffix} {iterations} "
                         f"{ms * 1e6 + 1000:.4g} ns/op {ms:.4g} first_reply_ms {ms * 1e6:.4g} first_reply_ns "
                         f"{int(mode != 'cold')} did_resume {int(mode == 'early')} used_0rtt "
                         f"{rtt} rtt_ms 772 tls_version 4865 tls_cipher_suite 4588 tls_curve 8 payload_bytes{normalized}")
    return "\n".join(["goos: linux", *lines, "PASS", ""])


def collection(path, scales=(1, 2, 10), counts=(20, 20, 20)):
    path.mkdir(parents=True)
    meta = {"schema_version": 1, "kind": "rpc-micro",
            "config": {"cpu": 1, "repeats": len(scales), "reconnect_iterations": 20}, "jobs": []}
    for repeat, (scale, count) in enumerate(zip(scales, counts, strict=True), 1):
        for family in ("overhead", "reconnect"):
            raw = f"{repeat}-{family}.txt"
            text = batch(family, scale=scale, iterations=count if family == "overhead" else 20)
            (path / raw).write_text(text)
            meta["jobs"].append({"family": family, "repeat": repeat, "order": len(meta["jobs"]) + 1,
                                 "returncode": 0, "raw": raw, "raw_sha256": hashlib.sha256(text.encode()).hexdigest()})
    (path / "meta.json").write_text(json.dumps(meta))
    return meta


@pytest.mark.parametrize("family,n", [("overhead", 32), ("reconnect", 6)])
@pytest.mark.parametrize("cpu", [1, 4])
def test_parse_complete_batches(family, n, cpu):
    rows = micro.parse_job(batch(family, cpu=cpu), family, cpu)
    assert len(rows) == n
    assert {r["cpu"] for r in rows} == {cpu}


@pytest.mark.parametrize("transform", [
    lambda s: s.replace("PASS", "FAIL"),
    lambda s: s + s.splitlines()[1] + "\n",
    lambda s: "\n".join(s.splitlines()[2:]),
    lambda s: s.replace("20 1000", "0 1000", 1),
    lambda s: s.replace("1000 ns/op", "nan ns/op", 1),
    lambda s: s.replace("1000 ns/op", "-1 ns/op", 1),
    lambda s: s.replace("1000 ns/op", "1000 unknown", 1),
    lambda s: s.replace("64 B/op", "64 B/op 64 B/op", 1),
    lambda s: s.replace("64 B/op", "", 1),
    lambda s: s.replace("Bytes=65536", "Bytes=65537", 1),
])
def test_reject_bad_memory(transform):
    with pytest.raises(ValueError):
        micro.parse_job(transform(batch("overhead")), "overhead", 1)


@pytest.mark.parametrize("old,new", [
    ("0 did_resume", "1 did_resume"), ("0 used_0rtt", "1 used_0rtt"),
    ("772 tls_version", "771 tls_version"), ("4588 tls_curve", "29 tls_curve"),
    ("4865 tls_cipher_suite", "1 tls_cipher_suite"), ("8 payload_bytes", "9 payload_bytes"),
    ("0 rtt_ms", "50 rtt_ms"), ("1 first_reply_ms", "1000 first_reply_ms"),
    ("2.02 RTT/reply", "1 RTT/reply"), ("8 payload_bytes", "8 payload_bytes 1 RTT/reply"),
])
def test_reject_contradictory_tls(old, new):
    text = batch("reconnect")
    assert old in text
    with pytest.raises(ValueError):
        micro.parse_job(text.replace(old, new, 1), "reconnect", 1)


def test_reject_wrong_cpu():
    with pytest.raises(ValueError, match="CPU"):
        micro.parse_job(batch("overhead", cpu=4), "overhead", 1)


def test_custom_metric_rounding():
    text = batch("reconnect").replace("51 first_reply_ms 5.1e+07 first_reply_ns", "54.13 first_reply_ms 5.413e+07 first_reply_ns")
    text = text.replace("1.02 RTT/reply", "1.083 RTT/reply")
    assert len(micro.parse_job(text, "reconnect", 1)) == 6


def test_statistics_use_processes_not_operations(tmp_path):
    path = tmp_path / "micro"
    collection(path, counts=(1, 100000, 1))
    _, rows = micro.read(path)
    summary, gains = micro.tables(rows)
    memory = summary[(summary.family == "overhead") & (summary.metric == "ns_per_op")]
    assert set(memory["repeats"]) == {3}
    assert set(memory["median"]) == {2000}
    assert set(memory["lo"]) == {1000}
    assert set(memory["hi"]) == {10000}
    fifty = gains[gains.rtt_ms == 50].iloc[0]
    assert fifty.gain_ms == 100
    assert fifty.gain_rtts == 2
    assert np.isnan(gains[gains.rtt_ms == 0].iloc[0].gain_rtts)
    again, _ = micro.tables(rows.sample(frac=1, random_state=3))
    pd.testing.assert_frame_equal(summary, again)


def test_single_repeat_and_cli(tmp_path):
    path = tmp_path / "micro"
    collection(path, scales=(1,), counts=(20,))
    assert main(["micro", str(path)]) == 0
    table = pd.read_csv(path / "analysis/summary.csv")
    assert len(table) == 32 * 3 + 6
    assert table.lo.isna().all() and table.hi.isna().all()
    assert "Единица анализа" in (path / "analysis/report.md").read_text()


@pytest.mark.parametrize("mutation", ["missing", "duplicate", "failed", "iterations", "path", "checksum", "order", "config"])
def test_reject_broken_collection(tmp_path, mutation):
    path = tmp_path / "micro"
    meta = collection(path)
    if mutation == "missing":
        meta["jobs"].pop()
    elif mutation == "duplicate":
        meta["jobs"].append(meta["jobs"][0])
    elif mutation == "failed":
        meta["jobs"][0]["returncode"] = 1
    elif mutation == "iterations":
        meta["config"]["reconnect_iterations"] = 21
    elif mutation == "path":
        meta["jobs"][0]["raw"] = "../escape.txt"
    elif mutation == "checksum":
        (path / meta["jobs"][0]["raw"]).write_text("changed")
    elif mutation == "order":
        meta["jobs"][0]["order"] = 2
    else:
        meta["config"]["repeats"] = 0
    (path / "meta.json").write_text(json.dumps(meta))
    with pytest.raises(ValueError):
        micro.read(path)


def test_unfinished_collection_is_not_readable(tmp_path):
    (tmp_path / "failed.json").write_text("{}")
    with pytest.raises(FileNotFoundError):
        micro.read(tmp_path)


@pytest.mark.parametrize("argv", [["--repeats", "0"], ["--cpu", "0"], ["--reconnect-iterations", "0"],
                                  ["--benchtime", "0s"], ["--benchtime", "bogus"]])
def test_runner_arguments(argv):
    with pytest.raises(SystemExit):
        runner.arguments(argv)


def test_schedule():
    order = runner.schedule(5, 3)
    assert order == runner.schedule(5, 3)
    assert set(order) == set(itertools.product(range(1, 6), ("overhead", "reconnect")))
    assert order != runner.schedule(5, 7)


@pytest.mark.parametrize("mode", ["success", "source-change", "bad-tls", "failed-process"])
def test_runner_publishes_only_complete_results(tmp_path, monkeypatch, mode):
    root, out = tmp_path / "repo", tmp_path / "results"
    root.mkdir()
    (root / "input.go").write_text("package input\n")
    manifests = iter([{"input.go": "a"}, {"input.go": "b" if mode == "source-change" else "a"}])
    monkeypatch.setattr(runner, "sources", lambda _: next(manifests))
    monkeypatch.setattr(runner, "environment", lambda: {})

    def fake(argv, cwd, raw, timeout=300):
        if "-c" in argv:
            Path(argv[argv.index("-o") + 1]).write_bytes(b"binary")
        family = "overhead" if "overhead.test" in argv[0] else "reconnect"
        text = batch(family) if argv[0].endswith(".test") else "version"
        if mode == "bad-tls" and argv[0].endswith("reconnect.test"):
            text = text.replace("0 did_resume", "1 did_resume", 1)
        raw.write_text(text)
        if mode == "failed-process" and argv[0].endswith(".test"):
            raise RuntimeError("failed job")
        return {"command": argv, "raw": str(raw), "raw_sha256": runner.digest(raw), "returncode": 0}

    monkeypatch.setattr(runner, "command", fake)
    config = {"cpu": 1, "repeats": 1, "seed": 1, "benchtime": "200ms", "reconnect_iterations": 20}
    if mode == "success":
        runner.collect(root, out, config)
        meta, rows = micro.read(out)
        assert len(rows) == 38
        assert len(meta["builds"]) == 2
        assert (out / "provenance/sources.tar.gz").exists()
        with pytest.raises(FileExistsError):
            runner.collect(root, out, config)
    else:
        with pytest.raises((ValueError, RuntimeError)):
            runner.collect(root, out, config)
        assert not (out / "meta.json").exists()
        assert (out / "failed.json").exists()
        assert list((out / "raw").iterdir())


def test_command_retains_failure_and_timeout(tmp_path, monkeypatch):
    monkeypatch.setattr(runner.subprocess, "run", lambda *a, **k: subprocess.CompletedProcess(a, 1, b"failed output"))
    raw = tmp_path / "raw.txt"
    with pytest.raises(RuntimeError):
        runner.command(["false"], tmp_path, raw)
    assert raw.read_bytes() == b"failed output"

    def timeout(*args, **kwargs):
        raise subprocess.TimeoutExpired(args[0], 1, output=b"partial")

    monkeypatch.setattr(runner.subprocess, "run", timeout)
    with pytest.raises(RuntimeError):
        runner.command(["sleep"], tmp_path, raw)
    assert b"partial" in raw.read_bytes() and b"timeout" in raw.read_bytes()
