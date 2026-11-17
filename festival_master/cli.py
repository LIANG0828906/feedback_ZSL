from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any, Sequence

from .demo import write_demo_manifest
from .errors import MasterError
from .manifest import load_manifest
from .render import InterruptFlag, run_render, write_report
from .synth import generate_material


def _json_line(payload: dict[str, Any]) -> str:
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":"), allow_nan=False)


def _event(payload: dict[str, Any]) -> None:
    print(_json_line(payload), file=sys.stderr, flush=True)


def _install_sigint(flag: InterruptFlag) -> Any:
    def handler(signum: int, frame: Any) -> None:
        del signum, frame
        flag.requested = True

    return signal.signal(signal.SIGINT, handler)


def _generate(manifest_path: Path, report_path: Path) -> dict[str, Any]:
    manifest = load_manifest(manifest_path, check_audio=False)
    report = generate_material(manifest)
    write_report(report_path, report)
    return report


def _render(
    manifest_path: Path,
    output_path: Path,
    report_path: Path,
    work_dir: Path,
    block_samples: int | None,
    fresh: bool,
) -> tuple[str, dict[str, Any]]:
    manifest = load_manifest(manifest_path, check_audio=True)
    flag = InterruptFlag()
    previous = _install_sigint(flag)
    try:
        status = run_render(
            manifest,
            output_path,
            report_path,
            work_dir,
            block_samples=block_samples,
            fresh=fresh,
            interrupt=flag,
            event=_event,
        )
    finally:
        signal.signal(signal.SIGINT, previous)
    report = json.loads(report_path.read_text(encoding="utf-8"))
    return status, report


def _summary(
    event: str,
    status: str,
    report: dict[str, Any],
    report_path: Path,
    *,
    output_path: Path | None = None,
) -> dict[str, Any]:
    value: dict[str, Any] = {
        "ok": True,
        "event": event,
        "status": status,
        "exit_code": 0,
        "report_file": report_path.name,
        "business_codes": report.get("business_codes", []),
        "progress": report.get("progress"),
    }
    if output_path is not None and report.get("master"):
        value["master_file"] = output_path.name
        value["master_sha256"] = report["master"]["sha256"]
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="festival-master",
        description="Deterministic offline stereo mastering for six festival sources.",
    )
    commands = parser.add_subparsers(dest="command", required=True)

    generate = commands.add_parser("generate", help="synthesize the six deterministic WAV sources")
    generate.add_argument("--manifest", required=True, type=Path)
    generate.add_argument("--report", type=Path)

    render = commands.add_parser("render", help="validate, align, spatialize, and render the master")
    render.add_argument("--manifest", required=True, type=Path)
    render.add_argument("--output", required=True, type=Path)
    render.add_argument("--report", required=True, type=Path)
    render.add_argument("--work-dir", type=Path)
    render.add_argument("--block-samples", type=int)
    render.add_argument("--fresh", action="store_true", help="discard committed resume progress")

    demo = commands.add_parser("demo", help="create and render a complete local acceptance fixture")
    demo.add_argument("--workspace", type=Path, default=Path("demo_run"))
    demo.add_argument("--duration", type=float, default=7.0)
    demo.add_argument("--block-samples", type=int, default=4096)
    demo.add_argument("--target-loudness", type=float, default=-16.0)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            report_path = args.report or (args.manifest.resolve().parent / "generation_report.json")
            report = _generate(args.manifest, report_path)
            print(
                _json_line(
                    {
                        "ok": True,
                        "event": "generation_complete",
                        "status": "generated",
                        "exit_code": 0,
                        "report_file": report_path.name,
                        "manifest_sha256": report["manifest_sha256"],
                        "channel_count": len(report["channels"]),
                    }
                ),
                flush=True,
            )
            return 0

        if args.command == "render":
            work_dir = args.work_dir or args.output.with_name(f".{args.output.stem}.work")
            status, report = _render(
                args.manifest,
                args.output,
                args.report,
                work_dir,
                args.block_samples,
                args.fresh,
            )
            event = "render_interrupted" if status == "interrupted" else "render_complete"
            print(
                _json_line(
                    _summary(
                        event,
                        status,
                        report,
                        args.report,
                        output_path=args.output,
                    )
                ),
                flush=True,
            )
            return 0

        workspace = args.workspace
        workspace.mkdir(parents=True, exist_ok=True)
        manifest_path = workspace / "manifest.json"
        write_demo_manifest(
            manifest_path,
            duration_seconds=args.duration,
            block_samples=args.block_samples,
            target_loudness_lufs=args.target_loudness,
        )
        generation_report = workspace / "generation_report.json"
        _generate(manifest_path, generation_report)
        output_path = workspace / "master.wav"
        report_path = workspace / "master_report.json"
        work_dir = workspace / "work"
        status, report = _render(
            manifest_path,
            output_path,
            report_path,
            work_dir,
            args.block_samples,
            False,
        )
        event = "demo_interrupted" if status == "interrupted" else "demo_complete"
        print(
            _json_line(
                _summary(event, status, report, report_path, output_path=output_path)
            ),
            flush=True,
        )
        return 0
    except MasterError as exc:
        print(_json_line(exc.as_dict()), flush=True)
        return exc.exit_code
    except KeyboardInterrupt:
        print(
            _json_line(
                {
                    "ok": False,
                    "event": "error",
                    "code": "E_INTERRUPTED_OUTSIDE_BLOCK",
                    "channel": "MASTER",
                    "exit_code": 130,
                    "errors": [
                        {
                            "code": "E_INTERRUPTED_OUTSIDE_BLOCK",
                            "channel": "MASTER",
                            "message": "interrupted before a recoverable render block committed",
                        }
                    ],
                }
            ),
            flush=True,
        )
        return 130
    except Exception as exc:
        print(
            _json_line(
                {
                    "ok": False,
                    "event": "error",
                    "code": "E_INTERNAL",
                    "channel": "MASTER",
                    "exit_code": 1,
                    "message": str(exc),
                    "errors": [
                        {
                            "code": "E_INTERNAL",
                            "channel": "MASTER",
                            "message": str(exc),
                        }
                    ],
                }
            ),
            flush=True,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
