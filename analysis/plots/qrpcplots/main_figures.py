"""Figures for the main series and optional Q-04 controls."""

from dataclasses import dataclass
from pathlib import Path

import numpy as np
import pandas as pd

from .figures import DPI, plt, colors
from . import stats
from .main_data import CPU, MainData
from .main_design import H2, INTENSITIES, LOSSES, LOSS_NAMES, PRIMARY, QUIC, RTTS, SIZES, profile_cells


@dataclass
class Panel:
    data: pd.DataFrame
    ticks: list[str]
    title: str
    xlabel: str
    ylabel: str
    connect: bool = True


def select(frame: pd.DataFrame, **where) -> pd.DataFrame:
    for column, value in where.items():
        frame = frame[frame[column] == value]
    return frame


def points(frame: pd.DataFrame, locations: list[dict]) -> pd.DataFrame:
    return pd.concat([select(frame, **where).assign(x=i) for i, where in enumerate(locations)], ignore_index=True)


def write_panels(path: Path, title: str, panels: list[Panel], note: str = "") -> bool:
    panels = [panel for panel in panels if not panel.data.empty and panel.data["median"].notna().any()]
    if not panels:
        return False
    palette = colors(sorted({v for panel in panels for v in panel.data["variant"]}))
    cols = min(len(panels), 2)
    rows = (len(panels) + cols - 1) // cols
    sizes = [int(n) for panel in panels for n in panel.data.get("samples", panel.data["runs"]) if n > 0]
    count = f"{min(sizes)}–{max(sizes)}" if sizes and min(sizes) != max(sizes) else str(sizes[0]) if sizes else "0"
    fig, axes = plt.subplots(rows, cols, squeeze=False, figsize=(max(6.8, 5.4 * cols), 3.4 * rows + 0.6), layout="constrained")
    try:
        for ax, panel in zip(axes.flat, panels):
            for variant, group in panel.data.groupby("variant", observed=True):
                group = group.set_index("x").reindex(range(len(panel.ticks)))
                y = group["median"].to_numpy(dtype=float)
                line = "--" if variant == "resumed" else "-"
                ax.plot(group.index, y, marker="o", label=variant, color=palette[variant], linestyle=line if panel.connect else "none")
                intervals = group["lo"].notna() & group["hi"].notna()
                if intervals.any():
                    g = group[intervals]
                    err = np.maximum(0, np.vstack([g["median"] - g["lo"], g["hi"] - g["median"]]))
                    ax.errorbar(g.index, g["median"], yerr=err, fmt="none", capsize=3, color=palette[variant])
                if "overloaded" in group:
                    overloaded = group[group["overloaded"].eq(True)]
                    ax.plot(overloaded.index, overloaded["median"], "o", markerfacecolor="white", markeredgecolor=palette[variant])
                partial = group[group["runs"] < group["expected"]]
                ax.plot(partial.index, partial["median"], "x", color=palette[variant])
            ax.set(title=panel.title, xlabel=panel.xlabel, ylabel=panel.ylabel)
            if panel.ylabel in ("p99, мс", "задержка, мс") and panel.data["median"].min() > 0:
                ax.set_yscale("log")
            ax.set_xticks(range(len(panel.ticks)), panel.ticks, fontsize=8)
            ax.grid(alpha=0.2, axis="y")
            ax.legend(fontsize=7, ncols=2)
        for ax in list(axes.flat)[len(panels):]:
            ax.set_visible(False)
        caption = [title, f"N={count} на точку, 95%-интервалы при N ≥ 2"]
        markers = []
        if any("overloaded" in p.data and p.data["overloaded"].any() for p in panels):
            markers.append("○ перегрузка")
        if any((p.data["runs"] < p.data["expected"]).any() for p in panels):
            markers.append("× неполная ячейка")
        caption.extend(part for part in (" · ".join(markers), note) if part)
        fig.suptitle("\n".join(caption), fontsize=10)
        fig.savefig(path, dpi=DPI)
    finally:
        plt.close(fig)
    return True


def heatmap(grid: pd.DataFrame, path: Path) -> bool:
    if grid["regime"].eq("missing").all():
        return False
    fig, axs = plt.subplots(1, 2, figsize=(12, 4.6), layout="constrained")
    try:
        for ax, load in zip(axs, ("small", "small-heavy"), strict=True):
            group = grid[grid["load"] == load].set_index(["rtt_ms", "loss_pct"])
            values = group["median"].unstack().reindex(index=RTTS, columns=LOSSES).to_numpy(dtype=float)
            image = ax.imshow(values, vmin=0, vmax=2, cmap="RdBu_r", aspect="auto")
            for i, rtt in enumerate(RTTS):
                for j, loss in enumerate(LOSSES):
                    row = group.loc[(rtt, loss)]
                    color = "black"
                    if row["regime"] == "missing":
                        label = f"{row['runs']}/{row['expected']}\nнеполные"
                    elif row["regime"] != "steady":
                        variant = {"other-overloaded": "gRPC", "quic-overloaded": "QUIC", "both-overloaded": "оба"}[row["regime"]]
                        label = f"перегрузка\n{variant}"
                    elif np.isfinite(row["median"]):
                        label = f"{row['median']:.2f}\n[{row['lo']:.2f}; {row['hi']:.2f}]"
                        if row["median"] < .35 or row["median"] > 1.65:
                            color = "white"
                    else:
                        label = "нет оценки"
                    ax.text(j, i, label, ha="center", va="center", fontsize=7, color=color)
            ax.set(title=load, xlabel="потери в каждом направлении, %", ylabel="RTT, мс")
            ax.set_xticks(range(len(LOSSES)), [f"{x:g}" for x in LOSSES])
            ax.set_yticks(range(len(RTTS)), RTTS)
        fig.colorbar(image, ax=axs, label="p99 QUIC / gRPC HTTP/2", extend="max")
        fig.suptitle("Рис. 3. Мелкие RPC: сетка потери × RTT, 100 Мбит/с\n25 прогонов на вариант, 95%-интервалы по прогонам", fontsize=10)
        fig.savefig(path, dpi=DPI)
    finally:
        plt.close(fig)
    return True


def ecdf(main: MainData, path: Path) -> bool:
    if not main.ecdf:
        return False
    fig, (ax, tail) = plt.subplots(1, 2, figsize=(11, 4.4), layout="constrained")
    try:
        for variant, runs in sorted(main.ecdf.items()):
            latency, counts = np.unique(np.concatenate(runs) / 1e6, return_counts=True)
            fraction = np.cumsum(counts) / counts.sum()
            label = f"{variant}, N={len(runs)}"
            line, = ax.step(latency, fraction, where="post", label=label)
            tail.step(latency[:-1], 1 - fraction[:-1], where="post", label=label, color=line.get_color())
            positive = latency[latency > 0]
            if len(runs) >= 2 and len(positive):
                grid = np.geomspace(positive[0], positive[-1], 160)
                cdf_counts = np.asarray([np.searchsorted(np.sort(run) / 1e6, grid, side="right") for run in runs])
                # Bootstrap draws use whole runs and keep their call counts.
                rng = stats.rng_for("main-ecdf", variant)
                weights = rng.multinomial(len(runs), np.full(len(runs), 1 / len(runs)), size=stats.BOOTSTRAP)
                samples = (weights @ cdf_counts) / (weights @ [len(run) for run in runs])[:, None]
                lo, hi = np.quantile(samples, [0.025, 0.975], axis=0)
                ax.fill_between(grid, lo, hi, alpha=.15, color=line.get_color())
                tail.fill_between(grid, 1 - hi, 1 - lo, where=hi < 1, alpha=.15, color=line.get_color())
        for panel, ylabel in ((ax, "доля вызовов не дольше"), (tail, "доля вызовов дольше")):
            panel.set(xlabel="задержка, мс", ylabel=ylabel, xscale="log")
            panel.grid(alpha=0.2)
        tail.set_yscale("log")
        tail.legend(fontsize=7)
        fig.suptitle("Рис. 4. Small при потерях 2%, RTT 50 мс, 100 Мбит/с\nРаспределение вызовов измерения. "
                     "Поточечные 95%-интервалы по прогонам при N ≥ 2", fontsize=10)
        fig.savefig(path, dpi=DPI)
    finally:
        plt.close(fig)
    return True


def draw(main: MainData, tables: dict[str, pd.DataFrame], out: Path, micro_summary: pd.DataFrame | None = None) -> list[Path]:
    written = []
    latency = tables["latency"]
    p99 = select(latency, metric="p99", kind="small")

    def plot(name: str, title: str, panels: list[Panel], note: str = "") -> None:
        path = out / (name + ".png")
        if write_panels(path, title, panels, note):
            written.append(path)

    def axis(frame: pd.DataFrame, sources: list[tuple[str, str, str]], ticks: list[str], title: str, xlabel: str, ylabel: str = "p99, мс") -> Panel:
        return Panel(points(frame, [{"series": s, "network": n, "load": l} for s, n, l in sources]), ticks, title, xlabel, ylabel)

    for i, load in enumerate(("small", "small-heavy"), 1):
        plot(f"fig0{i}-loss-{load}", f"Рис. {i}. Равномерные потери: мелкие RPC",
             [axis(p99, [("loss-axis", n, load) for n in LOSS_NAMES], [f"{x:g}" for x in LOSSES], load, "потери, %")],
             "RTT 50 мс, полоса 100 Мбит/с")
    for name, draw_extra in (("fig03-grid", lambda p: heatmap(tables["grid"], p)), ("fig04-ecdf-loss-2", lambda p: ecdf(main, p))):
        path = out / f"{name}.png"
        if draw_extra(path):
            written.append(path)

    panels = []
    for network, load in ((n, l) for n in ("base", "loss-2") for l in ("small", "small-heavy")):
        data = select(p99, series="sweep-pool", network=network, load=load)
        x = {f"grpc-h2-pool-{n}": i for i, n in enumerate((1, 2, 4, 8))}
        pool = data[data["variant"].isin(x)].assign(x=lambda df: df["variant"].map(x), variant="grpc-h2-pool")
        reference = pd.concat([data[data["variant"].isin((QUIC, H2))].assign(x=i) for i in range(4)])
        panels.append(Panel(pd.concat([pool, reference]), ["1", "2", "4", "8"], f"{network}, {load}", "соединения в пуле", "p99, мс"))
    plot("fig05-pool", "Рис. 5. Размер пула gRPC/HTTP/2", panels, "RTT 50 мс, полоса 100 Мбит/с")

    panels = []
    for load in ("small", "small-heavy"):
        a = axis(p99, [("loss-axis", n, load) for n in LOSS_NAMES], [], "", "").data
        b = axis(p99, [("one-factor", n.replace("loss-", "burst-"), load) for n in LOSS_NAMES[1:]], [], "", "").data
        a = a[a["variant"].isin((QUIC, H2))].assign(variant=lambda f: f["variant"] + " / равномерные")
        b = b[b["variant"].isin((QUIC, H2))].assign(x=lambda f: f["x"] + 1, variant=lambda f: f["variant"] + " / пачки")
        panels.append(Panel(pd.concat([a, b]), [f"{v:g}" for v in LOSSES], load, "средняя доля потерь, %", "p99, мс"))
    plot("fig06-loss-burst", "Рис. 6. Потери равномерные и пачками", panels, "RTT 50 мс, полоса 100 Мбит/с, средняя пачка 4 пакета")

    panels = []
    for load in ("small", "small-heavy"):
        panels.append(axis(p99, [("one-factor", "rtt-20", load), ("loss-axis", "base", load), ("one-factor", "rtt-150", load)],
                           ["20", "50", "150"], load, "RTT, мс"))
        panels.append(axis(p99, [("one-factor", n, load) for n in ("rtt-150", "rtt-150-jitter-10", "rtt-150-jitter-30")],
                           ["0", "10", "30"], f"{load}, RTT 150 мс", "джиттер, мс"))
    plot("fig07-rtt-jitter", "Рис. 7. Оси RTT и джиттера", panels, "Без потерь, полоса 100 Мбит/с")

    reordering = tables["reorder"]
    panels = [Panel(select(reordering, load=load).assign(x=range(4), variant="QUIC / gRPC HTTP/2"),
                    ["0", "1", "5", "10"], load, "перестановки, %", "отношение p99") for load in ("small", "small-heavy")]
    plot("fig08-reorder", "Рис. 8. Эффект перестановок пакетов", panels, "RTT 50 мс, полоса 100 Мбит/с. Отношение показано для полных ячеек без перегрузки.")

    panels = [axis(p99, [("rate-1", "rate-1", "small-50"), ("rate-10", "rate-10", "small"), ("loss-axis", "base", "small")],
                   ["1 · 50/с", "10 · 200/с", "100 · 200/с"], "Мелкие RPC", "Мбит/с · интенсивность"),
              axis(p99, [("rate-1", "rate-1", "small-heavy-1mbit"), ("rate-10", "rate-10", "small-heavy-10mbit"),
                         ("loss-axis", "base", "small-heavy")], ["1 · 64 КиБ/2 с", "10 · 1 МиБ/2,2 с", "100 · 1 МиБ/1 с"],
                   "Мелкие RPC рядом с тяжёлым", "Мбит/с · размер и период ответа")]
    plot("fig09-bandwidth", "Рис. 9. Узкие каналы и согласованные нагрузки", panels, "RTT 50 мс, без потерь")

    connections = select(tables["connections"], load="reconnect", metric="p50").assign(
        median=lambda f: f["median_rtt"], lo=lambda f: f["lo_rtt"], hi=lambda f: f["hi_rtt"])
    plot("fig10-connections", "Рис. 10. Установка соединения или пула",
         [axis(connections, [("loss-axis", n, "reconnect") for n in LOSS_NAMES], [f"{x:g}" for x in LOSSES],
               "p50 соединения по прогонам", "потери, %", "p50 / RTT")], "RTT 50 мс, полоса 100 Мбит/с")

    cpu_labels = ("Сервер, процесс", "Сервер, ядра", "Клиент, процесс", "Клиент, ядра")
    plot("fig11-cpu", "Рис. 11. Стоимость CPU мелких RPC",
         [axis(select(tables["cpu"], metric=metric), [("loss-axis", n, "small") for n in LOSS_NAMES], [f"{x:g}" for x in LOSSES],
               label, "потери, %", "CPU, мкс/вызов") for metric, label in zip(CPU, cpu_labels, strict=True)], "RTT 50 мс, полоса 100 Мбит/с")

    for load in ("small", "small-heavy", "bidi", "reconnect"):
        panels = []
        for cell in profile_cells():
            if cell.load != load and not (load == "small-heavy" and cell.load == "small-heavy-10mbit"):
                continue
            group = select(latency, series=cell.series, network=cell.network, load=cell.load, kind=cell.kind)
            group = group[group["variant"].isin(PRIMARY)].assign(x=lambda f: f["metric"].map({"p50": 0, "p95": 1, "p99": 2}))
            panels.append(Panel(group, ["p50", "p95", "p99"], f"{cell.network}, {cell.load}", "перцентиль", "задержка, мс"))
        plot(f"fig13-profiles-{load}", f"Рис. 13. Составные профили: {load}", panels, "Параметры NET-7 сохранены в runs.csv")

    if "bulk" in tables and not tables["bulk"].empty:
        b = tables["bulk"]
        labels = b["variant"].tolist()
        all_metrics = ("goodput_mbps", *(f"{side}_{kind}_us_per_mib" for side in ("server", "client") for kind in ("cpu", "cores")))
        panels = []
        for key, label in zip(all_metrics, ("Полезная скорость", *cpu_labels), strict=True):
            group = b.assign(x=range(len(b)), median=b[key], lo=b[key + "_lo"], hi=b[key + "_hi"],
                             expected=25, samples=b["completed"], variant="завершённые передачи")
            panels.append(Panel(group, labels, label, "транспорт", "Мбит/с" if key == "goodput_mbps" else "мкс/МиБ", connect=False))
        plot("fig14-bulk", "Рис. 14. Bulk: goodput и CPU", panels, "100 МиБ, RTT 50 мс, 100 Мбит/с. Интервалы по успешным передачам.")

    for block, labels, loads, xlabel in (("sweep-intensity", ["50", "200", "1000"], INTENSITIES, "малых RPC/с"),
                                         ("sweep-response-size", ["64 КиБ", "1 МиБ", "8 МиБ"], SIZES, "размер ответа раз в 2 с")):
        plot(block, f"Развёртка: {xlabel}", [axis(p99, [(block, n, l) for l in loads], labels, n, xlabel) for n in ("base", "loss-2")],
             "RTT 50 мс, полоса 100 Мбит/с")

    if micro_summary is not None:
        m = micro_summary.rename(columns={"repeats": "runs"}).assign(expected=lambda f: f["runs"], samples=lambda f: f["runs"])
        early = select(m, family="reconnect").assign(variant=lambda f: f["mode"], x=lambda f: f["rtt_ms"].map({0: 0, 50: 1}))
        plot("q04-first-reply", "Q-04. Первый ответ QUIC при переподключении",
             [Panel(early, ["0", "50"], "cold / resumed / early", "добавленный RTT, мс", "первый ответ, мс")], "8 Б, loopback, таймер включает первый RPC")
        mem = select(m, family="overhead").assign(variant=lambda f: f["backend"], x=lambda f: f["payload_bytes"].map({n: i for i, n in enumerate((0, 100, 1024, 65536))}))
        plot("q04-memory-overhead", "Q-04. Накладные расходы RPC в памяти",
             [Panel(select(mem, kind=kind, metric=metric), ["0", "100", "1024", "65536"], kind, "payload, Б", unit)
              for kind in ("unary", "bidi") for metric, unit in (("ns_per_op", "нс/операцию"), ("bytes_per_op", "Б/операцию"), ("allocs_per_op", "выделений/операцию"))],
             "Без сети и TLS, интервалы по процессам Q-04")
    return written
