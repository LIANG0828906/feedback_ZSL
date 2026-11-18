"""Command-line interface."""

from __future__ import annotations

import argparse
import json
import sys
from typing import Any

from .errors import HandoverError
from .generator import generate_all
from .http_api import create_server
from .service import build_service


def _pretty(payload: Any) -> str:
    return json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True)


def _add_input_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--data-dir", default="data", help="Input data directory")
    parser.add_argument("--samples", default=None, help="SLI file or directory")
    parser.add_argument("--handover", default=None, help="Default handover JSON file")
    parser.add_argument("--policy", default=None, help="Decision policy JSON file")
    parser.add_argument("--db", default="var/handover.db", help="SQLite database path")


def _service_from_args(args: argparse.Namespace):
    return build_service(
        data_dir=args.data_dir,
        samples=args.samples,
        handover=args.handover,
        db_path=args.db,
        policy=args.policy,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="duty-handover",
        description="Duty handover SLI, error-budget, grading, and silence decisions.",
    )
    parser.add_argument("--version", action="version", version="1.0.0")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="Generate deterministic sample data")
    generate.add_argument("--out", default="data")
    generate.add_argument("--seed", type=int, default=304)

    decide = subparsers.add_parser("decide", help="Compute a decision without committing")
    _add_input_options(decide)

    submit = subparsers.add_parser("submit", help="Compute and commit a snapshot")
    _add_input_options(submit)

    history = subparsers.add_parser("history", help="List committed snapshots")
    _add_input_options(history)
    history.add_argument("--limit", type=int, default=100)

    get_snapshot = subparsers.add_parser("get", help="Read one committed snapshot")
    _add_input_options(get_snapshot)
    get_snapshot.add_argument("handover_id")

    replay = subparsers.add_parser("replay", help="Replay a committed handover read-only")
    _add_input_options(replay)
    replay.add_argument("handover_id")
    replay.add_argument("--until", default=None)
    replay.add_argument("--step-minutes", type=int, default=1)
    replay.add_argument(
        "--no-series",
        action="store_true",
        help="Return the summary and first divergence only",
    )

    status = subparsers.add_parser("status", help="Return JSON decision status")
    _add_input_options(status)
    status.add_argument("handover_id", nargs="?")

    metrics = subparsers.add_parser("metrics", help="Print Prometheus metrics")
    _add_input_options(metrics)

    window = subparsers.add_parser("window", help="Recompute one service window")
    _add_input_options(window)
    window.add_argument("service")
    window.add_argument("start")
    window.add_argument("end")

    serve = subparsers.add_parser("serve", help="Start the local HTTP service")
    _add_input_options(serve)
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8765)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            print(_pretty(generate_all(args.out, args.seed)))
            return 0
        service = _service_from_args(args)
        if args.command == "decide":
            print(_pretty(service.decide()))
        elif args.command == "submit":
            print(_pretty(service.submit()))
        elif args.command == "history":
            print(_pretty({"items": service.history(args.limit)}))
        elif args.command == "get":
            print(_pretty(service.get_snapshot(args.handover_id)))
        elif args.command == "replay":
            print(
                _pretty(
                    service.replay(
                        args.handover_id,
                        until=args.until,
                        step_minutes=args.step_minutes,
                        include_series=not args.no_series,
                    )
                )
            )
        elif args.command == "status":
            print(_pretty(service.status(args.handover_id)))
        elif args.command == "metrics":
            sys.stdout.write(service.metrics_text())
        elif args.command == "window":
            print(_pretty(service.window(args.service, args.start, args.end)))
        elif args.command == "serve":
            server = create_server(service, args.host, args.port)
            host, port = server.server_address[:2]
            print(
                f"Duty handover service listening on http://{host}:{port}",
                file=sys.stderr,
                flush=True,
            )
            try:
                server.serve_forever()
            except KeyboardInterrupt:
                pass
            finally:
                server.server_close()
        return 0
    except HandoverError as exc:
        print(
            _pretty(
                {
                    "error": exc.to_dict(),
                    "http_status": exc.http_status,
                }
            ),
            file=sys.stderr,
        )
        return 2
    except OSError as exc:
        print(
            _pretty(
                {
                    "error": {
                        "code": "IO_ERROR",
                        "message": str(exc),
                    }
                }
            ),
            file=sys.stderr,
        )
        return 2
