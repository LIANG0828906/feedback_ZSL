from __future__ import annotations

from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from . import __version__
from .io_utils import sha256_bytes, stable_float, write_json
from .projection import build_projection_report
from .procedural import TextureSet, save_textures
from .renderer import RenderResult
from .scene import SceneSpec


def save_outputs(
    scene: SceneSpec,
    textures: TextureSet,
    result: RenderResult,
    output_dir: Path,
    scene_digest: str,
    *,
    status: str = "complete",
) -> tuple[dict[str, Any], dict[str, Any]]:
    output_dir.mkdir(parents=True, exist_ok=True)
    texture_report = save_textures(textures, scene, output_dir)

    np.save(output_dir / "rgba.npy", result.rgba, allow_pickle=False)
    np.save(output_dir / "layer_premultiplied.npy", result.layer_rgba, allow_pickle=False)
    np.save(output_dir / "ownership.npy", result.ownership, allow_pickle=False)
    attribution = _attribution_image(result.layer_rgba)
    np.save(output_dir / "ownership_weights.npy", attribution, allow_pickle=False)
    _save_png(output_dir / "rgba.png", result.rgba, "RGBA")
    _save_png(output_dir / "ownership.png", attribution, "RGB")
    _save_png(output_dir / "ownership_codes.png", result.ownership[..., 0], "L")

    layer_files: list[str] = []
    layer_dir = output_dir / "layers"
    layer_dir.mkdir(parents=True, exist_ok=True)
    for index, layer in enumerate(scene.layers):
        filename = f"layer_{layer.layer:02d}_premultiplied.png"
        relative = f"layers/{filename}"
        _save_png(layer_dir / filename, result.layer_rgba[index], "RGBA")
        layer_files.append(relative)

    projection_files: dict[str, str] = {}
    projection_report, projection_maps = build_projection_report(
        scene,
        textures,
        scene.camera,
        result.projection_gray,
    )
    projection_dir = output_dir / "projection"
    projection_dir.mkdir(parents=True, exist_ok=True)
    for pair_id, values in sorted(projection_maps.items()):
        stem = _pair_stem(pair_id)
        filename = f"{stem}.png"
        relative = f"projection/{filename}"
        _save_png(projection_dir / filename, values, "L")
        np.save(projection_dir / f"{stem}.npy", values, allow_pickle=False)
        projection_files[pair_id] = relative
    projection_points = {
        "schema_version": 1,
        "scene_digest": scene_digest,
        "points": [
            {
                "pair": pair["pair"],
                "source_layer": pair["source_layer"],
                "target": pair["target"],
                "source_world": pair["source_world"],
                "target_world": pair["target_world"],
                "target_uv": pair["target_uv"],
                "within_target_geometry": pair["within_target_geometry"],
            }
            for pair in projection_report["pairs"]
        ],
    }
    write_json(output_dir / "projection_points.json", projection_points)
    projection_points_sha = sha256_bytes(
        (output_dir / "projection_points.json").read_bytes()
    )
    projection_report["output_files"] = projection_files
    write_json(output_dir / "projection_report.json", projection_report)

    run_report = _build_run_report(
        scene,
        result,
        scene_digest,
        projection_report,
        layer_files,
        projection_files,
        projection_points_sha,
        status,
    )
    write_json(output_dir / "texture_report.json", texture_report)
    write_json(output_dir / "run_report.json", run_report)
    return run_report, projection_report


def _build_run_report(
    scene: SceneSpec,
    result: RenderResult,
    scene_digest: str,
    projection_report: dict[str, Any],
    layer_files: list[str],
    projection_files: dict[str, str],
    projection_points_sha: str,
    status: str,
) -> dict[str, Any]:
    luma_weights = np.array([0.2126, 0.7152, 0.0722], dtype=np.float64)
    layers: list[dict[str, Any]] = []
    for index, layer in enumerate(scene.layers):
        transmission, reflection, scatter, caustic, visible = (
            float(value) for value in result.metrics[index]
        )
        total = transmission + reflection + scatter + caustic
        denominator = max(total, 1e-12)
        transport_denominator = max(transmission + reflection, 1e-12)
        rgba = result.layer_rgba[index].astype(np.float64) / 255.0
        alpha = rgba[..., 3]
        valid = alpha > 0.0
        color = np.divide(
            rgba[..., :3],
            np.maximum(alpha[..., None], 1e-9),
        )
        luma = color @ luma_weights
        layers.append(
            {
                "layer": layer.layer,
                "name": layer.name,
                "plane_z": stable_float(layer.plane_z),
                "air_gap_from_previous": stable_float(layer.air_gap),
                "visible_fragments": int(round(visible)),
                "transmission_energy": stable_float(transmission),
                "reflection_energy": stable_float(reflection),
                "scatter_energy": stable_float(scatter),
                "caustic_energy": stable_float(caustic),
                "transmission_share": stable_float(transmission / denominator),
                "reflection_share": stable_float(reflection / denominator),
                "scatter_share": stable_float(scatter / denominator),
                "caustic_share": stable_float(caustic / denominator),
                "transmission_vs_reflection_share": stable_float(transmission / transport_denominator),
                "reflection_vs_transmission_share": stable_float(reflection / transport_denominator),
                "mean_premultiplied_luma": stable_float(
                    float(
                        np.sum(
                            (result.layer_rgba[index].astype(np.float64) / 255.0)[..., :3]
                            * luma_weights[None, None, :]
                        )
                    )
                    / max(1, result.rgba.shape[0] * result.rgba.shape[1])
                ),
                "mean_unpremultiplied_luma": stable_float(
                    float(luma[valid].mean()) if np.any(valid) else 0.0
                ),
                "premultiplied_file": layer_files[index],
            }
        )

    final_luma = (result.rgba[..., :3].astype(np.float64) / 255.0) @ luma_weights
    pure_white = np.all(result.rgba[..., :3] >= 250, axis=-1)
    edge_widths = [
        {
            "pair": pair["pair"],
            "gap": pair["gap"],
            "edge_width_world": pair["edge_width_world"],
            "edge_width_pixels": pair["edge_width_pixels"],
        }
        for pair in projection_report["pairs"]
    ]
    output_sha = {
        "rgba": sha256_bytes(np.ascontiguousarray(result.rgba).tobytes()),
        "layer_premultiplied": sha256_bytes(
            np.ascontiguousarray(result.layer_rgba).tobytes()
        ),
        "ownership": sha256_bytes(np.ascontiguousarray(result.ownership).tobytes()),
        "projection_points": projection_points_sha,
    }
    return {
        "schema_version": 1,
        "status": status,
        "renderer_version": __version__,
        "scene_name": scene.scene_name,
        "scene_digest": scene_digest,
        "seed": scene.seed,
        "image": {
            "width": scene.image.width,
            "height": scene.image.height,
            "chunk_size": [scene.image.chunk_width, scene.image.chunk_height],
            "block_count": scene.image.block_count,
        },
        "layer_order_inner_to_outer": [layer.layer for layer in scene.layers],
        "layers": layers,
        "projection": {
            "sampling_density": projection_report["sampling_density"],
            "sample_count": projection_report["sample_count"],
            "penumbra_per_gap": projection_report["penumbra_per_gap"],
            "edge_widths": edge_widths,
            "output_files": projection_files,
        },
        "image_checks": {
            "mean_rgba": [
                stable_float(float(value)) for value in result.rgba.reshape(-1, 4).mean(axis=0)
            ],
            "mean_luma": stable_float(float(final_luma.mean())),
            "max_luma": stable_float(float(final_luma.max())),
            "pure_white_fraction": stable_float(float(np.mean(pure_white))),
            "transparent_fraction": stable_float(float(np.mean(result.rgba[..., 3] == 0))),
        },
        "outputs": {
            "rgba": "rgba.png",
            "rgba_raw": "rgba.npy",
            "ownership": "ownership.png",
            "ownership_raw": "ownership.npy",
            "ownership_weights": "ownership_weights.npy",
            "ownership_codes": "ownership_codes.png",
            "layer_premultiplied": "layer_premultiplied.npy",
            "layer_files": layer_files,
            "projection_points": "projection_points.json",
            "projection_report": "projection_report.json",
            "texture_report": "texture_report.json",
        },
        "sha256": output_sha,
    }
def _save_png(path: Path, array: np.ndarray, mode: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    Image.fromarray(array, mode=mode).save(
        path,
        format="PNG",
        compress_level=6,
    )


def _pair_stem(pair_id: str) -> str:
    return "projection_" + pair_id.replace("->", "_to_").replace("-", "_")


def _attribution_image(layer_rgba: np.ndarray) -> np.ndarray:
    layer_count, height, width, _ = layer_rgba.shape
    alpha = layer_rgba[..., 3].astype(np.float64) / 255.0
    occlusion = np.ones((height, width), dtype=np.float64)
    weights: list[np.ndarray] = []
    for index in range(layer_count):
        weights.append(alpha[index] * occlusion)
        occlusion *= 1.0 - alpha[index]
    weights.append(occlusion)

    palette = (
        (0.92, 0.12, 0.08),
        (0.10, 0.78, 0.20),
        (0.12, 0.38, 0.95),
        (0.50, 0.47, 0.42),
    )
    image = np.zeros((height, width, 3), dtype=np.float64)
    for index, weight in enumerate(weights):
        color = np.asarray(palette[index] if index < len(palette) else palette[-1])
        image += weight[..., None] * color[None, None, :]
    image = np.power(np.clip(image, 0.0, 1.0), 0.75)
    return np.rint(image * 255.0).astype(np.uint8)
