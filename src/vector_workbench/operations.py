from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import numpy as np

from . import ALGORITHM_VERSION
from .checkpoint import CheckpointStore
from .diff import DiffResult, compare_pipelines
from .errors import DiffError, ErrorCode, WorkbenchError
from .export import save_coverage_png, save_rgba_png
from .geometry import (
    flatten_path,
    point_to_polyline_distance,
    polygon_area,
)
from .model import RenderParams, Scene, Stage
from .pipeline import Pipeline, RunControl
from .validation import load_sample_manifest, load_scene

DEFAULT_TOLERANCES = {
    "area_relative": 0.01,
    "curve_distance_at_scale_1": 0.75,
    "min_length_monotonic_ratios": [2.0, 1.0, 0.5, 0.25, 0.125],
}


def image_sha256(image: np.ndarray) -> str:
    return hashlib.sha256(np.ascontiguousarray(image).tobytes()).hexdigest()


def array_sha256(array: np.ndarray) -> str:
    return hashlib.sha256(np.ascontiguousarray(array).tobytes()).hexdigest()


def load_sample_scene(data_dir: str | Path, scene_id: str) -> Scene:
    data = Path(data_dir)
    manifest, _ = load_sample_manifest(data)
    entry = next(
        (item for item in manifest.get("scenes", []) if item.get("scene_id") == scene_id),
        None,
    )
    if entry is None:
        raise WorkbenchError(
            ErrorCode.E_DECODE, f"sample scene {scene_id!r} is not declared", field="scene_id"
        )
    return load_scene(
        data / entry["scene_file"],
        data / str(manifest["truth_manifest"]),
    )


def list_samples(data_dir: str | Path) -> list[dict[str, Any]]:
    manifest, _ = load_sample_manifest(data_dir)
    return list(manifest.get("scenes", []))


def validate_flattening(
    scene: Scene, params: RenderParams
) -> dict[str, Any]:
    params.validate()
    tolerances = (
        scene.truth.get("_validation_tolerances", DEFAULT_TOLERANCES)
        if scene.truth
        else DEFAULT_TOLERANCES
    )
    ratios = [float(v) for v in tolerances.get("min_length_monotonic_ratios", [])]
    area_limit = float(tolerances.get("area_relative", 0.01))
    curve_limit = max(
        float(tolerances.get("curve_distance_at_scale_1", 0.75)) * params.scale,
        params.tolerance * 2.5,
    )
    results: list[dict[str, Any]] = []
    all_passed = True
    for path in scene.paths:
        truth = (scene.truth or {}).get("paths", {}).get(path.id, {})
        counts: list[int] = []
        for ratio in ratios:
            tolerance = params.tolerance * ratio
            points = flatten_path(
                path, scene.coord_scale, params.scale, tolerance
            )
            counts.append(int(len(points)))
        default_points = flatten_path(
            path, scene.coord_scale, params.scale, params.tolerance
        )
        area = polygon_area(default_points) / (params.scale * params.scale)
        truth_area = float(truth.get("area", 0.0))
        relative_area_error = abs(area - truth_area) / max(abs(truth_area), 1e-12)

        sample_distances = [
            point_to_polyline_distance(
                default_points,
                (
                    float(sample["point"][0]) * params.scale,
                    float(sample["point"][1]) * params.scale,
                ),
            )
            for sample in truth.get("curve_samples", [])
        ]
        checkpoint_distances = [
            point_to_polyline_distance(
                default_points,
                (
                    float(checkpoint["point"][0]) * params.scale,
                    float(checkpoint["point"][1]) * params.scale,
                ),
            )
            for checkpoint in truth.get("closure_checkpoints", [])
        ]
        first_last_distinct = bool(
            len(default_points) < 2
            or not np.array_equal(default_points[0], default_points[-1])
        )
        if len(default_points) > 1:
            deltas = np.diff(default_points, axis=0)
            no_adjacent_duplicates = bool(
                np.all(np.hypot(deltas[:, 0], deltas[:, 1]) > 1e-12)
            )
        else:
            no_adjacent_duplicates = False
        monotonic = all(
            counts[index] >= counts[index - 1] for index in range(1, len(counts))
        )
        closure_ok = bool(
            checkpoint_distances and max(checkpoint_distances) <= 1e-7
        )
        passed = (
            first_last_distinct
            and no_adjacent_duplicates
            and monotonic
            and closure_ok
            and relative_area_error <= area_limit
            and (not sample_distances or max(sample_distances) <= curve_limit)
        )
        all_passed = all_passed and passed
        results.append(
            {
                "path_id": path.id,
                "point_counts_by_tolerance": counts,
                "monotonic_non_decreasing": monotonic,
                "first_last_distinct": first_last_distinct,
                "no_adjacent_duplicate_endpoints": no_adjacent_duplicates,
                "flattened_area_scene": area,
                "truth_area_scene": truth_area,
                "relative_area_error": relative_area_error,
                "max_curve_sample_distance_px": (
                    max(sample_distances) if sample_distances else 0.0
                ),
                "curve_distance_limit_px": curve_limit,
                "max_closure_checkpoint_distance_px": (
                    max(checkpoint_distances) if checkpoint_distances else 0.0
                ),
                "passed": passed,
            }
        )
    return {
        "scene_id": scene.scene_id,
        "scale": params.scale,
        "tolerance": params.tolerance,
        "passed": all_passed,
        "paths": results,
    }


def _run_three_paths(
    scene: Scene,
    params: RenderParams,
    root: Path,
    *,
    incremental_commits: int,
    resume_commits: int,
) -> dict[str, Any]:
    total = len(scene.paths) * len(Stage.ordered())
    incremental_commits = max(1, min(total - 1, incremental_commits))
    resume_commits = max(1, min(total - 1, resume_commits))

    full = Pipeline(scene, params, root / "full")
    full_result = full.run("full")

    incremental_prefix = Pipeline(
        scene,
        params,
        root / "incremental",
        control=RunControl(cancel_after_commits=incremental_commits),
    )
    incremental_prefix_result = incremental_prefix.run("full", raise_on_error=False)
    incremental = Pipeline(scene, params, root / "incremental")
    incremental_result = incremental.run("incremental", raise_on_error=False)

    resume_prefix = Pipeline(
        scene,
        params,
        root / "resume",
        control=RunControl(terminate_after_commits=resume_commits),
    )
    resume_prefix_result = resume_prefix.run("full", raise_on_error=False)
    resumed = Pipeline(scene, params, root / "resume")
    resumed_result = resumed.run("resume", raise_on_error=False)

    diff_incremental = compare_pipelines(full, incremental)
    diff_resume = compare_pipelines(full, resumed)
    sequence_consistent = (
        full_result.commit_sequence
        == incremental_result.commit_sequence
        == resumed_result.commit_sequence
    )
    exact_pixels = bool(
        np.array_equal(full.canvas, incremental.canvas)
        and np.array_equal(full.canvas, resumed.canvas)
    )
    passed = bool(
        full_result.completed
        and incremental_result.completed
        and resumed_result.completed
        and incremental_prefix_result.status == "cancelled"
        and incremental_prefix_result.recoverable
        and resume_prefix_result.status == "terminated"
        and not resume_prefix_result.recoverable
        and diff_incremental.equal
        and diff_resume.equal
        and sequence_consistent
        and exact_pixels
    )
    return {
        "passed": passed,
        "total_stages": total,
        "full": {
            "status": full_result.status,
            "progress": [full_result.progress_committed, full_result.progress_total],
            "image_sha256": image_sha256(full.canvas),
            "dimensions": list(full.image_size),
        },
        "incremental": {
            "prefix_status": incremental_prefix_result.status,
            "prefix_recoverable": incremental_prefix_result.recoverable,
            "prefix_progress": [
                incremental_prefix_result.progress_committed,
                incremental_prefix_result.progress_total,
            ],
            "final_status": incremental_result.status,
            "progress": [
                incremental_result.progress_committed,
                incremental_result.progress_total,
            ],
            "image_sha256": image_sha256(incremental.canvas),
            "diff": diff_incremental.to_dict(),
        },
        "resume": {
            "prefix_status": resume_prefix_result.status,
            "prefix_recoverable": resume_prefix_result.recoverable,
            "prefix_progress": [
                resume_prefix_result.progress_committed,
                resume_prefix_result.progress_total,
            ],
            "final_status": resumed_result.status,
            "progress": [
                resumed_result.progress_committed,
                resumed_result.progress_total,
            ],
            "image_sha256": image_sha256(resumed.canvas),
            "diff": diff_resume.to_dict(),
        },
        "commit_sequence_consistent": sequence_consistent,
        "commit_sequence": full_result.commit_sequence,
        "exact_pixel_equal": exact_pixels,
    }, {
        "full": full,
        "incremental": incremental,
        "resume": resumed,
    }


def verify_three_paths(
    scene: Scene,
    params: RenderParams,
    checkpoint_root: str | Path,
) -> tuple[dict[str, Any], dict[str, Pipeline]]:
    total = len(scene.paths) * len(Stage.ordered())
    return _run_three_paths(
        scene,
        params,
        Path(checkpoint_root),
        incremental_commits=max(1, total // 3),
        resume_commits=max(2, total // 2),
    )


def verify_anomalies(
    data_dir: str | Path,
    checkpoint_root: str | Path,
) -> dict[str, Any]:
    data = Path(data_dir)
    samples = list_samples(data)
    anomaly_entries = [
        entry for entry in samples if entry.get("category") == "anomaly"
    ]
    results: list[dict[str, Any]] = []
    for entry in anomaly_entries:
        scene = load_sample_scene(data, str(entry["scene_id"]))
        path = next(
            (item for item in scene.paths if item.injection is not None), None
        )
        if path is None:
            raise WorkbenchError(
                ErrorCode.E_DECODE,
                "anomaly sample does not declare an injected path",
            )
        pipeline = Pipeline(
            scene,
            RenderParams(),
            Path(checkpoint_root) / scene.scene_id,
        )
        result = pipeline.run("full", raise_on_error=False)
        codes = [error["code"] for error in result.errors]
        expected = str(entry.get("expected_error"))
        row = next(item for item in pipeline.rows() if item["path_id"] == path.id)
        other_rows = [
            item for item in pipeline.rows() if item["path_id"] != path.id
        ]
        good_coverage_hashes = {
            path_id: array_sha256(array)
            for path_id, array in pipeline.coverage_cache.items()
            if path_id != path.id
        }

        disabled = Pipeline(
            scene,
            RenderParams(),
            Path(checkpoint_root) / f"{scene.scene_id}_actual",
            injections={path.id: None},
        )
        disabled_result = disabled.run("full", raise_on_error=False)
        disabled_codes = [error["code"] for error in disabled_result.errors]

        retry_result = None
        retry_rows = None
        good_paths_unchanged = True
        if path.injection and path.injection.kind == "stage_failure":
            pipeline.set_injections({path.id: None})
            retry_result = pipeline.run_path(path.id, raise_on_error=False)
            retry_rows = pipeline.rows()
            good_hashes_after = {
                path_id: array_sha256(array)
                for path_id, array in pipeline.coverage_cache.items()
                if path_id != path.id
            }
            good_paths_unchanged = good_hashes_after == good_coverage_hashes

        expected_stage_error = expected in {"E_SELF", "E_DEGEN", "E_CLIP", "E_STAGE"}
        if expected == "E_SELF":
            expected_failed_stage = "flatten"
        elif expected in {"E_DEGEN", "E_CLIP"}:
            expected_failed_stage = "coverage"
        else:
            expected_failed_stage = (
                path.injection.stage.value
                if path.injection.stage is not None
                else "coverage"
            )
        passed = bool(
            result.status == "failed"
            and expected in codes
            and row.get(expected_failed_stage) == "failed"
            and sum(
                row.get(stage) == "failed"
                for stage in ("flatten", "coverage", "compose")
            )
            == 1
            and all(
                other["flatten"] == "committed"
                and other["coverage"] == "committed"
                and other["compose"] == "committed"
                for other in other_rows
            )
            and expected_stage_error
        )
        if disabled_codes:
            passed = passed and expected == disabled_codes[0]
        else:
            passed = passed and expected == "E_STAGE"
        if retry_result is not None:
            passed = passed and retry_result.status == "completed"
            passed = passed and good_paths_unchanged
            passed = passed and bool(
                retry_rows
                and all(
                    item["flatten"] == "committed"
                    and item["coverage"] == "committed"
                    and item["compose"] == "committed"
                    for item in retry_rows
                )
            )
        results.append(
            {
                "scene_id": scene.scene_id,
                "injected_path": path.id,
                "expected_error": expected,
                "observed_codes": codes,
                "status": result.status,
                "path_status_row": row,
                "other_paths_committed": all(
                    other["compose"] == "committed" for other in other_rows
                ),
                "good_coverage_hashes": good_coverage_hashes,
                "good_paths_unchanged": good_paths_unchanged,
                "injection_disabled_codes": disabled_codes,
                "retry": (
                    {
                        "status": retry_result.status,
                        "rows": retry_rows,
                    }
                    if retry_result is not None
                    else None
                ),
                "passed": passed,
            }
        )
    return {
        "passed": all(item["passed"] for item in results),
        "cases": results,
    }


def verify_snapshot_invalidation(
    scene: Scene,
    params: RenderParams,
    checkpoint_root: str | Path,
) -> dict[str, Any]:
    root = Path(checkpoint_root)
    base_root = root / "base"
    base = Pipeline(scene, params, base_root)
    base_result = base.run("full")

    changed_params = RenderParams(
        scale=params.scale,
        tolerance=params.tolerance / 2.0,
        subsamples=params.subsamples,
        max_parallelism=params.max_parallelism,
        coverage_buffer_limit=params.coverage_buffer_limit,
        max_stroke_pixels=params.max_stroke_pixels,
    )
    param_pipeline = Pipeline(scene, changed_params, base_root)
    param_invalidated = bool(
        param_pipeline.invalidated_snapshot
        and "parameter" in (param_pipeline.invalidation_reason or "").lower()
    )

    changed_raw = dict(scene.raw)
    changed_raw["seed"] = int(changed_raw.get("seed", 0)) + 1
    changed_scene = Scene(
        scene_id=scene.scene_id,
        seed=scene.seed + 1,
        coord_scale=scene.coord_scale,
        coord_bounds=scene.coord_bounds,
        render_bounds=scene.render_bounds,
        paths=scene.paths,
        raw=changed_raw,
        truth=scene.truth,
        source_path=scene.source_path,
    )
    scene_root = root / "scene_hash"
    scene_pipeline = Pipeline(scene, params, scene_root)
    scene_pipeline.run("full")
    scene_changed_pipeline = Pipeline(changed_scene, params, scene_root)
    scene_invalidated = bool(
        scene_changed_pipeline.invalidated_snapshot
        and "scene" in (scene_changed_pipeline.invalidation_reason or "").lower()
    )

    algorithm_root = root / "algorithm"
    algorithm_pipeline = Pipeline(scene, params, algorithm_root)
    algorithm_pipeline.run("full")
    wrong_store = CheckpointStore(
        algorithm_root,
        scene_id=scene.scene_id,
        scene_hash=scene.canonical_hash(),
        param_hash=params.fingerprint(),
        algorithm_version="vector-workbench.algorithm.wrong",
        expected_order=[
            (path.id, stage) for path in scene.paths for stage in Stage.ordered()
        ],
    )
    wrong_load = wrong_store.load()
    algorithm_invalidated = bool(
        wrong_load.invalidated and "algorithm" in (wrong_load.reason or "").lower()
    )
    passed = bool(
        base_result.completed
        and param_invalidated
        and scene_invalidated
        and algorithm_invalidated
    )
    return {
        "passed": passed,
        "full_run_status": base_result.status,
        "parameter_version_invalidated": param_invalidated,
        "parameter_reason": param_pipeline.invalidation_reason,
        "scene_hash_invalidated": scene_invalidated,
        "scene_reason": scene_changed_pipeline.invalidation_reason,
        "algorithm_version_invalidated": algorithm_invalidated,
        "algorithm_reason": wrong_load.reason,
    }


def verify_diff_detection(
    scene: Scene, params: RenderParams, checkpoint_root: str | Path
) -> dict[str, Any]:
    root = Path(checkpoint_root)
    reference = Pipeline(scene, params, root / "reference")
    reference.run("full")
    candidate = Pipeline(scene, params, root / "candidate")
    candidate.run("full")
    equal_diff = compare_pipelines(reference, candidate)
    target_path = next(
        path for path in scene.paths if path.id in candidate.coverage_cache
    )
    changed = candidate.coverage_cache[target_path.id].copy()
    changed[0, 0] = np.float32(1.0 - changed[0, 0])
    candidate.coverage_cache[target_path.id] = changed
    detected = compare_pipelines(reference, candidate)
    raised_code = None
    try:
        detected.raise_if_different()
    except DiffError as exc:
        raised_code = exc.code.value
    passed = bool(
        equal_diff.equal
        and not detected.equal
        and detected.first_pixel == (0, 0)
        and detected.path_id == target_path.id
        and detected.stage == "coverage"
        and raised_code == "E_DIFF"
    )
    return {
        "passed": passed,
        "equal_before_mutation": equal_diff.equal,
        "detected_diff": detected.to_dict(),
        "raised_code": raised_code,
    }


def run_acceptance(
    data_dir: str | Path,
    artifact_dir: str | Path,
    checkpoint_root: str | Path,
    *,
    seed: int = 20261123,
    params: RenderParams | None = None,
) -> dict[str, Any]:
    data = Path(data_dir)
    artifacts = Path(artifact_dir)
    checkpoints = Path(checkpoint_root)
    artifacts.mkdir(parents=True, exist_ok=True)
    params = params or RenderParams()
    samples = list_samples(data)
    normal_entries = [
        entry for entry in samples if entry.get("category") == "normal"
    ]
    scene_reports: list[dict[str, Any]] = []
    pipeline_outputs: dict[str, dict[str, Pipeline]] = {}
    for entry in normal_entries:
        scene = load_sample_scene(data, entry["scene_id"])
        flatten_report = validate_flattening(scene, params)
        execution_report, pipelines = _run_three_paths(
            scene,
            params,
            checkpoints / "three_paths" / scene.scene_id,
            incremental_commits=max(1, len(scene.paths) * 3 // 3),
            resume_commits=max(2, len(scene.paths) * 3 // 2),
        )
        pipeline_outputs[scene.scene_id] = pipelines
        full = pipelines["full"]
        save_rgba_png(full.canvas, artifacts / f"{scene.scene_id}_composite.png")
        first_path = scene.paths[0]
        if first_path.id in full.coverage_cache:
            save_coverage_png(
                full.coverage_cache[first_path.id],
                artifacts / f"{scene.scene_id}_{first_path.id}_coverage.png",
            )
        scene_reports.append(
            {
                "scene_id": scene.scene_id,
                "dimensions": list(full.image_size),
                "flattening": flatten_report,
                "three_paths": execution_report,
                "passed": flatten_report["passed"] and execution_report["passed"],
            }
        )

    anomaly_report = verify_anomalies(data, checkpoints / "anomalies")
    reference_scene = load_sample_scene(data, normal_entries[0]["scene_id"])
    invalidation_report = verify_snapshot_invalidation(
        reference_scene,
        params,
        checkpoints / "invalidation",
    )
    diff_report = verify_diff_detection(
        reference_scene,
        params,
        checkpoints / "diff",
    )
    passed = bool(
        all(item["passed"] for item in scene_reports)
        and anomaly_report["passed"]
        and invalidation_report["passed"]
        and diff_report["passed"]
    )
    report = {
        "schema_version": 1,
        "algorithm_version": ALGORITHM_VERSION,
        "seed": seed,
        "parameters": params.version_dict(),
        "passed": passed,
        "scenes": scene_reports,
        "anomaly_injection": anomaly_report,
        "snapshot_invalidation": invalidation_report,
        "diff_detection": diff_report,
    }
    report_path = artifacts / "validation_report.json"
    report_path.write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    return report

