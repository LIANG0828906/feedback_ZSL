"""Command line entry point."""

from __future__ import annotations

import argparse
import json
import sys
import threading
import time
from pathlib import Path

from .generator import MiB
from .http_server import create_server
from .service import ArchiveService


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Local revision-aware object bucket")
    subparsers = parser.add_subparsers(dest="command", required=True)
    serve = subparsers.add_parser("serve", help="run the HTTP service")
    serve.add_argument("--root", default="data", help="workspace-local data directory")
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=0, help="0 selects a free port")
    serve.add_argument("--seed", type=int, default=20261119)
    serve.add_argument("--max-object-mib", type=int, default=64)
    demo = subparsers.add_parser("demo", help="run the full workload and fault scenario")
    demo.add_argument("--root", default="data-demo")
    demo.add_argument("--seed", type=int, default=20261119)
    demo.add_argument("--count", type=int, default=200)
    demo.add_argument("--target-mib", type=int, default=120)
    demo.add_argument("--prefix", default=f"archive/run-{time.time_ns()}")
    demo.add_argument("--history-count", type=int, default=12)
    demo.add_argument("--write-rate", type=float, default=0.0)
    demo.add_argument("--delete-rate", type=float, default=0.0)
    demo.add_argument("--snapshot-rate", type=float, default=0.0)
    demo.add_argument("--timeout", type=float, default=300.0)
    demo.add_argument("--output", default="artifacts/scenario_result.json")
    demo.add_argument("--max-object-mib", type=int, default=64)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "serve":
        service = ArchiveService(
            args.root,
            seed=args.seed,
            max_object_bytes=args.max_object_mib * MiB,
        )
        server = create_server(service, args.host, args.port)
        print(
            "READY "
            + json.dumps(
                {
                    "base_url": service.base_url,
                    "root": args.root,
                },
                sort_keys=True,
            ),
            flush=True,
        )
        try:
            server.serve_forever(poll_interval=0.25)
        except KeyboardInterrupt:
            pass
        finally:
            server.shutdown()
            server.server_close()
        return 0
    if args.command == "demo":
        from .scenario import Scenario

        service = ArchiveService(
            args.root,
            seed=args.seed,
            max_object_bytes=args.max_object_mib * MiB,
        )
        server = create_server(service, "127.0.0.1", 0)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        scenario_args = argparse.Namespace(
            base_url=service.base_url,
            count=args.count,
            delete_rate=args.delete_rate,
            history_count=args.history_count,
            output=args.output,
            prefix=args.prefix,
            seed=args.seed,
            snapshot_rate=args.snapshot_rate,
            target_mib=args.target_mib,
            timeout=args.timeout,
            write_rate=args.write_rate,
        )
        try:
            result = Scenario(scenario_args).run()
        except Exception as exc:  # noqa: BLE001 - CLI result record
            print(f"SCENARIO FAILED: {type(exc).__name__}: {exc}", file=sys.stderr)
            output = Path(args.output)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(
                json.dumps(
                    {
                        "error": f"{type(exc).__name__}: {exc}",
                        "scenario": "archive-bucket-fault-recovery-v1",
                        "status": "failed",
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
            return 1
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=10)
        rendered = json.dumps(result, indent=2, sort_keys=True, ensure_ascii=False)
        print(rendered)
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(rendered + "\n", encoding="utf-8")
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
