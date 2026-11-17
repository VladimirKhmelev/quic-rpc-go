"""Finite server-stream transfers, kept separate from open-loop latency cells."""

from __future__ import annotations

from pathlib import Path

import numpy as np
import pandas as pd

from . import stats


def tables(runs: pd.DataFrame) -> tuple[pd.DataFrame, pd.DataFrame]:
    """Returns one row per attempted transfer and aggregates over runs.

    Failed transfers retain partial progress and observed throughput; successful
    goodput and CPU estimates use only completed transfers. Chunks are never
    statistical samples.
    """
    transfers = runs[runs["bulk_total_bytes"] > 0].copy()
    transfers["duration_s"] = transfers["bulk_duration_ns"] / 1e9
    transfers["observed_mbps"] = transfers["bulk_received_bytes"] * 8 / 1e6 / transfers["duration_s"]
    transfers["goodput_mbps"] = transfers["observed_mbps"].where(transfers["bulk_completed"])
    mib = (transfers["bulk_received_bytes"] / (1 << 20)).replace(0, np.nan)
    metrics = ["duration_s", "goodput_mbps"]
    for side in ("server", "client"):
        for suffix, source in (("cpu", "cpu_ns"), ("cores", "cores_busy_ns")):
            column = f"{side}_{suffix}_us_per_mib"
            transfers[column] = transfers[f"{side}_{source}"] / 1e3 / mib
            metrics.append(column)
    rows = []
    keys = ["network", "load", "variant", "bulk_total_bytes", "bulk_chunk_bytes", "bulk_timeout_ns"]
    measured = transfers[transfers["role"] == ""]
    for key, group in measured.groupby(keys, observed=True):
        complete = group[group["bulk_completed"]]
        row = dict(zip(keys, key, strict=True))
        row.update(runs=len(group), completed=len(complete), errors=len(group) - len(complete))
        for metric in metrics:
            values = complete[metric].dropna().to_numpy()
            if len(values) >= 2:
                median, lo, hi = stats.bootstrap_median(values, stats.rng_for("bulk", *key, metric))
            elif len(values) == 1:
                median, lo, hi = float(values[0]), float("nan"), float("nan")
            else:
                median = lo = hi = float("nan")
            row.update({metric: median, metric + "_lo": lo, metric + "_hi": hi})
        rows.append(row)
    return transfers, pd.DataFrame(rows)


def write(runs: pd.DataFrame, out: Path) -> list[Path]:
    """Writes transfer and run-aggregate tables without changing pilot reports."""
    transfers, summary = tables(runs)
    paths = [out / "bulk_runs.csv", out / "bulk.csv"]
    for table, path in zip((transfers, summary), paths, strict=True):
        table.to_csv(path, index=False, float_format="%.9g")
    return paths


def text(runs: pd.DataFrame) -> str:
    """Returns a Russian report section with explicit finite-work semantics."""
    transfers, summary = tables(runs)
    lines = ["## Передача большого объёма", "",
             f"Передач: {len(transfers)}, завершено: {int(transfers['bulk_completed'].sum())}. "
             "Один прогон — один измеряемый server-stream; блоки не являются независимыми наблюдениями.", "",
             "Goodput считается по фактической длительности до конечного статуса. "
             "Незавершённые передачи и полученные до ошибки байты сохранены в bulk_runs.csv; "
             "они исключены из оценок успешного goodput и CPU на МиБ. "
             "CPU процессов и их ядер охватывает окно с подтверждёнными границами; "
             "его длительность window_ns сохранена отдельно от времени передачи.", "",
             "| Сеть | Вариант | Успешно / прогонов | Goodput, Мбит/с | CPU сервера, мкс/МиБ | CPU клиента, мкс/МиБ |",
             "|---|---|---|---|---|---|"]
    if not summary.empty and (summary["completed"] < 2).any():
        lines[-2:-2] = ["При единственном успешном прогоне интервал не оценивается; "
                  "такие числа описывают приёмочный запуск и не обосновывают сравнительные выводы.", ""]
    for row in summary.to_dict("records"):
        fmt = lambda v: "—" if pd.isna(v) else f"{v:.6g}"
        lines.append(f"| {row['network']} | {row['variant']} | {row['completed']} / {row['runs']} | "
                     f"{fmt(row['goodput_mbps'])} | {fmt(row['server_cpu_us_per_mib'])} | {fmt(row['client_cpu_us_per_mib'])} |")
    return "\n".join(lines) + "\n"
