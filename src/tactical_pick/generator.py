"""Deterministic local scene, query and baseline generator."""

from __future__ import annotations

import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Sequence

from .camera import Camera
from .editor import SceneEditor
from .errors import fail
from .math3d import Vec3
from .query import PickRequest, Picker
from .region import PlacementRequest, judge_snapshot
from .scene import CoverTriangle, SceneData, Unit, make_passable_mask, read_scene, write_scene
from .snapshot import SceneManager, SceneSnapshot
from .util import f32, sha256_hex, write_json

SAMPLE_SCHEMA = 1


@dataclass(frozen=True)
class CaseSpec:
    name: str
    seed_offset: int
    grid_width: int
    grid_height: int
    cell_size: float


CASES = (
    CaseSpec("grid_small", 101, 12, 10, 1.0),
    CaseSpec("grid_medium", 211, 18, 14, 1.25),
    CaseSpec("grid_large", 307, 24, 18, 0.8),
)


def _q3(value: Sequence[float]) -> Vec3:
    return (f32(value[0]), f32(value[1]), f32(value[2]))


def _center(bounds: tuple[Vec3, Vec3]) -> Vec3:
    lower, upper = bounds
    return tuple((lower[index] + upper[index]) * 0.5 for index in range(3))  # type: ignore[return-value]


def _find_unit(scene: SceneData, unit_id: int) -> Unit:
    for unit in scene.units:
        if unit.unit_id == unit_id:
            return unit
    fail("E_INTERNAL", f"sample generator lost unit {unit_id}", "generator.unit")


def _build_initial_scene(spec: CaseSpec, seed: int) -> SceneData:
    cell = f32(spec.cell_size)
    ground = 0.0
    width, height = spec.grid_width, spec.grid_height
    blocked = {(width - 3, height - 2), (2, height - 3)}
    mask = make_passable_mask(width, height, blocked)

    x1 = f32((3.2 + (seed % 3) * 0.06) * cell)
    z1 = f32((3.0 + ((seed >> 2) % 3) * 0.05) * cell)
    unit1 = Unit.box(
        101,
        _q3((x1 - 0.65 * cell, ground + 0.05, z1 - 0.55 * cell)),
        _q3((x1 + 0.65 * cell, ground + 1.85, z1 + 0.55 * cell)),
    )

    x2 = f32(width * 0.66 * cell)
    z2 = f32(height * 0.60 * cell)
    unit2 = Unit.capsule(
        102,
        _q3((x2, ground + 0.22, z2)),
        _q3((x2, ground + 1.60, z2)),
        f32(0.38 * cell),
    )

    x3 = f32(width * 0.21 * cell)
    z3 = f32(height * 0.76 * cell)
    unit3 = Unit.box(
        103,
        _q3((x3 - 0.55 * cell, ground + 0.04, z3 - 0.5 * cell)),
        _q3((x3 + 0.55 * cell, ground + 1.55, z3 + 0.5 * cell)),
    )

    cx = f32(width * 0.52 * cell)
    cz = f32(height * 0.34 * cell)
    cover900 = CoverTriangle(
        900,
        _q3((cx - 0.75 * cell, ground, cz)),
        _q3((cx + 0.75 * cell, ground, cz)),
        _q3((cx, ground + 1.35, cz)),
    )
    cover901 = CoverTriangle(
        901,
        _q3((width * 0.76 * cell, ground, height * 0.72 * cell)),
        _q3((width * 0.84 * cell, ground, height * 0.69 * cell)),
        _q3((width * 0.79 * cell, ground + 0.9, height * 0.73 * cell)),
    )

    return SceneData(
        version=1,
        grid_width=width,
        grid_height=height,
        cell_size=cell,
        ground_y=ground,
        units=(unit1, unit2, unit3),
        cover_triangles=(cover900, cover901),
        passable_mask=mask,
    )


def _sample_batches(spec: CaseSpec, seed: int) -> tuple[tuple[dict[str, Any], ...], ...]:
    cell = f32(spec.cell_size)
    new_cover_x = f32(spec.grid_width * 0.70 * cell)
    new_cover_z = f32(spec.grid_height * 0.22 * cell)
    return (
        (
            {"op": "move_unit", "unit_id": 101, "delta": [f32(0.45 * cell), 0.0, f32(0.30 * cell)]},
            {"op": "set_walkable", "x": 0, "z": 0, "walkable": False},
        ),
        (
            {"op": "move_unit", "unit_id": 102, "delta": [f32(-0.25 * cell), 0.0, f32(0.20 * cell)]},
            {"op": "set_walkable", "x": spec.grid_width - 2, "z": spec.grid_height - 2, "walkable": False},
        ),
        (
            {
                "op": "add_cover_triangle",
                "cover_id": 950 + (seed % 10),
                "a": [new_cover_x - 0.6 * cell, 0.0, new_cover_z],
                "b": [new_cover_x + 0.6 * cell, 0.0, new_cover_z],
                "c": [new_cover_x, 1.1, new_cover_z],
            },
            {"op": "set_walkable", "x": 0, "z": 0, "walkable": True},
            {"op": "move_unit", "unit_id": 103, "delta": [f32(0.20 * cell), 0.0, f32(-0.15 * cell)]},
        ),
    )


def _camera_for_target(target: Vec3, offset: Sequence[float], variant: int) -> Camera:
    eye = (target[0] + offset[0], target[1] + offset[1], target[2] + offset[2])
    fx = 760.0 + variant * 25.0
    fy = 780.0 + variant * 20.0
    return Camera.look_at(eye, target, width=960.0, height=540.0, fx=fx, fy=fy)


def _pick_request(camera: Camera, version: int) -> dict[str, Any]:
    return {
        "screen": [camera.width * 0.5, camera.height * 0.5],
        "camera": camera.to_dict(),
        "requested_version": version,
    }


def _pick_definitions(snapshot: SceneSnapshot, spec: CaseSpec) -> list[dict[str, Any]]:
    scene = snapshot.scene
    cell = scene.cell_size
    definitions: list[dict[str, Any]] = []

    unit1 = _find_unit(scene, 101)
    target = _center(unit1.bounds())
    camera = _camera_for_target(target, (-0.8 * cell, 6.0 * cell, 5.2 * cell), 0)
    definitions.append({"name": "pick_aabb_unit", "request": _pick_request(camera, scene.version)})

    unit2 = _find_unit(scene, 102)
    capsule_target = (
        (unit2.capsule_a[0] + unit2.capsule_b[0]) * 0.5,
        (unit2.capsule_a[1] + unit2.capsule_b[1]) * 0.5,
        (unit2.capsule_a[2] + unit2.capsule_b[2]) * 0.5,
    )
    camera = _camera_for_target(capsule_target, (1.4 * cell, 5.4 * cell, 4.8 * cell), 1)
    definitions.append({"name": "pick_capsule_unit", "request": _pick_request(camera, scene.version)})

    cover = scene.cover_triangles[0]
    camera = _camera_for_target(cover.centroid(), (0.6 * cell, 4.6 * cell, 4.4 * cell), 2)
    definitions.append({"name": "pick_cover_primary", "request": _pick_request(camera, scene.version)})

    ground_target = (f32(1.35 * cell), scene.ground_y, f32(1.25 * cell))
    camera = _camera_for_target(ground_target, (0.4 * cell, 7.0 * cell, 6.6 * cell), 3)
    definitions.append({"name": "pick_ground_empty", "request": _pick_request(camera, scene.version)})

    if len(scene.cover_triangles) > 2:
        added = scene.cover_triangles[-1]
        camera = _camera_for_target(added.centroid(), (-0.5 * cell, 4.2 * cell, 4.0 * cell), 4)
        definitions.append({"name": "pick_cover_added", "request": _pick_request(camera, scene.version)})
    return definitions


def _rectangle(lower: tuple[float, float], upper: tuple[float, float]) -> tuple[tuple[float, float], ...]:
    return (
        (f32(lower[0]), f32(lower[1])),
        (f32(upper[0]), f32(lower[1])),
        (f32(upper[0]), f32(upper[1])),
        (f32(lower[0]), f32(upper[1])),
    )


def _blocked_region(snapshot: SceneSnapshot) -> tuple[tuple[float, float], ...] | None:
    scene = snapshot.scene
    cell = scene.cell_size
    margin = 0.12 * cell
    for z in range(scene.grid_height):
        for x in range(scene.grid_width):
            if scene.is_walkable(x, z):
                continue
            return _rectangle((x * cell + margin, z * cell + margin), ((x + 1) * cell - margin, (z + 1) * cell - margin))
    return None


def _legal_region(snapshot: SceneSnapshot) -> tuple[tuple[float, float], ...] | None:
    scene = snapshot.scene
    cell = scene.cell_size
    margin = 0.18 * cell
    for z in range(scene.grid_height):
        for x in range(scene.grid_width):
            polygon = _rectangle((x * cell + margin, z * cell + margin), ((x + 1) * cell - margin, (z + 1) * cell - margin))
            verdict = judge_snapshot(snapshot, PlacementRequest(polygon, requested_version=scene.version))
            if verdict.valid:
                return polygon
    return None


def _unit_region(snapshot: SceneSnapshot) -> tuple[tuple[float, float], ...]:
    unit = _find_unit(snapshot.scene, 101)
    lower, upper = unit.bounds()
    return _rectangle((lower[0] - 0.08, lower[2] - 0.08), (upper[0] + 0.08, upper[2] + 0.08))


def _cover_region(snapshot: SceneSnapshot) -> tuple[tuple[float, float], ...]:
    cover = snapshot.scene.cover_triangles[0]
    xs = (cover.a[0], cover.b[0], cover.c[0])
    zs = (cover.a[2], cover.b[2], cover.c[2])
    return _rectangle((min(xs) - 0.08, min(zs) - 0.08), (max(xs) + 0.08, max(zs) + 0.08))


def _region_definitions(snapshot: SceneSnapshot) -> list[dict[str, Any]]:
    definitions: list[dict[str, Any]] = []
    legal = _legal_region(snapshot)
    if legal is not None:
        definitions.append(
            {
                "name": "region_legal",
                "request": {
                    "polygon": [list(point) for point in legal],
                    "requested_version": snapshot.version,
                    "height": 2.0,
                },
            }
        )
    blocked = _blocked_region(snapshot)
    if blocked is not None:
        definitions.append(
            {
                "name": "region_blocked_cell",
                "request": {
                    "polygon": [list(point) for point in blocked],
                    "requested_version": snapshot.version,
                    "height": 2.0,
                },
            }
        )
    definitions.append(
        {
            "name": "region_unit_overlap",
            "request": {
                "polygon": [list(point) for point in _unit_region(snapshot)],
                "requested_version": snapshot.version,
                "height": 2.0,
            },
        }
    )
    definitions.append(
        {
            "name": "region_cover_clip",
            "request": {
                "polygon": [list(point) for point in _cover_region(snapshot)],
                "requested_version": snapshot.version,
                "height": 2.0,
            },
        }
    )
    return definitions


def _file_hash(path: Path) -> str:
    return sha256_hex(path.read_bytes())


def generate_samples(root: str | Path, seed: int = 20261122) -> dict[str, Any]:
    root_path = Path(root)
    target = root_path / f"seed_{seed}"
    if target.exists():
        shutil.rmtree(target)
    target.mkdir(parents=True)

    case_documents: list[dict[str, Any]] = []
    for spec in CASES:
        case_seed = seed + spec.seed_offset
        case_dir = target / spec.name
        case_dir.mkdir(parents=True)
        initial = _build_initial_scene(spec, case_seed)
        batches = _sample_batches(spec, case_seed)
        manager = SceneManager(initial)
        editor = SceneEditor(manager)
        picker = Picker(manager)

        input_versions: list[dict[str, Any]] = []
        baseline_versions: list[dict[str, Any]] = []
        scene_files: list[dict[str, Any]] = []
        for state_index in range(len(batches) + 1):
            snapshot = manager.current
            version = snapshot.version
            filename = f"scene_v{version:04d}.tacbin"
            scene_path = case_dir / filename
            write_scene(scene_path, snapshot.scene)
            decoded = read_scene(scene_path)
            if decoded.compute_hash() != snapshot.content_hash:
                fail(
                    "E_INTERNAL",
                    "binary scene round-trip changed the sample content hash",
                    "generator.binary_roundtrip",
                    first_conflict={"version": version},
                )
            scene_files.append(
                {"version": version, "file": filename, "hash": snapshot.content_hash, "sha256": _file_hash(scene_path)}
            )

            pick_defs = _pick_definitions(snapshot, spec)
            region_defs = _region_definitions(snapshot)
            expected_picks = []
            for definition in pick_defs:
                request_doc = definition["request"]
                request = PickRequest(
                    screen=(float(request_doc["screen"][0]), float(request_doc["screen"][1])),
                    camera=Camera.from_dict(request_doc["camera"]),
                    requested_version=request_doc["requested_version"],
                )
                result = picker.pick(request)
                expected_picks.append({"name": definition["name"], "expected": result.to_dict()})

            expected_regions = []
            for definition in region_defs:
                request_doc = definition["request"]
                request = PlacementRequest(
                    polygon=tuple((float(point[0]), float(point[1])) for point in request_doc["polygon"]),
                    requested_version=request_doc["requested_version"],
                    height=float(request_doc["height"]),
                )
                verdict = judge_snapshot(snapshot, request)
                expected_regions.append({"name": definition["name"], "expected": verdict.to_dict()})

            input_versions.append(
                {
                    "version": version,
                    "hash": snapshot.content_hash,
                    "picks": pick_defs,
                    "regions": region_defs,
                }
            )
            baseline_versions.append(
                {
                    "version": version,
                    "hash": snapshot.content_hash,
                    "picks": expected_picks,
                    "regions": expected_regions,
                }
            )
            if state_index < len(batches):
                editor.apply_batch(batches[state_index])

        case_document = {
            "schema": SAMPLE_SCHEMA,
            "name": spec.name,
            "seed": case_seed,
            "grid": [spec.grid_width, spec.grid_height],
            "cell_size": initial.cell_size,
            "initial_version": 1,
            "initial_file": scene_files[0]["file"],
            "batches": [list(batch) for batch in batches],
            "scene_files": scene_files,
            "queries": input_versions,
        }
        baseline_document = {
            "schema": SAMPLE_SCHEMA,
            "name": spec.name,
            "seed": case_seed,
            "versions": baseline_versions,
            "summary": {
                "versions": len(baseline_versions),
                "picks": sum(len(item["picks"]) for item in baseline_versions),
                "regions": sum(len(item["regions"]) for item in baseline_versions),
                "legal_regions": sum(
                    1
                    for version in baseline_versions
                    for region in version["regions"]
                    if region["expected"]["valid"]
                ),
            },
        }
        write_json(case_dir / "case.json", case_document)
        write_json(case_dir / "baseline.json", baseline_document)
        case_documents.append(
            {
                "name": spec.name,
                "directory": spec.name,
                "case_file": f"{spec.name}/case.json",
                "baseline_file": f"{spec.name}/baseline.json",
                "case_sha256": _file_hash(case_dir / "case.json"),
                "baseline_sha256": _file_hash(case_dir / "baseline.json"),
                "scene_files": scene_files,
            }
        )

    manifest = {
        "schema": SAMPLE_SCHEMA,
        "seed": seed,
        "cases": case_documents,
    }
    write_json(target / "manifest.json", manifest)
    return manifest


def load_manifest(samples_root: str | Path, seed: int = 20261122) -> tuple[Path, dict[str, Any]]:
    import json

    target = Path(samples_root) / f"seed_{seed}"
    manifest_path = target / "manifest.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail("E_SCENE_INVALID", f"cannot load sample manifest: {exc}", "samples.manifest")
    return target, manifest

