"""Command-line entry points for deterministic verification."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .config import AnalysisConfig
from .errors import WorkbenchError
from .io import SampleRepository
from .io_export import export_json
from .models import RenderConfig
from .samplegen import DEFAULT_SEED, generate_samples
from .verify import (
    run_injection_suite,
    run_scale_report,
    run_three_path_verification,
)


def _common_analysis(args: argparse.Namespace) -> AnalysisConfig:
    return AnalysisConfig(
        frame_length=args.frame_length,
        hop_length=args.hop_length,
        estimation_window=args.estimation_window,
        early_window_ms=args.early_window,
        mode=args.mode,
        max_parallelism=args.parallel,
        thread_buffer_mb=args.buffer_mb,
    )


def _render(args: argparse.Namespace) -> RenderConfig:
    return RenderConfig(
        convolution_window=args.convolution_window,
        render_block_size=args.render_block,
    )


def cmd_generate(args: argparse.Namespace) -> int:
    root = Path(args.samples_dir)
    manifest = generate_samples(root, seed=args.seed, force=args.force)
    print(
        json.dumps(
            {
                "ok": True,
                "root": str(root),
                "samples": len(manifest["samples"]),
                "suites": [item["name"] for item in manifest["suites"]],
                "seed": manifest["seed"],
            },
            indent=2,
        )
    )
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    repository = SampleRepository(args.samples_dir)
    sample_id = args.sample
    if sample_id is None:
        sample_id = repository.sample_ids()[0]
    sample = repository.load(sample_id)
    analysis = _common_analysis(args)
    render = _render(args)
    workspace = Path(args.workspace)
    workspace.mkdir(parents=True, exist_ok=True)
    verification = run_three_path_verification(
        sample,
        analysis,
        render,
        workspace / "three_path",
        interrupt_after=args.interrupt_after,
    )
    injection_report = run_injection_suite(
        repository,
        analysis,
        render,
        workspace / "injections",
    )
    scale_report = run_scale_report(repository, analysis)
    report = {
        "ok": bool(
            verification.report["ok"]
            and injection_report["ok"]
            and scale_report["ok"]
        ),
        "sample_id": sample.id,
        "three_path": verification.report,
        "injections": injection_report,
        "scale": scale_report,
        "analysis_config": analysis.to_dict(),
        "render_config": render.to_dict(),
    }
    output = Path(args.output)
    export_json(output, report)
    print(
        json.dumps(
            {
                "ok": report["ok"],
                "sample_id": sample.id,
                "three_path": {
                    "partial_committed_frames": verification.report[
                        "partial_committed_frames"
                    ],
                    "output_sha256": verification.report["realtime_vs_resume"][
                        "output_sha256"
                    ],
                },
                "injections": {
                    item["id"]: item["observed_code"]
                    for item in injection_report["cases"]
                },
                "scale_suites": [
                    {
                        "suite": item["suite"],
                        "samples": item["sample_count"],
                        "mode": item["decision"]["effective_mode"],
                        "window": item["decision"]["estimation_window"],
                        "overlap": item["decision"]["overlap_ratio"],
                        "delay_error": item[
                            "truth_direct_delay_max_abs_error_samples"
                        ],
                    }
                    for item in scale_report["suites"]
                ],
                "report": str(output),
            },
            indent=2,
            ensure_ascii=False,
        )
    )
    return 0 if report["ok"] else 1


def cmd_inject(args: argparse.Namespace) -> int:
    repository = SampleRepository(args.samples_dir)
    report = run_injection_suite(
        repository,
        _common_analysis(args),
        _render(args),
        Path(args.workspace),
    )
    print(json.dumps(report, indent=2, ensure_ascii=False))
    return 0 if report["ok"] else 1


def cmd_scale(args: argparse.Namespace) -> int:
    repository = SampleRepository(args.samples_dir)
    report = run_scale_report(repository, _common_analysis(args))
    if args.output:
        export_json(args.output, report)
    print(json.dumps(report, indent=2, ensure_ascii=False))
    return 0


def _add_analysis_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--mode", choices=["auto", "calibration", "preview"], default="auto")
    parser.add_argument("--frame-length", type=int, default=4096)
    parser.add_argument("--hop-length", type=int, default=1024)
    parser.add_argument("--estimation-window", type=int, default=2048)
    parser.add_argument("--early-window", type=float, default=20.0)
    parser.add_argument("--parallel", type=int, default=2)
    parser.add_argument("--buffer-mb", type=int, default=16)


def _add_render_options(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--convolution-window", type=int, default=2048)
    parser.add_argument("--render-block", type=int, default=512)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="acoustic-workbench")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate", help="generate deterministic samples")
    generate_parser.add_argument("--samples-dir", default="samples")
    generate_parser.add_argument("--seed", type=int, default=DEFAULT_SEED)
    generate_parser.add_argument("--force", action="store_true")
    generate_parser.set_defaults(func=cmd_generate)

    verify_parser = subparsers.add_parser("verify", help="run all acceptance checks")
    verify_parser.add_argument("--samples-dir", default="samples")
    verify_parser.add_argument("--sample")
    verify_parser.add_argument("--workspace", default="artifacts/verification_work")
    verify_parser.add_argument(
        "--output", default="artifacts/verification_report.json"
    )
    verify_parser.add_argument("--interrupt-after", type=int, default=None)
    _add_analysis_options(verify_parser)
    _add_render_options(verify_parser)
    verify_parser.set_defaults(func=cmd_verify)

    inject_parser = subparsers.add_parser("inject", help="run four injection drills")
    inject_parser.add_argument("--samples-dir", default="samples")
    inject_parser.add_argument("--workspace", default="artifacts/injection_work")
    _add_analysis_options(inject_parser)
    _add_render_options(inject_parser)
    inject_parser.set_defaults(func=cmd_inject)

    scale_parser = subparsers.add_parser("scale", help="report scale trade-off policy")
    scale_parser.add_argument("--samples-dir", default="samples")
    scale_parser.add_argument("--output")
    _add_analysis_options(scale_parser)
    scale_parser.set_defaults(func=cmd_scale)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        print(json.dumps(exc.to_dict(), indent=2, ensure_ascii=False), file=sys.stderr)
        return 1
    except Exception as exc:  # pragma: no cover - CLI safety net
        print(
            json.dumps(
                {
                    "ok": False,
                    "code": "E_INTERNAL",
                    "message": str(exc),
                },
                indent=2,
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
