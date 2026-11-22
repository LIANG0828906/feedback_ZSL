"""End-to-end verification of realtime, resume, and full recompute paths."""

from __future__ import annotations

import shutil
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .checkpoint import CheckpointManager
from .compare import compare_outputs
from .errors import WorkbenchError
from .metadata import load_scene
from .models import CompositeOutput
from .parameters import parameter_version, validate_parameters
from .pipeline import assemble_output, evaluate_color_truth, prepare_scene
from .scheduler import ProcessingJob, ResourceConfig
from .segmentation import make_segments, segmentation_spec
from .util import atomic_write_json, hash_arrays


@dataclass
class PathRun:
    name: str
    workspace: Path
    output: CompositeOutput
    run_result: dict[str, Any]
    initial_committed: int
    recovered_committed: int
    elapsed_s: float


def _output_hash(output: CompositeOutput) -> str:
    return hash_arrays(
        [
            ("radiance", output.radiance),
            ("rgb_chroma", output.rgb_chroma),
            ("linear_rgb", output.linear_rgb),
            ("display", output.display),
            ("gamut_channel_mask", output.gamut_channel_mask),
            ("segment_index", output.segment_index),
        ],
        extra={
            "peak": output.peak_radiance,
            "peak_location": output.peak_location,
            "gamut_count": output.gamut_count,
            "param_versions": output.param_versions,
        },
    )


def _run_single(
    scene_path: Path,
    workspace: Path,
    params: dict[str, Any],
    *,
    split_mode: str,
    tile_width: int,
    tile_height: int,
    workers: int,
    pause_after: int | None = None,
) -> PathRun:
    scene = load_scene(scene_path)
    segments = make_segments(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode=split_mode,
        tile_width=tile_width,
        tile_height=tile_height,
    )
    config = segmentation_spec(
        scene.width,
        scene.height,
        scene.band_count,
        split_mode=split_mode,
        tile_width=tile_width,
        tile_height=tile_height,
        interpolation=str(params["interpolation"]),
    )
    manager = CheckpointManager(workspace, scene, segments, config)
    initial = manager.progress()["committed"]
    job = ProcessingJob(
        scene,
        manager,
        resource=ResourceConfig(workers, 64),
        params=params,
    )
    started = time.perf_counter()
    first = job.run(pause_after=pause_after)
    partial_committed = manager.progress()["committed"]
    recovered = partial_committed
    if pause_after is not None and not first.get("ok"):
        manager.close()
        manager = CheckpointManager(workspace, scene, segments, config)
        recovered = manager.progress()["committed"]
        job = ProcessingJob(
            scene,
            manager,
            resource=ResourceConfig(max(1, workers), 64),
            params=params,
        )
        second = job.run()
        output = assemble_output(scene, manager, require_complete=True)
        manager.close()
        elapsed = time.perf_counter() - started
        return PathRun(
            name="resume",
            workspace=workspace,
            output=output,
            run_result=second,
            initial_committed=initial,
            recovered_committed=recovered,
            elapsed_s=elapsed,
        )
    output = assemble_output(scene, manager, require_complete=True)
    manager.close()
    elapsed = time.perf_counter() - started
    return PathRun(
        name="path",
        workspace=workspace,
        output=output,
        run_result=first,
        initial_committed=initial,
        recovered_committed=recovered,
        elapsed_s=elapsed,
    )


def _safe_reset(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)


def verify_paths(
    scene_path: str | Path,
    workspace: str | Path,
    *,
    params: dict[str, Any] | None = None,
    split_mode: str = "tiles",
    tile_width: int = 32,
    tile_height: int = 32,
    live_workers: int = 4,
    report_path: str | Path | None = None,
) -> dict[str, Any]:
    scene_path = Path(scene_path).resolve()
    root = Path(workspace).resolve()
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    run_root = root / "verification" / stamp
    run_root.mkdir(parents=True, exist_ok=True)
    scene = load_scene(scene_path)
    normalized = validate_parameters(
        params if params is not None else scene.metadata.default_parameters,
        scene.exposure_count,
    )
    total_segments = len(
        make_segments(
            scene.width,
            scene.height,
            scene.band_count,
            split_mode=split_mode,
            tile_width=tile_width,
            tile_height=tile_height,
        )
    )
    pause_after = max(1, total_segments // 2)
    paths: dict[str, PathRun] = {}
    for name, workers, pause in (
        ("live", live_workers, None),
        ("resume", max(1, min(2, live_workers)), pause_after),
        ("full", 1, None),
    ):
        path_workspace = run_root / name
        _safe_reset(path_workspace)
        result = _run_single(
            scene_path,
            path_workspace,
            normalized,
            split_mode=split_mode,
            tile_width=tile_width,
            tile_height=tile_height,
            workers=workers,
            pause_after=pause,
        )
        result.name = name
        paths[name] = result
    comparisons = {
        "live_vs_resume": compare_outputs(paths["live"].output, paths["resume"].output).to_dict(),
        "live_vs_full": compare_outputs(paths["live"].output, paths["full"].output).to_dict(),
        "resume_vs_full": compare_outputs(paths["resume"].output, paths["full"].output).to_dict(),
    }
    truth = evaluate_color_truth(scene, prepare_scene(scene, str(normalized["interpolation"])))
    equal = all(report["equal"] for report in comparisons.values())
    report = {
        "ok": bool(equal and truth["all_patches_within_tolerance"]),
        "kind": "three-path-verification",
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="milliseconds"),
        "scene_id": scene.metadata.scene_id,
        "scene_path": str(scene_path),
        "workspace": str(root),
        "param_version": parameter_version(normalized, scene.exposure_count),
        "parameters": normalized,
        "segmentation": {
            "mode": split_mode,
            "tile_width": tile_width,
            "tile_height": tile_height,
            "total": total_segments,
            "resume_pause_after": pause_after,
        },
        "paths": {
            name: {
                "workspace": str(run.workspace),
                "elapsed_s": run.elapsed_s,
                "initial_committed": run.initial_committed,
                "recovered_committed": run.recovered_committed,
                "progress": run.run_result.get("progress"),
                "resource_metrics": run.run_result.get("resource_metrics"),
                "output_hash": _output_hash(run.output),
                "peak_radiance": run.output.peak_radiance,
                "peak_location": list(run.output.peak_location),
                "gamut_count": run.output.gamut_count,
                "param_versions": run.output.param_versions,
            }
            for name, run in paths.items()
        },
        "comparisons": comparisons,
        "truth": truth,
        "checkpoint_resume": {
            "fresh_snapshot_committed": paths["resume"].initial_committed,
            "committed_before_restart": paths["resume"].recovered_committed,
            "recovered_committed": paths["resume"].recovered_committed,
            "rehydrated_not_recomputed": 0
            < paths["resume"].recovered_committed
            < total_segments,
        },
    }
    target = Path(report_path) if report_path is not None else run_root / "report.json"
    atomic_write_json(target, report)
    if not equal:
        first = next(
            (item["first_mismatch"] for item in comparisons.values() if not item["equal"]),
            None,
        )
        raise WorkbenchError(
            "E_DIVERGE",
            "Three processing paths are not pixel-identical",
            {"first_mismatch": first, "report": str(target)},
        )
    if not truth["all_patches_within_tolerance"]:
        raise WorkbenchError(
            "E_DIVERGE",
            "Analytical color patches exceed the declared tolerance",
            {"truth": truth, "report": str(target)},
        )
    report["report_path"] = str(target)
    return report
