from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .engine import TranscodeEngine
from .errors import TaskError
from .models import canonical_json_bytes
from .samples import generate_fixtures


def _print(value: Any) -> None:
    sys.stdout.buffer.write(canonical_json_bytes(value) + b"\n")
    sys.stdout.buffer.flush()


def _add_tier_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--tier", required=True, help="tier id")
    parser.add_argument("--rate", type=int, default=44100, help="target sample rate")
    parser.add_argument("--filter", type=int, default=48, help="even filter length")
    parser.add_argument("--quant", type=int, choices=(16, 24), default=16)
    parser.add_argument("--slice-samples", type=int, default=2048)


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="live-transcode",
        description="Deterministic local multichannel WAV transcode slicer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic WAV fixtures")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=19093)
    generate.add_argument("--duration", type=float, default=2.4)

    transcode = subparsers.add_parser("transcode", help="generate or resume one tier")
    transcode.add_argument("--wav", required=True)
    transcode.add_argument("--store", required=True)
    _add_tier_arguments(transcode)
    transcode.add_argument(
        "--kill-after",
        type=int,
        default=0,
        help="hard-exit after this many newly committed slices (recovery test)",
    )

    audit = subparsers.add_parser("audit", help="audit a published tier")
    audit.add_argument("--wav", required=True)
    audit.add_argument("--store", required=True)
    audit.add_argument("--tier", required=True)
    audit.add_argument("--expected-manifest-hash")

    audit_run = subparsers.add_parser("audit-run", help="audit an in-progress or complete run")
    audit_run.add_argument("--wav", required=True)
    audit_run.add_argument("--store", required=True)
    audit_run.add_argument("--run-dir", required=True)

    status = subparsers.add_parser("status", help="show published manifest")
    status.add_argument("--store", required=True)

    gui = subparsers.add_parser("gui", help="launch the PySide6 review interface")
    gui.add_argument("--store", default="artifacts/review_store")
    gui.add_argument("--samples", default="artifacts/samples")

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            fixtures = generate_fixtures(
                args.out,
                seed=args.seed,
                duration=args.duration,
            )
            _print({"ok": True, "fixtures": fixtures})
            return 0

        if args.command == "transcode":
            engine = TranscodeEngine(args.store)
            tier = engine.catalog.configure(
                args.tier,
                target_rate=args.rate,
                filter_length=args.filter,
                quant_bits=args.quant,
                slice_samples=args.slice_samples,
            )
            committed = {"count": 0}

            def progress(event: dict[str, Any]) -> None:
                if event.get("event") == "slice":
                    committed["count"] += 1
                    if args.kill_after and committed["count"] >= args.kill_after:
                        os._exit(137)

            result = engine.generate_tier(args.wav, tier, progress=progress)
            _print(
                {
                    "ok": True,
                    "run_id": result.run_id,
                    "run_dir": str(result.run_dir),
                    "cancelled": result.cancelled,
                    "resumed": result.resumed,
                    "resume_index": result.resume_index,
                    "tier_version_hash": result.tier_version_hash,
                    "generation": result.generation,
                    "generation_hash": result.generation_hash,
                    "total_slices": (
                        result.sidecar["total_slices"] if result.sidecar else None
                    ),
                    "reused_slices": result.reused_slices,
                    "recomputed_slices": result.recomputed_slices,
                    "reused_audio_slices": result.reused_audio_slices,
                    "regenerated_audio_slices": result.regenerated_audio_slices,
                    "sidecar_recomputed_slices": result.sidecar_recomputed_slices,
                }
            )
            return 0

        if args.command == "audit":
            engine = TranscodeEngine(args.store)
            report = engine.audit_published_tier(
                args.wav,
                args.tier,
                expected_manifest_hash=args.expected_manifest_hash,
            )
            _print({"ok": True, "report": report})
            return 0

        if args.command == "audit-run":
            engine = TranscodeEngine(args.store)
            report = engine.audit_committed_run(args.wav, args.run_dir)
            _print({"ok": True, "report": report})
            return 0

        if args.command == "status":
            engine = TranscodeEngine(args.store)
            manifest = engine.store.read_manifest()
            _print({"ok": True, "manifest": manifest})
            return 0

        if args.command == "gui":
            from .gui import run_gui

            return run_gui(store_root=args.store, sample_root=args.samples)
    except TaskError as exc:
        payload = {"ok": False, "error": exc.as_dict()}
        sys.stderr.buffer.write(canonical_json_bytes(payload) + b"\n")
        sys.stderr.buffer.flush()
        return 2
    except KeyboardInterrupt:
        return 130
    parser.error("unreachable command")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())

