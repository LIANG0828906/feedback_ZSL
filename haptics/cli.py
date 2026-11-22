from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from .errors import HapticError
from .limits import ResourceLimits
from .sample import sample_capabilities, sample_events, write_sample_files
from .transport import MockDeviceService
from .validation import load_and_validate


def _add_verification_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--workdir", default="artifacts/demo", help="checkpoint/sample directory")
    parser.add_argument("--timeout-ms", type=float, default=30.0)
    parser.add_argument("--parallel", action="store_true", help="use bounded scheduling/encoding pool")
    parser.add_argument("--max-workers", type=int, default=4)
    parser.add_argument("--max-events-per-task", type=int, default=256)
    parser.add_argument("--max-packets-per-task", type=int, default=128)
    parser.add_argument("--json-out", default=None, help="write full verification JSON")
    parser.add_argument("--quiet-json", action="store_true", help="emit machine JSON only")
    parser.add_argument("--events", default=None, help="optional events JSON (requires --capabilities)")
    parser.add_argument(
        "--capabilities",
        default=None,
        help="optional capability JSON (requires --events)",
    )


def _limits_from_args(args: argparse.Namespace) -> ResourceLimits:
    return ResourceLimits(
        max_parallelism=args.max_workers,
        max_events_per_task=args.max_events_per_task,
        max_packets_per_task=args.max_packets_per_task,
    )


def cmd_sample(args: argparse.Namespace) -> int:
    events, capabilities = write_sample_files(args.workdir)
    print(json.dumps({"events": str(events), "capabilities": str(capabilities)}, indent=2))
    return 0


def cmd_validate(args: argparse.Namespace) -> int:
    stream, capabilities, result = load_and_validate(args.events, args.capabilities)
    print(json.dumps(result.to_dict(), ensure_ascii=False, indent=2))
    return 0 if result.ok else 2


def cmd_verify(args: argparse.Namespace) -> int:
    from .pipeline import verify_three_paths

    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    if bool(args.events) != bool(args.capabilities):
        print("--events and --capabilities must be supplied together", file=sys.stderr)
        return 2
    if args.events and args.capabilities:
        stream, capabilities, validation = load_and_validate(args.events, args.capabilities)
        if not validation.ok or stream is None or capabilities is None:
            print(json.dumps(validation.to_dict(), ensure_ascii=False, indent=2), file=sys.stderr)
            return 2
    else:
        write_sample_files(workdir)
        stream = sample_events()
        capabilities = sample_capabilities()
    summary = verify_three_paths(
        stream,
        capabilities,
        workdir,
        timeout_ms=args.timeout_ms,
        parallel=args.parallel,
        limits=_limits_from_args(args),
    )
    payload = summary.to_dict()
    if args.json_out:
        Path(args.json_out).write_text(
            json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
    if args.quiet_json:
        print(json.dumps(payload, ensure_ascii=False))
    else:
        print(
            json.dumps(
                {
                    "ok": summary.ok,
                    "sample_hash": summary.sample_hash,
                    "packet_count": len(summary.realtime.packets),
                    "decision_count": len(summary.realtime.decisions),
                    "service_codes": list(summary.service_codes),
                    "alignment_stats": dict(summary.realtime.alignment.stats),
                    "comparisons": [item.to_dict() for item in summary.comparisons],
                },
                ensure_ascii=False,
                indent=2,
            )
        )
    return 0 if summary.ok else 3


def cmd_gui(args: argparse.Namespace) -> int:
    from .workbench.app import main as gui_main

    return gui_main(
        [
            "--port",
            str(args.port),
            "--workdir",
            str(args.workdir),
        ]
    )


def cmd_serve(args: argparse.Namespace) -> int:
    service = MockDeviceService(port=args.port).start()
    print(json.dumps({"host": service.host, "port": service.port}), flush=True)
    try:
        while True:
            input()
    except (EOFError, KeyboardInterrupt):
        pass
    finally:
        service.stop()
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="haptic-kernel")
    subparsers = parser.add_subparsers(dest="command", required=True)

    sample = subparsers.add_parser("sample", help="write deterministic sample files")
    sample.add_argument("--workdir", default="artifacts/demo")
    sample.set_defaults(func=cmd_sample)

    validate = subparsers.add_parser("validate", help="load and validate JSON inputs")
    validate.add_argument("--events", required=True)
    validate.add_argument("--capabilities", required=True)
    validate.set_defaults(func=cmd_validate)

    verify = subparsers.add_parser("verify", help="run realtime/resume/replay verification")
    _add_verification_arguments(verify)
    verify.set_defaults(func=cmd_verify)

    gui = subparsers.add_parser("gui", help="open the PySide6 diagnostic workbench")
    gui.add_argument("--port", type=int, default=0, help="loopback port; 0 selects an ephemeral port")
    gui.add_argument("--workdir", default="artifacts/workbench")
    gui.set_defaults(func=cmd_gui)

    serve = subparsers.add_parser("serve", help="run only the mock loopback device")
    serve.add_argument("--port", type=int, default=0)
    serve.set_defaults(func=cmd_serve)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except HapticError as exc:
        print(json.dumps(exc.issue.to_dict(), ensure_ascii=False, indent=2), file=sys.stderr)
        return 2

