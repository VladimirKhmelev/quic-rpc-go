"""Memory-stack and QUIC first-reply benchmarks, separate from network series."""

from __future__ import annotations

import itertools
import hashlib
import json
import math
from pathlib import Path
import re

import numpy as np
import pandas as pd

from . import stats

BACKENDS = ("direct", "protobuf", "rpc-mem", "grpc-bufconn")
KINDS = ("unary", "bidi")
SIZES = (0, 100, 1024, 65536)
MODES = ("cold", "resumed", "early")
RTTS = (0, 50)
MEMORY_RE = re.compile(r"BenchmarkRPCOverhead/Backend=([a-z-]+)/Kind=([a-z]+)/Bytes=(\d+)(?:-(\d+))?$")
EARLY_RE = re.compile(r"BenchmarkEarlyReconnect/RTT=(\d+)ms/Mode=([a-z]+)(?:-(\d+))?$")
UNITS = {
    "ns/op": "ns_per_op", "B/op": "bytes_per_op", "allocs/op": "allocs_per_op",
    "first_reply_ns": "first_reply_ns", "first_reply_ms": "reported_first_reply_ms",
    "RTT/reply": "reply_rtts", "rtt_ms": "reported_rtt_ms", "did_resume": "did_resume",
    "used_0rtt": "used_0rtt", "tls_version": "tls_version", "tls_cipher_suite": "tls_cipher_suite",
    "tls_curve": "tls_curve", "payload_bytes": "reported_payload_bytes",
}
GROUP = ["family", "backend", "kind", "payload_bytes", "rtt_ms", "mode", "cpu"]


def parse_job(text: str, family: str, cpu: int) -> list[dict]:
    """Rejects incomplete, failed or contradictory benchmark batches."""
    if family not in ("overhead", "reconnect") or cpu < 1:
        raise ValueError("invalid microbenchmark family or CPU count")
    lines = text.splitlines()
    if "PASS" not in [s.strip() for s in lines] or any(s.startswith(("FAIL", "--- FAIL")) for s in lines):
        raise ValueError("benchmark process did not pass")
    rows = []
    for line in lines:
        if not line.startswith("Benchmark"):
            continue
        fields = line.split()
        if len(fields) < 4 or len(fields) % 2:
            raise ValueError(f"incomplete benchmark line: {line}")
        name, count = fields[:2]
        if not count.isdigit() or int(count) < 1:
            raise ValueError("invalid operation count")
        row = {"family": family, "benchmark": name, "iterations": int(count)}
        for value, unit in zip(fields[2::2], fields[3::2], strict=True):
            if unit not in UNITS or UNITS[unit] in row:
                raise ValueError(f"unknown or duplicate benchmark metric {unit}")
            number = float(value)
            if not math.isfinite(number) or number < 0:
                raise ValueError(f"invalid metric {unit}")
            row[UNITS[unit]] = number
        if row.get("ns_per_op", 0) <= 0:
            raise ValueError("missing positive ns/op")
        if family == "overhead":
            m = MEMORY_RE.fullmatch(name)
            if not m:
                raise ValueError(f"unexpected memory benchmark {name}")
            backend, kind, size, suffix = m.groups()
            row.update(backend=backend, kind=kind, payload_bytes=int(size), mode="", rtt_ms=0)
            if not {"bytes_per_op", "allocs_per_op"} <= row.keys():
                raise ValueError("missing allocation metrics")
        else:
            m = EARLY_RE.fullmatch(name)
            if not m:
                raise ValueError(f"unexpected reconnect benchmark {name}")
            rtt, mode, suffix = m.groups()
            row.update(backend="quic", kind="unary", payload_bytes=8, mode=mode, rtt_ms=int(rtt))
            required = {"first_reply_ns", "reported_first_reply_ms", "reported_rtt_ms", "did_resume",
                        "used_0rtt", "tls_version", "tls_cipher_suite", "tls_curve", "reported_payload_bytes"}
            if not required <= row.keys() or row["first_reply_ns"] <= 0:
                raise ValueError("missing first-reply or TLS metrics")
            if (row["did_resume"], row["used_0rtt"]) != (float(mode != "cold"), float(mode == "early")):
                raise ValueError("TLS mode contradicts benchmark name")
            if row["tls_version"] != 0x0304 or row["tls_curve"] != 4588 or row["tls_cipher_suite"] not in (4865, 4866, 4867):
                raise ValueError("unexpected TLS configuration")
            if row["reported_rtt_ms"] != row["rtt_ms"] or row["reported_payload_bytes"] != 8:
                raise ValueError("RTT or payload metric contradicts setup")
            row["first_reply_ms"] = row["first_reply_ns"] / 1e6
            # Go prints custom metrics with four significant digits.
            if not math.isclose(row["reported_first_reply_ms"], row["first_reply_ms"], rel_tol=0.001):
                raise ValueError("first-reply units disagree")
            if row["rtt_ms"]:
                if not math.isclose(row.get("reply_rtts", -1), row["first_reply_ms"] / row["rtt_ms"], rel_tol=0.001):
                    raise ValueError("RTT/reply metric disagrees")
            elif "reply_rtts" in row:
                raise ValueError("normalized RTT is undefined at zero added RTT")
        row["cpu"] = int(suffix or 1)
        if row["cpu"] != cpu:
            raise ValueError("benchmark CPU count differs from plan")
        rows.append(row)
    if family == "overhead":
        actual = [(r["backend"], r["kind"], r["payload_bytes"]) for r in rows]
        expected = set(itertools.product(BACKENDS, KINDS, SIZES))
    else:
        actual = [(r["rtt_ms"], r["mode"]) for r in rows]
        expected = set(itertools.product(RTTS, MODES))
    if len(actual) != len(set(actual)) or set(actual) != expected:
        raise ValueError("missing, duplicate or unexpected benchmark cases")
    return rows


def read(path: str | Path, *, manifest: str = "meta.json") -> tuple[dict, pd.DataFrame]:
    """Reads a completed collection with exactly one job per family/repeat."""
    path = Path(path)
    meta = json.loads((path / manifest).read_text())
    if meta.get("schema_version") != 1 or meta.get("kind") != "rpc-micro":
        raise ValueError("unsupported microbenchmark schema")
    config = meta["config"]
    for name in ("cpu", "repeats", "reconnect_iterations"):
        if type(config.get(name)) is not int or config[name] < 1:
            raise ValueError(f"invalid microbenchmark config: {name}")
    seen, orders, rows = set(), set(), []
    for job in meta["jobs"]:
        if type(job["repeat"]) is not int or type(job["order"]) is not int:
            raise ValueError("invalid repeat or job order")
        key = (job["family"], job["repeat"])
        if key in seen or job["order"] in orders or job["returncode"] != 0:
            raise ValueError("duplicate or failed microbenchmark job")
        seen.add(key)
        orders.add(job["order"])
        raw = Path(job["raw"])
        if raw.is_absolute() or ".." in raw.parts or not (path / raw).resolve().is_relative_to(path.resolve()):
            raise ValueError("invalid raw result path")
        content = (path / raw).read_bytes()
        if hashlib.sha256(content).hexdigest() != job["raw_sha256"]:
            raise ValueError("raw benchmark checksum mismatch")
        parsed = parse_job(content.decode(), job["family"], config["cpu"])
        if job["family"] == "reconnect" and any(r["iterations"] != config["reconnect_iterations"] for r in parsed):
            raise ValueError("reconnect operation count differs from plan")
        for row in parsed:
            rows.append({"repeat": job["repeat"], "job_order": job["order"], **row})
    expected = set(itertools.product(("overhead", "reconnect"), range(1, config["repeats"] + 1)))
    if seen != expected or orders != set(range(1, 2 * config["repeats"] + 1)):
        raise ValueError("collection does not match its planned repetitions")
    return meta, pd.DataFrame(rows)


def estimate(values: np.ndarray, *key: object) -> tuple[float, float, float]:
    if len(values) < 2:
        return float(values[0]), float("nan"), float("nan")
    return stats.bootstrap_median(values, stats.rng_for("micro", *key))


def tables(rows: pd.DataFrame) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Summarizes batch means without weighting by their operation counts."""
    summaries = []
    for key, group in rows.groupby(GROUP, observed=True):
        metrics = ("ns_per_op", "bytes_per_op", "allocs_per_op") if key[0] == "overhead" else ("first_reply_ms",)
        for metric in metrics:
            median, lo, hi = estimate(group[metric].to_numpy(), *key, metric)
            summaries.append({**dict(zip(GROUP, key, strict=True)), "metric": metric, "repeats": len(group),
                              "median": median, "lo": lo, "hi": hi})
    pairs = []
    reconnect = rows[rows["family"] == "reconnect"]
    for rtt, group in reconnect.groupby("rtt_ms"):
        paired = group.pivot(index="repeat", columns="mode", values="first_reply_ms")
        gain = (paired["resumed"] - paired["early"]).to_numpy()
        median, lo, hi = estimate(gain, "gain", rtt)
        pairs.append({"rtt_ms": rtt, "repeats": len(gain), "gain_ms": median,
                      "gain_lo_ms": lo, "gain_hi_ms": hi, "gain_rtts": median / rtt if rtt else float("nan")})
    return pd.DataFrame(summaries), pd.DataFrame(pairs)


def report(path: str | Path, out: str | Path | None = None) -> list[Path]:
    meta, rows = read(path)
    out = Path(out) if out else Path(path) / "analysis"
    out.mkdir(parents=True, exist_ok=True)
    summary, gains = tables(rows)
    paths = []
    for name, table in (("benchmarks", rows), ("summary", summary), ("early_gain", gains)):
        target = out / (name + ".csv")
        table.to_csv(target, index=False, float_format="%.12g")
        paths.append(target)
    text = ["# Замеры RPC в памяти и времени до первого ответа", "",
            f"Повторов: {meta['config']['repeats']}; GOMAXPROCS: {meta['config']['cpu']}. "
            "Единица анализа — отдельный запуск benchmark-процесса, а не его операция.", "",
            "## Стек в памяти", "",
            "Медиана средних ns/op по повторам; B/op и allocs/op — в summary.csv. "
            "Оба RPC-стека без сети, TLS и компрессии. direct и protobuf — контроли обработчика "
            "и кодека; их времена не вычитаются из RPC как изолированная причинная оценка.", "",
            "| Шаблон | Payload, Б | Эталон | ns/op |", "|---|---|---|---|"]
    for r in summary[(summary["family"] == "overhead") & (summary["metric"] == "ns_per_op")].to_dict("records"):
        text.append(f"| {r['kind']} | {r['payload_bytes']} | {r['backend']} | {r['median']:.6g} |")
    text += ["", "## Время до первого ответа", "",
             "Медиана средних first_reply_ms по повторам. Включает подключение и первый unary-ответ, "
             "исключает создание ClientConn, проверку состояния и закрытие. ns/op Go benchmark "
             "включает эти действия и не подменяет first_reply_ms.", "",
             "| Добавленный RTT, мс | Режим | Первый ответ, мс |", "|---|---|---|"]
    for r in summary[summary["family"] == "reconnect"].to_dict("records"):
        text.append(f"| {r['rtt_ms']} | {r['mode']} | {r['median']:.6g} |")
    text += ["", "Выигрыш resumed − early по парным повторам:", "",
             "| Добавленный RTT, мс | Выигрыш, мс | В долях RTT |", "|---|---|---|"]
    for r in gains.to_dict("records"):
        ratio = "—" if pd.isna(r["gain_rtts"]) else f"{r['gain_rtts']:.6g}"
        text.append(f"| {r['rtt_ms']} | {r['gain_ms']:.6g} | {ratio} |")
    text += ["", "95%-интервалы bootstrap по повторам (10 000 выборок, воспроизводимый RNG) — в CSV; "
             "при одном повторе интервал не оценивается. Полезный payload reconnect — 8 Б, "
             "TLS 1.3 / X25519MLKEM768; DidResume и Used0RTT проверяются в каждой операции.", "",
             "Это отдельный опыт со стоимостью реализаций и первым ответом. Нет результатов H1–H6 "
             "основной серии, процентилей отдельных вызовов или автоматического включения 0-RTT в Wisply. "
             "Порядок внутри семейства фиксирован; порядок двух семейств по повторам задан сидом. "
             "У mem и bufconn разные модели буферов и flow control; CPU клиента и сервера здесь не разделены. "
             "Хост автоматически не настраивался.", ""]
    target = out / "report.md"
    target.write_text("\n".join(text), encoding="utf-8")
    return paths + [target]
