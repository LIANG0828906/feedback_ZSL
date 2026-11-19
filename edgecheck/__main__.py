"""CLI: python -m edgecheck run [--seed N] [--front-port N] ..."""
from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

from .config import load_config
from .orchestrator import async_run


def _limit_resources() -> None:
    """Best-effort: pin to at most 2 logical CPUs (parallel-run constraint)."""
    try:
        cpus = sorted(os.sched_getaffinity(0))
        if len(cpus) > 2:
            os.sched_setaffinity(0, set(cpus[:2]))
    except (AttributeError, OSError):
        pass


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="edgecheck")
    sub = parser.add_subparsers(dest="command")
    run = sub.add_parser("run", help="run the full check topology")
    run.add_argument("--seed", type=int)
    run.add_argument("--front-port", type=int)
    run.add_argument("--backend-a-port", type=int)
    run.add_argument("--backend-b-port", type=int)
    run.add_argument("--backend-c-port", type=int)
    run.add_argument("--bridge-port", type=int)
    run.add_argument("--admin-port", type=int)
    run.add_argument("--varnish-mode", choices=["auto", "local", "docker"])
    run.add_argument("--varnishd-path")
    run.add_argument("--report-path")
    run.add_argument("--verbose", action="store_true")
    args = parser.parse_args(argv)

    logging.basicConfig(
        level=logging.DEBUG if getattr(args, "verbose", False) else logging.INFO,
        format="%(levelname)s %(name)s: %(message)s")

    if args.command in (None, "run"):
        _limit_resources()
        config = load_config(args)
        return asyncio.run(async_run(config))
    parser.error(f"unknown command {args.command}")
    return 2


if __name__ == "__main__":
    sys.exit(main())
