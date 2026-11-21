"""Command-line entry point."""

from __future__ import annotations

import argparse
import json
import sys
from typing import Any

from .demo import run_demo
from .errors import E_RECOVERY_INVALID, RhythmError
from .samples import generate_all
from .service import compact_view
from .store import DeliverySession
from .timebase import load_source_bundle


def _json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2)


def _load(source: str):
    bundle, wav_info, frame_table = load_source_bundle(source)
    coverage = min(wav_info.sample_count, frame_table.duration_samples)
    return bundle, frame_table, coverage


def _open_session(source: str, store: str) -> DeliverySession:
    bundle, frame_table, coverage = _load(source)
    return DeliverySession.open(bundle, frame_table, coverage, store)


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_all(args.out)
    print(_json(manifest))
    return 0


def command_verify(args: argparse.Namespace) -> int:
    bundle, wav_info, frame_table = load_source_bundle(args.source)
    print(
        _json(
            {
                "material_id": bundle.material_id,
                "sample_rate": bundle.sample_rate,
                "channels": wav_info.channels,
                "sample_count": wav_info.sample_count,
                "frame_count": frame_table.frame_count,
                "frame_duration_samples": frame_table.duration_samples,
                "sentence_count": len(bundle.items),
                "source_hash": bundle.source_hash,
            }
        )
    )
    return 0


def command_solve(args: argparse.Namespace) -> int:
    session = _open_session(args.source, args.store)
    if session.store.journal_path.exists():
        raise RhythmError(
            E_RECOVERY_INVALID,
            "a pending edit journal exists; run the recover command first",
            None,
            {"journal": str(session.store.journal_path)},
        )
    snapshot = session.initialize()
    print(_json(compact_view(snapshot.payload)))
    return 0


def command_edit(args: argparse.Namespace) -> int:
    session = _open_session(args.source, args.store)
    if session.store.journal_path.exists():
        raise RhythmError(
            E_RECOVERY_INVALID,
            "a pending edit journal exists; recovery is required",
            args.item_id,
            {"journal": str(session.store.journal_path)},
        )
    patch: dict[str, Any] = {}
    if args.text is not None:
        patch["text"] = args.text
    if args.priority is not None:
        patch["priority"] = args.priority
    if args.max_cps is not None:
        patch["max_cps"] = args.max_cps
    if not patch:
        raise RhythmError(E_RECOVERY_INVALID, "no edit fields were supplied", args.item_id)
    snapshot = session.update_item(
        args.item_id,
        patch,
        expected_version=args.expected_version,
    )
    print(_json(compact_view(snapshot.payload)))
    return 0


def command_recover(args: argparse.Namespace) -> int:
    session = _open_session(args.source, args.store)
    status = session.recover(expected_version=args.expected_version)
    if session.snapshot is None:
        raise RhythmError(E_RECOVERY_INVALID, "recovery produced no snapshot", None)
    print(
        _json(
            {
                "recovery": status.to_dict(),
                "snapshot": compact_view(session.snapshot.payload),
                "artifacts": {
                    "snapshot": str(session.store.path),
                    "speech_track": str(session.store.speech_track_path),
                    "description_track": str(session.store.description_track_path),
                    "compliance": str(session.store.compliance_path),
                    "trace": str(session.store.trace_path),
                },
            }
        )
    )
    return 0


def command_demo(args: argparse.Namespace) -> int:
    result = run_demo(args.out)
    print(_json(result))
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui import run_gui

    return run_gui(args.source, args.store)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="a11y-rhythm",
        description="Local deterministic accessibility rhythm solver",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic local samples")
    generate.add_argument("--out", default="artifacts/samples")
    generate.set_defaults(func=command_generate)

    verify = subparsers.add_parser("verify-timebase", help="validate WAV/JSON/frame timebase")
    verify.add_argument("--source", required=True)
    verify.set_defaults(func=command_verify)

    solve = subparsers.add_parser("solve", help="create version 1 from a source JSON")
    solve.add_argument("--source", required=True)
    solve.add_argument("--store", required=True)
    solve.set_defaults(func=command_solve)

    edit = subparsers.add_parser("edit", help="apply one sentence edit as a new version")
    edit.add_argument("--source", required=True)
    edit.add_argument("--store", required=True)
    edit.add_argument("--item-id", required=True)
    edit.add_argument("--text")
    edit.add_argument("--priority", type=int)
    edit.add_argument("--max-cps")
    edit.add_argument("--expected-version", type=int)
    edit.set_defaults(func=command_edit)

    recover = subparsers.add_parser("recover", help="resume a journal after termination")
    recover.add_argument("--source", required=True)
    recover.add_argument("--store", required=True)
    recover.add_argument("--expected-version", type=int)
    recover.set_defaults(func=command_recover)

    demo = subparsers.add_parser("demo", help="run deterministic acceptance scenario")
    demo.add_argument("--out", default="artifacts/demo")
    demo.set_defaults(func=command_demo)

    gui = subparsers.add_parser("gui", help="launch the PyQt5 workbench")
    gui.add_argument("--source", required=True)
    gui.add_argument("--store", required=True)
    gui.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except RhythmError as error:
        print(json.dumps(error.to_dict(), ensure_ascii=False, sort_keys=True), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        return 130

