"""Command-line interface for generation, batch processing, verification, and UI."""

from __future__ import annotations

import argparse
import copy
import json
from pathlib import Path
from typing import Any

import numpy as np

from .checkpoint import CheckpointManager
from .color import map_gamut
from .compare import compare_outputs
from .errors import WorkbenchError
from .export import export_output
from .metadata import load_scene
from .parameters import validate_parameters
from .pipeline import assemble_output, evaluate_color_truth, prepare_scene
from .sample_generator import generate_all
from .scheduler import ProcessingJob, ResourceConfig
from .segmentation import make_segments, segmentation_spec
from .state import SegmentRecord, SegmentState, SegmentStateMachine
from .util import read_json
from .verification import verify_paths


def _print(value: Any) -> None:
    print(json.dumps(value, indent=2, ensure_ascii=False, default=str))


def _catalog(root: Path) -> dict[str, Any]:
    path = root / "catalog.json"
    if not path.exists():
        raise WorkbenchError("E_META", "Sample catalog is missing", {"path": str(path)})
    return read_json(path)


def _resolve_scene(samples: Path, value: str | None) -> Path:
    if value:
        candidate = Path(value)
        if candidate.is_dir() and (candidate / "scene.json").exists():
            return candidate.resolve()
        catalog = _catalog(samples)
        for entry in catalog.get("scenes", []):
            if entry["scene_id"] == value:
                return (samples / entry["path"]).resolve()
        candidate = samples / value
        if (candidate / "scene.json").exists():
            return candidate.resolve()
        raise WorkbenchError("E_META", "Scene was not found", {"value": value, "samples": str(samples)})
    catalog = _catalog(samples)
    scenes = catalog.get("scenes", [])
    if not scenes:
        raise WorkbenchError("E_META", "No scenes are available", {"samples": str(samples)})
    return (samples / scenes[0]["path"]).resolve()


def _load_params(path: str | None, scene_path: Path) -> dict[str, Any]:
    scene = load_scene(scene_path)
    params = copy.deepcopy(scene.metadata.default_parameters)
    if path:
        params.update(read_json(Path(path), code="E_PARA", label="parameter JSON"))
    return validate_parameters(params, scene.exposure_count)


def _build_manager(
    scene: Any,
    workspace: Path,
    args: argparse.Namespace,
    *,
    interpolation: str | None = None,
) -> CheckpointManager:
    tile_width = int(getattr(args, "tile", 32))
    tile_height = int(getattr(args, "tile_height", tile_width))
    segments = make_segments(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode=str(getattr(args, "split", "tiles")),
        tile_width=tile_width,
        tile_height=tile_height,
    )
    config = segmentation_spec(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode=str(getattr(args, "split", "tiles")),
        tile_width=tile_width,
        tile_height=tile_height,
        interpolation=str(interpolation or scene.metadata.default_parameters["interpolation"]),
    )
    return CheckpointManager(workspace, scene, segments, config)


def command_generate(args: argparse.Namespace) -> int:
    catalog = generate_all(
        Path(args.out),
        seed=args.seed,
        scene_count=args.scenes,
        width=args.width,
        height=args.height,
        band_count=args.bands,
        exposure_count=args.exposures,
        with_faults=not args.no_faults,
    )
    _print(catalog)
    return 0


def command_inspect(args: argparse.Namespace) -> int:
    scene_path = _resolve_scene(Path(args.samples), args.scene)
    scene = load_scene(scene_path)
    result: dict[str, Any] = {
        "ok": True,
        "scene_id": scene.metadata.scene_id,
        "title": scene.metadata.title,
        "path": str(scene_path),
        "seed": scene.metadata.seed,
        "size": [scene.width, scene.height],
        "pixel_pitch_um": scene.metadata.pixel_pitch_um,
        "band_count": scene.band_count,
        "exposure_count": scene.exposure_count,
        "band_centers_nm": [band.center_nm for band in scene.metadata.bands],
        "exposure_times_s": [exposure.exposure_time for exposure in scene.metadata.exposures],
        "metadata_version": scene.metadata.metadata_version,
        "metadata_hash": scene.metadata.metadata_hash,
        "registration_offsets": scene.metadata.registration_offsets.tolist(),
        "default_parameters": scene.metadata.default_parameters,
    }
    if args.with_truth:
        result["truth"] = scene.truth
    if args.with_alignment:
        prepared = prepare_scene(scene, str(scene.metadata.default_parameters["interpolation"]))
        result["alignment"] = prepared.alignment_report
        result["color_truth"] = evaluate_color_truth(scene, prepared)
    _print(result)
    return 0


def command_process(args: argparse.Namespace) -> int:
    scene_path = _resolve_scene(Path(args.samples), args.scene)
    scene = load_scene(scene_path)
    params = _load_params(args.params, scene_path)
    workspace = Path(args.workspace).resolve()
    manager = _build_manager(scene, workspace, args, interpolation=str(params["interpolation"]))
    if args.fresh:
        manager.reset()
    job = ProcessingJob(
        scene,
        manager,
        resource=ResourceConfig(args.workers, args.buffer_mb),
        params=params,
    )
    result = job.run(
        retry_failed=not args.skip_failed,
        pause_after=args.pause_after,
        fail_segment=args.fail_segment,
        crash_after=args.crash_after,
    )
    payload: dict[str, Any] = {
        "ok": result["ok"],
        "scene_id": scene.metadata.scene_id,
        "snapshot": str(manager.snapshot_dir),
        "invalidated_checkpoint": manager.invalidated_reason,
        **result,
    }
    if manager.is_complete():
        output = assemble_output(scene, manager, require_complete=True)
        payload["output"] = output.summary()
        if args.export:
            export_dir = Path(args.export_dir or (workspace / "exports" / scene.metadata.scene_id))
            payload["export"] = export_output(
                output,
                export_dir,
                scene_id=scene.metadata.scene_id,
                title=scene.metadata.title,
                segment_order=[segment.index for segment in manager.segments],
                alignment=prepare_scene(scene, str(params["interpolation"])),
            )
    manager.close()
    _print(payload)
    if result.get("paused"):
        return 0
    if not result["ok"]:
        return 41
    return 0


def command_export(args: argparse.Namespace) -> int:
    scene_path = _resolve_scene(Path(args.samples), args.scene)
    scene = load_scene(scene_path)
    manager = _build_manager(scene, Path(args.workspace).resolve(), args)
    try:
        output = assemble_output(scene, manager, require_complete=True)
        manifest = export_output(
            output,
            Path(args.out),
            scene_id=scene.metadata.scene_id,
            title=scene.metadata.title,
            segment_order=[segment.index for segment in manager.segments],
            alignment=prepare_scene(
                scene,
                str(
                    manager.segmentation_config.get(
                        "interpolation",
                        scene.metadata.default_parameters["interpolation"],
                    )
                ),
            ),
        )
    finally:
        manager.close()
    _print({"ok": True, "export": manifest, "output": output.summary()})
    return 0


def command_verify(args: argparse.Namespace) -> int:
    scene_path = _resolve_scene(Path(args.samples), args.scene)
    params = _load_params(args.params, scene_path)
    report = verify_paths(
        scene_path,
        Path(args.workspace).resolve(),
        params=params,
        split_mode=args.split,
        tile_width=args.tile,
        tile_height=args.tile_height,
        live_workers=args.workers,
        report_path=args.report,
    )
    _print(report)
    return 0


def _expect_error(code: str, callback: Any) -> dict[str, Any]:
    try:
        callback()
    except WorkbenchError as exc:
        return {
            "expected": code,
            "actual": exc.code,
            "ok": exc.code == code,
            "message": exc.message,
            "details": exc.details,
        }
    return {"expected": code, "actual": None, "ok": False, "message": "No error was raised"}


def command_check_errors(args: argparse.Namespace) -> int:
    samples = Path(args.samples).resolve()
    catalog = _catalog(samples)
    faults = catalog.get("faults", {})
    results: dict[str, Any] = {}
    for name in ("missing_meta", "dim_mismatch", "corrupt_image"):
        entry = faults.get(name)
        if not entry:
            results[name] = {"ok": False, "message": "Fault sample missing"}
            continue
        results[name] = _expect_error(entry["expected_code"], lambda entry=entry: load_scene(samples / entry["path"]))
    scene_path = _resolve_scene(samples, args.scene)
    scene = load_scene(scene_path)
    results["E_PARA"] = _expect_error(
        "E_PARA",
        lambda: validate_parameters({**scene.metadata.default_parameters, "weight_power": 99}, scene.exposure_count),
    )
    records = {0: SegmentRecord(0), 1: SegmentRecord(1)}
    state = SegmentStateMachine(records)
    results["E_STATE"] = _expect_error("E_STATE", lambda: state.transition(0, SegmentState.COMMITTED))
    results["E_STATE_preserved"] = {
        "ok": records[0].state == SegmentState.PENDING,
        "state": records[0].state.value,
    }
    results["E_GAMUT"] = _expect_error(
        "E_GAMUT",
        lambda: map_gamut(np.array([[[-0.1, 0.5, 1.1]]]), "strict"),
    )
    workspace = Path(args.workspace).resolve() / "error-check"
    from .verification import _run_single

    run = _run_single(
        scene_path,
        workspace,
        scene.metadata.default_parameters,
        split_mode="tiles",
        tile_width=32,
        tile_height=32,
        workers=2,
    )
    mutated = copy.deepcopy(run.output)
    mutated.display[0, 0, 0] = int(mutated.display[0, 0, 0]) ^ 1
    comparison = compare_outputs(run.output, mutated)
    results["E_DIVERGE"] = {
        "expected": "E_DIVERGE",
        "ok": not comparison.equal and comparison.first_mismatch is not None,
        "first_mismatch": comparison.first_mismatch,
    }
    all_ok = all(item.get("ok") for item in results.values())
    _print({"ok": all_ok, "results": results})
    return 0 if all_ok else 1


def command_gui(args: argparse.Namespace) -> int:
    from .ui.app import run

    return run(
        samples=Path(args.samples).resolve(),
        workspace=Path(args.workspace).resolve(),
        scene=args.scene,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="spectraforge", description="Multispectral + HDR workbench")
    parser.add_argument("--samples", default="samples", help="Deterministic samples root")
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="Generate deterministic local samples")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=424242)
    generate.add_argument("--scenes", type=int, default=3)
    generate.add_argument("--width", type=int, default=128)
    generate.add_argument("--height", type=int, default=96)
    generate.add_argument("--bands", type=int, default=8)
    generate.add_argument("--exposures", type=int, default=6)
    generate.add_argument("--no-faults", action="store_true")
    generate.set_defaults(func=command_generate)

    inspect = sub.add_parser("inspect", help="Inspect a scene and its analytical truth")
    inspect.add_argument("--scene")
    inspect.add_argument("--with-truth", action="store_true")
    inspect.add_argument("--with-alignment", action="store_true")
    inspect.set_defaults(func=command_inspect)

    process = sub.add_parser("process", help="Process or resume a scene snapshot")
    _common_process_arguments(process)
    process.add_argument("--fresh", action="store_true", help="Archive the snapshot and start over")
    process.add_argument("--pause-after", type=int)
    process.add_argument("--fail-segment", type=int)
    process.add_argument("--crash-after", type=int, help="Test-only forced termination after N commits")
    process.add_argument("--skip-failed", action="store_true")
    process.add_argument("--export", action="store_true")
    process.add_argument("--export-dir")
    process.set_defaults(func=command_process)

    export = sub.add_parser("export", help="Export a fully committed snapshot")
    export.add_argument("--scene")
    export.add_argument("--workspace", default="runtime")
    export.add_argument("--out", default="runtime/exports/latest")
    export.add_argument("--split", default="tiles", choices=["tiles", "bands"])
    export.add_argument("--tile", type=int, default=32)
    export.add_argument("--tile-height", type=int, default=32)
    export.set_defaults(func=command_export)

    verify = sub.add_parser("verify", help="Verify realtime/resume/full pixel equality")
    verify.add_argument("--scene")
    verify.add_argument("--workspace", default="runtime")
    verify.add_argument("--params")
    verify.add_argument("--report")
    verify.add_argument("--workers", type=int, default=4)
    verify.add_argument("--split", default="tiles", choices=["tiles", "bands"])
    verify.add_argument("--tile", type=int, default=32)
    verify.add_argument("--tile-height", type=int, default=32)
    verify.set_defaults(func=command_verify)

    errors = sub.add_parser("check-errors", help="Exercise each required error code")
    errors.add_argument("--scene")
    errors.add_argument("--workspace", default="runtime")
    errors.set_defaults(func=command_check_errors)

    gui = sub.add_parser("gui", help="Launch the PySide6 workbench")
    gui.add_argument("--scene")
    gui.add_argument("--workspace", default="runtime")
    gui.set_defaults(func=command_gui)
    return parser


def _common_process_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--scene")
    parser.add_argument("--workspace", default="runtime")
    parser.add_argument("--params")
    parser.add_argument("--split", default="tiles", choices=["tiles", "bands"])
    parser.add_argument("--tile", type=int, default=32)
    parser.add_argument("--tile-height", type=int, default=32)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--buffer-mb", type=int, default=64)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _print(exc.to_dict())
        return exc.exit_code
    except KeyboardInterrupt:
        _print({"ok": False, "code": "E_CANCEL", "message": "Interrupted by user"})
        return 60
    except Exception as exc:
        _print(
            {
                "ok": False,
                "code": "E_INTERNAL",
                "message": "Unhandled application error",
                "details": {"reason": str(exc), "type": type(exc).__name__},
            }
        )
        return 70
