"""Stable statistics and final artifact construction."""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

import numpy as np

from .geometry import LayerGeometry
from .pngio import encode_rgba_png
from .projection import build_projection_report
from .renderer import ownership_layer_manifest
from .scene import canonical_json
from .snapshot import SnapshotStore
from .texture import texture_manifest
from .version import ALGORITHM_VERSION


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _round(value: float) -> float:
    return round(float(value), 9)


def assemble_tiles(
    store: SnapshotStore,
) -> tuple[np.ndarray, np.ndarray, dict[str, np.ndarray]]:
    image = store.image_height, store.image_width
    rgba_image = np.zeros((image[0], image[1], 4), dtype=np.uint8)
    ownership_image = np.zeros((image[0], image[1], 4), dtype=np.uint8)
    layer_count = store.scene_layer_count
    hit_count = np.zeros(layer_count, dtype=np.uint64)
    transmittance_sum = np.zeros(layer_count, dtype=np.float64)
    transmission_sum = np.zeros(layer_count, dtype=np.float64)
    reflection_sum = np.zeros(layer_count, dtype=np.float64)
    tiles = store.load_all()
    if set(tiles) != set(range(store.total_tiles)):
        missing = sorted(set(range(store.total_tiles)) - set(tiles))
        raise RuntimeError(f"cannot assemble incomplete snapshot; missing tiles {missing[:8]}")
    for tile_id in sorted(tiles):
        rgba, ownership, stats = tiles[tile_id]
        x0, y0 = store.tile_origin(tile_id)
        height, width = store.tile_shape(tile_id)
        rgba_image[y0 : y0 + height, x0 : x0 + width] = rgba
        ownership_image[y0 : y0 + height, x0 : x0 + width] = ownership
        hit_count += stats["hit_count"]
        transmittance_sum += stats["transmittance_sum"]
        transmission_sum += stats["transmission_contribution_sum"]
        reflection_sum += stats["reflection_contribution_sum"]
    return (
        rgba_image,
        ownership_image,
        {
            "hit_count": hit_count,
            "transmittance_sum": transmittance_sum,
            "transmission_contribution_sum": transmission_sum,
            "reflection_contribution_sum": reflection_sum,
        },
    )


def build_stats_report(
    scene: dict[str, Any],
    scene_content_digest: str,
    geometries: list[LayerGeometry],
    aggregated: dict[str, np.ndarray],
    projection_report: dict[str, Any],
    rgba_png: bytes,
    ownership_png: bytes,
    snapshot_commit_count: int,
) -> dict[str, Any]:
    layers = []
    for geometry in geometries:
        index = geometry.ordinal
        hit_count = int(aggregated["hit_count"][index])
        transmission = float(aggregated["transmission_contribution_sum"][index])
        reflection = float(aggregated["reflection_contribution_sum"][index])
        contribution_total = transmission + reflection
        layer_record: dict[str, Any] = {
            "layer": geometry.layer,
            "id": geometry.layer_id,
            "kind": geometry.kind,
            "thickness": float(geometry.definition["thickness"]),
            "visible_fragment_count": hit_count,
            "average_transmittance": _round(
                float(aggregated["transmittance_sum"][index]) / hit_count
                if hit_count
                else 0.0
            ),
            "transmission_contribution": _round(transmission),
            "reflection_contribution": _round(reflection),
            "transmission_ratio": _round(
                transmission / contribution_total if contribution_total > 1e-15 else 0.0
            ),
            "reflection_ratio": _round(
                reflection / contribution_total if contribution_total > 1e-15 else 0.0
            ),
        }
        if geometry.kind == "gauze":
            layer_record.update(
                {
                    "transmittance": float(geometry.definition["transmittance"]),
                    "tint": [float(value) for value in geometry.definition["tint"]],
                    "weave_seed": int(geometry.definition["weave_seed"]),
                    "texture_resolution": [
                        int(geometry.definition["resolution"][0]),
                        int(geometry.definition["resolution"][1]),
                    ],
                }
            )
        else:
            layer_record.update(
                {
                    "reflectance": float(geometry.definition["reflectance"]),
                    "width": float(geometry.definition["width"]),
                    "pattern_style": str(geometry.definition["pattern"]["style"]),
                    "pattern_pitch": float(geometry.definition["pattern"]["pitch"]),
                }
            )
        layers.append(layer_record)

    texture_manifests = [
        texture_manifest(geometry.definition)
        for geometry in geometries
        if geometry.kind == "gauze"
    ]
    receiver_series = projection_report["penumbra_audit"]["receiver_distance_series"]
    source_series = projection_report["penumbra_audit"]["source_distance_series"]
    report = {
        "schema_version": "veil-report/1",
        "algorithm_version": ALGORITHM_VERSION,
        "scene_digest": scene_content_digest,
        "image": {
            "width": int(scene["image"]["width"]),
            "height": int(scene["image"]["height"]),
            "tile_width": int(scene["image"]["tile_width"]),
            "tile_height": int(scene["image"]["tile_height"]),
            "rgba_sha256": _sha256(rgba_png),
            "layer_ownership_sha256": _sha256(ownership_png),
        },
        "layers": layers,
        "textures": texture_manifests,
        "lattice_projection_sample_count": int(projection_report["counts"]["total"]),
        "projection": {
            "samples_per_axis": int(projection_report["samples_per_axis"]),
            "landing_samples_per_axis": int(projection_report["landing_samples_per_axis"]),
            "landing_count": int(projection_report["counts"]["total"]),
            "inside_receiver_count": int(projection_report["counts"]["inside_receiver"]),
            "penumbra_width_samples": {
                "receiver_distance": [
                    {
                        "distance_lattice_to_receiver": item["distance_lattice_to_receiver"],
                        "distance_light_to_lattice": item["distance_light_to_lattice"],
                        "penumbra_width": item["penumbra_width"],
                        "edge_profile": item["edge_profile"],
                    }
                    for item in receiver_series
                ],
                "source_distance": [
                    {
                        "distance_lattice_to_receiver": item["distance_lattice_to_receiver"],
                        "distance_light_to_lattice": item["distance_light_to_lattice"],
                        "penumbra_width": item["penumbra_width"],
                        "edge_profile": item["edge_profile"],
                    }
                    for item in source_series
                ],
            },
            "monotonic_with_receiver_distance": bool(
                projection_report["penumbra_audit"]["monotonic_with_receiver_distance"]
            ),
            "monotonic_with_source_distance": bool(
                projection_report["penumbra_audit"]["monotonic_with_source_distance"]
            ),
        },
        "snapshot": {
            "commit_count": int(snapshot_commit_count),
            "total_tiles": int(
                ((scene["image"]["width"] + scene["image"]["tile_width"] - 1) // scene["image"]["tile_width"])
                * ((scene["image"]["height"] + scene["image"]["tile_height"] - 1) // scene["image"]["tile_height"])
            ),
        },
    }
    return report


def build_run_metadata(
    scene: dict[str, Any],
    scene_content_digest: str,
) -> dict[str, Any]:
    return {
        "schema_version": "veil-run-meta/1",
        "algorithm_version": ALGORITHM_VERSION,
        "scene_digest": scene_content_digest,
        "image": {
            "width": int(scene["image"]["width"]),
            "height": int(scene["image"]["height"]),
            "tile_width": int(scene["image"]["tile_width"]),
            "tile_height": int(scene["image"]["tile_height"]),
        },
        "ownership": ownership_layer_manifest(scene),
    }


def write_json(path: str | Path, value: Any) -> None:
    Path(path).write_text(canonical_json(value) + "\n", encoding="utf-8")


def finalize_artifacts(
    output_dir: str | Path,
    scene: dict[str, Any],
    scene_content_digest: str,
    geometries: list[LayerGeometry],
    store: SnapshotStore,
) -> dict[str, Any]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    rgba, ownership, aggregated = assemble_tiles(store)
    rgba_png = encode_rgba_png(rgba)
    ownership_png = encode_rgba_png(ownership)

    projection_report = build_projection_report(
        scene,
        geometries,
        scene_content_digest,
    )
    projection_bytes = (canonical_json(projection_report) + "\n").encode("utf-8")
    report = build_stats_report(
        scene,
        scene_content_digest,
        geometries,
        aggregated,
        projection_report,
        rgba_png,
        ownership_png,
        len(store.entries),
    )
    report_bytes = (canonical_json(report) + "\n").encode("utf-8")
    metadata = build_run_metadata(scene, scene_content_digest)

    Path(output / "rgba.png").write_bytes(rgba_png)
    Path(output / "layer_ownership.png").write_bytes(ownership_png)
    Path(output / "projection_landings.json").write_bytes(projection_bytes)
    Path(output / "layer_ownership.json").write_text(
        canonical_json(ownership_layer_manifest(scene)) + "\n", encoding="utf-8"
    )
    Path(output / "run_meta.json").write_text(
        canonical_json(metadata) + "\n", encoding="utf-8"
    )
    Path(output / "report.json").write_bytes(report_bytes)
    return report

