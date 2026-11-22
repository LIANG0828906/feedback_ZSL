"""Command line entry point for generation, rendering, verification, and UI."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .engine import RenderEngine, SimulatedCrash
from .errors import E_STATE, PipelineError
from .opengl_probe import probe_opengl
from .renderer import RenderConfig
from .resources import ResourceLimits
from .sample_generator import DEFAULT_SEED, generate_sample
from .scene import Scene, load_truth
from .ui import export_static_dashboard, serve_dashboard
from .util import write_json_atomic
from .verify import verify_three_paths, verify_truth


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _render_config(args: argparse.Namespace) -> RenderConfig:
    return RenderConfig(
        block_width=args.block_width,
        block_height=args.block_height,
        step_size=args.step_size,
        sampling_strategy=args.sampling_strategy,
        max_segments=args.max_segments,
        preview_step_size=args.preview_step_size,
    )


def _resource_limits(args: argparse.Namespace) -> ResourceLimits:
    return ResourceLimits(
        max_parallelism=args.max_parallelism,
        per_thread_sample_buffer=args.per_thread_sample_buffer,
        intersection_cache_budget=args.intersection_cache_budget,
        max_pending_blocks=args.max_pending_blocks,
    )


def _ensure_sample(args: argparse.Namespace) -> tuple[Scene, dict[str, Any], Path, Path]:
    sample_dir = Path(args.sample)
    scene_path = sample_dir / "scene.json"
    truth_path = sample_dir / "truth.json"
    if args.generate_if_missing and (not scene_path.exists() or not truth_path.exists()):
        generate_sample(sample_dir, args.seed)
    scene = Scene.load(scene_path)
    truth = load_truth(truth_path, scene)
    return scene, truth, scene_path, truth_path


def command_generate(args: argparse.Namespace) -> int:
    manifest = generate_sample(args.out, args.seed)
    _print_json({"ok": True, "command": "generate", "directory": str(Path(args.out).resolve()), "manifest": manifest})
    return 0


def command_doctor(_: argparse.Namespace) -> int:
    report = {
        "ok": True,
        "opengl": probe_opengl(),
        "python": sys.version,
        "executable": sys.executable,
    }
    try:
        import numpy

        report["numpy"] = numpy.__version__
    except Exception as exc:
        report["numpy"] = f"unavailable: {exc}"
    _print_json(report)
    return 0


def command_render(args: argparse.Namespace) -> int:
    scene, _, _, _ = _ensure_sample(args)
    engine = RenderEngine(
        scene,
        _render_config(args),
        checkpoint_directory=args.checkpoint,
        limits=_resource_limits(args),
        sample_index=args.sample_index,
    )
    try:
        outcome = engine.render(mode=args.mode, crash_after_commits=args.simulate_crash_after)
    except SimulatedCrash as exc:
        partial = {
            "ok": False,
            "error": {
                "code": E_STATE,
                "field": "process",
                "message": str(exc),
                "details": {"committed": exc.committed, "complete": False},
            },
            "progress": engine.progress(),
            "commit_order": engine.committed_ids(),
        }
        _print_json(partial)
        return 4
    payload = outcome.to_dict(include_arrays=False, include_heatmap=False)
    if args.out:
        payload["export"] = outcome.export(args.out, include_arrays=args.export_arrays)
    _print_json(payload)
    return 0 if outcome.complete else 3


def command_verify(args: argparse.Namespace) -> int:
    scene, truth, scene_path, truth_path = _ensure_sample(args)
    config = _render_config(args)
    truth_report = verify_truth(scene, truth, config)
    consistency_report = verify_three_paths(
        scene,
        config,
        work_directory=args.work,
        sample_index=args.sample_index,
        crash_after_commits=args.crash_after,
        limits=_resource_limits(args),
    )
    report = {
        "ok": truth_report["ok"] and consistency_report["ok"],
        "scene_file": str(scene_path.resolve()),
        "truth_file": str(truth_path.resolve()),
        "scene_hash": scene.content_hash,
        "truth": truth_report,
        "consistency": consistency_report,
    }
    output = Path(args.report) if args.report else Path(args.work) / "full_verification.json"
    write_json_atomic(output, report)
    _print_json(report)
    return 0 if report["ok"] else 1


def command_serve(args: argparse.Namespace) -> int:
    scene, _, _, _ = _ensure_sample(args)
    serve_dashboard(
        scene,
        _render_config(args),
        checkpoint_directory=args.checkpoint,
        export_directory=args.exports,
        sample_index=args.sample_index,
        host=args.host,
        port=args.port,
        limits=_resource_limits(args),
    )
    return 0


def command_export_ui(args: argparse.Namespace) -> int:
    scene, _, _, _ = _ensure_sample(args)
    engine = RenderEngine(
        scene,
        _render_config(args),
        checkpoint_directory=args.checkpoint,
        limits=_resource_limits(args),
        sample_index=args.sample_index,
    )
    outcome = engine.render(mode=args.mode)
    files = outcome.export(args.out, include_arrays=True)
    files.update(export_static_dashboard(engine, outcome, args.out, sample_index=args.sample_index))
    _print_json({"ok": outcome.complete, "progress": outcome.progress, "files": files})
    return 0 if outcome.complete else 3


def _add_render_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--sample", default="samples", help="directory containing scene.json and truth.json")
    parser.add_argument("--seed", type=int, default=DEFAULT_SEED, help="deterministic sample seed")
    parser.add_argument(
        "--generate-if-missing",
        action=argparse.BooleanOptionalAction,
        default=True,
        help="generate the sample when absent",
    )
    parser.add_argument("--step-size", type=float, default=0.05, help="scattering integration step")
    parser.add_argument("--preview-step-size", type=float, default=None, help="optional reduced preview step")
    parser.add_argument("--sampling-strategy", choices=("midpoint", "left", "right", "trapezoid"), default="midpoint")
    parser.add_argument("--block-width", type=int, default=16)
    parser.add_argument("--block-height", type=int, default=12)
    parser.add_argument("--max-segments", type=int, default=8)
    parser.add_argument("--sample-index", type=int, default=0)
    parser.add_argument("--max-parallelism", type=int, default=2)
    parser.add_argument("--per-thread-sample-buffer", type=int, default=4096)
    parser.add_argument("--intersection-cache-budget", type=int, default=512)
    parser.add_argument("--max-pending-blocks", type=int, default=8)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="tvol", description="Transparent media and volumetric light pipeline")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic scene and analytic truth")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=DEFAULT_SEED)
    generate.set_defaults(handler=command_generate)

    doctor = subparsers.add_parser("doctor", help="report Python, NumPy, PyOpenGL, and display availability")
    doctor.set_defaults(handler=command_doctor)

    render = subparsers.add_parser("render", help="render one path with checkpoint support")
    _add_render_arguments(render)
    render.add_argument("--mode", choices=("full", "preview", "resume", "incremental"), default="full")
    render.add_argument("--checkpoint", default="runtime/checkpoints")
    render.add_argument("--out", default=None)
    render.add_argument("--export-arrays", action="store_true")
    render.add_argument("--simulate-crash-after", type=int, default=None)
    render.set_defaults(handler=command_render)

    verify = subparsers.add_parser("verify", help="verify analytic truth and three rendering paths")
    _add_render_arguments(verify)
    verify.add_argument("--work", default="runtime/verification")
    verify.add_argument("--crash-after", type=int, default=None)
    verify.add_argument("--report", default=None)
    verify.set_defaults(handler=command_verify)

    serve = subparsers.add_parser("serve", help="serve the local diagnostic dashboard")
    _add_render_arguments(serve)
    serve.add_argument("--checkpoint", default="runtime/ui-checkpoints")
    serve.add_argument("--exports", default="runtime/exports")
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8765)
    serve.set_defaults(handler=command_serve)

    export_ui = subparsers.add_parser("export-ui", help="render and export a static dashboard plus buffers")
    _add_render_arguments(export_ui)
    export_ui.add_argument("--mode", choices=("full", "preview"), default="full")
    export_ui.add_argument("--checkpoint", default="runtime/export-checkpoints")
    export_ui.add_argument("--out", default="runtime/exports")
    export_ui.set_defaults(handler=command_export_ui)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except PipelineError as exc:
        _print_json(exc.to_dict())
        return 2
    except KeyboardInterrupt:
        _print_json(
            {
                "ok": False,
                "error": {
                    "code": E_STATE,
                    "field": "process",
                    "message": "interrupted; only committed checkpoints are guaranteed",
                },
            }
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
