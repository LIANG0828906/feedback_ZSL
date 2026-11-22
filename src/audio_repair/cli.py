"""Command line entry points for generation, repair, verification and UI."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .audio_io import load_audio, write_wav_atomic
from .config import RepairConfig, SessionConfig, default_config
from .engine import RepairSession, process_full_recompute
from .errors import AppError
from .samples import generate_deterministic_samples
from .verification import render_markdown_summary, run_acceptance


def _print(payload: object) -> None:
    print(json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False))


def _repair_config(args: argparse.Namespace) -> RepairConfig:
    base = default_config(args.profile)
    values = base.canonical_dict()
    for field in (
        "frame_size",
        "hop_size",
        "window",
        "strength",
        "floor",
        "noise_frames",
        "noise_mode",
        "freq_smooth",
        "temporal_smooth",
    ):
        supplied = getattr(args, field, None)
        if supplied is not None:
            values[field] = supplied
    return RepairConfig(**values).validate()


def _cmd_samples(args: argparse.Namespace) -> int:
    _print(generate_deterministic_samples(args.output))
    return 0


def _cmd_run(args: argparse.Namespace) -> int:
    audio = load_audio(args.input)
    repair_config = _repair_config(args)
    session_config = SessionConfig(
        segment_samples=args.segment_samples,
        max_workers=args.workers,
        max_buffer_bytes=args.max_buffer_bytes,
    )
    if args.full_recompute:
        output = process_full_recompute(audio, repair_config, session_config)
        write_wav_atomic(args.output, output, audio.sample_rate)
        _print(
            {
                "ok": True,
                "path": str(args.output),
                "samples": int(output.shape[0]),
                "channels": int(output.shape[1]),
                "full_recompute": True,
            }
        )
        return 0
    session = RepairSession(
        audio,
        repair_config,
        session_config,
        args.checkpoint,
        clear_checkpoint=args.clear,
    )

    def progress(event: object) -> None:
        if args.verbose:
            print(json.dumps(event.to_dict(), ensure_ascii=False), file=sys.stderr)

    session.set_callbacks(progress_callback=progress)
    result = session.run(retry_failed=args.retry_failed)
    payload: dict[str, object] = {
        "ok": result.complete,
        "result": result.to_dict(),
        "load_info": session.load_info.to_dict(),
    }
    if result.complete:
        stats = session.export(args.output)
        payload["output"] = str(args.output)
        payload["stats"] = stats.to_dict()
    _print(payload)
    return 0 if result.complete else 2


def _cmd_verify(args: argparse.Namespace) -> int:
    report = run_acceptance(
        args.samples, args.work, args.exports, report_path=args.report
    )
    args.summary.write_text(render_markdown_summary(report), encoding="utf-8")
    _print(
        {
            "ok": report["ok"],
            "summary": report.get("summary"),
            "report": str(args.report),
            "summary_markdown": str(args.summary),
        }
    )
    return 0 if report["ok"] else 1


def _cmd_ui(_args: argparse.Namespace) -> int:
    from .app import main as ui_main

    return ui_main()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="audio-repair")
    sub = parser.add_subparsers(dest="command", required=True)

    samples = sub.add_parser("samples", help="generate deterministic local fixtures")
    samples.add_argument("--output", type=Path, default=Path("artifacts/samples"))
    samples.set_defaults(func=_cmd_samples)

    run = sub.add_parser("run", help="repair a local WAV")
    run.add_argument("--input", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--checkpoint", type=Path, default=Path("artifacts/checkpoints"))
    run.add_argument("--profile", choices=("music", "speech"), default="music")
    run.add_argument("--segment-samples", dest="segment_samples", type=int, default=16384)
    run.add_argument("--workers", type=int, default=4)
    run.add_argument("--max-buffer-bytes", dest="max_buffer_bytes", type=int, default=64 * 1024 * 1024)
    run.add_argument("--clear", action="store_true", help="clear the checkpoint snapshot first")
    run.add_argument("--retry-failed", action="store_true")
    run.add_argument("--full-recompute", action="store_true")
    run.add_argument("--verbose", action="store_true")
    run.add_argument("--frame-size", type=int)
    run.add_argument("--hop-size", type=int)
    run.add_argument("--window", choices=("hann", "hamming", "blackman", "rectangular"))
    run.add_argument("--strength", type=float)
    run.add_argument("--floor", type=float)
    run.add_argument("--noise-frames", dest="noise_frames", type=int)
    run.add_argument("--noise-mode", dest="noise_mode", choices=("leading", "silence"))
    run.add_argument("--freq-smooth", dest="freq_smooth", type=int)
    run.add_argument("--temporal-smooth", dest="temporal_smooth", type=float)
    run.set_defaults(func=_cmd_run)

    verify = sub.add_parser("verify", help="run the deterministic acceptance suite")
    verify.add_argument("--samples", type=Path, default=Path("artifacts/samples"))
    verify.add_argument("--work", type=Path, default=Path("artifacts/verify"))
    verify.add_argument("--exports", type=Path, default=Path("artifacts/exports"))
    verify.add_argument(
        "--report", type=Path, default=Path("artifacts/acceptance_report.json")
    )
    verify.add_argument(
        "--summary", type=Path, default=Path("artifacts/acceptance_summary.md")
    )
    verify.set_defaults(func=_cmd_verify)

    ui = sub.add_parser("ui", help="launch the PySide6 desktop application")
    ui.set_defaults(func=_cmd_ui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except AppError as exc:
        _print(exc.to_dict())
        return 1
    except KeyboardInterrupt:
        _print({"ok": False, "code": "E_CANCELLED", "message": "Interrupted"})
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

