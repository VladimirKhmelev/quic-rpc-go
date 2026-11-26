"""The raw-to-report path, using small, deterministic on-disk runs."""

import csv
from itertools import product
import json
from pathlib import Path

import pandas as pd
import pytest

from qrpcplots import __main__ as cli, data, main_data, main_tables, report, stats
from qrpcplots.main_design import H2, MATRICES, QUIC

from conftest import NETWORKS


def setup(variant):
    transport = variant.removesuffix("-tcp-default").removesuffix("-9m")
    is_pool = variant.startswith("grpc-h2-pool-")
    return {"transport": "grpc-h2-pool" if is_pool else transport,
            "tcp": "default" if variant.endswith("-tcp-default") else "aligned",
            "pool_size": int(variant[-1]) if is_pool else 1, "pool_policy": "round-robin" if is_pool else "single",
            "max_recv_msg_bytes": (9 if variant.endswith("-9m") else 4) << 20,
            "max_send_msg_bytes": 9 << 20 if variant.endswith("-9m") else 0}


def make_plan(root: Path, block: str) -> dict:
    spec = MATRICES[block]
    items = []
    for network, load, repeat, role in [
        *(('base', l, 0, 'drift-start') for l in spec.loads),
        *((n, l, r, '') for n, l, r in product(spec.networks, spec.loads, range(1, spec.repeats + 1))),
        *(('base', l, 0, 'drift-end') for l in spec.loads),
    ]:
        for variant in reversed(spec.variants):
            items.append({"id": f"{len(items):04d}-{variant}-{network}-{load}-{role or f'r{repeat}'}",
                          "variant": variant, **setup(variant), "repeat": repeat,
                          "role": role, "network": network, "load": load, "seed": max(repeat, 1)})
    loads = [{"name": l, **({"bulk": {"total_bytes": 100 << 20, "chunk_bytes": 64 << 10,
                                       "timeout_ns": 60_000_000_000}} if l == "bulk" else {})} for l in spec.loads]
    plan = {"matrix": {"name": "main-" + block, "repeats": spec.repeats, "drift": "base",
            "warmup_ns": 0 if block == "bulk" else 5_000_000_000, "duration_ns": 30_000_000_000,
            "networks": list(spec.networks), "loads": list(spec.loads),
            "variants": [{"name": v, **setup(v)} for v in spec.variants]},
            "runs": items, "networks": [{"name": n, **NETWORKS.get(n, NETWORKS["base"])} for n in sorted(set(spec.networks) | {"base"})],
            "loads": loads}
    directory = root / block
    directory.mkdir(parents=True)
    (directory / "series.json").write_text(json.dumps(plan), encoding="utf-8")
    return plan


def finish(root: Path, block: str, plan: dict, item: dict, short: bool = False) -> Path:
    run = root / block / item["id"]
    run.mkdir()
    matrix = plan["matrix"]
    load, network = (next(row for row in plan[f"{key}s"] if row["name"] == item[key]) for key in ("load", "network"))
    warmup, measure = matrix["warmup_ns"], matrix["duration_ns"]
    meta = {"run_id": item["id"], **setup(item["variant"]), "seed": item["seed"], "network": network, "load": load,
            "warmup_ns": warmup, "measure_ns": measure, "window": {"duration_ns": measure},
            "server": {"cpus": "0", "cpu": {"user_ns": 80_000, "system_ns": 20_000}},
            "client": {"cpus": "1", "cpu": {"user_ns": 50_000, "system_ns": 20_000}},
            "cores": [{"id": i, "user_ns": 150_000, "nice_ns": 0, "system_ns": 20_000, "irq_ns": 0,
                       "softirq_ns": 10_000, "steal_ns": 0} for i in (0, 1)],
            "results": {"sent_bytes_per_s": 1000, "received_bytes_per_s": 2000, "in_flight_mean": 1, "in_flight_max": 2},
            "tls": [], "problems": [], "build": {"commit": "test-build", "dirty": False}}
    kind = "echo" if item["load"] == "bidi" else "small"
    latency = 60_000_000 if item["variant"].startswith("quic") else 99_000_000
    offsets = (1, 2, 3) if short else (1, 7, 24)
    rows = [(seq, kind, warmup + s * 10**9, latency + seq * 1_000_000, 0, 500, 500, 0, "measure", 0)
            for seq, s in enumerate(offsets)]
    if item["load"] == "reconnect":
        rows += [(3, "connect", warmup + 5 * 10**9, latency * 2, 0, 0, 0, 1, "measure", 0)]
    if item["load"] == "bulk":
        size = load["bulk"]["total_bytes"]
        meta["results"]["bulk"] = {**load["bulk"], "received_bytes": size, "chunks": 1600,
                                 "duration_ns": measure, "code": 0, "completed": True}
        rows = [(0, "bulk", warmup, measure, 0, 0, size, 0, "measure", 1600)]
    (run / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
    with (run / "calls.csv").open("w", newline="", encoding="utf-8") as stream:
        writer = csv.writer(stream)
        writer.writerow(data.CALL_DTYPES)
        writer.writerows(rows)
    return run


def compact_main(root: Path) -> Path:
    """Writes partial series for every figure and one full comparison cell."""
    examples = {
        "loss-axis": [("loss-1", "small"), ("loss-2", "small"), ("base", "small-heavy"), ("base", "reconnect")],
        "one-factor": [("burst-1", "small"), ("rtt-150-jitter-30", "small"), ("reorder-1", "small")],
        "rate-1": [("rate-1", "small-50")], "rate-10": [("congested", "small-heavy-10mbit")],
        "datacenter": [("datacenter", "small"), ("datacenter", "bidi"), ("datacenter", "reconnect")],
        "compound": [("nearby-zones", "small-heavy")], "loss-rtt": [("rtt-20-loss-2", "small")],
        "sweep-intensity": [("base", "small-50")], "sweep-response-size": [("base", "small-heavy-8m")],
        "sweep-pool": [("base", "small")], "bulk": [("base", "bulk")],
    }
    for block, cells in examples.items():
        plan = make_plan(root, block)
        for item in plan["runs"]:
            cell = (item["network"], item["load"])
            full = (block, *cell) in (("loss-axis", "loss-1", "small"), ("one-factor", "reorder-1", "small")) and item["variant"] in (QUIC, H2)
            if cell in cells and (item["repeat"] == 1 or full) and not item["role"]:
                finish(root, block, plan, item)
    return root


def test_empty_main_has_coverage_and_no_inferred_verdict(tmp_path):
    assert cli.main(["main", str(tmp_path)]) == 0
    out = tmp_path / "analysis"
    coverage = pd.read_csv(out / "coverage.csv")
    assert len(coverage) == 11 and coverage["planned"].sum() == 12495
    assert coverage["finished"].sum() == 0 and coverage["missing"].sum() == 12495
    assert set(pd.read_csv(out / "hypotheses.csv")["verdict"]) == {"insufficient"}
    assert pd.read_csv(out / "runs.csv").empty
    assert "не хватило данных" in (out / "report.md").read_text()


def test_partial_raw_report_and_plots(tmp_path):
    root = compact_main(tmp_path / "data")
    main = main_data.read(root)
    assert main.coverage["planned"].sum() == 12495
    assert len(main.missing) + len(main.runs) == 12495
    assert set(main.ecdf) == set(MATRICES["loss-axis"].variants)
    assert main.summaries[main.summaries["kind"].eq("connect")]["p50"].gt(0).all()
    out = tmp_path / "output"
    assert cli.main(["main", str(root), "-o", str(out)]) == 0
    expected = {"fig01-loss-small", "fig02-loss-small-heavy", "fig03-grid", "fig04-ecdf-loss-2", "fig05-pool", "fig06-loss-burst",
                "fig07-rtt-jitter", "fig08-reorder", "fig09-bandwidth", "fig10-connections", "fig11-cpu",
                "fig13-profiles-small", "fig13-profiles-small-heavy", "fig13-profiles-bidi", "fig13-profiles-reconnect",
                "fig14-bulk", "sweep-intensity", "sweep-response-size"}
    assert {p.stem for p in out.glob("*.png")} == expected
    assert all(p.stat().st_size > 1000 for p in out.glob("*.png"))
    verdicts = pd.read_csv(out / "hypothesis_cells.csv")
    complete = verdicts[verdicts["family"].eq("H1") & verdicts["network"].eq("loss-1") & verdicts["load"].eq("small")].iloc[0]
    assert complete["outcome"] == "support" and complete["observed"] == "quic=25 / grpc-h2=25"
    assert set(pd.read_csv(out / "hypotheses.csv")["verdict"]) == {"insufficient"}
    assert len(pd.read_csv(out / "bulk.csv")) == 5
    assert "Готово" in (out / "report.md").read_text()


def test_main_rejects_duplicate_plan_and_metadata_mismatch(tmp_path):
    plan = make_plan(tmp_path, "loss-axis")
    item = next(item for item in plan["runs"] if item["role"] == "")
    run = finish(tmp_path, "loss-axis", plan, item)
    meta_path = run / "meta.json"
    meta = json.loads(meta_path.read_text())
    meta_path.write_text(json.dumps({**meta, "seed": 17}))
    with pytest.raises(ValueError, match="seed or TCP"):
        main_data.read(tmp_path)
    meta_path.write_text(json.dumps(meta))
    plan["runs"][0]["id"] = plan["runs"][1]["id"]
    (tmp_path / "loss-axis/series.json").write_text(json.dumps(plan))
    with pytest.raises(ValueError, match="duplicate run"):
        main_data.read(tmp_path)


def test_short_window_produces_absent_warmup_estimate(tmp_path):
    plan = make_plan(tmp_path, "loss-axis")
    item = next(item for item in plan["runs"] if item["load"] == "small" and item["role"] == "")
    finish(tmp_path, "loss-axis", plan, item, short=True)
    main = main_data.read(tmp_path)
    assert main_tables.tables(main)["warmup"]["head_ratio"].isna().all()
    old = data.read_series(tmp_path / "loss-axis")
    assert stats.warmup(old.runs, old.calls)["head_ratio"].isna().all()
    output = tmp_path / "legacy-report"
    report.report(old.runs, old.calls, output)
    assert "Оценка отсутствует" in (output / "report.md").read_text()
