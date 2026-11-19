"""Command line entry points.

Subcommands:
  run        bring up the full local stack and execute the workload once
  stack      run the services only (target, proxies, API) until Ctrl+C
  validate   run manifest/resource contract checks without uploads
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

from .config import load_config
from .supervisor import bring_up_stack
from .workload import build_manifest, validate_envelope


def _write_report(config, report: dict) -> Path:
    path = Path(config.report_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(report, indent=2, sort_keys=True), encoding="utf-8")
    return path


def cmd_validate(args) -> int:
    config = load_config(args.config)
    manifest = build_manifest(config)
    problems = validate_envelope(config, manifest)
    total_bytes = sum(task.total_bytes for task in manifest)
    payload = {
        "tasks": len(manifest),
        "shards": sum(task.shard_count for task in manifest),
        "total_bytes": total_bytes,
        "total_mib": round(total_bytes / 1024 / 1024, 2),
        "problems": problems,
    }
    print(json.dumps(payload, indent=2, sort_keys=True))
    return 1 if problems else 0


def cmd_run(args) -> int:
    config = load_config(args.config)
    manifest = build_manifest(config)
    problems = validate_envelope(config, manifest)
    if problems:
        print(json.dumps({"error": "contract_violation", "problems": problems}, indent=2))
        return 2
    stack = bring_up_stack(config, reset=not args.keep)
    started = time.monotonic()
    try:
        for task in manifest:
            stack.target_store.register_task(task.task_id, task.shard_count, task.total_bytes)
        stack.orchestrator.run(manifest)
        report = stack.orchestrator.report()
        report["wall_duration_s"] = round(time.monotonic() - started, 3)
        path = _write_report(config, report)
        totals = report["totals"]
        print(json.dumps({"report": str(path), "totals": totals}, indent=2, sort_keys=True))
        if totals["failed_shards"]:
            return 1
        return 0
    finally:
        stack.shutdown()


def cmd_stack(args) -> int:
    config = load_config(args.config)
    stack = bring_up_stack(config)
    print(json.dumps(stack.addresses(), indent=2, sort_keys=True))
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        stack.shutdown()
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="edge-sync")
    sub = parser.add_subparsers(dest="command", required=True)
    for name, function in (("run", cmd_run), ("stack", cmd_stack), ("validate", cmd_validate)):
        item = sub.add_parser(name)
        item.add_argument("--config", default="configs/edge_sync.json")
        item.set_defaults(func=function)
    parser_run = sub.choices["run"]
    parser_run.add_argument("--keep", action="store_true", help="keep existing runtime DB rows")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
