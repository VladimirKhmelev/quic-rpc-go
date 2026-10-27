"""A synthetic series in the format of bench series, for the tests."""

from __future__ import annotations

import csv
import json
from pathlib import Path

import numpy as np
import pytest

NETWORKS = {
    "base": {"rtt_ns": 50_000_000, "jitter_ns": 0, "loss_pct": 0, "loss_burst": 0, "reorder_pct": 0, "rate_bps": 100_000_000, "queue_bdp": 1},
    "loss-1": {"rtt_ns": 50_000_000, "jitter_ns": 0, "loss_pct": 1, "loss_burst": 0, "reorder_pct": 0, "rate_bps": 100_000_000, "queue_bdp": 1},
    "loss-2": {"rtt_ns": 50_000_000, "jitter_ns": 0, "loss_pct": 2, "loss_burst": 0, "reorder_pct": 0, "rate_bps": 100_000_000, "queue_bdp": 1},
    "rtt-150": {"rtt_ns": 150_000_000, "jitter_ns": 0, "loss_pct": 0, "loss_burst": 0, "reorder_pct": 0, "rate_bps": 100_000_000, "queue_bdp": 0.333333},
}
VARIANTS = [("quic", "quic", "aligned"), ("grpc-h2", "grpc-h2", "aligned"), ("grpc-h2-tcp-default", "grpc-h2", "default")]
WARMUP_NS = 1_000_000_000
MEASURE_NS = 10_000_000_000


def latency_ms(variant: str, network: str, rng: np.random.Generator, scheduled: np.ndarray) -> np.ndarray:
    """The latencies of the calls of a synthetic run due at scheduled: an
    RTT, more on QUIC-less variants as the losses grow. From 2% of losses
    on, these fall behind: their latencies grow over the measurement up to
    three times."""
    base = NETWORKS[network]["rtt_ns"] / 1e6
    slow = 1 + NETWORKS[network]["loss_pct"] * (0.1 if variant == "quic" else 1.0)
    latency = base * slow * rng.lognormal(0, 0.1, len(scheduled))
    if variant != "quic" and NETWORKS[network]["loss_pct"] >= 2:
        latency *= 1 + 2 * np.clip(scheduled - WARMUP_NS, 0, None) / MEASURE_NS
    return latency


def write_run(directory: Path, planned: dict, rng: np.random.Generator, calls: int = 200) -> None:
    directory.mkdir()
    network = planned["network"]
    meta = {
        "network": {"name": network, **NETWORKS[network]},
        "warmup_ns": WARMUP_NS,
        "measure_ns": MEASURE_NS,
        "window": {"duration_ns": MEASURE_NS},
        "server": {"cpus": "2-3", "cpu": {"user_ns": 300, "system_ns": 100}},
        "client": {"cpus": "4", "cpu": {"user_ns": 200, "system_ns": 50}},
        "cores": [{"id": i, "user_ns": 10 * i, "nice_ns": 0, "system_ns": 1, "idle_ns": 1000, "iowait_ns": 5,
                   "irq_ns": 0, "softirq_ns": 2, "steal_ns": 0} for i in range(6)],
        "results": {"sent_bytes_per_s": 1000.0, "received_bytes_per_s": 900.0, "in_flight_mean": 3.5, "in_flight_max": 9},
        "tls": [{"version": "TLS 1.3", "cipher_suite": "TLS_AES_128_GCM_SHA256", "group": "X25519MLKEM768"}],
        "problems": [],
        "build": {"commit": "abc", "dirty": False},
    }
    (directory / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
    scheduled = np.sort(rng.uniform(0, WARMUP_NS + MEASURE_NS, calls)).astype(np.int64)
    latency = latency_ms(planned["variant"], network, rng, scheduled)
    with open(directory / "calls.csv", "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(["seq", "kind", "scheduled_ns", "latency_ns", "code", "req_bytes", "resp_bytes", "conn", "phase"])
        for i, (at, ms) in enumerate(zip(scheduled, latency, strict=True)):
            code = 4 if ms > 1000 else 0
            w.writerow([i, "small", at, int(ms * 1e6), code, 500, 0 if code else 500, 0, "warmup" if at < WARMUP_NS else "measure"])


def make_series(path: Path, networks: list[str], loads: list[str], repeats: int, missing: set[str] = frozenset()) -> Path:
    """Writes a series like bench series does, with the drift runs on base;
    the runs named in missing get no directory."""
    rng = np.random.default_rng(1)
    runs = []

    def cell(network: str, load: str, repeat: int, role: str = "") -> None:
        for name, transport, tcp in VARIANTS:
            tag = role or f"r{repeat}"
            runs.append({"id": f"{len(runs):03d}-{name}-{network}-{load}-{tag}", "variant": name, "transport": transport,
                         "tcp": tcp, "network": network, "load": load, "repeat": repeat, "seed": max(repeat, 1),
                         **({"role": role} if role else {})})

    for load in loads:
        cell("base", load, 0, "drift-start")
    for repeat in range(1, repeats + 1):
        for network in networks:
            for load in loads:
                cell(network, load, repeat)
    for load in loads:
        cell("base", load, 0, "drift-end")
    path.mkdir(parents=True)
    (path / "series.json").write_text(json.dumps({"runs": runs}), encoding="utf-8")
    for planned in runs:
        if planned["id"] not in missing:
            write_run(path / planned["id"], planned, rng)
    return path


@pytest.fixture
def series(tmp_path: Path) -> Path:
    return make_series(tmp_path / "series", ["base", "loss-1", "loss-2", "rtt-150"], ["small", "small-heavy"], 3)
