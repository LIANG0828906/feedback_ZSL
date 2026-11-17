from __future__ import annotations

import argparse
import json
import signal
import sys
from pathlib import Path
from typing import Any

from .errors import RenderError, ValidationFailure
from .manifest import load_project
from .render import render_project
from .synth import DemoConfig, generate_demo_project


def _parse_events(value: str) -> tuple[float, ...]:
    try:
        events = tuple(float(item.strip()) for item in value.split(",") if item.strip())
    except ValueError as exc:
        raise argparse.ArgumentTypeError("events must be comma-separated seconds") from exc
    if not events:
        raise argparse.ArgumentTypeError("at least one event is required")
    return events


def _write_validation_report(
    output_dir: Path,
    manifest_path: Path,
    failure: ValidationFailure,
) -> Path:
    output_dir.mkdir(parents=True, exist_ok=True)
    report = {
        "status": "aborted_validation",
        "exit_code": 2,
        "manifest": str(manifest_path),
        "warnings": [issue.as_dict() for issue in failure.issues],
    }
    path = output_dir / "validation_report.json"
    path.write_text(
        json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    return path


def _generate(args: argparse.Namespace) -> int:
    events = args.events or DemoConfig().reference_times
    config = DemoConfig(
        seed=args.seed,
        sample_rate=args.sample_rate,
        bit_depth=args.bit_depth,
        duration_seconds=args.duration,
        reference_times=events,
        target_lufs=args.target_lufs,
        true_peak_db=args.true_peak_db,
        block_size=args.block_size,
    )
    manifest = generate_demo_project(args.output, config)
    print(str(manifest))
    return 0


def _summary(result: Any) -> dict[str, Any]:
    output: dict[str, Any] = {
        "status": result.status,
        "bus_wav": str(result.bus_path),
        "snapshot": str(result.snapshot_path),
    }
    if result.recovery_report_path is not None:
        output["recovery_report"] = str(result.recovery_report_path)
        output["completed_blocks"] = result.report["completed_blocks"]
        output["total_blocks"] = result.report["total_blocks"]
    elif result.status == "complete":
        output["measured_lufs"] = result.report["master"]["measured_lufs"]
        output["true_peak_dbtp"] = result.report["master"]["true_peak_dbtp"]
        output["warnings"] = result.report["warnings"]
        output["completed_blocks"] = result.report["block_render"]["completed_blocks"]
        output["total_blocks"] = result.report["block_render"]["total_blocks"]
    return output


def _render(args: argparse.Namespace) -> int:
    manifest_path = Path(args.manifest).resolve()
    output_dir = Path(args.output).resolve()
    interrupted = {"requested": False}

    def handle_interrupt(_signum: int, _frame: Any) -> None:
        interrupted["requested"] = True

    previous_handler = signal.signal(signal.SIGINT, handle_interrupt)
    try:
        project = load_project(manifest_path)
        result = render_project(
            project,
            output_dir,
            block_size=args.block_size,
            block_delay_seconds=max(0.0, args.block_delay_ms) / 1000.0,
            stop_requested=lambda: interrupted["requested"],
        )
    except ValidationFailure as exc:
        report_path = _write_validation_report(output_dir, manifest_path, exc)
        for issue in exc.issues:
            print(
                f"[{issue.code}] channel={issue.channel} {issue.message}",
                file=sys.stderr,
            )
        print(f"validation_report={report_path}", file=sys.stderr)
        return 2
    except (RenderError, OSError, ValueError) as exc:
        print(f"[RENDER_FAILED] {exc}", file=sys.stderr)
        return 3
    finally:
        signal.signal(signal.SIGINT, previous_handler)

    print(json.dumps(_summary(result), ensure_ascii=False, sort_keys=True))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="drum-bus",
        description="Deterministic offline percussion bus renderer",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic demo WAVs")
    generate.add_argument("--output", required=True, help="output case directory")
    generate.add_argument("--seed", type=int, default=241)
    generate.add_argument("--sample-rate", type=int, default=48000)
    generate.add_argument("--bit-depth", type=int, choices=(16, 24, 32), default=24)
    generate.add_argument("--duration", type=float, default=5.0)
    generate.add_argument("--events", type=_parse_events, default=None)
    generate.add_argument("--target-lufs", type=float, default=-16.0)
    generate.add_argument("--true-peak-db", type=float, default=-1.0)
    generate.add_argument("--block-size", type=int, default=4096)
    generate.set_defaults(handler=_generate)

    render = subparsers.add_parser("render", help="render and checkpoint a stereo bus")
    render.add_argument("--manifest", required=True)
    render.add_argument("--output", required=True)
    render.add_argument("--block-size", type=int, default=None)
    render.add_argument(
        "--block-delay-ms",
        type=float,
        default=0.0,
        help="optional delay between committed blocks for interruption tests",
    )
    render.set_defaults(handler=_render)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.handler(args))


if __name__ == "__main__":
    raise SystemExit(main())

