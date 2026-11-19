from __future__ import annotations

import argparse

from .app import run_cli


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="dualwrite",
        description="Bidirectional PostgreSQL convergence service",
    )
    parser.add_argument("--config", default="config.example.json")
    parser.add_argument("--log-level", default="INFO")
    sub = parser.add_subparsers(dest="command", required=True)

    serve = sub.add_parser("serve", help="run sync workers and HTTP API")
    serve.add_argument("--no-bootstrap", action="store_true")

    init = sub.add_parser("init", help="create schema and deterministic seed rows")
    init.add_argument("--members", type=int, default=20000)
    init.add_argument("--orders", type=int, default=30000)
    init.add_argument("--reset", action="store_true")

    seed = sub.add_parser("seed", help="seed rows into existing schema")
    seed.add_argument("--members", type=int, default=20000)
    seed.add_argument("--orders", type=int, default=30000)
    seed.add_argument("--reset", action="store_true")

    validate = sub.add_parser("validate", help="run a full convergence batch")
    validate.add_argument("--scope", choices=["all", "members", "orders"], default="all")
    validate.add_argument("--wait", action="store_true")
    validate.add_argument("--timeout", type=float, default=180.0)
    validate.add_argument("--limit", type=int, default=1000)

    trace = sub.add_parser("trace", help="trace a member or order")
    trace.add_argument("--type", required=True, choices=["member", "members", "order", "orders"])
    trace.add_argument("--id", type=int, required=True)
    trace.add_argument("--limit", type=int, default=30)

    conflicts = sub.add_parser("conflicts", help="query persisted conflict decisions")
    conflicts.add_argument("--type", choices=["members", "orders"])
    conflicts.add_argument("--id", type=int)
    conflicts.add_argument("--from", dest="from_time")
    conflicts.add_argument("--to", dest="to_time")
    conflicts.add_argument("--limit", type=int, default=100)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return run_cli(args)
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

