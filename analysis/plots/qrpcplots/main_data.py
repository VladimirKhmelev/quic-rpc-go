"""Streams the raw runs of the Q-05 matrices, retaining compact summaries."""

from dataclasses import dataclass
from itertools import product
import json
from pathlib import Path
import re

import numpy as np
import pandas as pd

from . import data, stats
from .main_design import MATRICES, LIMITED_PAIR

CPU = {"server_cpu_us": "server_cpu_ns", "server_cores_us": "server_cores_busy_ns",
       "client_cpu_us": "client_cpu_ns", "client_cores_us": "client_cores_busy_ns"}


@dataclass
class MainData:
    runs: pd.DataFrame
    summaries: pd.DataFrame
    coverage: pd.DataFrame
    missing: pd.DataFrame
    ecdf: dict[str, list[np.ndarray]]


def same_members(actual: list, expected) -> bool:
    return len(actual) == len(expected) and set(actual) == set(expected)


def variant_setup(name: str) -> tuple:
    if name.startswith("grpc-h2-pool-"):
        transport, pool, policy = "grpc-h2-pool", int(name.rsplit("-", 1)[1]), "round-robin"
    else:
        transport, pool, policy = name.removesuffix("-9m").removesuffix("-tcp-default"), 1, "single"
    tcp = "default" if name == "grpc-h2-tcp-default" else "aligned"
    limits = (9 << 20, 9 << 20) if name in LIMITED_PAIR else (4 << 20, 0)
    return transport, tcp, pool, policy, *limits


def actual_setup(record: dict, transport: str) -> tuple:
    return transport, record["tcp"], *data.connection_setup(record, transport), *data.message_limits(record)


def validate_plan(plan: dict, block: str) -> tuple[dict, dict]:
    """Checks the planned cells, repetitions and variant setups before ingest."""
    spec = MATRICES[block]
    matrix = plan["matrix"]
    if (matrix["name"], matrix["repeats"], matrix["warmup_ns"], matrix["duration_ns"], matrix["drift"]) != (
        f"main-{block}", spec.repeats, 0 if block == "bulk" else 5_000_000_000, 30_000_000_000, "base"
    ) or not same_members(matrix["networks"], spec.networks) or not same_members(matrix["loads"], spec.loads):
        raise ValueError(f"{block}: matrix differs from Q-05")
    if not same_members([v["name"] for v in matrix["variants"]], spec.variants):
        raise ValueError(f"{block}: planned variant names differ from Q-05")
    for variant in matrix["variants"]:
        if actual_setup(variant, variant["transport"]) != variant_setup(variant["name"]):
            raise ValueError(f"{block}: invalid setup for {variant['name']}")
    networks = {p["name"]: p for p in plan["networks"]}
    loads = {p["name"]: p for p in plan["loads"]}
    if (set(networks) != set(spec.networks) | {"base"} or len(networks) != len(plan["networks"])
            or set(loads) != set(spec.loads) or len(loads) != len(plan["loads"])):
        raise ValueError(f"{block}: missing or duplicate profile definitions")
    wanted = set(product(spec.networks, spec.loads, spec.variants, range(1, spec.repeats + 1), ("",)))
    wanted.update(product(("base",), spec.loads, spec.variants, (0,), ("drift-start", "drift-end")))
    width = len(spec.variants)
    edge = width * len(spec.loads)
    runs = plan["runs"]
    for offset in range(0, len(runs), width):
        batch = runs[offset:offset + width]
        keys = {(r["network"], r["load"], r["repeat"], r.get("role", "")) for r in batch}
        role = "drift-start" if offset < edge else "drift-end" if offset >= len(runs) - edge else ""
        if len(keys) != 1 or next(iter(keys))[-1] != role or not same_members([r["variant"] for r in batch], spec.variants):
            raise ValueError(f"{block}: mixed, incomplete or misplaced batch at run {offset}")
    ids = set()
    for run in plan["runs"]:
        key = (run["network"], run["load"], run["variant"], run["repeat"], run.get("role", ""))
        if key not in wanted or run["id"] in ids or not re.fullmatch(r"[a-z0-9-]+", run["id"]):
            raise ValueError(f"{block}: unexpected or duplicate run {run['id']}")
        if run["seed"] != max(run["repeat"], 1) or actual_setup(run, run["transport"]) != variant_setup(run["variant"]):
            raise ValueError(f"{block}: seed or setup differs from plan for {run['id']}")
        wanted.remove(key)
        ids.add(run["id"])
    if wanted:
        raise ValueError(f"{block}: {len(wanted)} runs missing from the plan")
    return networks, loads


def summarize(row: dict, calls: pd.DataFrame, ecdf: dict) -> list[dict]:
    """Keeps exactly one row per run and measured kind, excluding bulk."""
    if row["bulk_total_bytes"]:
        return []
    measured = calls[calls["phase"] == "measure"]
    if measured.empty:
        return []
    cpu = {column: row[source] / (len(measured) * 1000) for column, source in CPU.items()}
    goodput = 8 * (row["sent_bytes_per_s"] + row["received_bytes_per_s"]) / 1e6
    rows = []
    for kind, group in measured.groupby("kind", observed=True):
        if (row["series"], row["network"], row["load"], row["role"], kind) == ("loss-axis", "loss-2", "small", "", "small"):
            ecdf.setdefault(row["variant"], []).append(group["latency_ns"].to_numpy(copy=True))
        w = row["warmup_ns"]
        rows.append({**row, **stats.summarize_calls(group, w, row["measure_ns"]), **cpu, "kind": kind,
                     "head_ratio": stats.warmup_ratio(group, w), "goodput_mbps": goodput})
    return rows


def read(root: str | Path) -> MainData:
    root = Path(root)
    if not root.is_dir():
        raise FileNotFoundError(f"main series directory: {root}")
    runs, summaries, coverage, missing, ecdf = [], [], [], [], {}
    all_networks, all_loads = {}, {}
    for block, spec in MATRICES.items():
        directory = root / block
        path = directory / "series.json"
        expected = len(spec.variants) * len(spec.loads) * (len(spec.networks) * spec.repeats + 2)
        progress = {"series": block, "planned": expected, "finished": 0, "missing": expected, "status": "no-plan"}
        coverage.append(progress)
        if not path.exists():
            continue
        plan = json.loads(path.read_text(encoding="utf-8"))
        networks, loads = validate_plan(plan, block)
        for source, past in ((networks, all_networks), (loads, all_loads)):
            for name, definition in source.items():
                if past.setdefault(name, definition) != definition:
                    raise ValueError(f"{block}: inconsistent definition of {name!r} across matrices")
        for planned in plan["runs"]:
            item = data.read_run(directory, planned, loads, networks)
            if item is None:
                missing.append({"series": block, "run_id": planned["id"], "network": planned["network"],
                                "load": planned["load"], "variant": planned["variant"], "role": planned.get("role", ""),
                                "repeat": planned["repeat"]})
                continue
            row, calls = item
            if not row["bulk_total_bytes"] and (row["warmup_ns"], row["measure_ns"]) != (
                plan["matrix"]["warmup_ns"], plan["matrix"]["duration_ns"]
            ):
                raise ValueError(f"{block}/{row['run_id']}: measurement window differs from plan")
            row["series"] = block
            runs.append(row)
            summaries.extend(summarize(row, calls, ecdf))
            progress["finished"] += 1
            del item, calls
        progress["missing"] -= progress["finished"]
        progress["status"] = "partial" if progress["missing"] else "complete"
    revisions = {row["commit"] for row in runs if row["commit"]}
    if len(revisions) > 1:
        raise ValueError("main series contains different build revisions")
    missing_columns = ["series", "run_id", "network", "load", "variant", "role", "repeat"]
    run_keys = ["series", "run_id", "network", "load", "variant", "repeat", "role"]
    return MainData(pd.DataFrame(runs) if runs else pd.DataFrame(columns=run_keys),
                    pd.DataFrame(summaries) if summaries else pd.DataFrame(columns=[*run_keys, "kind", "head_ratio"]), pd.DataFrame(coverage),
                    pd.DataFrame(missing, columns=missing_columns), ecdf)
