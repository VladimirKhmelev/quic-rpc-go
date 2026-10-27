"""The tables, figures and summary of a series, from runs.csv and
calls.parquet (ANL-1)."""

from __future__ import annotations

from pathlib import Path

import numpy as np
import pandas as pd

from . import figures, stats

# The figures of the loss axis: (load, number of the figure).
LOSS_FIGURES = [("small", 1), ("small-heavy", 2)]
KIND = "small"
ECDF_LOSS = 2.0
REGIMES = {
    "steady": "без перегрузки",
    "other-overloaded": "перегружен другой",
    "quic-overloaded": "перегружен QUIC",
    "both-overloaded": "перегружены оба",
}


def fmt(x: float) -> str:
    """Formats a number with three significant digits."""
    if x is None or (isinstance(x, float) and np.isnan(x)):
        return "—"
    return f"{x:.3g}"


def decimal(x: float) -> str:
    """Formats a number with a decimal comma, as the Russian text does."""
    return f"{x:g}".replace(".", ",")


def markdown(table: pd.DataFrame) -> str:
    """Renders a table as Markdown."""
    head = "| " + " | ".join(table.columns) + " |"
    rule = "|" + "---|" * len(table.columns)
    rows = ["| " + " | ".join(str(v) for v in row) + " |" for row in table.itertuples(index=False)]
    return "\n".join([head, rule, *rows])


def report(runs: pd.DataFrame, calls: pd.DataFrame, out: str | Path) -> list[Path]:
    """Writes the tables, the figures and report.md into out and returns the
    files written."""
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    summary = stats.run_summary(runs, calls)
    tables = {
        "summary": summary,
        "cells": stats.cells(summary),
        "comparisons": stats.comparisons(summary),
        "spread": stats.spread(summary),
        "cpu": stats.cpu(summary),
        "warmup": stats.warmup(runs, calls),
        "drift": stats.drift(summary),
    }
    written = []
    for name, table in tables.items():
        path = out / f"{name}.csv"
        table.to_csv(path, index=False, float_format="%.6g")
        written.append(path)

    cells = tables["cells"]
    drawn = []
    for load, number in LOSS_FIGURES:
        path = out / f"fig{number:02d}-loss-{load}.png"
        if figures.fig_loss(cells, runs, load, KIND, number, path):
            drawn.append(path)
    axis = figures.loss_axis(runs)
    if axis:
        network, _ = min(axis, key=lambda a: abs(a[1] - ECDF_LOSS))
        path = out / f"fig04-ecdf-{network}.png"
        if figures.fig_ecdf(calls, runs, network, "small", KIND, path):
            drawn.append(path)
    path = out / "fig13-profiles.png"
    if figures.fig_profiles(cells, runs, KIND, path):
        drawn.append(path)
    written += drawn

    path = out / "report.md"
    path.write_text(summary_text(runs, tables, drawn), encoding="utf-8")
    written.append(path)
    return written


def summary_text(runs: pd.DataFrame, tables: dict[str, pd.DataFrame], drawn: list[Path]) -> str:
    """Returns report.md: the main numbers of the tables, in Russian, like
    the rest of the documentation."""
    parts = [
        "# Сводка серии",
        "",
        f"Прогонов: {len(runs)}, из них прогонов дрейфа: {int((runs['role'] != '').sum())}. "
        f"Вариантов: {runs['variant'].nunique()}, сетей: {runs['network'].nunique()}, нагрузок: {runs['load'].nunique()}. "
        f"Прогонов на ненастроенной машине: {int((runs['problems'] > 0).sum())}.",
        "",
        f"## p99 вызовов {KIND}, мс",
        "",
        "Медиана p99 по прогонам ячейки и 95%-интервал bootstrap; в скобках — доля вызовов с ошибкой, если они были, "
        "и сколько прогонов перегружено, если ячейка перегружена (раздел «Перегрузка»). "
        "Вызов с ошибкой входит в перцентили со своей задержкой, не больше дедлайна 10 с.",
        "",
    ]
    cells = tables["cells"]
    small = cells[cells["kind"] == KIND].copy()
    if not small.empty:
        def p99_cell(r: pd.Series) -> str:
            notes = []
            if r["error_pct"] > 0:
                notes.append(f"ошибок {r['error_pct']:.2g}%")
            if r["overloaded"]:
                notes.append(f"перегрузка {r['overloaded_runs']} из {r['runs']}")
            text = f"{fmt(r['p99'])} [{fmt(r['p99_lo'])}, {fmt(r['p99_hi'])}]"
            return text + (f" ({'; '.join(notes)})" if notes else "")

        small["p99 [интервал]"] = small.apply(p99_cell, axis=1)
        wide = small.pivot_table(index=["network", "load"], columns="variant", values="p99 [интервал]", aggfunc="first").reset_index()
        parts += [markdown(wide), ""]

    touched = cells.groupby(["network", "load"])["overloaded_runs"].transform("sum") > 0
    if touched.any():
        over = cells[touched].sort_values(["network", "load", "variant", "kind"]).copy()
        over["перегружено"] = over.apply(lambda r: f"{r['overloaded_runs']} из {r['runs']}", axis=1)
        over["ошибок, %"] = over["error_pct"].map(fmt)
        over["рост"] = over["growth"].map(fmt)
        over["goodput, Мбит/с"] = over["goodput_mbps"].map(fmt)
        parts += [
            "## Перегрузка",
            "",
            f"Прогон перегружен по виду вызова, если с ошибкой завершилось больше {decimal(stats.OVERLOAD_ERRORS_PCT)}% вызовов "
            f"или задержка выросла за измерение больше чем в {decimal(stats.OVERLOAD_GROWTH)} раза: рост — медиана задержки "
            "вызовов последней трети измерения против первой. Ячейка перегружена, если перегружено больше половины её прогонов. "
            "Перегруженный прогон не успевает за нагрузкой: задержки растут с длиной прогона до дедлайна, "
            "и его перцентили описывают прогон, а не транспорт.",
            "",
            "Ниже — сети и нагрузки, где перегружен хотя бы один прогон. Рост — медиана по прогонам; goodput — "
            "полезные данные отвеченных вызовов всех видов в обе стороны, медиана по прогонам.",
            "",
            markdown(over[["network", "load", "variant", "kind", "перегружено", "ошибок, %", "рост", "goodput, Мбит/с"]]),
            "",
        ]

    comp = tables["comparisons"]
    if not comp.empty:
        p99 = comp[(comp["metric"] == "p99") & (comp["kind"] == KIND)].copy()
        p99["эффект, % [интервал]"] = p99.apply(
            lambda r: f"{r['effect_pct']:+.1f} [{r['effect_lo_pct']:+.1f}, {r['effect_hi_pct']:+.1f}]", axis=1)
        p99["p (Холм)"] = p99["p_holm"].map(fmt)
        p99["режим"] = p99["regime"].map(REGIMES)
        parts += [
            f"## Сравнение p99 вызовов {KIND}",
            "",
            "Эффект — медиана p99 QUIC против медианы другого варианта; отрицательный — QUIC быстрее. "
            "p — двусторонний критерий Манна–Уитни по прогонам с поправкой Холма в пределах нагрузки и пары вариантов. "
            "Режим — какая из двух ячеек перегружена; если перегружена хоть одна, эффект говорит о перегрузке, а не о задержке.",
            "",
            markdown(p99[["network", "load", "quic", "other", "режим", "эффект, % [интервал]", "p (Холм)"]]),
            "",
        ]

    spread = tables["spread"]
    if not spread.empty:
        columns = ["network", "load", "variant", "runs", "перегрузка", "cv", "s_log", "runs_for_5pct", "runs_for_10pct", "runs_for_20pct"]
        need = spread[spread["kind"] == KIND].copy()
        need["перегрузка"] = need["overloaded"].map({True: "да", False: ""})
        need = need[columns]
        for c in ("cv", "s_log"):
            need[c] = need[c].map(fmt)
        parts += [
            "## Разброс p99 между прогонами",
            "",
            "cv — коэффициент вариации p99 по прогонам, s_log — стандартное отклонение его логарифма. "
            "runs_for_N — сколько прогонов на вариант нужно, чтобы различить разницу p99 в N% "
            "(критерий Манна–Уитни, α = 0,05, мощность 0,8, нормальное приближение для логарифма p99). "
            "В перегруженной ячейке p99 упирается в дедлайн или растёт с длиной прогона, и его разброс о транспорте не говорит.",
            "",
            markdown(need),
            "",
        ]

    cpu = tables["cpu"]
    if not cpu.empty:
        c = cpu.copy()
        names = {"server_cpu_us": "сервер, процесс", "server_cores_us": "сервер, ядра",
                 "client_cpu_us": "клиент, процесс", "client_cores_us": "клиент, ядра"}
        for column in names:
            c[column] = c[column].map(fmt)
        parts += [
            "## CPU на вызов, мкс",
            "",
            "Медиана по прогонам: время CPU процесса (user + system) или занятость его ядер (всё, кроме простоя), "
            "делённые на число вызовов измерения всех видов. Занятость ядер учитывает и работу ядра ОС для сети, "
            "которая процессу не засчитывается.",
            "",
            markdown(c.rename(columns=names)[["network", "load", "variant", "runs", *names.values()]]),
            "",
        ]

    warm = tables["warmup"]
    if not warm.empty:
        w = warm[(warm["kind"] == KIND) & (warm["role"] == "")].groupby(["network", "load", "variant"])["head_ratio"].median().reset_index()
        w["head_ratio"] = w["head_ratio"].map(fmt)
        parts += [
            "## Прогрев",
            "",
            "Медиана по прогонам отношения медианной задержки первых 5 с измерения к остальным. "
            "Заметно больше 1 — прогрева не хватает; заметно меньше 1 — задержка растёт за прогон, это перегрузка.",
            "",
            markdown(w),
            "",
        ]

    drift = tables["drift"]
    if not drift.empty:
        d = drift[drift["kind"] == KIND][["variant", "load", "p50_change_pct", "p99_change_pct"]].copy()
        for c in ("p50_change_pct", "p99_change_pct"):
            d[c] = d[c].map(lambda v: f"{v:+.1f}")
        parts += ["## Дрейф", "", "Изменение перцентилей прогонов дрейфа от начала серии к концу, %.", "", markdown(d), ""]

    if drawn:
        parts += ["## Рисунки", ""] + [f"![{p.stem}]({p.name})" for p in drawn] + [""]
    return "\n".join(parts)
