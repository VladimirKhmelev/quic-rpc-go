"""Command line of the analysis.

    python -m qrpcplots ingest SERIES [-o DIR]   runs.csv and calls.parquet of a series
    python -m qrpcplots report DIR [-o OUT]      tables, figures and report.md from them
    python -m qrpcplots micro DIR [-o OUT]       memory and first-reply benchmark report
    python -m qrpcplots main DIR [-o OUT] [--micro Q04]   main-series report from raw data

ingest writes into SERIES/analysis by default, report into DIR.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from . import data, main_report, micro, report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="qrpcplots", description="Statistics and figures of a series.")
    commands = parser.add_subparsers(dest="command", required=True)
    ingest = commands.add_parser("ingest", help="read a series into runs.csv and calls.parquet")
    ingest.add_argument("series", type=Path)
    ingest.add_argument("-o", "--out", type=Path)
    rep = commands.add_parser("report", help="compute the tables and the figures")
    rep.add_argument("dir", type=Path)
    rep.add_argument("-o", "--out", type=Path)
    mic = commands.add_parser("micro", help="analyze memory and first-reply benchmarks")
    mic.add_argument("dir", type=Path)
    mic.add_argument("-o", "--out", type=Path)
    series = commands.add_parser("main", help="analyze the Q-05 matrices from their raw runs")
    series.add_argument("dir", type=Path)
    series.add_argument("-o", "--out", type=Path)
    series.add_argument("--micro", type=Path, help="optional completed Q-04 microbenchmark collection")
    args = parser.parse_args(argv)

    if args.command == "ingest":
        series = data.read_series(args.series)
        out = args.out or args.series / "analysis"
        data.write_tables(series, out)
        print(f"qrpcplots: {len(series.runs)} runs, {len(series.calls)} calls into {out}", file=sys.stderr)
        if series.missing:
            print(f"qrpcplots: {len(series.missing)} planned runs are not finished: {', '.join(series.missing)}", file=sys.stderr)
        return 0
    if args.command == "micro":
        for path in micro.report(args.dir, args.out):
            print(f"qrpcplots: wrote {path}", file=sys.stderr)
        return 0
    if args.command == "main":
        try:
            paths = main_report.report(args.dir, args.out, args.micro)
        except (OSError, ValueError, KeyError, TypeError) as exc:
            print(f"qrpcplots main: {exc}", file=sys.stderr)
            return 1
        for path in paths:
            print(f"qrpcplots: wrote {path}", file=sys.stderr)
        return 0
    runs, calls = data.read_tables(args.dir)
    for path in report.report(runs, calls, args.out or args.dir):
        print(f"qrpcplots: wrote {path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
