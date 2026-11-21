from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path
from typing import Any

from .demo import run_demo
from .errors import BusinessError
from .generator import generate_corpus
from .hashing import pretty_json_bytes, write_bytes_atomic
from .session import LayoutSession
from .timebase import load_media
from .models import SubtitleAsset


def _json_print(value: Any) -> None:
    sys.stdout.buffer.write(pretty_json_bytes(value))


def _require_case(args: argparse.Namespace) -> Path:
    if args.case is None:
        raise BusinessError("E_INVALID_INPUT", "--case is required")
    return Path(args.case)


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_corpus(Path(args.out), seed=args.seed)
    _json_print(manifest)
    return 0


def command_demo(args: argparse.Namespace) -> int:
    report = run_demo(Path(args.out), seed=args.seed, clean=not args.keep)
    _json_print(report)
    return 0


def command_offline(args: argparse.Namespace) -> int:
    case_dir = _require_case(args)
    asset = SubtitleAsset.load(args.asset or case_dir / "subtitles.json")
    wav, frames, _ = load_media(asset, args.wav or case_dir / "program.wav", args.frames or case_dir / "frames.ftbl")
    from .layout import ZERO_HASH, compute_layout

    report = compute_layout(asset, wav, frames, revision=1, parent_hash=ZERO_HASH)
    if args.output:
        write_bytes_atomic(Path(args.output), pretty_json_bytes(report))
    _json_print(
        {
            "commit_hash": report["commit_hash"],
            "counts": report["counts"],
            "output": args.output,
            "sentences": report["sentences"],
        }
    )
    return 0


def command_inspect(args: argparse.Namespace) -> int:
    case_dir = _require_case(args)
    with LayoutSession.open(case_dir, trace_path=os.devnull) as session:
        report = session.report
        payload = {
            "asset_id": session.asset.asset_id,
            "commit_hash": session.commit_hash,
            "has_pending": session.has_pending,
            "pending": session.pending_info() if session.has_pending else None,
            "revision": session.revision,
        }
        if report is not None:
            payload["counts"] = report["counts"]
        _json_print(payload)
    return 0


def command_compare(args: argparse.Namespace) -> int:
    case_dir = _require_case(args)
    with LayoutSession.open(case_dir, trace_path=os.devnull) as session:
        offline = session.compare_with_offline()
        _json_print(
            {
                "offline_commit_hash": offline["commit_hash"],
                "online_commit_hash": session.commit_hash,
                "status": "equal",
            }
        )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .ui import run_ui

    return run_ui(Path(args.case), refresh_ms=args.refresh_ms)


def command_crash_worker(args: argparse.Namespace) -> int:
    case_dir = _require_case(args)
    checkpoints = {"count": 0}

    def crash_after_checkpoint(_: Any) -> None:
        checkpoints["count"] += 1
        if checkpoints["count"] >= args.after_checkpoints:
            os._exit(70)

    # The journal is written by the normal commit path before checkpoints.
    # This worker consumes a pending journal if one exists, otherwise it stages
    # the same deterministic default-CPS edit used by the demo.
    with LayoutSession.open(case_dir, trace_path=args.trace or case_dir / "trace.jsonl") as session:
        if not session.has_pending:
            session.stage_params({"default_cps": args.default_cps})
            session.stage_descriptions(
                [
                    *[item.to_dict() for item in session.asset.descriptions],
                    {
                        "after_sentence_id": session.asset.sentences[2].id,
                        "id": "D9001",
                        "max_cps": "9",
                        "min_duration_ms": 400,
                        "priority": 70,
                        "speaker": "NARRATOR",
                        "text": "恢复检查点后的描述轨必须只重算未完成句区间",
                    },
                ]
            )
        session.commit(fault_hook=crash_after_checkpoint)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="subtitle-localizer")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", default="artifacts/demo")
    generate.add_argument("--seed", type=int, default=7)
    generate.set_defaults(func=command_generate)

    demo = sub.add_parser("demo", help="run the end-to-end deterministic demonstration")
    demo.add_argument("--out", default="artifacts/demo")
    demo.add_argument("--seed", type=int, default=7)
    demo.add_argument("--keep", action="store_true", help="keep existing output directory")
    demo.set_defaults(func=command_demo)

    offline = sub.add_parser("offline", help="compute a full offline layout")
    offline.add_argument("--case")
    offline.add_argument("--asset")
    offline.add_argument("--wav")
    offline.add_argument("--frames")
    offline.add_argument("--output")
    offline.set_defaults(func=command_offline)

    inspect = sub.add_parser("inspect", help="inspect committed state and pending recovery")
    inspect.add_argument("--case", required=True)
    inspect.set_defaults(func=command_inspect)

    compare = sub.add_parser("compare", help="compare committed layout with an offline pass")
    compare.add_argument("--case", required=True)
    compare.set_defaults(func=command_compare)

    gui = sub.add_parser("gui", help="open the PySide6 workbench")
    gui.add_argument("--case", required=True)
    gui.add_argument("--refresh-ms", type=int, default=250)
    gui.set_defaults(func=command_gui)

    crash = sub.add_parser("_crash-worker", help=argparse.SUPPRESS)
    crash.add_argument("--case", required=True)
    crash.add_argument("--trace")
    crash.add_argument("--after-checkpoints", type=int, default=2)
    crash.add_argument("--default-cps", default="11")
    crash.set_defaults(func=command_crash_worker)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        sys.stdout.buffer.write(pretty_json_bytes(exc.to_dict()))
        return 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

