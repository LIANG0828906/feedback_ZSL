from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .camera import Camera
from .errors import E_SNAPSHOT_INVALID, TerrainError
from .heightfield import HeightField
from .io_utils import atomic_write_bytes, canonical_json_bytes
from .material import MaterialMap
from .render import RenderResult
from .terrain import Terrain


SNAPSHOT_FORMAT = "terrainviz-render-snapshot"
SNAPSHOT_VERSION = 1


@dataclass(frozen=True)
class Snapshot:
    content: dict[str, Any]
    digest: str


def _snapshot_hash(content: dict[str, Any]) -> str:
    payload = {key: value for key, value in content.items() if key != "content_hash"}
    return hashlib.sha256(canonical_json_bytes(payload)).hexdigest()


def build_snapshot(
    result: RenderResult,
    terrain: Terrain,
    *,
    heightfield_name: str | None = None,
    material_name: str | None = None,
) -> dict[str, Any]:
    report = result.report
    levels = [
        {
            "block_id": item["block_id"],
            "level": item["level"],
            "stride": item["stride"],
            "error_upper_bound": item["error_upper_bound"],
            "projected_error_pixels": item["projected_error_pixels"],
            "budget_usage": item["budget_usage"],
        }
        for item in sorted(report["levels"], key=lambda value: value["block_id"])
    ]
    content: dict[str, Any] = {
        "format": SNAPSHOT_FORMAT,
        "version": SNAPSHOT_VERSION,
        "heightfield": {
            "name": heightfield_name,
            "source_digest": terrain.heightfield.source_digest,
            "height_digest": terrain.heightfield.height_digest,
            "width": terrain.width,
            "height": terrain.height,
            "spacing": terrain.spacing,
            "quantization_step": terrain.heightfield.quantization_step,
            "valid_range": [
                terrain.heightfield.valid_min,
                terrain.heightfield.valid_max,
            ],
        },
        "material": {
            "name": material_name,
            "version": terrain.material.material_version
            if terrain.material is not None
            else None,
        },
        "view": {
            "camera": report["camera"]["eye"],
            "target": report["camera"]["target"],
            "up": report["camera"]["up"],
            "view_matrix": report["camera"]["view_matrix"],
            "camera_signature": result.decision.camera.signature(),
        },
        "screen_error_threshold": report["screen_error_threshold"],
        "seam_tolerance": report["seam_tolerance"],
        "render_config": report["render_config"],
        "tree_build": report["tree_build"],
        "block_levels": levels,
        "material_version": terrain.material.material_version
        if terrain.material is not None
        else None,
        "normal_list": report["normals"]["normal_list"],
        "normal_reused_blocks": report["normals"]["reused_blocks"],
        "normal_recomputed_blocks": report["normals"]["recomputed_blocks"],
        "error_summary": report["error_summary"],
        "seam_summary": report["seams"]["summary"],
        "buffers": report["buffers"],
        "pixel_diff": report["pixel_diff"],
    }
    content["content_hash"] = _snapshot_hash(content)
    return content


def save_snapshot(
    path: str | Path,
    result: RenderResult,
    terrain: Terrain,
    *,
    heightfield_name: str | None = None,
    material_name: str | None = None,
) -> Snapshot:
    content = build_snapshot(
        result,
        terrain,
        heightfield_name=heightfield_name,
        material_name=material_name,
    )
    atomic_write_bytes(path, canonical_json_bytes(content) + b"\n")
    return Snapshot(content=content, digest=content["content_hash"])


def _validate_snapshot_content(content: Any) -> dict[str, Any]:
    if not isinstance(content, dict):
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot root must be a JSON object")
    required = {
        "format",
        "version",
        "heightfield",
        "material",
        "view",
        "screen_error_threshold",
        "seam_tolerance",
        "render_config",
        "tree_build",
        "block_levels",
        "material_version",
        "normal_list",
        "error_summary",
        "seam_summary",
        "buffers",
        "content_hash",
    }
    missing = sorted(required - set(content))
    if missing:
        raise TerrainError(
            E_SNAPSHOT_INVALID, "snapshot is missing required fields", missing=missing
        )
    if content["format"] != SNAPSHOT_FORMAT:
        raise TerrainError(
            E_SNAPSHOT_INVALID, "unexpected snapshot format", format=content["format"]
        )
    if content["version"] != SNAPSHOT_VERSION:
        raise TerrainError(
            E_SNAPSHOT_INVALID,
            "unsupported snapshot version",
            version=content["version"],
            supported=SNAPSHOT_VERSION,
        )
    expected_hash = _snapshot_hash(content)
    if not isinstance(content["content_hash"], str) or content["content_hash"] != expected_hash:
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot content hash mismatch")
    heightfield = content["heightfield"]
    if not isinstance(heightfield, dict):
        raise TerrainError(E_SNAPSHOT_INVALID, "heightfield metadata must be an object")
    missing_heightfield = sorted(
        {
            "name",
            "source_digest",
            "height_digest",
            "width",
            "height",
            "spacing",
            "quantization_step",
            "valid_range",
        }
        - set(heightfield)
    )
    if missing_heightfield:
        raise TerrainError(
            E_SNAPSHOT_INVALID,
            "snapshot heightfield metadata is incomplete",
            missing=missing_heightfield,
        )
    tree_build = content["tree_build"]
    if not isinstance(tree_build, dict) or "block_size" not in tree_build:
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot tree_build metadata is incomplete")
    render_config = content["render_config"]
    if not isinstance(render_config, dict) or render_config.get("precision") not in (
        "float32",
        "float64",
    ):
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot render precision is invalid")
    if not isinstance(content["view"], dict):
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot view must be an object")
    missing_view = sorted(
        {"camera", "target", "up", "view_matrix", "camera_signature"} - set(content["view"])
    )
    if missing_view:
        raise TerrainError(
            E_SNAPSHOT_INVALID, "snapshot view metadata is incomplete", missing=missing_view
        )
    if not isinstance(content["block_levels"], list):
        raise TerrainError(E_SNAPSHOT_INVALID, "block_levels must be a list")
    for item in content["block_levels"]:
        required_level_fields = {
            "block_id",
            "level",
            "stride",
            "error_upper_bound",
            "projected_error_pixels",
            "budget_usage",
        }
        if not isinstance(item, dict) or required_level_fields - set(item):
            raise TerrainError(
                E_SNAPSHOT_INVALID,
                "each block-level entry is incomplete",
            )
    if not isinstance(content["normal_list"], list):
        raise TerrainError(E_SNAPSHOT_INVALID, "normal_list must be a list")
    for item in content["normal_list"]:
        if not isinstance(item, dict) or {"block_id", "digest", "count"} - set(item):
            raise TerrainError(E_SNAPSHOT_INVALID, "normal_list entry is incomplete")
    if not isinstance(content["error_summary"], dict):
        raise TerrainError(E_SNAPSHOT_INVALID, "error_summary must be an object")
    if not isinstance(content["seam_summary"], dict):
        raise TerrainError(E_SNAPSHOT_INVALID, "seam_summary must be an object")
    buffers = content["buffers"]
    if not isinstance(buffers, dict) or {
        "color_sha256",
        "depth_sha256",
        "error_heatmap_sha256",
    } - set(buffers):
        raise TerrainError(E_SNAPSHOT_INVALID, "buffer hashes are incomplete")
    return content


def load_snapshot(path: str | Path) -> Snapshot:
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise TerrainError(
            E_SNAPSHOT_INVALID, "cannot read snapshot", path=str(path), error=str(exc)
        ) from exc
    try:
        content = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise TerrainError(E_SNAPSHOT_INVALID, "snapshot is not valid UTF-8 JSON") from exc
    validated = _validate_snapshot_content(content)
    return Snapshot(content=validated, digest=validated["content_hash"])


def _level_map(value: dict[str, Any] | list[dict[str, Any]] | None) -> dict[str, int]:
    if value is None:
        return {}
    if isinstance(value, dict):
        result: dict[str, int] = {}
        for key, item in value.items():
            if isinstance(item, dict):
                result[str(key)] = int(item["level"])
            else:
                result[str(key)] = int(item)
        return result
    result = {}
    for item in value:
        result[str(item["block_id"])] = int(item["level"])
    return result


def snapshot_changes(
    snapshot: Snapshot,
    camera: Camera,
    levels: dict[str, Any] | list[dict[str, Any]] | None = None,
    material_version: str | None = None,
    screen_error_threshold: float | None = None,
) -> dict[str, Any]:
    content = snapshot.content
    saved_view = content.get("view", {}).get("view_matrix")
    current_view = camera.view_matrix().round(12).tolist()
    view_changed = saved_view != current_view
    saved_levels = _level_map(content.get("block_levels"))
    current_levels = _level_map(levels)
    levels_changed = saved_levels != current_levels
    material_changed = content.get("material_version") != material_version
    threshold_changed = (
        screen_error_threshold is not None
        and content.get("screen_error_threshold") != screen_error_threshold
    )
    reasons: list[str] = []
    if view_changed:
        reasons.append("view")
    if levels_changed:
        reasons.append("levels")
    if material_changed:
        reasons.append("material")
    if threshold_changed:
        reasons.append("threshold")
    return {
        "view_changed": view_changed,
        "levels_changed": levels_changed,
        "material_changed": material_changed,
        "threshold_changed": threshold_changed,
        "requires_rerender": bool(reasons),
        "reasons": reasons,
        "snapshot_digest": snapshot.digest,
    }


def recover_terrain_from_disk(
    heightfield_path: str | Path,
    snapshot_path: str | Path,
    material_path: str | Path | None = None,
) -> tuple[Terrain, Snapshot]:
    """Rebuild a terrain using only committed snapshot metadata and source files."""
    snapshot = load_snapshot(snapshot_path)
    heightfield = HeightField.load(heightfield_path)
    expected_source_digest = snapshot.content["heightfield"].get("source_digest")
    if expected_source_digest and expected_source_digest != heightfield.source_digest:
        raise TerrainError(
            E_SNAPSHOT_INVALID,
            "snapshot and heightfield file digest do not match",
            snapshot_source_digest=expected_source_digest,
            heightfield_source_digest=heightfield.source_digest,
        )
    expected_height_digest = snapshot.content["heightfield"].get("height_digest")
    if expected_height_digest != heightfield.height_digest:
        raise TerrainError(
            E_SNAPSHOT_INVALID,
            "snapshot and heightfield content do not match",
            snapshot_height_digest=expected_height_digest,
            heightfield_digest=heightfield.height_digest,
        )
    material = MaterialMap.load(material_path) if material_path is not None else None
    expected_material = snapshot.content.get("material_version")
    if material is not None and expected_material != material.material_version:
        raise TerrainError(
            E_SNAPSHOT_INVALID,
            "snapshot and material content do not match",
            snapshot_material_version=expected_material,
            material_version=material.material_version,
        )
    tree_build = snapshot.content.get("tree_build")
    if not isinstance(tree_build, dict) or "block_size" not in tree_build:
        raise TerrainError(
            E_SNAPSHOT_INVALID, "snapshot lacks block-size recovery metadata"
        )
    block_size = int(tree_build["block_size"])
    spacing = float(snapshot.content["heightfield"].get("spacing", heightfield.spacing))
    terrain = Terrain(heightfield, material, block_size=block_size)
    if spacing != terrain.spacing:
        terrain.rebuild(spacing=spacing, reason="snapshot-recovery")
    return terrain, snapshot

