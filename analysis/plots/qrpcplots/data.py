"""The data of a series: the runs and their calls (data/schema.md).

read_series reads a series directory of bench series; write_tables and
read_tables keep the result as runs.csv, one row per run, and calls.parquet,
one row per call, compressed with zstd.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

import pandas as pd

RUNS_FILE = "runs.csv"
CALLS_FILE = "calls.parquet"

# The columns of calls.csv and their types.
CALL_DTYPES = {
    "seq": "int64",
    "kind": "category",
    "scheduled_ns": "int64",
    "latency_ns": "int64",
    "code": "int16",
    "req_bytes": "int64",
    "resp_bytes": "int64",
    "conn": "int32",
    "phase": "category",
}


@dataclass
class Series:
    """The finished runs of a series and their calls."""

    runs: pd.DataFrame
    calls: pd.DataFrame
    # The planned runs without meta.json: failed, cut short or not made yet.
    missing: list[str] = field(default_factory=list)


def parse_cpus(text: str) -> list[int]:
    """Parses a CPU list such as "2-3,5"; an empty list is no CPU."""
    cpus: list[int] = []
    for part in filter(None, text.split(",")):
        first, _, last = part.partition("-")
        cpus.extend(range(int(first), int(last or first) + 1))
    return cpus


def busy_ns(core: dict) -> int:
    """Returns the time a CPU of meta.json was not idle."""
    return sum(core[k] for k in ("user_ns", "nice_ns", "system_ns", "irq_ns", "softirq_ns", "steal_ns"))


def connection_setup(record: dict, transport: str) -> tuple[int, str]:
    """Reads the effective setup. Only historical single-connection data
    can omit the pool fields; a pool's size must never be guessed."""
    pooled = transport == "grpc-h2-pool"
    size = record.get("pool_size", None if pooled else 1)
    policy = record.get("pool_policy", None if pooled else "single")
    if isinstance(size, bool) or not isinstance(size, int) or size < 1:
        raise ValueError(f"{transport}: invalid or missing pool_size {size!r}")
    expected = "round-robin" if pooled else "single"
    if (not pooled and size != 1) or policy != expected:
        raise ValueError(f"{transport}: invalid connection setup ({size}, {policy!r})")
    return size, policy


def normalize_setups(runs: pd.DataFrame) -> pd.DataFrame:
    """Preserves legacy tables and rejects pooling different setups under
    the same variant name, which is the unit used by the statistics."""
    runs = runs.copy()
    setups = [connection_setup(r, r["transport"]) for r in runs.to_dict("records")]
    runs["pool_size"] = [s[0] for s in setups]
    runs["pool_policy"] = [s[1] for s in setups]
    for variant, group in runs.groupby("variant", observed=True):
        if len(group[["transport", "tcp", "pool_size", "pool_policy"]].drop_duplicates()) != 1:
            raise ValueError(f"variant {variant!r}: different connection setups")
    return runs


def run_row(planned: dict, meta: dict) -> dict:
    """Returns the row of runs.csv of a run: its place in the plan, the
    network, and the CPU, goodput and calls in flight of meta.json."""
    net = meta["network"]
    transport = planned["transport"]
    setup = connection_setup(planned, transport)
    if meta.get("transport", transport) != transport or connection_setup(meta, transport) != setup:
        raise ValueError(f"{planned['id']}: connection setup differs from plan")
    cores = {c["id"]: c for c in meta["cores"]}

    def cores_busy(process: dict) -> int:
        return sum(busy_ns(cores[i]) for i in parse_cpus(process["cpus"]) if i in cores)

    results = meta["results"]
    tls = meta["tls"][0] if meta["tls"] else {}
    return {
        "run_id": planned["id"],
        "variant": planned["variant"],
        "transport": planned["transport"],
        "pool_size": setup[0],
        "pool_policy": setup[1],
        "tcp": planned["tcp"],
        "network": planned["network"],
        "load": planned["load"],
        "repeat": planned["repeat"],
        "seed": planned["seed"],
        "role": planned.get("role", ""),
        "rtt_ms": net["rtt_ns"] / 1e6,
        "jitter_ms": net["jitter_ns"] / 1e6,
        "loss_pct": net["loss_pct"],
        "loss_burst": net["loss_burst"],
        "reorder_pct": net["reorder_pct"],
        "rate_bps": net["rate_bps"],
        "queue_bdp": net["queue_bdp"],
        "warmup_ns": meta["warmup_ns"],
        "measure_ns": meta["measure_ns"],
        "window_ns": meta["window"]["duration_ns"],
        "server_cpu_ns": meta["server"]["cpu"]["user_ns"] + meta["server"]["cpu"]["system_ns"],
        "client_cpu_ns": meta["client"]["cpu"]["user_ns"] + meta["client"]["cpu"]["system_ns"],
        "server_cores_busy_ns": cores_busy(meta["server"]),
        "client_cores_busy_ns": cores_busy(meta["client"]),
        "sent_bytes_per_s": results["sent_bytes_per_s"],
        "received_bytes_per_s": results["received_bytes_per_s"],
        "in_flight_mean": results["in_flight_mean"],
        "in_flight_max": results["in_flight_max"],
        "tls": " / ".join(tls.get(k, "") for k in ("version", "cipher_suite", "group")),
        "problems": len(meta["problems"] or []),
        "commit": meta["build"].get("commit", ""),
        "dirty": meta["build"].get("dirty", False),
    }


def read_series(path: str | Path) -> Series:
    """Reads the finished runs of the series in path, in the order of its
    plan."""
    path = Path(path)
    with open(path / "series.json", encoding="utf-8") as f:
        plan = json.load(f)
    rows, calls, missing = [], [], []
    for planned in plan["runs"]:
        run_dir = path / planned["id"]
        try:
            with open(run_dir / "meta.json", encoding="utf-8") as f:
                meta = json.load(f)
        except FileNotFoundError:
            missing.append(planned["id"])
            continue
        rows.append(run_row(planned, meta))
        c = pd.read_csv(run_dir / "calls.csv", dtype=CALL_DTYPES)
        c.insert(0, "run_id", planned["id"])
        calls.append(c)
    if not rows:
        raise ValueError(f"{path}: no finished runs")
    return Series(runs=normalize_setups(pd.DataFrame(rows)), calls=_categorize(pd.concat(calls, ignore_index=True)), missing=missing)


def _categorize(calls: pd.DataFrame) -> pd.DataFrame:
    for column in ("run_id", "kind", "phase"):
        calls[column] = calls[column].astype("category")
    return calls


def write_tables(series: Series, out: str | Path) -> None:
    """Writes runs.csv and calls.parquet into out."""
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    normalize_setups(series.runs).to_csv(out / RUNS_FILE, index=False)
    series.calls.to_parquet(out / CALLS_FILE, compression="zstd", index=False)


def read_tables(path: str | Path) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Reads runs.csv and calls.parquet from path."""
    path = Path(path)
    runs = pd.read_csv(path / RUNS_FILE, keep_default_na=False, na_values=[])
    calls = _categorize(pd.read_parquet(path / CALLS_FILE))
    return normalize_setups(runs), calls
