"""Command-line interface for generation, validation, runs, and verification."""

from __future__ import annotations

import argparse
import dataclasses
import json
import platform
import shutil
import sys
import threading
import time
import traceback
from pathlib import Path
from typing import Any, Sequence

import cv2
import numpy as np

from . import __version__
from .config import (
    ALGORITHM_VERSION,
    BlendConfig,
    ExposureConfig,
    FeatureConfig,
    HomographyConfig,
    PipelineConfig,
    RuntimeConfig,
    StrategyConfig,
)
from .dataset import load_dataset
from .errors import WorkbenchError
from .geometry import apply_homography
from .pipeline import RunResult, StitchPipeline
from .report import canonical_report, compare_panorama_outputs
from .sample_generator import DEFAULT_SPECS, generate_all


def _add_config_arguments(parser: argparse.ArgumentParser) -> None:
    defaults = PipelineConfig()
    parser.add_argument("--mode", choices=["auto", "full", "restricted"], default="auto")
    parser.add_argument("--max-features", type=int, default=defaults.features.max_features)
    parser.add_argument("--octave-layers", type=int, default=defaults.features.octave_layers)
    parser.add_argument(
        "--contrast-threshold",
        type=float,
        default=defaults.features.contrast_threshold,
    )
    parser.add_argument(
        "--edge-threshold", type=float, default=defaults.features.edge_threshold
    )
    parser.add_argument("--ratio-test", type=float, default=defaults.features.ratio_test)
    parser.add_argument("--min-matches", type=int, default=defaults.features.min_matches)
    parser.add_argument(
        "--reproj-threshold",
        type=float,
        default=defaults.homography.reproj_threshold,
    )
    parser.add_argument(
        "--consensus-threshold",
        type=float,
        default=defaults.homography.consensus_threshold,
    )
    parser.add_argument(
        "--ransac-iterations",
        type=int,
        default=defaults.homography.max_iterations,
    )
    parser.add_argument(
        "--min-inliers", type=int, default=defaults.homography.min_inliers
    )
    parser.add_argument(
        "--truth-tolerance",
        type=float,
        default=defaults.homography.truth_corner_tolerance,
    )
    parser.add_argument(
        "--high-budget", type=int, default=defaults.strategy.high_feature_budget
    )
    parser.add_argument(
        "--low-budget", type=int, default=defaults.strategy.low_feature_budget
    )
    parser.add_argument(
        "--work-threshold-mp",
        type=float,
        default=defaults.strategy.work_unit_threshold_mp,
    )
    parser.add_argument("--workers", type=int, default=defaults.runtime.max_workers)
    parser.add_argument(
        "--cache-images",
        type=int,
        default=defaults.runtime.max_cached_images,
    )
    parser.add_argument(
        "--buffer-pairs",
        type=int,
        default=defaults.runtime.max_buffered_pairs,
    )
    parser.add_argument(
        "--pyramid-levels", type=int, default=defaults.blend.pyramid_levels
    )
    parser.add_argument(
        "--seam-gradient-limit",
        type=float,
        default=defaults.blend.seam_gradient_limit,
    )
    parser.add_argument(
        "--canvas-reconcile-px",
        type=int,
        default=defaults.blend.canvas_reconcile_px,
    )


def _config_from_args(args: argparse.Namespace) -> PipelineConfig:
    return PipelineConfig(
        features=FeatureConfig(
            max_features=args.max_features,
            octave_layers=args.octave_layers,
            contrast_threshold=args.contrast_threshold,
            edge_threshold=args.edge_threshold,
            ratio_test=args.ratio_test,
            min_matches=args.min_matches,
        ),
        homography=HomographyConfig(
            reproj_threshold=args.reproj_threshold,
            consensus_threshold=args.consensus_threshold,
            max_iterations=args.ransac_iterations,
            min_inliers=args.min_inliers,
            truth_corner_tolerance=args.truth_tolerance,
        ),
        strategy=StrategyConfig(
            mode=args.mode,
            work_unit_threshold_mp=args.work_threshold_mp,
            high_feature_budget=args.high_budget,
            low_feature_budget=args.low_budget,
        ),
        exposure=ExposureConfig(),
        blend=BlendConfig(
            pyramid_levels=args.pyramid_levels,
            seam_gradient_limit=args.seam_gradient_limit,
            canvas_reconcile_px=args.canvas_reconcile_px,
        ),
        runtime=RuntimeConfig(
            max_workers=args.workers,
            max_cached_images=args.cache_images,
            max_buffered_pairs=args.buffer_pairs,
        ),
    ).validate()


def _run_summary(result: RunResult) -> dict[str, Any]:
    panorama = result.report.get("panorama") or {}
    registration = result.report.get("registration") or {}
    strategy = result.report.get("strategy") or {}
    return {
        "status": result.status,
        "complete": result.complete,
        "output_dir": str(result.output_dir),
        "checkpoint": str(result.checkpoint_path),
        "resumed_pairs": result.resumed_pairs,
        "checkpoint_invalid_reason": result.checkpoint_invalid_reason,
        "duration_seconds": round(result.duration_seconds, 6),
        "mode": strategy.get("mode"),
        "feature_budget": strategy.get("actual_feature_budget"),
        "candidate_pairs": strategy.get("candidate_pair_count"),
        "progress": result.report.get("progress"),
        "registration": registration,
        "panorama": {
            key: panorama.get(key)
            for key in (
                "state",
                "width",
                "height",
                "theoretical_width",
                "theoretical_height",
                "matches_theoretical",
                "canvas_reconciled",
                "seam_gradient_p95",
                "seam_gradient_limit",
                "seam_gradient_ok",
                "output_path",
                "error",
            )
            if key in panorama
        },
    }


def _result_exit_code(result: RunResult) -> int:
    return 0 if result.status in {"completed", "paused", "cancelled"} else 1


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )


def _first_report_difference(
    left: dict[str, Any], right: dict[str, Any], path: str = ""
) -> str | None:
    if type(left) is not type(right):
        return path or "/"
    if isinstance(left, dict):
        for key in sorted(set(left) | set(right)):
            if key not in left or key not in right:
                return f"{path}/{key}"
            difference = _first_report_difference(
                left[key], right[key], f"{path}/{key}"
            )
            if difference is not None:
                return difference
        return None
    if isinstance(left, list):
        if len(left) != len(right):
            return f"{path}/length"
        for index, (left_item, right_item) in enumerate(zip(left, right, strict=True)):
            difference = _first_report_difference(
                left_item, right_item, f"{path}/{index}"
            )
            if difference is not None:
                return difference
        return None
    if left != right:
        return path or "/"
    return None


def _command_generate(args: argparse.Namespace) -> int:
    requested = [item.strip() for item in args.sets.split(",") if item.strip()]
    by_name = {spec.name: spec for spec in DEFAULT_SPECS}
    if "all" in requested:
        specs = DEFAULT_SPECS
    else:
        unknown = [name for name in requested if name not in by_name]
        if unknown:
            raise WorkbenchError(
                "E_PARA",
                f"unknown sample set(s): {', '.join(unknown)}",
                field="sets",
            )
        specs = tuple(by_name[name] for name in requested)
    manifests = generate_all(
        args.output,
        seed=args.seed,
        specs=specs,
        overwrite=args.overwrite,
    )
    summary = {
        "status": "ok",
        "seed": args.seed,
        "output": str(Path(args.output).resolve()),
        "datasets": {
            name: {
                "images": manifest["image_count"],
                "canvas": manifest["canvas"],
                "spec": manifest["spec"],
            }
            for name, manifest in sorted(manifests.items())
        },
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0


def _command_validate(args: argparse.Namespace) -> int:
    dataset = load_dataset(args.dataset, strict=False)
    summary = {
        "status": "ok" if not dataset.errors else "invalid_images",
        "dataset": dataset.to_dict(),
        "summary": {
            "valid_images": dataset.image_count,
            "invalid_images": len(dataset.errors),
            "content_hash": dataset.content_hash,
        },
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    return 0 if not dataset.errors else 1


def _command_run(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    dataset = load_dataset(args.dataset)
    pipeline = StitchPipeline(
        dataset,
        config,
        args.output,
        checkpoint_path=args.checkpoint,
        resume=args.resume,
    )
    cancel_event = threading.Event()
    timer: threading.Timer | None = None
    if args.cancel_after is not None:
        timer = threading.Timer(args.cancel_after, cancel_event.set)
        timer.daemon = True
        timer.start()
    try:
        result = pipeline.run(
            pause_after_pairs=args.pause_after,
            cancel_event=cancel_event,
            progress=None,
        )
    finally:
        if timer is not None:
            timer.cancel()
    summary = _run_summary(result)
    summary["dataset"] = {
        "name": dataset.name,
        "content_hash": dataset.content_hash,
        "validation_errors": [error.to_dict() for error in dataset.errors],
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    return _result_exit_code(result)


def _create_pipeline(
    dataset: Any, config: PipelineConfig, output_dir: Path, *, resume: bool
) -> StitchPipeline:
    return StitchPipeline(dataset, config, output_dir, resume=resume)


def _command_verify(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    dataset = load_dataset(args.dataset)
    root = Path(args.output_root)
    paths = {
        "realtime": root / "realtime",
        "resume": root / "resume",
        "full_recompute": root / "full_recompute",
    }
    for path in paths.values():
        shutil.rmtree(path, ignore_errors=True)

    started = time.perf_counter()
    timings: dict[str, float] = {}
    realtime_started = time.perf_counter()
    realtime = _create_pipeline(dataset, config, paths["realtime"], resume=False).run()
    timings["realtime"] = time.perf_counter() - realtime_started

    pause_after = max(1, min(args.interrupt_after, realtime.report["progress"]["total_pairs"]))
    partial_started = time.perf_counter()
    partial = _create_pipeline(dataset, config, paths["resume"], resume=True).run(
        pause_after_pairs=pause_after
    )
    checkpoint_exists_at_pause = (paths["resume"] / "checkpoint.json").is_file()
    no_panorama_at_pause = not (paths["resume"] / "panorama.png").exists()
    timings["partial"] = time.perf_counter() - partial_started
    resume_started = time.perf_counter()
    resumed = _create_pipeline(dataset, config, paths["resume"], resume=True).run()
    timings["resume"] = time.perf_counter() - resume_started

    full_started = time.perf_counter()
    full = _create_pipeline(dataset, config, paths["full_recompute"], resume=False).run()
    timings["full_recompute"] = time.perf_counter() - full_started

    pixel_checks: dict[str, Any] = {}
    report_checks: dict[str, Any] = {}
    for name, result in (
        ("resume", resumed),
        ("full_recompute", full),
    ):
        comparison = compare_panorama_outputs(paths["realtime"], paths[name])
        pixel_checks[f"realtime_vs_{name}"] = comparison.to_dict()
        realtime_report = json.loads(
            (paths["realtime"] / "report.json").read_text(encoding="utf-8")
        )
        other_report = json.loads(
            (paths[name] / "report.json").read_text(encoding="utf-8")
        )
        canonical_realtime = canonical_report(realtime_report)
        canonical_other = canonical_report(other_report)
        report_checks[f"realtime_vs_{name}"] = {
            "identical": canonical_realtime == canonical_other,
            "first_difference": _first_report_difference(
                canonical_realtime, canonical_other
            ),
        }

    all_statuses = {
        "realtime": realtime.status,
        "partial": partial.status,
        "resume": resumed.status,
        "full_recompute": full.status,
    }
    passed = (
        realtime.status == "completed"
        and resumed.status == "completed"
        and full.status == "completed"
        and partial.status == "paused"
        and checkpoint_exists_at_pause
        and no_panorama_at_pause
        and all(item["identical"] for item in pixel_checks.values())
        and all(item["identical"] for item in report_checks.values())
    )
    panorama = realtime.report.get("panorama") or {}
    report = {
        "status": "passed" if passed else "failed",
        "algorithm_version": ALGORITHM_VERSION,
        "workbench_version": __version__,
        "dataset": {
            "name": dataset.name,
            "path": str(dataset.root),
            "content_hash": dataset.content_hash,
            "images": dataset.image_count,
            "theoretical_canvas": {
                "width": dataset.theoretical_width,
                "height": dataset.theoretical_height,
            },
        },
        "mode": realtime.report["strategy"]["mode"],
        "parameter_version": realtime.report["parameter_version"],
        "statuses": all_statuses,
        "partial_checkpoint": {
            "pause_after_pairs": pause_after,
            "committed_pairs": partial.report["progress"]["committed_pairs"],
            "checkpoint_exists": checkpoint_exists_at_pause,
            "panorama_absent": no_panorama_at_pause,
        },
        "resume": {
            "restored_pairs": resumed.resumed_pairs,
            "checkpoint_invalid_reason": resumed.checkpoint_invalid_reason,
        },
        "pixel_consistency": pixel_checks,
        "report_consistency": report_checks,
        "panorama": {
            "width": panorama.get("width"),
            "height": panorama.get("height"),
            "theoretical_width": panorama.get("theoretical_width"),
            "theoretical_height": panorama.get("theoretical_height"),
            "matches_theoretical": panorama.get("matches_theoretical"),
            "seam_gradient_p95": panorama.get("seam_gradient_p95"),
            "seam_gradient_limit": panorama.get("seam_gradient_limit"),
            "seam_gradient_ok": panorama.get("seam_gradient_ok"),
            "output_hash": panorama.get("output_hash"),
        },
        "registration": realtime.report.get("registration"),
        "timings_seconds": {
            key: round(value, 6) for key, value in timings.items()
        },
        "total_seconds": round(time.perf_counter() - started, 6),
        "environment": {
            "python": platform.python_version(),
            "platform": platform.platform(),
            "numpy": np.__version__,
            "opencv": cv2.__version__,
        },
    }
    _write_json(root / "verification.json", report)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if passed else 1


def _command_compare_modes(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    dataset = load_dataset(args.dataset)
    root = Path(args.output_root)
    reports: dict[str, dict[str, Any]] = {}
    durations: dict[str, float] = {}
    results: dict[str, RunResult] = {}
    for mode in ("full", "restricted"):
        mode_config = dataclasses.replace(
            config,
            strategy=dataclasses.replace(config.strategy, mode=mode),
        )
        mode_config.validate()
        output_dir = root / mode
        shutil.rmtree(output_dir, ignore_errors=True)
        started = time.perf_counter()
        result = _create_pipeline(dataset, mode_config, output_dir, resume=False).run()
        durations[mode] = time.perf_counter() - started
        results[mode] = result
        reports[mode] = result.report
    max_difference = 0.0
    per_image: list[dict[str, Any]] = []
    full_images = {entry["id"]: entry for entry in reports["full"]["images"]}
    restricted_images = {entry["id"]: entry for entry in reports["restricted"]["images"]}
    for index in sorted(full_images):
        full_entry = full_images[index]
        restricted_entry = restricted_images[index]
        if (
            full_entry["homography_to_base"] is None
            or restricted_entry["homography_to_base"] is None
        ):
            difference = float("inf")
        else:
            width, height = full_entry["width"], full_entry["height"]
            corners = np.array(
                [[0.0, 0.0], [width, 0.0], [width, height], [0.0, height]],
                dtype=np.float64,
            )
            full_points = apply_homography(
                np.asarray(full_entry["homography_to_base"], dtype=np.float64).reshape(3, 3),
                corners,
            )
            restricted_points = apply_homography(
                np.asarray(
                    restricted_entry["homography_to_base"], dtype=np.float64
                ).reshape(3, 3),
                corners,
            )
            difference = float(
                np.max(np.linalg.norm(full_points - restricted_points, axis=1))
            )
        max_difference = max(max_difference, difference)
        per_image.append(
            {
                "image": index,
                "corner_difference_px": difference,
                "within_tolerance": difference <= args.mode_tolerance,
            }
        )
    full_panorama = reports["full"].get("panorama") or {}
    restricted_panorama = reports["restricted"].get("panorama") or {}
    canvas_equal = (
        full_panorama.get("width") == restricted_panorama.get("width")
        and full_panorama.get("height") == restricted_panorama.get("height")
        and full_panorama.get("width") == dataset.theoretical_width
        and full_panorama.get("height") == dataset.theoretical_height
    )
    parameter_versions_differ = (
        reports["full"]["parameter_version"]
        != reports["restricted"]["parameter_version"]
    )
    passed = (
        results["full"].status == "completed"
        and results["restricted"].status == "completed"
        and canvas_equal
        and parameter_versions_differ
        and max_difference <= args.mode_tolerance
    )
    report = {
        "status": "passed" if passed else "failed",
        "dataset": {
            "name": dataset.name,
            "content_hash": dataset.content_hash,
            "images": dataset.image_count,
            "theoretical_canvas": {
                "width": dataset.theoretical_width,
                "height": dataset.theoretical_height,
            },
        },
        "modes": {
            mode: {
                "status": results[mode].status,
                "feature_budget": reports[mode]["strategy"]["actual_feature_budget"],
                "candidate_pairs": reports[mode]["strategy"]["candidate_pair_count"],
                "parameter_version": reports[mode]["parameter_version"],
                "panorama_width": (reports[mode].get("panorama") or {}).get("width"),
                "panorama_height": (reports[mode].get("panorama") or {}).get("height"),
                "max_pair_truth_corner_error_px": (
                    reports[mode].get("registration") or {}
                ).get("max_pair_truth_corner_error_px"),
                "max_image_truth_corner_error_px": (
                    reports[mode].get("registration") or {}
                ).get("max_image_truth_corner_error_px"),
                "duration_seconds": round(durations[mode], 6),
            }
            for mode in ("full", "restricted")
        },
        "canvas_equal": canvas_equal,
        "parameter_versions_differ": parameter_versions_differ,
        "mode_tolerance_px": args.mode_tolerance,
        "max_corner_difference_px": max_difference,
        "per_image": per_image,
    }
    _write_json(root / "mode_comparison.json", report)
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if passed else 1


def _command_gui(args: argparse.Namespace) -> int:
    from .gui import launch_gui

    return launch_gui(args.dataset)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="panoworkbench",
        description="Deterministic panorama registration and stitching workbench",
    )
    parser.add_argument("--version", action="version", version=__version__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--output", default="data/samples")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument(
        "--sets", default="all", help="comma-separated tiny,small,medium,large,highres"
    )
    generate.add_argument("--overwrite", action="store_true", default=True)
    generate.set_defaults(func=_command_generate)

    validate = subparsers.add_parser("validate", help="validate a generated dataset")
    validate.add_argument("--dataset", required=True)
    validate.set_defaults(func=_command_validate)

    config_parent = argparse.ArgumentParser(add_help=False)
    _add_config_arguments(config_parent)

    run = subparsers.add_parser(
        "run", parents=[config_parent], help="run registration and stitching"
    )
    run.add_argument("--dataset", required=True)
    run.add_argument("--output", default="output")
    run.add_argument("--checkpoint", default=None)
    run.add_argument("--no-resume", dest="resume", action="store_false", default=True)
    run.add_argument("--pause-after", type=int, default=None)
    run.add_argument("--cancel-after", type=float, default=None)
    run.set_defaults(func=_command_run)

    verify = subparsers.add_parser(
        "verify",
        parents=[config_parent],
        help="verify realtime, resumed, and full-recompute consistency",
    )
    verify.add_argument("--dataset", required=True)
    verify.add_argument("--output-root", default="artifacts/verification")
    verify.add_argument("--interrupt-after", type=int, default=2)
    verify.set_defaults(func=_command_verify)

    compare = subparsers.add_parser(
        "compare-modes",
        parents=[config_parent],
        help="compare full and restricted strategy modes on one dataset",
    )
    compare.add_argument("--dataset", required=True)
    compare.add_argument("--output-root", default="artifacts/mode-comparison")
    compare.add_argument("--mode-tolerance", type=float, default=3.0)
    compare.set_defaults(func=_command_compare_modes)

    gui = subparsers.add_parser("gui", help="launch the PyQt5 desktop workbench")
    gui.add_argument("--dataset", default=None)
    gui.set_defaults(func=_command_gui)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        print(
            json.dumps(
                {"status": "error", "error": exc.to_dict()},
                indent=2,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 1
    except KeyboardInterrupt:
        print(
            json.dumps(
                {
                    "status": "interrupted",
                    "note": "restart with --resume to use only persisted pairs",
                },
                indent=2,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 130
    except Exception as exc:  # pragma: no cover - CLI safety net
        print(
            json.dumps(
                {
                    "status": "error",
                    "error": {"code": "E_UNEXPECTED", "message": str(exc)},
                    "traceback": traceback.format_exc(),
                },
                indent=2,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
