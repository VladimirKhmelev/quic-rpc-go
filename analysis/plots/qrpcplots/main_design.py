"""Names and selections from the pre-registered Q-05 experiment."""

from dataclasses import dataclass

QUIC, H2, POOL, TCP_DEFAULT = "quic", "grpc-h2", "grpc-h2-pool-4", "grpc-h2-tcp-default"
PAIR = (QUIC, H2)
PRIMARY = (*PAIR, POOL, "grpc-h3")
CONTROLS = (*PRIMARY, "connect-h2", TCP_DEFAULT)
LIMITED_PAIR = ("quic-9m", "grpc-h2-9m")
LOADS = ("small", "small-heavy", "bidi", "reconnect")
LOADS_10MBIT = ("small", "small-heavy-10mbit", "bidi", "reconnect")
LOSSES = (0.0, 0.5, 1.0, 2.0, 5.0)
LOSS_NAMES = ("base", "loss-0p5", "loss-1", "loss-2", "loss-5")
RTTS = (20, 50, 150)
INTENSITIES = ("small-50", "small", "small-1000")
SIZES = ("small-heavy-64k", "small-heavy-1m", "small-heavy-8m")


@dataclass(frozen=True)
class Cell:
    series: str
    network: str
    load: str
    kind: str = "small"


def grid_cell(rtt: int, loss_index: int, load: str) -> Cell:
    if rtt == 50:
        return Cell("loss-axis", LOSS_NAMES[loss_index], load)
    if loss_index == 0:
        return Cell("one-factor", f"rtt-{rtt}", load)
    return Cell("loss-rtt", f"rtt-{rtt}-{LOSS_NAMES[loss_index]}", load)


@dataclass(frozen=True)
class Matrix:
    networks: tuple[str, ...]
    loads: tuple[str, ...]
    variants: tuple[str, ...]
    repeats: int = 25


MATRICES = {
    "loss-axis": Matrix(LOSS_NAMES, LOADS, CONTROLS),
    "one-factor": Matrix(("burst-0p5", "burst-1", "burst-2", "burst-5", "rtt-20", "rtt-150", "jitter-10",
                          "rtt-150-jitter-10", "rtt-150-jitter-30", "reorder-1", "reorder-5", "reorder-10"), LOADS, PRIMARY),
    "rate-1": Matrix(("rate-1",), ("small-50", "small-heavy-1mbit", "bidi", "reconnect"), PRIMARY),
    "rate-10": Matrix(("rate-10", "congested", "degraded"), LOADS_10MBIT, PRIMARY),
    "datacenter": Matrix(("datacenter",), LOADS, CONTROLS),
    "compound": Matrix(("nearby-zones", "interregion"), LOADS, PRIMARY),
    "loss-rtt": Matrix(tuple(grid_cell(rtt, loss, "small").network for rtt in (20, 150) for loss in range(1, 5)), LOADS[:2], PAIR),
    "sweep-intensity": Matrix(("base", "loss-2"), INTENSITIES, PAIR, 10),
    "sweep-response-size": Matrix(("base", "loss-2"), SIZES, LIMITED_PAIR, 10),
    "sweep-pool": Matrix(("base", "loss-2"), LOADS[:2], (*PAIR, *(f"grpc-h2-pool-{n}" for n in (1, 2, 4, 8))), 10),
    "bulk": Matrix(("base",), ("bulk",), (*PRIMARY, "connect-h2")),
}


def profile_cells() -> tuple[Cell, ...]:
    return tuple(Cell(block, network, load, "echo" if load == "bidi" else "small")
                 for block, networks, loads in (("datacenter", ("datacenter",), LOADS),
                                                ("compound", ("nearby-zones", "interregion"), LOADS),
                                                ("rate-10", ("congested", "degraded"), LOADS_10MBIT))
                 for network in networks for load in loads)
