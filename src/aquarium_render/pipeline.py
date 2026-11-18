"""Resumable orchestration for block rendering and report generation."""

from __future__ import annotations

import io
import os
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .canonical import canonical_bytes, sha256_bytes
from .errors import SnapshotResetRequired
from .particles import (
    ParticleTable,
    build_particle_table,
    particle_tables_fingerprint,
)
from .png import png_rgba_bytes, write_bytes_atomic, write_png_gray
from .projection import build_shadow_map
from .render import Renderer
from .scene import RENDERER_REVISION, Scene
from .snapshot import (
    CompletedBlock,
    SnapshotState,
    color_summary,
    load_snapshot,
    new_state,
    state_report,
    write_snapshot,
)
from .verify import compare_run_directories


@dataclass
class PipelineResult:
    status: str
    exit_code: int
    output_dir: Path
    snapshot: SnapshotState
    diffs: list[dict[str, Any]]


def _write_json(path: Path, value: Any) -> str:
    data = canonical_bytes(value)
    write_bytes_atomic(path, data)
    return sha256_bytes(data)


def _build_tables(scene: Scene) -> dict[str, ParticleTable]:
    return {
        component.component_id: build_particle_table(
            component.particle_seed,
            component.component_id,
            component.particle_resolution,
        )
        for component in scene.water
    }


def _write_particle_tables(directory: Path, tables: dict[str, ParticleTable]) -> dict[str, str]:
    directory.mkdir(parents=True, exist_ok=True)
    hashes: dict[str, str] = {}
    for component_id, table in sorted(tables.items()):
        path = directory / f"{component_id}.json"
        write_bytes_atomic(path, canonical_bytes(table.to_dict()))
        hashes[component_id] = sha256_bytes(path.read_bytes())
    return hashes


def _clear_rendered_outputs(directory: Path) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    patterns = (
        "angle_*_rgba.png",
        "angle_*_ownership.png",
        "angle_*_preview.png",
        "angle_*_rgba.npy",
        "angle_*_ownership.npy",
        "shadow_*.png",
        "report.json",
        "run_manifest.json",
        "recovery_report.json",
        "reference_diff.json",
    )
    for pattern in patterns:
        for path in directory.glob(pattern):
            if path.is_file():
                path.unlink()


def _ownership_image(scene: Scene, ownership: np.ndarray) -> np.ndarray:
    palette = np.zeros((max(scene.ownership_legend) + 1, 4), dtype=np.uint8)
    for index, entry in scene.ownership_legend.items():
        palette[index] = np.asarray(entry["color"], dtype=np.uint8)
    return palette[ownership.astype(np.int64)]


def _np_bytes(array: np.ndarray) -> bytes:
    buffer = io.BytesIO()
    np.save(buffer, array, allow_pickle=False)
    return buffer.getvalue()


def _preview_image(rgba: np.ndarray) -> np.ndarray:
    """Opaque art-review preview; the technical RGBA remains untouched."""

    height, width = rgba.shape[:2]
    premultiplied = rgba.astype(np.float64) / 255.0
    gradient = np.linspace(0.62, 0.34, height, dtype=np.float64)[:, None, None]
    background = np.empty((height, width, 3), dtype=np.float64)
    background[:, :, 0] = gradient[:, :, 0] * 0.76
    background[:, :, 1] = gradient[:, :, 0] * 0.90
    background[:, :, 2] = gradient[:, :, 0]
    base = np.array([0.018, 0.034, 0.055], dtype=np.float64)
    composed = premultiplied[:, :, :3] + (1.0 - premultiplied[:, :, 3:4]) * (
        background + base[None, None, :]
    )
    preview = np.empty((height, width, 4), dtype=np.uint8)
    preview[:, :, :3] = np.clip(np.rint(composed * 255.0), 0, 255).astype(np.uint8)
    preview[:, :, 3] = 255
    return preview


def _assemble_outputs(
    scene: Scene,
    state: SnapshotState,
    directory: Path,
) -> tuple[dict[str, Any], dict[str, str]]:
    by_angle: dict[int, list[CompletedBlock]] = {}
    for block in state.completed:
        by_angle.setdefault(block.angle_index, []).append(block)
    angle_hashes: dict[str, Any] = {}
    output_hashes: dict[str, str] = {}
    for angle_index in range(len(scene.camera.angles())):
        blocks = by_angle.get(angle_index, [])
        if len(blocks) != state.blocks_per_angle:
            continue
        rgba = np.zeros((scene.render.height, scene.render.width, 4), dtype=np.uint8)
        ownership = np.zeros((scene.render.height, scene.render.width), dtype=np.uint16)
        for block in blocks:
            x0, y0, x1, y1 = _block_rect(scene, block.block_id)
            rgba[y0:y1, x0:x1] = block.rgba
            ownership[y0:y1, x0:x1] = block.ownership
        ownership_rgba = _ownership_image(scene, ownership)
        preview_rgba = _preview_image(rgba)
        rgba_name = f"angle_{angle_index:04d}_rgba.png"
        ownership_name = f"angle_{angle_index:04d}_ownership.png"
        preview_name = f"angle_{angle_index:04d}_preview.png"
        rgba_data = png_rgba_bytes(rgba)
        ownership_data = png_rgba_bytes(ownership_rgba)
        preview_data = png_rgba_bytes(preview_rgba)
        write_bytes_atomic(directory / rgba_name, rgba_data)
        write_bytes_atomic(directory / ownership_name, ownership_data)
        write_bytes_atomic(directory / preview_name, preview_data)
        write_bytes_atomic(directory / f"angle_{angle_index:04d}_rgba.npy", _np_bytes(rgba))
        write_bytes_atomic(
            directory / f"angle_{angle_index:04d}_ownership.npy", _np_bytes(ownership)
        )
        rgba_sha = sha256_bytes(rgba_data)
        ownership_sha = sha256_bytes(ownership_data)
        angle_hashes[str(angle_index)] = {
            "angle_deg": round(float(scene.camera.angles()[angle_index]), 8),
            "rgba_png_sha256": rgba_sha,
            "ownership_png_sha256": ownership_sha,
            "preview_png_sha256": sha256_bytes(preview_data),
            "rgba_pixels_sha256": sha256_bytes(rgba.tobytes()),
            "ownership_pixels_sha256": sha256_bytes(ownership.astype("<u2").tobytes()),
        }
        output_hashes[rgba_name] = rgba_sha
        output_hashes[ownership_name] = ownership_sha
        output_hashes[preview_name] = sha256_bytes(preview_data)
        output_hashes[f"angle_{angle_index:04d}_rgba.npy"] = sha256_bytes(
            (directory / f"angle_{angle_index:04d}_rgba.npy").read_bytes()
        )
        output_hashes[f"angle_{angle_index:04d}_ownership.npy"] = sha256_bytes(
            (directory / f"angle_{angle_index:04d}_ownership.npy").read_bytes()
        )
    return angle_hashes, output_hashes


def _block_rect(scene: Scene, block_id: int) -> tuple[int, int, int, int]:
    render = scene.render
    by, bx = divmod(block_id, render.blocks_x)
    x0 = bx * render.block_width
    y0 = by * render.block_height
    return x0, y0, min(x0 + render.block_width, render.width), min(
        y0 + render.block_height, render.height
    )


def _layer_contribution_report(
    scene: Scene,
    state: SnapshotState,
) -> list[dict[str, Any]]:
    report = []
    for component in scene.components:
        values = state.layer_stats.get(component.component_id, {"transmission": 0.0, "scattering": 0.0})
        transmission = float(values.get("transmission", 0.0))
        scattering = float(values.get("scattering", 0.0))
        total = transmission + scattering
        radii = np.hypot(component.vertices[:, 0], component.vertices[:, 1])
        geometry = {
            "radial_span": [round(float(radii.min()), 8), round(float(radii.max()), 8)],
            "z_span": [
                round(float(component.vertices[:, 2].min()), 8),
                round(float(component.vertices[:, 2].max()), 8),
            ],
        }
        if component.kind == "glass":
            geometry.update(
                {
                    "thickness": component.thickness,
                    "transmission": component.transmission,
                    "ior": component.ior,
                }
            )
        else:
            geometry.update(
                {
                    "absorption_coefficient": [
                        round(float(value), 8) for value in component.absorption
                    ],
                    "particle_seed": component.particle_seed,
                    "particle_resolution": list(component.particle_resolution),
                }
            )
        report.append(
            {
                "component_id": component.component_id,
                "kind": component.kind,
                "layer_index": component.layer_index,
                "geometry": geometry,
                "transmission_contribution": round(transmission, 8),
                "scattering_contribution": round(scattering, 8),
                "transmission_ratio": round(transmission / total, 8) if total > 0.0 else 0.0,
                "scattering_ratio": round(scattering / total, 8) if total > 0.0 else 0.0,
            }
        )
    return report


def _penumbra_report(scene: Scene, projection_geometry: dict[str, Any]) -> dict[str, Any]:
    samples = []
    checks = 0
    violations = 0
    for light_entry in projection_geometry["lights"]:
        light_id = light_entry["light_id"]
        light = next(item for item in scene.lights if item.light_id == light_id)
        for fish_entry in light_entry["fish"]:
            receiver = fish_entry["receiver"]
            if receiver["status"] != "valid":
                continue
            fish_centroid = np.asarray(fish_entry["fish_centroid"], dtype=np.float64)
            target_centroid = np.asarray(receiver["centroid"], dtype=np.float64)
            samples.append(
                {
                    "light_id": light_id,
                    "fish_id": fish_entry["fish_id"],
                    "fish_to_receiver_distance": fish_entry["distances"]["fish_to_receiver"],
                    "light_to_fish_distance": fish_entry["distances"]["light_to_fish"],
                    "edge_width": receiver["edge_width"],
                    "edge_samples": fish_entry["profile_samples"],
                }
            )
    for first in samples:
        for second in samples:
            if first["fish_to_receiver_distance"] <= second["fish_to_receiver_distance"] and first[
                "light_to_fish_distance"
            ] <= second["light_to_fish_distance"]:
                checks += 1
                if first["edge_width"] > second["edge_width"] + 1e-9:
                    violations += 1
    return {
        "model": "deterministic stylised area-light penumbra; monotonic in fish-target and light-fish distance",
        "sampling_density": scene.render.shadow_samples,
        "samples": samples,
        "monotonicity_checks": {"pairs_checked": checks, "violations": violations},
    }


def _build_report(
    scene: Scene,
    tables: dict[str, ParticleTable],
    renderer: Renderer,
    state: SnapshotState,
    angle_hashes: dict[str, Any],
    shadow_hashes: dict[str, Any],
) -> dict[str, Any]:
    layers = _layer_contribution_report(scene, state)
    return {
        "schema_version": "1.0",
        "kind": "aquarium_render_report",
        "renderer_revision": RENDERER_REVISION,
        "scene_id": scene.scene_id,
        "scene_sha256": scene.summary,
        "scene_summary": scene.summary_record,
        "particle_tables": {
            component_id: {
                "seed": table.particle_seed,
                "resolution": list(table.resolution),
                "particle_count": int(table.u.size),
                "modulation_mean": round(float(table.modulation.mean()), 8),
                "modulation_max": round(float(table.modulation.max()), 8),
                "sha256": table.sha256,
            }
            for component_id, table in sorted(tables.items())
        },
        "particle_fingerprint": particle_tables_fingerprint(tables),
        "camera_angles_deg": [round(float(v), 8) for v in scene.camera.angles()],
        "layers": layers,
        "projections": {"lights": renderer.projection_geometry["lights"]},
        "penumbra": _penumbra_report(scene, renderer.projection_geometry),
        "shadow_maps": shadow_hashes,
        "angles": angle_hashes,
        "ownership_legend": {
            str(index): entry for index, entry in sorted(scene.ownership_legend.items())
        },
    }


def run_pipeline(
    scene: Scene,
    output_dir: str | Path,
    *,
    interrupt_requested: Callable[[], bool] | None = None,
    reference_dir: str | Path | None = None,
) -> PipelineResult:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    interrupt_requested = interrupt_requested or (lambda: False)
    tables = _build_tables(scene)
    fingerprint = particle_tables_fingerprint(tables)
    particle_hashes = _write_particle_tables(output / "particle_tables", tables)

    snapshot_path = output / "progress.snapshot.json"
    snapshot_existed = snapshot_path.exists()
    reset_reason: str | None = None
    try:
        state = load_snapshot(snapshot_path, scene.summary_record, fingerprint)
    except SnapshotResetRequired as exc:
        reset_reason = str(exc)
        _clear_rendered_outputs(output)
        state = new_state(scene.summary_record, fingerprint, reset_reason)
        write_snapshot(snapshot_path, state)
    if not snapshot_existed:
        write_snapshot(snapshot_path, state)

    renderer = Renderer(scene, tables)
    total_blocks = state.total_angles * state.blocks_per_angle
    while len(state.completed) < total_blocks:
        if interrupt_requested():
            break
        sequence = len(state.completed)
        angle_index, block_id = divmod(sequence, state.blocks_per_angle)
        result = renderer.render_block(angle_index, block_id)
        for component_id, values in result.layer_stats.items():
            total = state.layer_stats.setdefault(
                component_id, {"transmission": 0.0, "scattering": 0.0}
            )
            total["transmission"] += values["transmission"]
            total["scattering"] += values["scattering"]
        state.add(
            CompletedBlock(
                sequence=sequence,
                angle_index=angle_index,
                block_id=block_id,
                rgba=result.rgba,
                ownership=result.ownership,
                summary=color_summary(result.rgba),
            )
        )
        state.complete = len(state.completed) == total_blocks
        write_snapshot(snapshot_path, state)
        delay_ms = float(os.environ.get("AQUARIUM_BLOCK_DELAY_MS", "0") or "0")
        if delay_ms > 0.0:
            time.sleep(delay_ms / 1000.0)
        if interrupt_requested():
            break

    if not state.complete:
        angle_hashes, output_hashes = _assemble_outputs(scene, state, output)
        recovery = {
            "status": "interrupted",
            "exit_code": 0,
            "interruptible": True,
            "last_committed": state_report(state),
            "snapshot": "progress.snapshot.json",
            "scene_summary": scene.summary_record,
            "particle_fingerprint": fingerprint,
            "particle_tables": particle_hashes,
            "completed_angle_outputs": angle_hashes,
            "completed_output_hashes": output_hashes,
            "resume_instruction": "rerun the same render command; only committed snapshot blocks are reused",
        }
        _write_json(output / "recovery_report.json", recovery)
        return PipelineResult("interrupted", 0, output, state, [])

    # Removed for clarity: renderer_state was assigned immediately below.
    angle_hashes, output_hashes = _assemble_outputs(scene, state, output)

    shadow_hashes: dict[str, Any] = {}
    for light in scene.lights:
        shadow = build_shadow_map(scene, light, renderer.projection_geometry)
        filename = f"shadow_{light.light_id}.png"
        write_png_gray(output / filename, shadow)
        data = (output / filename).read_bytes()
        shadow_hashes[light.light_id] = {
            "file": filename,
            "png_sha256": sha256_bytes(data),
            "pixels_sha256": sha256_bytes(shadow.tobytes()),
            "width": int(shadow.shape[1]),
            "height": int(shadow.shape[0]),
            "shadow_samples": scene.render.shadow_samples,
        }
        output_hashes[filename] = shadow_hashes[light.light_id]["png_sha256"]

    report = _build_report(scene, tables, renderer, state, angle_hashes, shadow_hashes)
    report_hash = _write_json(output / "report.json", report)
    output_hashes["report.json"] = report_hash

    manifest = {
        "status": "complete",
        "exit_code": 0,
        "scene_summary": scene.summary_record,
        "particle_fingerprint": fingerprint,
        "particle_tables": particle_hashes,
        "snapshot": state_report(state),
        "report_sha256": report_hash,
        "outputs": dict(sorted(output_hashes.items())),
    }
    _write_json(output / "run_manifest.json", manifest)

    if reference_dir is not None:
        diffs = compare_run_directories(reference_dir, output, scene)
        if diffs:
            _write_json(
                output / "reference_diff.json",
                {
                    "status": "error",
                    "code": "AQ-RESUME-001",
                    "reference": str(Path(reference_dir)),
                    "actual": str(output),
                    "diffs": diffs,
                },
            )
            return PipelineResult("mismatch", 1, output, state, diffs)
    return PipelineResult("complete", 0, output, state, [])

