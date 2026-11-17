"""Collects independent benchmark processes and their reproducibility inputs."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import re
import subprocess
import sys
import tarfile
import time

from qrpcplots import micro

ROOT = Path(__file__).resolve().parents[1]


def utc() -> str:
    return datetime.now(timezone.utc).isoformat()


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path: Path, value: dict) -> None:
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def command(argv: list[str], cwd: Path, raw: Path, timeout: int = 300) -> dict:
    """Always keeps combined output, including a failed or timed-out process."""
    started, start = utc(), time.monotonic()
    try:
        proc = subprocess.run(argv, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
        output, code = proc.stdout, proc.returncode
    except subprocess.TimeoutExpired as exc:
        output, code = (exc.stdout or b"") + b"\ncollector: process timeout\n", 124
    raw.write_bytes(output)
    job = {"command": argv, "cwd": str(cwd), "started_utc": started,
           "elapsed_s": time.monotonic() - start, "returncode": code,
           "raw": str(raw), "raw_sha256": digest(raw)}
    if code:
        raise RuntimeError(f"command failed ({code}): {argv!r}; output: {raw}")
    return job


def capture(argv: list[str], cwd: Path, path: Path) -> str:
    command(argv, cwd, path)
    return path.read_text().strip()


def sources(root: Path) -> dict[str, str]:
    """Fingerprints source inputs, including new files and embedded fixtures."""
    names = subprocess.check_output(["git", "ls-files", "-co", "--exclude-standard", "-z"], cwd=root).decode().split("\0")
    suffixes = {".go", ".proto", ".binpb", ".py", ".yaml", ".yml"}
    return {name: digest(root / name) for name in sorted(set(names)) if name and
            (Path(name).suffix in suffixes or Path(name).name in {"Makefile", "go.mod", "go.sum", "requirements.txt"})}


def schedule(repeats: int, seed: int) -> list[tuple[int, str]]:
    rng, result = random.Random(seed), []
    for repeat in range(1, repeats + 1):
        families = ["overhead", "reconnect"]
        rng.shuffle(families)
        result.extend((repeat, family) for family in families)
    return result


def environment() -> dict:
    """Reads public host characteristics, not the whole process environment."""
    result = {"platform": platform.platform(), "python": sys.version,
              "affinity": sorted(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
              "env": {k: os.environ[k] for k in ("GOGC", "GOMEMLIMIT", "GODEBUG", "GOFLAGS", "GOMAXPROCS") if k in os.environ}}
    cpuinfo = Path("/proc/cpuinfo")
    if cpuinfo.exists():
        result["cpu_models"] = sorted({line.split(":", 1)[1].strip() for line in cpuinfo.read_text().splitlines()
                                       if line.startswith("model name")})
    result["governors"] = {str(p): p.read_text().strip() for p in
                           sorted(Path("/sys/devices/system/cpu").glob("cpu[0-9]*/cpufreq/scaling_governor"))}
    return result


def collect(root: Path, out: Path, config: dict) -> dict:
    """Writes meta.json only when all planned jobs and provenance checks pass."""
    out.mkdir(parents=True, exist_ok=False)
    for folder in ("raw", "provenance", "bin"):
        (out / folder).mkdir()
    meta = {"schema_version": 1, "kind": "rpc-micro", "started_utc": utc(), "config": config,
            "environment": environment(), "jobs": [], "builds": []}
    try:
        original = sources(root)
        write_json(out / "provenance/sources.json", original)
        with tarfile.open(out / "provenance/sources.tar.gz", "w:gz") as archive:
            for name in original:
                archive.add(root / name, arcname=name)
        meta["sources_sha256"] = digest(out / "provenance/sources.json")
        meta["source_archive_sha256"] = digest(out / "provenance/sources.tar.gz")
        meta["git"] = {
            "commit": capture(["git", "rev-parse", "HEAD"], root, out / "provenance/commit.txt"),
            "status": capture(["git", "status", "--porcelain=v1"], root, out / "provenance/status.txt"),
        }
        for name, argv in (("go-version", ["go", "version"]),
                           ("go-env", ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOAMD64", "CGO_ENABLED", "GOEXPERIMENT"])):
            capture(argv, root, out / f"provenance/{name}.txt")
        for module in (".", "rpc", "grpcbridge"):
            capture(["go", "list", "-m", "all"], root / module, out / f"provenance/modules-{Path(module).name or 'root'}.txt")
        for family, cwd, package in (("overhead", root, "./bench/overhead"),
                                     ("reconnect", root / "rpc", "./transport/quic")):
            binary = out / f"bin/{family}.test"
            build = command(["go", "test", "-c", "-o", str(binary), package], cwd, out / f"provenance/build-{family}.txt")
            build["raw"] = str(Path(build["raw"]).relative_to(out))
            build["binary_sha256"] = digest(binary)
            meta["builds"].append(build)
            capture(["go", "version", "-m", str(binary)], root, out / f"provenance/binary-{family}.txt")
        for order, (repeat, family) in enumerate(schedule(config["repeats"], config["seed"]), 1):
            print(f"microbench: {order}/{2 * config['repeats']} repeat={repeat} family={family}", flush=True)
            bench = "BenchmarkRPCOverhead" if family == "overhead" else "BenchmarkEarlyReconnect"
            benchtime = config["benchtime"] if family == "overhead" else f"{config['reconnect_iterations']}x"
            argv = [str(out / f"bin/{family}.test"), "-test.run=^$", f"-test.bench=^{bench}$",
                    f"-test.benchtime={benchtime}", f"-test.cpu={config['cpu']}", "-test.benchmem", "-test.timeout=5m"]
            job = command(argv, root if family == "overhead" else root / "rpc", out / f"raw/{order:02d}-{family}.txt")
            job.update(family=family, repeat=repeat, order=order)
            job["raw"] = str(Path(job["raw"]).relative_to(out))
            micro.parse_job((out / job["raw"]).read_text(), family, config["cpu"])
            meta["jobs"].append(job)
        if sources(root) != original:
            raise RuntimeError("source files changed during collection")
        meta["finished_utc"] = utc()
        # Validate the complete matrix before publishing the ready marker.
        write_json(out / "pending.json", meta)
        micro.read(out, manifest="pending.json")
        (out / "pending.json").rename(out / "meta.json")
    except Exception as exc:
        write_json(out / "failed.json", {**meta, "error": str(exc), "failed_utc": utc()})
        raise
    return meta


def arguments(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="RPC memory and TLS first-reply benchmark collection")
    parser.add_argument("--out", type=Path, default=Path("out") / ("micro-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")))
    parser.add_argument("--repeats", type=int, default=5)
    parser.add_argument("--cpu", type=int, default=1)
    parser.add_argument("--benchtime", default="200ms")
    parser.add_argument("--reconnect-iterations", type=int, default=20)
    parser.add_argument("--seed", type=int, default=1)
    args = parser.parse_args(argv)
    if min(args.repeats, args.cpu, args.reconnect_iterations) < 1:
        parser.error("repeats, cpu and reconnect-iterations must be positive")
    if not re.fullmatch(r"(?:[1-9]\d*x|(?:\d+(?:\.\d+)?)(?:ns|us|µs|ms|s|m|h))", args.benchtime) or not any(c in "123456789" for c in args.benchtime):
        parser.error("benchtime must be a positive Go benchmark duration or count (e.g. 200ms or 20x)")
    return args


def main(argv: list[str] | None = None) -> int:
    args = arguments(argv)
    out = args.out.resolve()
    config = {key: value for key, value in vars(args).items() if key != "out"}
    try:
        collect(ROOT, out, config)
        for path in micro.report(out):
            print(f"microbench: wrote {path}")
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(f"microbench: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
