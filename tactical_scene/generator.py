"""Deterministic local sample generation and verification."""

from __future__ import annotations

import hashlib
import json
import math
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Sequence

import numpy as np

from .camera import Camera
from .errors import ErrorCode, TacticalError
from .projection import ProjectionConfig
from .query import BudgetConfig, QueryEngine
from .scene import AABBShape, CapsuleShape, Scene, SceneUnit, load_scene, write_scene
from .trace import TraceRecorder, TraceReplayer, load_trace
from .versioning import load_published_scene, upsert_manifest_entry


def _json_bytes(payload: Any) -> bytes:
    return (
        json.dumps(payload, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n"
    ).encode("utf-8")


def _write_json(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp")
    temporary.write_bytes(_json_bytes(payload))
    temporary.replace(path)


def _prepare_output(output: Path) -> None:
    resolved = output.resolve()
    forbidden = {Path("/").resolve(), Path.home().resolve(), Path.cwd().resolve()}
    if resolved in forbidden:
        raise ValueError(f"refusing to clean unsafe sample output directory: {resolved}")
    output.mkdir(parents=True, exist_ok=True)
    for child in output.iterdir():
        if child.is_dir() and not child.is_symlink():
            shutil.rmtree(child)
        else:
            child.unlink()


def _rotation_z(angle: float) -> np.ndarray:
    cosine = math.cos(angle)
    sine = math.sin(angle)
    return np.asarray(
        [
            [cosine, -sine, 0.0, 0.0],
            [sine, cosine, 0.0, 0.0],
            [0.0, 0.0, 1.0, 0.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        dtype=np.float64,
    )


def _affine(rotation: np.ndarray, translation: Sequence[float]) -> tuple[float, ...]:
    matrix = np.asarray(rotation, dtype=np.float64).copy()
    matrix[0, 3] = float(translation[0])
    matrix[1, 3] = float(translation[1])
    matrix[2, 3] = float(translation[2])
    return tuple(float(value) for value in matrix.reshape(-1))


def _camera_basis(camera: Camera) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    inverse = np.linalg.inv(camera.view_array)
    eye = inverse[:3, 3]
    right = camera.view_array[0, :3]
    up = camera.view_array[1, :3]
    back = camera.view_array[2, :3]
    return eye, right, up, back


def _make_units(
    count: int,
    seed: int,
    *,
    camera: Camera,
    mode: str,
) -> tuple[SceneUnit, ...]:
    rng = np.random.default_rng(seed)
    eye, right, up, back = _camera_basis(camera)
    units: list[SceneUnit] = []
    behind_count = max(1, count // 100) if mode in {"front", "oblique", "boundary"} else 0
    for index in range(count):
        if mode == "ambiguous" and index < 2:
            x = y = 0.0
            z = 500.0 + index * 5e-7
            half = 0.5 if index == 0 else 0.5000004
            shape = AABBShape((-half, -half, z - half), (half, half, z + half))
            transform = tuple(float(value) for value in np.eye(4).reshape(-1))
        elif index >= count - behind_count and behind_count:
            distance = float(rng.uniform(10.0, 35.0))
            lateral = float(rng.uniform(-12.0, 12.0))
            vertical = float(rng.uniform(-8.0, 8.0))
            position = eye + back * distance + right * lateral + up * vertical
            shape_kind = index % 3
            if shape_kind:
                shape = AABBShape((-1.0, -1.0, -1.0), (1.0, 1.0, 1.0))
            else:
                shape = CapsuleShape((0.0, 0.0, -1.2), (0.0, 0.0, 1.2), 0.8)
            transform = _affine(_rotation_z(float(rng.uniform(-math.pi, math.pi))), position)
        else:
            spread = 95.0 if mode == "boundary" else 75.0
            x = float(rng.uniform(-spread, spread))
            y = float(rng.uniform(-spread * 0.7, spread * 0.7))
            z = float(rng.uniform(-6.0, 6.0))
            if mode == "boundary" and index % 37 == 0:
                x *= 1.8
                y *= 1.8
            if index % 3 == 0:
                radius = float(rng.uniform(0.55, 1.45))
                half_length = float(rng.uniform(0.6, 2.2))
                shape = CapsuleShape((0.0, 0.0, -half_length), (0.0, 0.0, half_length), radius)
            else:
                half_x = float(rng.uniform(0.7, 2.3))
                half_y = float(rng.uniform(0.7, 2.3))
                half_z = float(rng.uniform(0.7, 2.3))
                shape = AABBShape((-half_x, -half_y, -half_z), (half_x, half_y, half_z))
            transform = _affine(
                _rotation_z(float(rng.uniform(-math.pi, math.pi))),
                (x, y, z),
            )
        visible = index < 2 or index % 97 != 0
        units.append(
            SceneUnit(
                unit_id=index + 1,
                name=f"unit-{index + 1:06d}",
                shape=shape,
                transform=transform,
                visible=visible,
            )
        )
    return tuple(units)


def _baseline_budget() -> BudgetConfig:
    return BudgetConfig(
        budget_ms=1e15,
        memory_budget_bytes=2**62,
        bucket_grids=(64,),
        actual_degradation=False,
    )


def _polygon_centroid(points: Sequence[Sequence[float]]) -> tuple[float, float]:
    if not points:
        return (0.0, 0.0)
    return (
        sum(float(point[0]) for point in points) / len(points),
        sum(float(point[1]) for point in points) / len(points),
    )


@dataclass(frozen=True, slots=True)
class GeneratedCase:
    name: str
    directory: Path
    unit_count: int
    pose: str
    version: int
    content_hash: str
    selected_count: int


def _generate_case(
    output: Path,
    *,
    name: str,
    units: Sequence[SceneUnit],
    camera: Camera,
    pose: str,
    seed: int,
    projection_config: ProjectionConfig | None = None,
) -> GeneratedCase:
    case_dir = output / name
    case_dir.mkdir(parents=True, exist_ok=True)
    scene = Scene(version=1, units=tuple(units), source=name)
    scene_path = case_dir / "scene.bfscene"
    manifest_path = case_dir / "manifest.json"
    write_scene(scene_path, scene)
    upsert_manifest_entry(manifest_path, scene, scene_file="scene.bfscene")
    _write_json(case_dir / "camera.json", camera.to_dict())

    rect = (
        round(camera.width * 0.12, 6),
        round(camera.height * 0.16, 6),
        round(camera.width * 0.82, 6),
        round(camera.height * 0.78, 6),
    )
    engine = QueryEngine(
        scene,
        camera,
        projection_config=projection_config,
        budget_config=_baseline_budget(),
    )
    try:
        selection = engine.select_rect(*rect)
    except TacticalError as exc:
        if exc.code != ErrorCode.OCCLUSION_AMBIGUOUS:
            raise
        raise
    baseline = {
        "scene_version": selection.scene_version,
        "scene_hash": selection.scene_hash,
        "camera_hash": selection.camera_hash,
        "strategy": selection.strategy.value,
        "bucket_grid": [selection.bucket_columns, selection.bucket_rows],
        "refinement_depth": selection.refinement_depth,
        "stage_candidate_counts": dict(sorted(selection.stage_counts.items())),
        "selected": list(selection.selected),
        "selected_coverage": [
            {
                "unit_id": decision.unit_id,
                "coverage": round(decision.coverage, 12),
            }
            for decision in selection.decisions
            if decision.selected
        ],
        "coverage_error_upper_bound": selection.coverage_error_upper_bound,
        "rect": list(rect),
    }
    _write_json(case_dir / "baseline.json", baseline)

    trace_path = case_dir / "trace.jsonl"
    expected_errors: list[dict[str, Any]] = []
    with TraceRecorder(trace_path) as recorder:
        recorder.record_selection(selection, camera)
        hover_expected_error: dict[str, Any] | None = None
        selected_projections = [
            engine.projection_by_id[unit_id]
            for unit_id in selection.selected
            if unit_id in engine.projection_by_id
        ]
        if selected_projections:
            candidate = min(
                selected_projections,
                key=lambda projection: (projection.nearest_depth, projection.unit_id),
            )
            point = _polygon_centroid(candidate.polygon)
            try:
                hover = engine.hover(
                    point[0],
                    point[1],
                    target_unit_id=candidate.unit_id,
                )
                recorder.record_hover(hover, camera)
            except TacticalError as exc:
                if exc.code != ErrorCode.OCCLUSION_AMBIGUOUS:
                    raise
                hover_expected_error = {
                    "unit_id": candidate.unit_id,
                    "point": [point[0], point[1]],
                    "error": exc.code,
                    "depth_epsilon": engine.hover_config.depth_epsilon,
                    "details": exc.details,
                }
                expected_errors.append(hover_expected_error)

    case_payload = {
        "name": name,
        "pose": pose,
        "seed": seed,
        "unit_count": len(units),
        "version": scene.version,
        "content_hash": scene.content_hash,
        "selected_count": len(selection.selected),
        "expected_errors": expected_errors,
    }
    _write_json(case_dir / "case.json", case_payload)
    return GeneratedCase(
        name=name,
        directory=case_dir,
        unit_count=len(units),
        pose=pose,
        version=scene.version,
        content_hash=scene.content_hash,
        selected_count=len(selection.selected),
    )


def generate_sample_suite(
    output: str | Path,
    *,
    seed: int = 20261122,
    sizes: Sequence[int] = (64, 2048, 20000),
    projection_config: ProjectionConfig | None = None,
    clean: bool = True,
) -> dict[str, Any]:
    output_path = Path(output)
    if clean:
        _prepare_output(output_path)
    else:
        output_path.mkdir(parents=True, exist_ok=True)
    if not sizes or any(int(size) < 2 for size in sizes):
        raise ValueError("sample sizes must contain integers >= 2")
    cases: list[GeneratedCase] = []
    width, height = 1600, 900
    for size_index, size in enumerate(sizes):
        camera = Camera.look_at(
            (0.0, 0.0, 180.0),
            (0.0, 0.0, 0.0),
            (0.0, 1.0, 0.0),
            width=width,
            height=height,
            fov_y_degrees=50.0,
            near=0.5,
            far=5000.0,
        )
        units = _make_units(
            int(size),
            seed + size_index * 1009,
            camera=camera,
            mode="front",
        )
        cases.append(
            _generate_case(
                output_path,
                name=f"front_{size:06d}",
                units=units,
                camera=camera,
                pose="front",
                seed=seed,
                projection_config=projection_config,
            )
        )
        if size <= 4096:
            oblique_camera = Camera.look_at(
                (130.0, -155.0, 125.0),
                (0.0, 0.0, 0.0),
                (0.0, 0.0, 1.0),
                width=width,
                height=height,
                fov_y_degrees=55.0,
                near=0.5,
                far=5000.0,
            )
            oblique_units = _make_units(
                int(size),
                seed + size_index * 1009 + 17,
                camera=oblique_camera,
                mode="oblique",
            )
            cases.append(
                _generate_case(
                    output_path,
                    name=f"oblique_{size:06d}",
                    units=oblique_units,
                    camera=oblique_camera,
                    pose="oblique",
                    seed=seed,
                    projection_config=projection_config,
                )
            )

    boundary_camera = Camera.look_at(
        (0.0, 0.0, 55.0),
        (0.0, 0.0, 0.0),
        (0.0, 1.0, 0.0),
        width=960,
        height=540,
        fov_y_degrees=68.0,
        near=1.0,
        far=600.0,
    )
    boundary_units = _make_units(
        240,
        seed + 7001,
        camera=boundary_camera,
        mode="boundary",
    )
    cases.append(
        _generate_case(
            output_path,
            name="boundary_behind_00240",
            units=boundary_units,
            camera=boundary_camera,
            pose="boundary_behind",
            seed=seed,
            projection_config=projection_config,
        )
    )

    ambiguity_camera = Camera.orthographic(
        left=-20.0,
        right=20.0,
        bottom=-20.0,
        top=20.0,
        width=400,
        height=400,
        near=0.1,
        far=1000.0,
    )
    ambiguity_units = _make_units(
        48,
        seed + 8009,
        camera=ambiguity_camera,
        mode="ambiguous",
    )
    cases.append(
        _generate_case(
            output_path,
            name="occlusion_ambiguous_00048",
            units=ambiguity_units,
            camera=ambiguity_camera,
            pose="orthographic_ambiguous",
            seed=seed,
        projection_config=projection_config,
        )
    )

    suite = {
        "format": "tactical-sample-suite-v1",
        "seed": seed,
        "sizes": [int(size) for size in sizes],
        "cases": [
            {
                "name": case.name,
                "directory": case.name,
                "unit_count": case.unit_count,
                "pose": case.pose,
                "version": case.version,
                "content_hash": case.content_hash,
                "selected_count": case.selected_count,
            }
            for case in cases
        ],
    }
    _write_json(output_path / "suite.json", suite)
    return suite


def verify_sample_suite(
    output: str | Path,
    *,
    projection_config: ProjectionConfig | None = None,
) -> dict[str, Any]:
    output_path = Path(output)
    suite = json.loads((output_path / "suite.json").read_text(encoding="utf-8"))
    results: list[dict[str, Any]] = []
    for case_entry in suite["cases"]:
        case_dir = output_path / case_entry["directory"]
        scene = load_published_scene(
            case_dir / "scene.bfscene",
            case_dir / "manifest.json",
            expected_version=case_entry["version"],
            expected_hash=case_entry["content_hash"],
        )
        camera = Camera.from_dict(json.loads((case_dir / "camera.json").read_text(encoding="utf-8")))
        baseline = json.loads((case_dir / "baseline.json").read_text(encoding="utf-8"))
        rect = baseline["rect"]
        budget = BudgetConfig(
            budget_ms=1e15,
            memory_budget_bytes=2**62,
            selection_threshold=0.0,
            bucket_grids=(int(baseline["bucket_grid"][0]),),
            force_strategy=baseline["strategy"],
            force_bucket_columns=int(baseline["bucket_grid"][0]),
            actual_degradation=False,
        )
        selection = QueryEngine(
            scene,
            camera,
            projection_config=projection_config,
            budget_config=budget,
        ).select_rect(
            float(rect[0]),
            float(rect[1]),
            float(rect[2]),
            float(rect[3]),
            expected_version=int(baseline["scene_version"]),
            expected_hash=baseline["scene_hash"],
        )
        selected_coverage = [
            {
                "unit_id": decision.unit_id,
                "coverage": round(decision.coverage, 12),
            }
            for decision in selection.decisions
            if decision.selected
        ]
        if (
            list(selection.selected) != baseline["selected"]
            or selected_coverage != baseline["selected_coverage"]
        ):
            raise TacticalError(
                ErrorCode.TRACE_INVALID,
                "sample baseline selection does not reproduce",
                {
                    "case": case_entry["name"],
                    "expected_selected": baseline["selected"],
                    "actual_selected": list(selection.selected),
                },
            )
        trace_records = load_trace(case_dir / "trace.jsonl")
        TraceReplayer(
            lambda version, content_hash, scene=scene: scene,
            projection_config=projection_config,
        ).replay(case_dir / "trace.jsonl")
        case_metadata = json.loads((case_dir / "case.json").read_text(encoding="utf-8"))
        results.append(
            {
                "name": case_entry["name"],
                "unit_count": case_entry["unit_count"],
                "selected_count": len(selection.selected),
                "trace_records": len(trace_records),
                "expected_errors": case_metadata.get("expected_errors", []),
                "version": selection.scene_version,
                "content_hash": selection.scene_hash,
            }
        )
    return {"format": "tactical-sample-verification-v1", "cases": results}
