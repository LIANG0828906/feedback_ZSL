from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .common import BusinessError, SCHEMA_VERSION
from .http_api import serve
from .service import BackupChainService


def default_manifest(repo: Path) -> Path:
    manifest = repo / "manifest.json"
    if manifest.exists():
        return manifest
    return repo / "chain-manifest.json"


def print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def add_common(parser: argparse.ArgumentParser, *, require_manifest: bool) -> None:
    parser.add_argument("--repo", required=True, type=Path)
    if require_manifest:
        parser.add_argument("--manifest", type=Path)
    parser.add_argument("--db", type=Path, default=Path("state/backup-chain.sqlite3"))
    parser.add_argument(
        "--output-dir", type=Path, default=Path("restore-artifacts")
    )


def build_service(args: argparse.Namespace) -> BackupChainService:
    repo = args.repo
    manifest = args.manifest if getattr(args, "manifest", None) else default_manifest(repo)
    return BackupChainService(
        repo=repo,
        manifest=manifest,
        database=args.db,
        output_dir=args.output_dir,
    )


def cmd_audit(args: argparse.Namespace) -> int:
    service = build_service(args)
    try:
        result = service.audit()
    finally:
        service.close()
    print_json(result)
    return 0


def cmd_drill(args: argparse.Namespace) -> int:
    service = build_service(args)
    try:
        result = service.run_drill(
            restore_point=args.restore_point, audit_id=args.audit_id
        )
    finally:
        service.close()
    print_json(result)
    status = result.get("status")
    if status == "completed":
        return 0
    if status == "interrupted":
        return 75
    return 4


def cmd_status(args: argparse.Namespace) -> int:
    service = build_service(args)
    try:
        result = service.status(
            chain_id=args.chain_id,
            restore_point=args.restore_point,
            audit_id=args.audit_id,
        )
    finally:
        service.close()
    print_json(result)
    return 0


def cmd_metrics(args: argparse.Namespace) -> int:
    service = build_service(args)
    try:
        sys.stdout.write(service.metrics_text())
    finally:
        service.close()
    return 0


def cmd_serve(args: argparse.Namespace) -> int:
    service = build_service(args)
    try:
        if args.audit_on_start:
            service.audit()
        try:
            serve(service, host=args.host, port=args.port)
        except KeyboardInterrupt:
            return 0
    finally:
        service.close()
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m backup_chain",
        description=(
            "Read-only backup-chain audit, restore-point selection and "
            "resumable restore drill service."
        ),
    )
    sub = parser.add_subparsers(dest="command", required=True)

    audit = sub.add_parser("audit", help="audit the repository and commit a chain view")
    add_common(audit, require_manifest=True)
    audit.set_defaults(func=cmd_audit)

    drill = sub.add_parser("drill", help="run or resume a restore drill")
    add_common(drill, require_manifest=True)
    drill.add_argument("--restore-point", required=True, type=int)
    drill.add_argument("--audit-id", type=int)
    drill.set_defaults(func=cmd_drill)

    status = sub.add_parser("status", help="show frozen restore-point selections")
    add_common(status, require_manifest=True)
    status.add_argument("--chain-id")
    status.add_argument("--restore-point", type=int)
    status.add_argument("--audit-id", type=int)
    status.set_defaults(func=cmd_status)

    metrics = sub.add_parser("metrics", help="print Prometheus text metrics")
    add_common(metrics, require_manifest=False)
    metrics.set_defaults(func=cmd_metrics)

    server = sub.add_parser("serve", help="run the HTTP API and metrics endpoints")
    add_common(server, require_manifest=True)
    server.add_argument("--host", default="127.0.0.1")
    server.add_argument("--port", type=int, default=8765)
    server.add_argument("--audit-on-start", action="store_true")
    server.set_defaults(func=cmd_serve)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        print_json(
            {
                "error": exc.as_dict(),
                "schema_version": SCHEMA_VERSION,
            }
        )
        return 2
