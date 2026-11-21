"""Deterministic sample scenes, event sequences and baselines."""

from __future__ import annotations

import json
import os
import random
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

from .camera import Camera
from .canonical import canonical_json_bytes, sha256_bytes
from .errors import E_SCENE_INVALID, fail
from .geometry import Vec3
from .model import Cover, Grid, Scene, Unit
from .scenario import RunOutput, run_scenario
from .scene_io import decode_scene, encode_scene, scene_content_hash
from .trace import load_trace_bytes, replay_trace

SAMPLE_SIZES = (
    (8, 8, 960, 600, 48.0),
    (12, 10, 1024, 640, 45.0),
    (16, 14, 960, 600, 52.0),
)


@dataclass(frozen=True)
class DatasetBundle:
    seed: int
    sample_files: Mapping[str, bytes]
    baseline_files: Mapping[str, bytes]
    runs: tuple[RunOutput, ...]

    @property
    def sample_manifest(self) -> bytes:
        return self.sample_files["manifest.json"]

    @property
    def baseline_manifest(self) -> bytes:
        return self.baseline_files["manifest.json"]


def _rect(
    x0: float,
    y0: float,
    x1: float,
    y1: float,
) -> list[list[float]]:
    return [[x0, y0], [x1, y0], [x1, y1], [x0, y1]]


def _cell_rect(
    grid: Grid,
    row0: int,
    col0: int,
    row1: int,
    col1: int,
) -> list[list[float]]:
    x0 = grid.origin_x + col0 * grid.cell_size
    y0 = grid.origin_y + row0 * grid.cell_size
    x1 = grid.origin_x + (col1 + 1) * grid.cell_size
    y1 = grid.origin_y + (row1 + 1) * grid.cell_size
    return _rect(x0, y0, x1, y1)


def _aabb_unit(
    unit_id: int,
    name: str,
    center: tuple[float, float],
    half_width: float,
    half_depth: float,
    height: float,
    ground_height: float = 0.0,
) -> Unit:
    cx, cy = center
    minimum = (cx - half_width, cy - half_depth, ground_height)
    maximum = (cx + half_width, cy + half_depth, ground_height + height)
    return Unit(
        unit_id=unit_id,
        name=name,
        bounds_kind="aabb",
        minimum=minimum,
        maximum=maximum,
    )


def _capsule_unit(
    unit_id: int,
    name: str,
    center: tuple[float, float],
    radius: float,
    half_axis: float,
    z_center: float,
    ground_height: float = 0.0,
) -> Unit:
    cx, cy = center
    a = (cx - half_axis, cy, z_center)
    b = (cx + half_axis, cy, z_center)
    minimum = (
        min(a[0], b[0]) - radius,
        min(a[1], b[1]) - radius,
        min(a[2], b[2]) - radius,
    )
    maximum = (
        max(a[0], b[0]) + radius,
        max(a[1], b[1]) + radius,
        max(a[2], b[2]) + radius,
    )
    return Unit(
        unit_id=unit_id,
        name=name,
        bounds_kind="capsule",
        minimum=minimum,
        maximum=maximum,
        capsule_a=a,
        capsule_b=b,
        radius=radius,
    )


def _cover_triangle(
    cover_id: int,
    name: str,
    center: tuple[float, float],
    half_base: float,
    height: float,
    vertical_offset: float = 0.0,
) -> tuple[list[Vec3], Cover]:
    cx, cy = center
    vertices = [
        (cx - half_base, cy, 0.0),
        (cx + half_base, cy, 0.0),
        (cx + vertical_offset, cy + half_base * 0.8, height),
    ]
    return vertices, Cover(cover_id, 0, 1, 2, name)


def _make_scene(index: int, seed: int) -> Scene:
    cols, rows, _, _, _ = SAMPLE_SIZES[index]
    rng = random.Random(seed ^ (0x9E3779B97F4A7C15 * (index + 1)))
    cell_size = 1.0
    origin_x = 0.0
    origin_y = 0.0
    cell_count = cols * rows
    walkable = bytearray((cell_count + 7) // 8)
    for bit in range(cell_count):
        walkable[bit >> 3] |= 1 << (bit & 7)

    blocked = [(0, 3), (0, 4)]
    if cols > 10:
        blocked.append((2, cols - 5))
    for row, col in blocked:
        bit = row * cols + col
        walkable[bit >> 3] &= ~(1 << (bit & 7))
    grid = Grid(
        cols=cols,
        rows=rows,
        cell_size=cell_size,
        origin_x=origin_x,
        origin_y=origin_y,
        walkable=bytes(walkable),
    )

    jitter_x = rng.uniform(-0.12, 0.12)
    jitter_y = rng.uniform(-0.12, 0.12)
    aabb_center = (2.15 + jitter_x, 2.05 + jitter_y)
    capsule_center = (float(cols) - 2.15 + rng.uniform(-0.08, 0.08), 2.15)
    units = [
        _aabb_unit(10, "Scout", aabb_center, 0.38, 0.34, 1.6),
        _capsule_unit(11, "Heavy", capsule_center, 0.40, 0.30, 0.82),
    ]
    if index == 2:
        units.append(
            _aabb_unit(
                12,
                "Support",
                (2.4, float(rows) - 2.7),
                0.34,
                0.30,
                1.4,
            )
        )

    cover_center = (cols * 0.55, rows * 0.50)
    cover_vertices, cover = _cover_triangle(
        100,
        "CenterCrate",
        cover_center,
        0.75,
        1.35,
        vertical_offset=0.10,
    )
    second_center = (cols * 0.40, rows * 0.65)
    second_vertices, second_cover = _cover_triangle(
        101,
        "LowWall",
        second_center,
        0.65,
        0.9,
        vertical_offset=-0.15,
    )
    def shift_indices(cover_value: Cover, base: int) -> Cover:
        return Cover(
            cover_value.cover_id,
            cover_value.i0 + base,
            cover_value.i1 + base,
            cover_value.i2 + base,
            cover_value.name,
        )

    vertices = tuple(cover_vertices + second_vertices)
    covers = (cover, shift_indices(second_cover, 3))
    if index == 2:
        third_center = (cols * 0.70, rows * 0.74)
        third_vertices, third_cover = _cover_triangle(
            102,
            "Pillar",
            third_center,
            0.5,
            1.8,
            vertical_offset=0.0,
        )
        vertices = tuple(list(vertices) + third_vertices)
        covers = tuple(list(covers) + [shift_indices(third_cover, 6)])

    return Scene(
        structure_version=3,
        ground_height=0.0,
        grid=grid,
        vertices=vertices,
        units=tuple(units),
        covers=covers,
    )


def _camera(index: int, scene: Scene) -> Camera:
    cols, rows, width, height, fov = SAMPLE_SIZES[index]
    center = (cols * 0.5, rows * 0.5, 0.45)
    offsets = (
        (-8.5, -10.5, 14.5),
        (-6.5, -8.5, 17.0),
        (8.0, -10.0, 16.0),
    )
    dx, dy, dz = offsets[index]
    return Camera(
        camera_id=f"cam-{index}",
        position=(cols * 0.5 + dx, rows * 0.5 + dy, dz),
        target=center,
        up=(0.0, 0.0, 1.0),
        fov_y_deg=fov,
        near=0.1,
        far=200.0,
        viewport_width=width,
        viewport_height=height,
    )


def _screen(camera: Camera, point: Vec3, fallback: Sequence[float]) -> list[float]:
    sx, sy = camera.project(point)
    if (
        sx < 1.0
        or sy < 1.0
        or sx > camera.viewport_width - 1.0
        or sy > camera.viewport_height - 1.0
    ):
        return [float(fallback[0]), float(fallback[1])]
    return [sx, sy]


def _round_delta(rng: random.Random) -> tuple[float, float, float]:
    return (
        round(rng.uniform(0.28, 0.55), 3),
        round(rng.uniform(0.08, 0.28), 3),
        0.0,
    )


def _unit_top(unit: Unit) -> Vec3:
    center_x = (unit.minimum[0] + unit.maximum[0]) * 0.5
    center_y = (unit.minimum[1] + unit.maximum[1]) * 0.5
    return (center_x, center_y, unit.maximum[2])


def _make_scenario(index: int, seed: int, scene: Scene, camera: Camera) -> dict[str, Any]:
    rng = random.Random((seed + 17) ^ ((index + 3) * 0xBF58476D1CE4E5B9))
    cols, rows, _, _, _ = SAMPLE_SIZES[index]
    grid = scene.grid
    unit_aabb = scene.unit_by_id(10)
    unit_capsule = scene.unit_by_id(11)
    assert unit_aabb is not None and unit_capsule is not None
    cover = scene.cover_by_id(100)
    assert cover is not None
    cover_points = list(scene.triangle(cover))
    cover_target = (
        sum(point[0] for point in cover_points) / 3.0,
        sum(point[1] for point in cover_points) / 3.0,
        sum(point[2] for point in cover_points) / 3.0,
    )
    ground_target = (cols * 0.72, rows * 0.72, scene.ground_height)
    current_version = 1
    events: list[dict[str, Any]] = []

    def add_pick(label: str, target: Vec3, fallback: Sequence[float]) -> None:
        events.append(
            {
                "type": "pick",
                "label": label,
                "screen": _screen(camera, target, fallback),
                "expected_version": current_version,
            }
        )

    add_pick("ground-before-batch", ground_target, (camera.viewport_width * 0.54, camera.viewport_height * 0.58))
    add_pick("scout-before-batch", _unit_top(unit_aabb), (camera.viewport_width * 0.35, camera.viewport_height * 0.42))
    add_pick("heavy-before-batch", _unit_top(unit_capsule), (camera.viewport_width * 0.70, camera.viewport_height * 0.38))
    add_pick("cover-before-batch", cover_target, (camera.viewport_width * 0.52, camera.viewport_height * 0.50))

    valid_polygon = _cell_rect(grid, rows - 3, cols - 3, rows - 2, cols - 2)
    blocked_cell = (0, 3)
    blocked_polygon = _cell_rect(grid, blocked_cell[0], blocked_cell[1], blocked_cell[0], blocked_cell[1])
    unit_polygon = _rect(
        unit_aabb.minimum[0] + 0.02,
        unit_aabb.minimum[1] + 0.02,
        unit_aabb.maximum[0] - 0.02,
        unit_aabb.maximum[1] - 0.02,
    )
    cover_min_x = min(point[0] for point in cover_points)
    cover_min_y = min(point[1] for point in cover_points)
    cover_max_x = max(point[0] for point in cover_points)
    cover_max_y = max(point[1] for point in cover_points)
    cover_polygon = _rect(
        cover_min_x + 0.02,
        cover_min_y + 0.02,
        cover_max_x - 0.02,
        cover_max_y - 0.02,
    )
    for label, polygon in (
        ("placement-valid", valid_polygon),
        ("placement-unwalkable", blocked_polygon),
        ("placement-unit-overlap", unit_polygon),
        ("placement-cover-overlap", cover_polygon),
    ):
        events.append(
            {
                "type": "placement",
                "label": label,
                "vertices": polygon,
                "expected_version": current_version,
            }
        )

    delta_a = _round_delta(rng)
    events.append(
        {
            "type": "batch",
            "label": "move-scout",
            "base_version": current_version,
            "ops": [
                {
                    "op": "translate_unit",
                    "unit_id": 10,
                    "delta": list(delta_a),
                }
            ],
        }
    )
    current_version += 1
    moved_aabb = unit_aabb.translated(delta_a)
    add_pick("scout-after-move", _unit_top(moved_aabb), (camera.viewport_width * 0.38, camera.viewport_height * 0.44))
    add_pick("cover-versioned", cover_target, (camera.viewport_width * 0.52, camera.viewport_height * 0.50))

    events.append(
        {
            "type": "batch",
            "label": "edit-walkability",
            "base_version": current_version,
            "ops": [
                {
                    "op": "set_walkable_cells",
                    "cells": [
                        {"row": 0, "col": 4, "walkable": True},
                        {"row": 2, "col": 4, "walkable": False},
                    ],
                }
            ],
        }
    )
    current_version += 1
    add_pick("ground-after-mask", ground_target, (camera.viewport_width * 0.54, camera.viewport_height * 0.58))

    delta_cover = round(rng.uniform(-0.18, 0.18), 3)
    delta_capsule = (
        round(rng.uniform(0.10, 0.24), 3),
        round(rng.uniform(0.22, 0.42), 3),
        0.0,
    )
    moved_cover_points = [
        (point[0] + delta_cover, point[1], point[2]) for point in cover_points
    ]
    moved_cover_target = (
        sum(point[0] for point in moved_cover_points) / 3.0,
        sum(point[1] for point in moved_cover_points) / 3.0,
        sum(point[2] for point in moved_cover_points) / 3.0,
    )
    events.append(
        {
            "type": "batch",
            "label": "move-cover-and-heavy",
            "base_version": current_version,
            "ops": [
                {
                    "op": "set_cover_vertices",
                    "cover_id": 100,
                    "i0": list(moved_cover_points[0]),
                    "i1": list(moved_cover_points[1]),
                    "i2": list(moved_cover_points[2]),
                },
                {
                    "op": "translate_unit",
                    "unit_id": 11,
                    "delta": list(delta_capsule),
                },
            ],
        }
    )
    current_version += 1
    moved_capsule = unit_capsule.translated(delta_capsule)
    add_pick("cover-after-move", moved_cover_target, (camera.viewport_width * 0.52, camera.viewport_height * 0.50))
    add_pick("heavy-after-move", _unit_top(moved_capsule), (camera.viewport_width * 0.70, camera.viewport_height * 0.40))
    events.append(
        {
            "type": "placement",
            "label": "placement-valid-final-version",
            "vertices": valid_polygon,
            "expected_version": current_version,
        }
    )
    return {
        "schema": 1,
        "sample_id": f"battlefield-{cols}x{rows}-seed-{seed}",
        "seed": seed,
        "scene_structure_version": scene.structure_version,
        "camera": camera.to_dict(),
        "events": events,
    }


def build_dataset(seed: int = 7) -> DatasetBundle:
    """Build every sample and baseline entirely in memory."""

    sample_files: dict[str, bytes] = {}
    baseline_files: dict[str, bytes] = {}
    runs: list[RunOutput] = []
    manifest_samples: list[dict[str, Any]] = []
    baseline_samples: list[dict[str, Any]] = []
    for index, (cols, rows, _, _, _) in enumerate(SAMPLE_SIZES):
        sample_seed = seed * 100_003 + index * 1_009 + 41
        scene = _make_scene(index, sample_seed)
        camera = _camera(index, scene)
        scenario = _make_scenario(index, sample_seed, scene, camera)
        scene_bytes = encode_scene(scene)
        scenario_bytes = canonical_json_bytes(scenario) + b"\n"
        run = run_scenario(scene_bytes, scenario)
        trace = run.trace_bytes
        expected = run.expected_bytes
        loaded = load_trace_bytes(trace)
        replay_trace(loaded, run.store)
        if loaded.chain_head != run.recorder.chain_head:
            fail(
                E_SCENE_INVALID,
                "internal trace chain head mismatch",
                field="trace.chain_head",
            )
        base_name = f"battlefield-{cols}x{rows}-seed-{seed}"
        scene_name = f"{base_name}.tpscene"
        scenario_name = f"{base_name}.scenario.json"
        trace_name = f"{base_name}.trace.jsonl"
        expected_name = f"{base_name}.expected.json"
        sample_files[scene_name] = scene_bytes
        sample_files[scenario_name] = scenario_bytes
        baseline_files[trace_name] = trace
        baseline_files[expected_name] = expected
        manifest_samples.append(
            {
                "sample_id": scenario["sample_id"],
                "grid": [cols, rows],
                "seed": sample_seed,
                "scene_file": scene_name,
                "scenario_file": scenario_name,
                "scene_hash": sha256_bytes(scene_bytes),
                "content_hash": scene_content_hash(scene),
            }
        )
        baseline_samples.append(
            {
                "sample_id": scenario["sample_id"],
                "trace_file": trace_name,
                "expected_file": expected_name,
                "trace_hash": loaded.chain_head,
                "trace_bytes_hash": sha256_bytes(trace),
                "expected_bytes_hash": sha256_bytes(expected),
                "record_count": len(loaded.records),
                "final_version": run.final_version,
                "final_content_hash": run.final_hash,
            }
        )
        runs.append(run)
    sample_manifest = {
        "schema": 1,
        "kind": "tactical-pick-samples",
        "seed": seed,
        "samples": manifest_samples,
    }
    baseline_manifest = {
        "schema": 1,
        "kind": "tactical-pick-baselines",
        "seed": seed,
        "samples": baseline_samples,
    }
    sample_files["manifest.json"] = canonical_json_bytes(sample_manifest) + b"\n"
    baseline_files["manifest.json"] = canonical_json_bytes(baseline_manifest) + b"\n"
    return DatasetBundle(
        seed=seed,
        sample_files=dict(sorted(sample_files.items())),
        baseline_files=dict(sorted(baseline_files.items())),
        runs=tuple(runs),
    )


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, path)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except OSError:
            pass
        raise


def write_dataset(bundle: DatasetBundle, samples_dir: str | Path, baselines_dir: str | Path) -> None:
    samples_path = Path(samples_dir)
    baselines_path = Path(baselines_dir)
    samples_path.mkdir(parents=True, exist_ok=True)
    baselines_path.mkdir(parents=True, exist_ok=True)
    for name, data in bundle.sample_files.items():
        _atomic_write(samples_path / name, data)
    for name, data in bundle.baseline_files.items():
        _atomic_write(baselines_path / name, data)


def verify_dataset(samples_dir: str | Path, baselines_dir: str | Path) -> dict[str, Any]:
    samples_path = Path(samples_dir)
    baselines_path = Path(baselines_dir)
    try:
        manifest_raw = json.loads((samples_path / "manifest.json").read_text("utf-8"))
        baseline_manifest_raw = json.loads(
            (baselines_path / "manifest.json").read_text("utf-8")
        )
    except (OSError, json.JSONDecodeError) as exc:
        fail(
            E_SCENE_INVALID,
            "sample or baseline manifest could not be loaded",
            field="manifest",
            error=str(exc),
        )
    if manifest_raw.get("schema") != 1 or manifest_raw.get("kind") != "tactical-pick-samples":
        fail(
            E_SCENE_INVALID,
            "sample manifest schema is unsupported",
            field="samples.manifest.schema",
        )
    if (
        baseline_manifest_raw.get("schema") != 1
        or baseline_manifest_raw.get("kind") != "tactical-pick-baselines"
    ):
        fail(
            E_SCENE_INVALID,
            "baseline manifest schema is unsupported",
            field="baselines.manifest.schema",
        )
    baseline_by_id = {
        str(entry.get("sample_id")): entry
        for entry in baseline_manifest_raw.get("samples", [])
    }
    reports: list[dict[str, Any]] = []
    for sample in manifest_raw.get("samples", []):
        sample_id = str(sample.get("sample_id"))
        scene_path = samples_path / str(sample["scene_file"])
        scenario_path = samples_path / str(sample["scenario_file"])
        try:
            scene_bytes = scene_path.read_bytes()
            scenario = json.loads(scenario_path.read_text("utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            fail(
                E_SCENE_INVALID,
                "sample input could not be loaded",
                field="samples.sample",
                sample_id=sample_id,
                error=str(exc),
            )
        actual_scene_hash = sha256_bytes(scene_bytes)
        if sample.get("scene_hash") != actual_scene_hash:
            fail(
                E_SCENE_INVALID,
                "sample scene hash does not match its manifest entry",
                field=f"samples.{sample['scene_file']}",
                sample_id=sample_id,
                expected=sample.get("scene_hash"),
                actual=actual_scene_hash,
            )
        baseline_entry = baseline_by_id.get(sample_id)
        if baseline_entry is None:
            fail(
                E_SCENE_INVALID,
                "sample has no baseline manifest entry",
                field="baselines.manifest.samples",
                sample_id=sample_id,
            )
        run = run_scenario(scene_bytes, scenario)
        trace_name = str(baseline_entry.get("trace_file"))
        expected_name = str(baseline_entry.get("expected_file"))
        try:
            baseline_trace_bytes = (baselines_path / trace_name).read_bytes()
            baseline_expected_bytes = (baselines_path / expected_name).read_bytes()
        except OSError as exc:
            fail(
                E_SCENE_INVALID,
                "baseline file could not be loaded",
                field="baselines.file",
                sample_id=sample_id,
                error=str(exc),
            )
        if run.trace_bytes != baseline_trace_bytes:
            fail(
                E_SCENE_INVALID,
                "regenerated trace is not byte-identical to its baseline",
                field=f"baselines.{trace_name}",
                sample_id=sample_id,
                expected_hash=sha256_bytes(baseline_trace_bytes),
                actual_hash=sha256_bytes(run.trace_bytes),
            )
        if run.expected_bytes != baseline_expected_bytes:
            fail(
                E_SCENE_INVALID,
                "regenerated result file is not byte-identical to its baseline",
                field=f"baselines.{expected_name}",
                sample_id=sample_id,
                expected_hash=sha256_bytes(baseline_expected_bytes),
                actual_hash=sha256_bytes(run.expected_bytes),
            )
        if sha256_bytes(baseline_trace_bytes) != baseline_entry.get("trace_bytes_hash"):
            fail(
                E_SCENE_INVALID,
                "baseline trace manifest hash is inconsistent",
                field="baselines.manifest.trace_bytes_hash",
                sample_id=sample_id,
            )
        if sha256_bytes(baseline_expected_bytes) != baseline_entry.get("expected_bytes_hash"):
            fail(
                E_SCENE_INVALID,
                "baseline result manifest hash is inconsistent",
                field="baselines.manifest.expected_bytes_hash",
                sample_id=sample_id,
            )
        loaded = load_trace_bytes(run.trace_bytes)
        if loaded.chain_head != baseline_entry.get("trace_hash"):
            fail(
                E_SCENE_INVALID,
                "baseline trace chain head does not match its manifest",
                field="baselines.manifest.trace_hash",
                sample_id=sample_id,
            )
        if len(loaded.records) != baseline_entry.get("record_count"):
            fail(
                E_SCENE_INVALID,
                "baseline trace record count does not match its manifest",
                field="baselines.manifest.record_count",
                sample_id=sample_id,
            )
        if run.final_version != baseline_entry.get("final_version"):
            fail(
                E_SCENE_INVALID,
                "final region version does not match the baseline manifest",
                field="baselines.manifest.final_version",
                sample_id=sample_id,
            )
        if run.final_hash != baseline_entry.get("final_content_hash"):
            fail(
                E_SCENE_INVALID,
                "final region content hash does not match the baseline manifest",
                field="baselines.manifest.final_content_hash",
                sample_id=sample_id,
            )
        replay = replay_trace(loaded, run.store)
        reports.append(
            {
                "sample_id": sample_id,
                "trace_bytes_hash": sha256_bytes(run.trace_bytes),
                "expected_bytes_hash": sha256_bytes(run.expected_bytes),
                "trace_records": len(loaded.records),
                "replayed_records": replay["replayed_records"],
                "trace_hash": loaded.chain_head,
                "final_version": run.final_version,
                "final_content_hash": run.final_hash,
            }
        )
    return {
        "ok": True,
        "samples_verified": len(reports),
        "samples": reports,
    }


def load_scenario_file(path: str | Path) -> dict[str, Any]:
    try:
        value = json.loads(Path(path).read_text("utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(
            E_SCENE_INVALID,
            "scenario file could not be loaded",
            field="scenario.file",
            source=str(path),
            error=str(exc),
        )
    if not isinstance(value, dict):
        fail(E_SCENE_INVALID, "scenario root must be an object", field="scenario")
    return value


def load_sample_scene(path: str | Path) -> Scene:
    return decode_scene(Path(path).read_bytes(), source=str(path))
