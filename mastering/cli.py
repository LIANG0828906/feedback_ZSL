from __future__ import annotations

import argparse
import json
import os
import signal
import sys
from pathlib import Path
from typing import Any

from .dsp import true_peak_dbtp
from .engine import InterruptController, MasteringEngine
from .errors import MasteringError
from .loudness import integrated_loudness
from .synth import create_demo_workspace
from .wavio import probe_wav, read_wav, sha256_file


def _json_line(payload: dict[str, Any]) -> str:
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":"), allow_nan=False)


def _controller_from_environment() -> InterruptController:
    interrupt_after = os.environ.get("MASTERING_SIMULATE_SIGINT_AFTER_BLOCKS")
    crash_after = os.environ.get("MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS")
    return InterruptController(
        interrupt_after_blocks=int(interrupt_after) if interrupt_after else None,
        crash_after_blocks=int(crash_after) if crash_after else None,
    )


def _run(args: argparse.Namespace) -> int:
    controller = _controller_from_environment()
    previous_handler = None
    try:
        previous_handler = signal.signal(signal.SIGINT, controller.request_interrupt)
    except (ValueError, OSError):
        previous_handler = None
    try:
        engine = MasteringEngine(
            args.project,
            rules_path=args.rules,
            output_dir=args.output,
            state_dir=args.state_dir,
            block_frames=args.block_frames,
            controller=controller,
        )
        result = engine.run()
        if result.status == "completed":
            languages = [entry["language"] for entry in result.payload.get("languages", [])]
            payload = {
                "status": "completed",
                "exit_code": 0,
                "report": "mix_report.json",
                "languages": languages,
            }
        else:
            payload = result.payload
        print(_json_line(payload))
        return result.exit_code
    except MasteringError as exc:
        print(_json_line(exc.as_dict()))
        return exc.exit_code
    except KeyboardInterrupt:
        print(
            _json_line(
                {
                    "status": "error",
                    "errors": [
                        {
                            "code": "INTERRUPTED_BEFORE_CHECKPOINT",
                            "track": "engine",
                            "message": "interrupt arrived before a recoverable checkpoint",
                        }
                    ],
                }
            )
        )
        return 130
    except Exception as exc:
        print(
            _json_line(
                {
                    "status": "error",
                    "errors": [
                        {
                            "code": "INTERNAL_ERROR",
                            "track": "engine",
                            "message": str(exc),
                        }
                    ],
                }
            )
        )
        return 3
    finally:
        if previous_handler is not None:
            signal.signal(signal.SIGINT, previous_handler)


def _verify(args: argparse.Namespace) -> int:
    report_path = args.report.resolve()
    checks: list[dict[str, Any]] = []
    try:
        report = json.loads(report_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(
            _json_line(
                {
                    "status": "error",
                    "errors": [
                        {
                            "code": "REPORT_INVALID",
                            "track": "mix_report",
                            "message": str(exc),
                        }
                    ],
                }
            )
        )
        return 2

    ok = True
    for entry in report.get("languages", []):
        language = entry.get("language", "unknown")
        file_entry = entry.get("file", {})
        relative = file_entry.get("path")
        if not isinstance(relative, str) or Path(relative).is_absolute() or ".." in Path(relative).parts:
            checks.append({"language": language, "ok": False, "problem": "invalid file path"})
            ok = False
            continue
        path = report_path.parent / relative
        info, issues = probe_wav(path, f"master_{language}")
        if info is None or issues:
            checks.append(
                {
                    "language": language,
                    "ok": False,
                    "problem": issues[0].message if issues else "WAV cannot be read",
                }
            )
            ok = False
            continue
        problems: list[str] = []
        if info.sample_rate != report.get("sample_rate"):
            problems.append("sample rate differs from report")
        if info.channels != report.get("channels"):
            problems.append("channel count differs from report")
        if info.bit_depth != report.get("bit_depth"):
            problems.append("bit depth differs from report")
        if info.frames != report.get("total_frames"):
            problems.append("frame count differs from report")
        if path.stat().st_size != file_entry.get("bytes"):
            problems.append("file size differs from report")
        if sha256_file(path) != file_entry.get("sha256"):
            problems.append("SHA-256 differs from report")
        loudness: float | None = None
        peak: float | None = None
        samples, read_info, read_issues = read_wav(path, f"master_{language}")
        if samples is None or read_info is None or read_issues:
            problems.append("WAV samples cannot be decoded")
        else:
            loudness = integrated_loudness(samples, read_info.sample_rate)
            master = entry.get("master", {})
            oversample = int(
                args.oversample
                if args.oversample is not None
                else master.get("true_peak_oversample", 4)
            )
            peak = true_peak_dbtp(samples, oversample)
            if loudness is None or master.get("loudness_lufs") is None:
                problems.append("loudness is undefined")
            elif abs(loudness - float(master["loudness_lufs"])) > 1e-3:
                problems.append("loudness cannot be reproduced from the master")
            if abs(peak - float(master.get("true_peak_dbtp", -300.0))) > 1e-3:
                problems.append("true peak cannot be reproduced from the master")
            limit = float(master.get("true_peak_limit_dbtp", -1.0))
            target = float(master.get("target_lufs", 0.0))
            if peak > limit + 1e-6:
                problems.append("true peak exceeds its limit")
            if loudness is None or abs(loudness - target) > 0.5:
                problems.append("loudness is outside the target tolerance")
        checks.append(
            {
                "language": language,
                "ok": not problems,
                "loudness_lufs": round(loudness, 6) if loudness is not None else None,
                "true_peak_dbtp": round(peak, 6) if peak is not None else None,
                "problems": problems,
            }
        )
        if problems:
            ok = False
    payload = {"status": "ok" if ok else "failed", "checks": checks}
    print(_json_line(payload))
    return 0 if ok else 1


def _demo(args: argparse.Namespace) -> int:
    workdir = Path(args.workdir)
    project, _rules = create_demo_workspace(workdir)
    return _run(
        argparse.Namespace(
            project=project,
            rules=None,
            output=None,
            state_dir=None,
            block_frames=args.block_frames,
        )
    )


def _synth(args: argparse.Namespace) -> int:
    project, _rules = create_demo_workspace(Path(args.workdir))
    print(_json_line({"status": "created", "project": project.name}))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mastering")
    subparsers = parser.add_subparsers(dest="command", required=True)

    run_parser = subparsers.add_parser("run", help="master all language deliverables")
    run_parser.add_argument("--project", required=True, type=Path)
    run_parser.add_argument("--rules", type=Path)
    run_parser.add_argument("--output", type=Path)
    run_parser.add_argument("--state-dir", type=Path)
    run_parser.add_argument("--block-frames", type=int)
    run_parser.set_defaults(func=_run)

    verify_parser = subparsers.add_parser("verify", help="recompute report values from final WAVs")
    verify_parser.add_argument("--report", required=True, type=Path)
    verify_parser.add_argument("--oversample", type=int)
    verify_parser.set_defaults(func=_verify)

    demo_parser = subparsers.add_parser("demo", help="synthesize and master the deterministic demo")
    demo_parser.add_argument("--workdir", default=".demo")
    demo_parser.add_argument("--block-frames", type=int)
    demo_parser.set_defaults(func=_demo)

    synth_parser = subparsers.add_parser("synth", help="synthesize deterministic demo inputs only")
    synth_parser.add_argument("--workdir", default=".demo")
    synth_parser.set_defaults(func=_synth)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())

