"""Builds the main-series report from raw matrices and optional Q-04 data."""

import json
from pathlib import Path

import pandas as pd

from . import main_data, main_figures, main_tables, micro
from .report import fmt, markdown

VERDICTS = {"accepted": "принята", "rejected": "отвергнута", "insufficient": "не хватило данных"}
RECOMMENDATIONS = {"quic": "QUIC", "grpc-h2": "gRPC/HTTP/2", "no-difference": "без разницы",
                   "insufficient": "не хватило данных"}
REASONS = {
    "missing-repeats": "нет всех запланированных повторов",
    "goodput-unavailable": "отсутствует оценка goodput",
    "higher-goodput": "оба перегружены, преимущество по goodput",
    "goodput-rule-no-winner": "оба перегружены, критерий goodput не выбрал лидера",
    "cpu-over-limit": "превышен лимит CPU",
    "latencies-equivalent": "p50 и p99 эквивалентны",
    "no-practical-advantage": "у QUIC нет практического преимущества по p99",
    "advantage-within-cpu-limit": "преимущество по p99 и допустимая цена CPU",
    "latency-or-cpu-inconclusive": "неопределённая задержка или стоимость CPU",
}


def interval(value: float, lo: float, hi: float) -> str:
    if pd.isna(value):
        return "—"
    return f"{fmt(value)} [{fmt(lo)}; {fmt(hi)}]" if pd.notna(lo) and pd.notna(hi) else fmt(value)


def hypothesis_details(tables: dict[str, pd.DataFrame]) -> list[str]:
    parts = []
    pairs = tables["hypothesis_cells"].dropna(subset=["median_quic", "median_other"])
    if not pairs.empty:
        keys = ["family", "series", "network", "load", "metric"]
        detail = pairs[keys].copy()
        detail["QUIC, мс"] = pairs["median_quic"].map(fmt)
        detail["gRPC, мс"] = pairs["median_other"].map(fmt)
        detail["E, % [95%]"] = [interval(*(100 * (x - 1) for x in row))
                                 for row in pairs[["ratio", "ratio_lo", "ratio_hi"]].itertuples(index=False, name=None)]
        detail["p Холма"] = pairs["p_holm"].map(fmt)
        detail["режим"] = pairs["regime"].map({"steady": "без перегрузки", "both-overloaded": "оба перегружены",
                                               "quic-overloaded": "перегружен QUIC", "other-overloaded": "перегружен gRPC"})
        parts += ["### Сравнения задержки", "", "Оценки по полным ячейкам. При перегрузке результат определяется её режимом.",
                  "", markdown(detail), ""]
    contrasts = tables["hypothesis_contrasts"].dropna(subset=["estimate"])
    if not contrasts.empty:
        detail = contrasts[["family", "network", "load", "contrast", "unit"]].copy()
        for label, lo, hi in (("Оценка [95%]", "lo", "hi"), ("Оценка [границы решения]", "adjusted_lo", "adjusted_hi")):
            detail[label] = [interval(*row) for row in contrasts[["estimate", lo, hi]].itertuples(index=False, name=None)]
        parts += ["### Совместные контрасты H3 и H5", "", markdown(detail), ""]
    return parts


def text(tables: dict[str, pd.DataFrame], figures: list[Path], micro_gain: pd.DataFrame | None) -> str:
    coverage = tables["coverage"].copy()
    coverage["status"] = coverage["status"].map({"no-plan": "нет series.json", "partial": "неполная", "complete": "готова"})
    ready, missing = coverage["finished"].sum(), coverage["missing"].sum()
    verdicts = tables["hypotheses"].copy()
    verdicts["verdict"] = verdicts["verdict"].map(VERDICTS)
    recommendations = tables["transport_recommendations"].copy()
    recommendations["decision"] = recommendations["decision"].map(RECOMMENDATIONS)
    recommendations["reason"] = recommendations["reason"].map(REASONS)
    recommendations["preferred"] = recommendations["preferred"].map(RECOMMENDATIONS).fillna("—")
    run_meta = tables["runs"]
    parts = ["# Основная серия QUIC и gRPC", "",
             f"Готово {ready} из {ready + missing} запланированных прогонов, включая дрейф. "
             "Единица анализа — независимый прогон. Ошибочные RPC входят в задержки с наблюдаемым временем.", "",
             "## Готовность матриц", "", markdown(coverage.rename(columns={"series": "матрица", "planned": "план",
             "finished": "готово", "missing": "осталось", "status": "состояние"})), ""]
    if not run_meta.empty:
        revisions = ", ".join(sorted(run_meta["commit"].unique())) or "отсутствует"
        parts += [f"Ревизия сборки: `{revisions}`. Прогонов с dirty-сборкой: {int(run_meta['dirty'].sum())}. "
                  f"Прогонов с замечаниями к настройке хоста: {int((run_meta['problems'] > 0).sum())}.", ""]
    parts += ["## Гипотезы", "", markdown(verdicts.rename(columns={"hypothesis": "гипотеза", "verdict": "вердикт",
               "support": "в пользу", "oppose": "против", "inconclusive": "неясно", "excluded": "исключено"})), "",
              "Подробности по ячейкам есть в `hypothesis_cells.csv`, совместные интервалы H3/H5 в "
              "`hypothesis_contrasts.csv`, четыре оценки CPU для каждой сети H4 в `hypothesis_cpu.csv`. "
              "Пропущенные повторы сохраняют размер статистического семейства. Перегрузка выводится с её режимом.", ""]
    parts += hypothesis_details(tables)
    parts += [
              "## Рекомендации для составных профилей", "",
              markdown(recommendations[["network", "load", "kind", "decision", "preferred", "reason"]].rename(columns={
                  "network": "сеть", "load": "нагрузка", "kind": "вызов", "decision": "результат",
                  "preferred": "рекомендация", "reason": "основание"})), "",
              "Подробные оценки p50, p99, goodput, CPU и поправки приведены в `transport_recommendations.csv`. "
              "Нагрузки сравниваются по их собственным параметрам. Для bidi используется `echo`, для остальных `small`.", ""]
    latency = tables["latency"]
    overloaded = latency[(latency["metric"] == "p99") & (latency["overloaded_runs"] > 0)]
    if not overloaded.empty:
        details = overloaded[["series", "network", "load", "kind", "variant", "overloaded_runs", "runs", "error_pct"]].copy()
        details["error_pct"] = details["error_pct"].map(fmt)
        parts += ["## Перегрузка", "", "Ячейка перегружена, когда перегружено больше половины прогонов. "
                  "Для неполных ячеек числа описывают только собранные прогоны.", "", markdown(details), ""]
    if "warmup" in tables:
        count = int(tables["warmup"]["head_ratio"].notna().sum())
        parts += ["## Диагностика", "", f"Оценки прогрева доступны для {count} строк вида «прогон и тип вызова». "
                  "Пустое значение означает, что сравнить первые 5 с окна с остатком не удалось. "
                  "Дрейф хранится по матрицам отдельно в `drift.csv`.", ""]
    if "bulk" in tables:
        b = tables["bulk"]
        details = b[["network", "variant", "runs", "completed", "errors"]].copy()
        for label, key in (("Goodput, Мбит/с [95%]", "goodput_mbps"),
                           ("CPU сервера, мкс/МиБ [95%]", "server_cpu_us_per_mib"),
                           ("CPU клиента, мкс/МиБ [95%]", "client_cpu_us_per_mib")):
            details[label] = [interval(*row) for row in b[[key, key + "_lo", key + "_hi"]].itertuples(index=False, name=None)]
        parts += ["## Передача 100 МиБ", "", markdown(details), "",
                  "Оценки по завершённым передачам. Полезные байты частичных передач и ошибки сохранены в `bulk_runs.csv`. "
                  "CPU процессов и занятость ядер приведены в `bulk.csv`.", ""]
    if micro_gain is not None:
        parts += ["## Отдельный замер Q-04", "", "Установка соединения в основной серии и первый ответ QUIC "
                  "в Q-04 имеют разные границы таймера. Панели используют отдельные данные.", "",
                  markdown(micro_gain.rename(columns={"rtt_ms": "добавленный RTT, мс", "repeats": "повторы",
                           "gain_ms": "выигрыш, мс", "gain_lo_ms": "нижняя 95%", "gain_hi_ms": "верхняя 95%",
                           "gain_rtts": "выигрыш / RTT"})), ""]
    if figures:
        parts += ["## Графики", "", *[f"![{p.stem}]({p.name})\n" for p in figures]]
    parts += ["## Границы анализа", "", "Отчёт относится к постановке Q-05. Для причинного объяснения потерь "
              "и очередей нужны трассы Q-07. Прикладные результаты Wisply анализируются отдельно.", ""]
    return "\n".join(parts)


def report(root: str | Path, out: str | Path | None = None, q04: str | Path | None = None) -> list[Path]:
    out = Path(out) if out is not None else Path(root) / "analysis"
    main = main_data.read(root)
    tables = main_tables.tables(main)
    micro_summary = gain = None
    if q04 is not None:
        _, benchmark_rows = micro.read(q04)
        micro_summary, gain = micro.tables(benchmark_rows)
        tables["q04_micro"], tables["q04_early_gain"] = micro_summary, gain
    out.mkdir(parents=True, exist_ok=True)
    written = []
    for key, table in tables.items():
        path = out / (key + ".csv")
        table.to_csv(path, index=False, float_format="%.17g")
        written.append(path)
    figures = main_figures.draw(main, tables, out, micro_summary)
    path = out / "report.md"
    path.write_text(text(tables, figures, gain), encoding="utf-8")
    written += [*figures, path]
    manifest = out / "artifacts.json"
    manifest.write_text(json.dumps([p.name for p in written], indent=2) + "\n", encoding="utf-8")
    return [*written, manifest]
