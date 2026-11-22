"""Command-line entry point for the workbench."""

from __future__ import annotations

import argparse
import json
import platform
import sys
from pathlib import Path
from typing import Any

from . import ALGORITHM_VERSION, __version__
from .comparison import compare_outputs
from .dataset import DatasetIndex
from .errors import WorkbenchError
from .injections import ANOMALY_KINDS, create_injected_dataset
from .io_utils import read_json
from .params import WorkbenchParams
from .paths import run_three_paths
from .pipeline import WorkbenchEngine
from .samplegen import generate_dataset, verify_generated_dataset


def _json_print(value: Any, *, stream: Any = sys.stdout) -> None:
    print(json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False, allow_nan=False), file=stream)


def _params_from_args(args: argparse.Namespace) -> WorkbenchParams:
    values: dict[str, Any] = {}
    if getattr(args, "params_json", None):
        values.update(read_json(Path(args.params_json), field="params_json"))
    mapping = {
        "interpolation_order": getattr(args, "interpolation_order", None),
        "guide_radius": getattr(args, "guide_radius", None),
        "guide_eps": getattr(args, "guide_eps", None),
        "contrast_gain": getattr(args, "contrast_gain", None),
        "contrast_threshold": getattr(args, "contrast_threshold", None),
        "gamut_intent": getattr(args, "gamut_intent", None),
        "max_workers": getattr(args, "workers", None),
        "max_pixels_per_thread": getattr(args, "max_pixels_per_thread", None),
    }
    values.update({key: value for key, value in mapping.items() if value is not None})
    if getattr(args, "segment_size", None):
        values["segment_size"] = tuple(int(value) for value in args.segment_size)
    if getattr(args, "exposure_weights", None):
        values["exposure_weights"] = tuple(float(value) for value in args.exposure_weights.split(","))
    return WorkbenchParams.from_mapping(values)


def _add_processing_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--params-json", type=Path, help="JSON mapping of WorkbenchParams fields")
    parser.add_argument("--workers", type=int, help="Maximum worker threads")
    parser.add_argument("--segment-size", nargs=2, type=int, metavar=("H", "W"))
    parser.add_argument("--interpolation-order", type=int)
    parser.add_argument("--exposure-weights", help="Comma-separated exposure weights")
    parser.add_argument("--guide-radius", type=int)
    parser.add_argument("--guide-eps", type=float)
    parser.add_argument("--contrast-gain", type=float)
    parser.add_argument("--contrast-threshold", type=float)
    parser.add_argument("--gamut-intent", choices=("perceptual", "relative_colorimetric"))
    parser.add_argument("--max-pixels-per-thread", type=int)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mshdr",
        description="Deterministic multispectral HDR synthesis and enhancement workbench",
    )
    parser.add_argument("--debug", action="store_true", help="Show unexpected tracebacks")
    subparsers = parser.add_subparsers(dest="command", required=True)

    subparsers.add_parser("doctor", help="Check local runtime dependencies")

    generate = subparsers.add_parser("generate", help="Generate deterministic local samples")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument("--out", type=Path, default=Path("samples"))
    generate.add_argument("--scenes", type=int, default=3)

    inspect = subparsers.add_parser("inspect", help="Validate and summarize a generated dataset")
    inspect.add_argument("--dataset", type=Path, default=Path("samples/dataset.json"))

    run = subparsers.add_parser("run", help="Run one scene")
    run.add_argument("--dataset", type=Path, default=Path("samples/dataset.json"))
    run.add_argument("--scene")
    run.add_argument("--output", type=Path)
    run.add_argument("--checkpoint-root", type=Path, default=Path("checkpoints/run"))
    run.add_argument("--start-mode", choices=("none", "fresh", "resume", "reuse"), default="fresh")
    run.add_argument("--bad-segment", type=int, help="Inject a bad segment index (E_SEG)")
    run.add_argument("--crash-after-commits", type=int, help="Hard-exit after N committed segments")
    run.add_argument("--commit-limit", type=int, help="Process at most N pending segments")
    run.add_argument("--verbose", action="store_true")
    _add_processing_arguments(run)

    paths = subparsers.add_parser("run-paths", help="Run and compare all three processing paths")
    paths.add_argument("--dataset", type=Path, default=Path("samples/dataset.json"))
    paths.add_argument("--out", type=Path, default=Path("outputs/paths"))
    paths.add_argument("--checkpoint-root", type=Path, default=Path("checkpoints/paths"))
    paths.add_argument("--scene", action="append", dest="scenes")
    paths.add_argument("--resume-fraction", type=float, default=0.34)
    _add_processing_arguments(paths)

    compare = subparsers.add_parser("compare", help="Compare two output directories exactly")
    compare.add_argument("--left", type=Path, required=True)
    compare.add_argument("--right", type=Path, required=True)
    compare.add_argument("--report", type=Path)
    compare.add_argument("--allow-diverge", action="store_true")

    inject = subparsers.add_parser("inject", help="Create a dataset with a real input anomaly")
    inject.add_argument("--source", type=Path, default=Path("samples/dataset.json"))
    inject.add_argument("--destination", type=Path, required=True)
    inject.add_argument("--kind", choices=ANOMALY_KINDS, required=True)
    inject.add_argument("--scene")
    inject.add_argument("--band-index", type=int, default=0)

    gui = subparsers.add_parser("gui", help="Open the optional tkinter result viewer")
    gui.add_argument("--dataset", type=Path, default=Path("samples/dataset.json"))
    gui.add_argument("--output-root", type=Path, default=Path("outputs/paths"))
    return parser


def _doctor() -> dict[str, Any]:
    imports: dict[str, Any] = {}
    ok = True
    for name in ("numpy", "scipy", "PIL", "tkinter"):
        try:
            module = __import__(name)
            version = getattr(module, "__version__", getattr(module, "TkVersion", "ok"))
            imports[name] = {"ok": True, "version": str(version)}
        except Exception as exc:
            ok = False
            imports[name] = {"ok": False, "error": f"{type(exc).__name__}: {exc}"}
    for directory in (Path("samples"), Path("checkpoints"), Path("outputs")):
        directory.mkdir(parents=True, exist_ok=True)
    return {
        "ok": ok,
        "python": sys.version.split()[0],
        "python_executable": sys.executable,
        "platform": platform.platform(),
        "workbench_version": __version__,
        "algorithm_version": ALGORITHM_VERSION,
        "dependencies": imports,
        "network_required": False,
        "gpu_required": False,
        "external_service_required": False,
        "directories": {
            "samples": str(Path("samples").resolve()),
            "checkpoints": str(Path("checkpoints").resolve()),
            "outputs": str(Path("outputs").resolve()),
        },
    }


def _run_scene(args: argparse.Namespace) -> dict[str, Any]:
    index = DatasetIndex.load(args.dataset)
    scene_id = args.scene or str(index.scene_records[0]["scene_id"])
    scene = index.load_scene(scene_id)
    params = _params_from_args(args)
    output = args.output or Path("outputs/run") / scene_id
    checkpoint_root = None if args.start_mode == "none" else args.checkpoint_root
    engine = WorkbenchEngine(
        scene,
        params,
        checkpoint_root=checkpoint_root,
        start_mode=args.start_mode,
    )
    result = engine.run(
        output_dir=output,
        commit_limit=args.commit_limit,
        bad_segment=args.bad_segment,
        crash_after_commits=args.crash_after_commits,
        mode=args.start_mode,
        verbose=args.verbose,
    )
    return {
        "ok": True,
        "scene_id": scene_id,
        "mode": args.start_mode,
        "output": str(output) if result.output_dir is not None else None,
        "progress": f"{result.committed_count}/{result.total_count}",
        "params_version": params.version(),
        "filtering_quality": result.stats["filtering_quality"] if result.stats else None,
        "gamut": result.stats["gamut"] if result.stats else None,
        "checkpoint_invalid_reason": result.checkpoint_invalid_reason,
    }


def _dispatch(args: argparse.Namespace) -> Any:
    if args.command == "doctor":
        return _doctor()
    if args.command == "generate":
        dataset_path = generate_dataset(args.out, seed=args.seed, scene_count=args.scenes)
        return {"ok": True, "dataset": str(dataset_path), "verification": verify_generated_dataset(dataset_path)}
    if args.command == "inspect":
        return {"ok": True, "verification": verify_generated_dataset(args.dataset)}
    if args.command == "run":
        return _run_scene(args)
    if args.command == "run-paths":
        params = _params_from_args(args)
        return run_three_paths(
            args.dataset,
            output_root=args.out,
            checkpoint_root=args.checkpoint_root,
            scene_ids=args.scenes,
            params=params,
            resume_fraction=args.resume_fraction,
        )
    if args.command == "compare":
        return compare_outputs(
            args.left,
            args.right,
            report_path=args.report,
            raise_on_diverge=not args.allow_diverge,
        )
    if args.command == "inject":
        return create_injected_dataset(
            args.source,
            args.destination,
            kind=args.kind,
            scene_id=args.scene,
            band_index=args.band_index,
        )
    if args.command == "gui":
        from .gui import launch_gui

        launch_gui(args.dataset, args.output_root)
        return {"ok": True, "gui": "closed"}
    raise WorkbenchError("E_PARA", f"Unknown command: {args.command}", field="command")


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = _dispatch(args)
    except WorkbenchError as exc:
        _json_print(exc.to_dict(), stream=sys.stderr)
        return exc.exit_code
    except Exception as exc:
        if getattr(args, "debug", False):
            raise
        _json_print(
            {
                "ok": False,
                "code": "E_IO",
                "message": f"Unexpected failure: {type(exc).__name__}: {exc}",
            },
            stream=sys.stderr,
        )
        return 1
    _json_print(result)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

