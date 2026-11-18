"""Command-line interface for scanning, recovery, reports and serving."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .archive import ScanConfig
from .errors import ArchiveError
from .http_api import serve
from .samplegen import generate_archive
from .service import ArchiveService
from .wal import DEFAULT_SEGMENT_SIZE


def _print_json(payload: Any) -> None:
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2))


def _write_json(path: str, payload: Any) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    target.write_text(text, encoding="utf-8", newline="\n")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="wal-archive",
        description=(
            "Auditable shared PostgreSQL WAL archive view, recovery-point "
            "selection, validation and reporting."
        ),
    )
    parser.add_argument(
        "--root",
        default=os.environ.get("WAL_ARCHIVE_ROOT", "./archive"),
        help="archive root (default: $WAL_ARCHIVE_ROOT or ./archive)",
    )
    parser.add_argument(
        "--db",
        default=os.environ.get("WAL_ARCHIVE_DB", "./wal_archive.sqlite3"),
        help="SQLite catalog path",
    )
    parser.add_argument(
        "--segment-size",
        type=int,
        default=int(os.environ.get("WAL_SEGMENT_SIZE", DEFAULT_SEGMENT_SIZE)),
        help="WAL segment size in bytes (default: 524288)",
    )
    parser.add_argument(
        "--writer-grace-seconds",
        type=float,
        default=float(os.environ.get("WAL_WRITER_GRACE_SECONDS", "5")),
    )
    parser.add_argument(
        "--stability-probe-seconds",
        type=float,
        default=float(os.environ.get("WAL_STABILITY_PROBE_SECONDS", "0.2")),
    )
    parser.add_argument(
        "--no-scan",
        action="store_true",
        help="use the latest committed scan instead of scanning before the command",
    )

    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("scan", help="scan the archive and commit a new view")

    recover = sub.add_parser("recover", help="select, verify and commit a recovery point")
    recover.add_argument("--instance", required=True)
    recover.add_argument("--timeline", type=int, required=True)
    recover.add_argument("--target-time", required=True)
    recover.add_argument("--source", default=None)

    report = sub.add_parser("report", help="emit the deterministic archive report")
    report.add_argument("--output", default=None)

    status = sub.add_parser("status", help="emit a JSON status summary")
    status.add_argument("--instance", required=True)
    status.add_argument("--timeline", type=int, default=None)

    metrics = sub.add_parser("metrics", help="emit Prometheus text metrics")
    metrics.add_argument("--output", default=None)

    conflicts = sub.add_parser("conflicts", help="list conflicting segment versions")
    conflicts.add_argument("--timeline", type=int, default=None)
    conflicts.add_argument("--segno", type=int, default=None)

    validate = sub.add_parser("validate", help="live-validate one segment version")
    validate.add_argument("--instance", required=True)
    validate.add_argument("--timeline", type=int, required=True)
    validate.add_argument("--segno", type=int, required=True)
    validate.add_argument("--digest", default=None)

    recoveries = sub.add_parser("recoveries", help="list committed recovery points")
    recoveries.add_argument("--instance", default=None)
    recoveries.add_argument("--timeline", type=int, default=None)

    clear = sub.add_parser(
        "clear-quarantine", help="explicitly clear a sticky truncated-path quarantine"
    )
    clear.add_argument("--instance", required=True)
    clear.add_argument("--filename", required=True)

    server = sub.add_parser("serve", help="run the HTTP service")
    server.add_argument("--host", default=os.environ.get("WAL_ARCHIVE_HOST", "127.0.0.1"))
    server.add_argument("--port", type=int, default=int(os.environ.get("WAL_ARCHIVE_PORT", "8080")))

    gen = sub.add_parser("generate", help="generate the deterministic sample archive")
    gen.add_argument("--profile", choices=("full", "small"), default="full")
    gen.add_argument("--seed", type=int, default=310)
    gen.add_argument("--force", action="store_true")

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)

    if args.command == "generate":
        try:
            generated = generate_archive(
                args.root,
                seed=args.seed,
                segment_size=args.segment_size,
                profile=args.profile,
                force=args.force,
            )
        except (FileExistsError, ValueError) as exc:
            print(
                json.dumps(
                    {
                        "error": {
                            "code": "WAL007",
                            "name": "E_INVALID_REQUEST",
                            "message": str(exc),
                        }
                    },
                    sort_keys=True,
                    indent=2,
                ),
                file=sys.stderr,
            )
            return 2
        _print_json(generated.as_dict())
        return 0

    config = ScanConfig(
        segment_size=args.segment_size,
        writer_grace_seconds=args.writer_grace_seconds,
        stability_probe_seconds=args.stability_probe_seconds,
    )
    service: ArchiveService | None = None
    try:
        service = ArchiveService(args.root, args.db, config=config)
        if args.command != "serve" and not args.no_scan:
            service.scan()
        if args.command == "scan":
            _print_json(service.last_scan_summary or service.scan())
            return 0
        if args.command == "recover":
            _print_json(
                service.resolve_recovery(
                    instance=args.instance,
                    timeline=args.timeline,
                    target_time=args.target_time,
                    source_instance=args.source,
                    refresh=False,
                )
            )
            return 0
        if args.command == "report":
            payload = service.report()
            if args.output:
                _write_json(args.output, payload)
            else:
                _print_json(payload)
            return 0
        if args.command == "status":
            _print_json(
                service.status(instance=args.instance, timeline=args.timeline)
            )
            return 0
        if args.command == "metrics":
            text = service.metrics()
            if args.output:
                Path(args.output).write_text(text, encoding="utf-8", newline="\n")
            else:
                sys.stdout.write(text)
            return 0
        if args.command == "conflicts":
            _print_json(
                service.find_conflicts(timeline=args.timeline, segno=args.segno)
            )
            return 0
        if args.command == "validate":
            _print_json(
                service.validate_segment(
                    instance=args.instance,
                    timeline=args.timeline,
                    segno=args.segno,
                    digest=args.digest,
                    refresh=False,
                )
            )
            return 0
        if args.command == "recoveries":
            _print_json(
                service.list_recoveries(
                    instance=args.instance, timeline=args.timeline
                )
            )
            return 0
        if args.command == "clear-quarantine":
            removed = service.store.clear_quarantine(args.instance, args.filename)
            _print_json({"cleared": removed})
            return 0
        if args.command == "serve":
            if not args.no_scan:
                service.scan()
            print(
                f"wal-archive listening on http://{args.host}:{args.port} "
                f"(db={args.db})",
                file=sys.stderr,
            )
            try:
                serve(service, args.host, args.port)
            except KeyboardInterrupt:
                pass
            return 0
        parser.error(f"unsupported command: {args.command}")
        return 2
    except ArchiveError as exc:
        print(
            json.dumps({"error": exc.as_dict()}, sort_keys=True, indent=2),
            file=sys.stderr,
        )
        return 2
    except (FileExistsError, ValueError) as exc:
        payload = {
            "error": {
                "code": "WAL007",
                "name": "E_INVALID_REQUEST",
                "message": str(exc),
            }
        }
        print(json.dumps(payload, sort_keys=True, indent=2), file=sys.stderr)
        return 2
    finally:
        if service is not None:
            service.close()


if __name__ == "__main__":
    sys.exit(main())
