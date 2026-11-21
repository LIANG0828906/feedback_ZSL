from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path
from typing import Any

from .engine import TranscodeEngine
from .errors import BusinessError, ErrorCode
from .samples import generate_samples
from .simulator import simulate_adaptive_playback
from .util import canonical_json_text, pretty_json_text, read_json, write_bytes_atomic
from .verify import verify_workspace


def _print(value: Any) -> None:
    if isinstance(value, str):
        print(value)
    else:
        print(pretty_json_text(value), end="")


def _split_tracks(value: str | None) -> list[str] | None:
    if value is None:
        return None
    tracks = [item.strip() for item in value.split(",") if item.strip()]
    return tracks or None


def _case_paths(sample_dir: Path) -> tuple[Path, Path, Path, Path | None]:
    return (
        sample_dir / "audio.wav",
        sample_dir / "frames.json",
        sample_dir / "config.json",
        sample_dir / "bandwidth.json" if (sample_dir / "bandwidth.json").exists() else None,
    )


def _build_or_load(
    workspace: Path,
    sample_dir: Path | None,
    audio: str | None,
    frames: str | None,
    config: str | None,
    clean: bool,
) -> tuple[TranscodeEngine, Path | None]:
    if clean and workspace.exists():
        shutil.rmtree(workspace)
    bandwidth_path: Path | None = None
    if sample_dir is not None:
        audio_path, frames_path, config_path, bandwidth_path = _case_paths(sample_dir)
        engine = TranscodeEngine.prepare_workspace(workspace, audio_path, frames_path, config_path)
        if bandwidth_path is not None:
            workspace.mkdir(parents=True, exist_ok=True)
            write_bytes_atomic(workspace / "bandwidth.json", bandwidth_path.read_bytes())
    elif audio or frames or config:
        if not (audio and frames and config):
            raise BusinessError(
                ErrorCode.CONFIG_INVALID,
                "--audio, --frames and --config must be supplied together",
            )
        engine = TranscodeEngine.prepare_workspace(workspace, audio, frames, config)
    else:
        engine = TranscodeEngine.load_workspace(workspace)
    if (workspace / "bandwidth.json").exists():
        bandwidth_path = workspace / "bandwidth.json"
    return engine, bandwidth_path


def _run_command(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace)
    sample_dir = Path(args.sample).resolve() if args.sample else None
    engine, inferred_bandwidth = _build_or_load(
        workspace,
        sample_dir,
        args.audio,
        args.frames,
        args.config,
        args.clean,
    )
    report = engine.run(
        track_ids=_split_tracks(args.tracks),
        crash_after_commits=args.crash_after_commits,
    )
    bandwidth_path = Path(args.bandwidth) if args.bandwidth else inferred_bandwidth
    if bandwidth_path is not None and report.get("status") != "cancelled":
        try:
            switch_trace = simulate_adaptive_playback(
                workspace,
                engine.config,
                engine.timeline,
                engine._indexes,
                read_json(bandwidth_path),
            )
        except BusinessError as exc:
            report["switch_error"] = exc.to_dict()
            _print(report)
            return 5
        report["switch_summary"] = {
            "valid": switch_trace["valid"],
            "switch_count": switch_trace["switch_count"],
            "final_track_id": switch_trace["final_track_id"],
        }
    _print(report)
    if report.get("status") == "cancelled":
        return 130
    if report.get("failed_tracks"):
        codes = {value.get("code") for value in report["failed_tracks"].values()}
        return 7 if ErrorCode.BUDGET_EXCEEDED in codes else 1
    return 0


def _simulate_command(args: argparse.Namespace) -> int:
    engine = TranscodeEngine.load_workspace(args.workspace)
    trace = simulate_adaptive_playback(
        args.workspace,
        engine.config,
        engine.timeline,
        engine._indexes,
        read_json(args.bandwidth),
    )
    _print(
        {
            "ok": trace["valid"],
            "valid": trace["valid"],
            "switch_count": trace["switch_count"],
            "final_track_id": trace["final_track_id"],
            "switches": trace["switches"],
            "trace": str(Path(args.workspace).resolve() / "switches.json"),
        }
    )
    return 0


def _verify_command(args: argparse.Namespace) -> int:
    result = verify_workspace(args.workspace)
    _print(result)
    return 0 if result["ok"] else 1


def _generate_command(args: argparse.Namespace) -> int:
    baseline = generate_samples(args.out, args.seed)
    _print(baseline)
    return 0


def _ui_command(args: argparse.Namespace) -> int:
    try:
        from .ui import run_ui
    except ImportError as exc:
        raise BusinessError(
            ErrorCode.CONFIG_INVALID,
            "PySide6 is required for the GUI; install the 'gui' extra or use the script commands",
            detail=str(exc),
        ) from exc
    return run_ui(
        workspace=args.workspace,
        sample=args.sample,
        bandwidth=args.bandwidth,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="livetranscode",
        description="Deterministic multi-track slice preparation and ABR simulation",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="write deterministic local sample cases")
    generate.add_argument("--out", required=True, help="sample output directory")
    generate.add_argument("--seed", type=int, default=1234)
    generate.set_defaults(func=_generate_command)

    run = subparsers.add_parser("run", help="prepare/resume slicing and optionally simulate playback")
    run.add_argument("--workspace", required=True)
    run.add_argument("--sample", help="sample case directory containing audio/frames/config")
    run.add_argument("--audio")
    run.add_argument("--frames")
    run.add_argument("--config")
    run.add_argument("--bandwidth", help="bandwidth JSON; defaults to workspace/bandwidth.json")
    run.add_argument("--tracks", help="comma-separated track ids")
    run.add_argument("--clean", action="store_true", help="remove the workspace before a fresh run")
    run.add_argument(
        "--crash-after-commits",
        type=int,
        default=None,
        help="fault-injection: hard-exit without an exit report after N atomic commits",
    )
    run.set_defaults(func=_run_command)

    resume = subparsers.add_parser("resume", help="resume only from committed disk indexes and snapshot")
    resume.add_argument("--workspace", required=True)
    resume.add_argument("--bandwidth")
    resume.add_argument("--tracks")
    resume.add_argument("--crash-after-commits", type=int, default=None)
    resume.set_defaults(
        func=lambda args: _run_command(
            argparse.Namespace(
                workspace=args.workspace,
                sample=None,
                audio=None,
                frames=None,
                config=None,
                bandwidth=args.bandwidth,
                tracks=args.tracks,
                clean=False,
                crash_after_commits=args.crash_after_commits,
            )
        )
    )

    simulate = subparsers.add_parser("simulate", help="run the adaptive playback switch validator")
    simulate.add_argument("--workspace", required=True)
    simulate.add_argument("--bandwidth", required=True)
    simulate.set_defaults(func=_simulate_command)

    verify = subparsers.add_parser("verify", help="validate committed slices, timestamps and hash chains")
    verify.add_argument("--workspace", required=True)
    verify.set_defaults(func=_verify_command)

    ui = subparsers.add_parser("ui", help="open the PySide6 operator interface")
    ui.add_argument("--workspace", required=True)
    ui.add_argument("--sample")
    ui.add_argument("--bandwidth")
    ui.set_defaults(func=_ui_command)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BusinessError as exc:
        print(canonical_json_text(exc.to_dict()), file=sys.stderr)
        if exc.code == ErrorCode.BUSY:
            return 3
        if exc.code == ErrorCode.SNAPSHOT_MISMATCH:
            return 4
        if exc.code == ErrorCode.SWITCH_INVALID:
            return 5
        if exc.code == ErrorCode.TIMEBASE_INVALID:
            return 6
        if exc.code == ErrorCode.BUDGET_EXCEEDED:
            return 7
        if exc.code == ErrorCode.CANCELLED:
            return 130
        return 2
    except json.JSONDecodeError as exc:
        print(
            canonical_json_text(
                {"ok": False, "code": ErrorCode.IO_ERROR, "message": "invalid JSON", "context": {"detail": str(exc)}}
            ),
            file=sys.stderr,
        )
        return 2
    except KeyboardInterrupt:
        print(
            canonical_json_text(
                {"ok": False, "code": ErrorCode.CANCELLED, "message": "interrupted", "context": {}}
            ),
            file=sys.stderr,
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

