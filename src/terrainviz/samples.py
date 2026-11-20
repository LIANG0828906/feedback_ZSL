from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .camera import Camera
from .io_utils import atomic_write_bytes, atomic_write_json, canonical_json_bytes
from .material import MaterialMap
from .heightfield import HeightField
from .render import RenderConfig, Renderer
from .terrain import Terrain


@dataclass(frozen=True)
class SampleTerrain:
    name: str
    heightfield_path: Path
    material_path: Path
    height_digest: str
    material_digest: str


def _quantize(values: np.ndarray, step: float, minimum: float, maximum: float) -> np.ndarray:
    quantized = np.round(values / step) * step
    quantized = np.clip(quantized, minimum, maximum)
    as_float32 = quantized.astype(np.float32)
    as_float32 = np.clip(as_float32, np.float32(minimum), np.float32(maximum))
    return as_float32


def _plain_heights(rows: int, cols: int, seed: int) -> np.ndarray:
    z, x = np.mgrid[0:rows, 0:cols]
    phase = (seed % 997) * 0.001
    heights = (
        5.0
        + 1.35 * np.sin(x * 0.117 + phase) * np.cos(z * 0.089 - phase)
        + 0.55 * np.sin((x + z) * 0.181 + phase * 3.0)
        + 0.25 * np.cos((2.0 * x - z) * 0.057)
    )
    return heights


def _mountain_heights(rows: int, cols: int, seed: int) -> np.ndarray:
    z, x = np.mgrid[0:rows, 0:cols]
    cx = cols * 0.58
    cz = rows * 0.44
    phase = (seed % 991) * 0.0017
    ridge = np.exp(
        -(
            ((x - cx) / max(cols * 0.23, 1.0)) ** 2
            + ((z - cz) / max(rows * 0.28, 1.0)) ** 2
        )
    )
    secondary = np.exp(
        -(
            ((x - cols * 0.23) / max(cols * 0.17, 1.0)) ** 2
            + ((z - rows * 0.70) / max(rows * 0.18, 1.0)) ** 2
        )
    )
    corrugation = (
        0.5
        + 0.5
        * np.sin(x * 0.23 + phase)
        * np.cos(z * 0.19 - phase * 0.7)
    )
    heights = -8.0 + 205.0 * ridge + 82.0 * secondary * corrugation
    heights += 10.0 * np.sin((x + 1.5 * z) * 0.075)
    return heights


def _mixed_heights(rows: int, cols: int, seed: int) -> np.ndarray:
    plain = _plain_heights(rows, cols, seed)
    mountain = _mountain_heights(rows, cols, seed + 17)
    z, x = np.mgrid[0:rows, 0:cols]
    split = (x / max(cols - 1, 1)) * 0.65 + (z / max(rows - 1, 1)) * 0.35
    blend = np.clip((split - 0.34) / 0.32, 0.0, 1.0)
    blend = blend * blend * (3.0 - 2.0 * blend)
    return plain * (1.0 - blend) + mountain * blend


def _material_indices(name: str, heights: np.ndarray) -> np.ndarray:
    rows, cols = heights.shape
    d_row = np.gradient(heights, axis=0)
    d_col = np.gradient(heights, axis=1)
    slope = np.hypot(d_row, d_col)
    normalized = (heights - float(np.min(heights))) / max(
        float(np.max(heights) - np.min(heights)), 1e-9
    )
    indices = np.full(heights.shape, 1, dtype=np.uint8)
    if name == "plain":
        indices[normalized < 0.25] = 8
        indices[(normalized >= 0.25) & (normalized < 0.72)] = 0
        indices[normalized >= 0.72] = 2
        indices[slope > 0.35] = 9
    elif name == "mountain":
        indices[:] = 4
        indices[slope < 0.9] = 0
        indices[normalized > 0.48] = 3
        indices[(slope > 1.5) & (normalized < 0.82)] = 4
        indices[normalized > 0.78] = 5
    else:
        indices[:] = 0
        indices[normalized > 0.25] = 3
        indices[normalized > 0.44] = 4
        indices[normalized > 0.74] = 5
        indices[slope > 1.2] = 4
        indices[(normalized < 0.18) & (slope < 0.5)] = 8
    return indices


def generate_sample_set(
    outdir: str | Path,
    seed: int = 20261120,
    rows: int = 65,
    cols: int = 81,
) -> list[SampleTerrain]:
    if rows < 5 or cols < 5:
        raise ValueError("sample dimensions must be at least 5x5")
    output = Path(outdir)
    output.mkdir(parents=True, exist_ok=True)
    definitions = [
        ("plain", _plain_heights, -10.0, 20.0, 0.001),
        ("mountain", _mountain_heights, -30.0, 270.0, 0.01),
        ("mixed", _mixed_heights, -30.0, 270.0, 0.01),
    ]
    samples: list[SampleTerrain] = []
    manifest: list[dict[str, Any]] = []
    for name, factory, valid_min, valid_max, quant_step in definitions:
        heights = _quantize(factory(rows, cols, seed), quant_step, valid_min, valid_max)
        material = _material_indices(name, heights.astype(np.float64))
        heightfield = HeightField(
            heights=heights,
            spacing=1.0,
            quantization_step=quant_step,
            valid_min=valid_min,
            valid_max=valid_max,
            source_digest="",
        )
        material_map = MaterialMap(indices=material, source_digest="")
        height_path = output / f"{name}.thf"
        material_path = output / f"{name}.matidx"
        heightfield.save(height_path)
        material_map.save(material_path)
        height_file_digest = hashlib.sha256(height_path.read_bytes()).hexdigest()
        material_file_digest = hashlib.sha256(material_path.read_bytes()).hexdigest()
        sample = SampleTerrain(
            name=name,
            heightfield_path=height_path,
            material_path=material_path,
            height_digest=height_file_digest,
            material_digest=material_file_digest,
        )
        samples.append(sample)
        manifest.append(
            {
                "name": name,
                "heightfield_file": height_path.name,
                "material_file": material_path.name,
                "heightfield_sha256": sample.height_digest,
                "material_sha256": sample.material_digest,
                "height_range": [float(np.min(heights)), float(np.max(heights))],
                "valid_range": [valid_min, valid_max],
                "quantization_step": quant_step,
                "width": cols,
                "height": rows,
            }
        )
    atomic_write_json(
        output / "manifest.json",
        {
            "format": "terrainviz-sample-manifest",
            "version": 1,
            "seed": int(seed),
            "samples": manifest,
        },
    )
    return samples


def load_sample(sample: SampleTerrain, block_size: int = 16) -> Terrain:
    return Terrain(
        HeightField.load(sample.heightfield_path),
        MaterialMap.load(sample.material_path),
        block_size=block_size,
    )


def sample_camera(terrain: Terrain, variant: str) -> Camera:
    world_width = terrain.heightfield.world_width
    world_depth = terrain.heightfield.world_height
    mean_height = float(np.mean(terrain.heightfield.heights))
    if variant == "overview":
        eye = (
            world_width * 0.08,
            mean_height + max(world_width, world_depth) * 0.72,
            world_depth * 1.02,
        )
    else:
        eye = (
            world_width * 0.92,
            mean_height + max(world_width, world_depth) * 0.42,
            world_depth * 0.96,
        )
    return Camera(
        eye=eye,
        target=(world_width * 0.52, mean_height * 0.85, world_depth * 0.46),
        up=(0.0, 1.0, 0.0),
        fov_y_degrees=48.0,
        width=64,
        height=48,
    )


def write_baseline_trajectory(
    samples: Iterable[SampleTerrain],
    path: str | Path,
    *,
    screen_error_threshold: float = 1.5,
    seam_tolerance: float = 1.0,
    block_size: int = 16,
) -> Path:
    output = Path(path)
    lines: list[bytes] = []
    sequence = 0
    for sample in sorted(samples, key=lambda item: item.name):
        terrain = load_sample(sample, block_size=block_size)
        renderer = Renderer()
        for variant in ("overview", "oblique"):
            camera = sample_camera(terrain, variant)
            result = renderer.render(
                terrain,
                camera,
                screen_error_threshold,
                seam_tolerance,
                max_level=4,
                config=RenderConfig(precision="float64"),
            )
            payload = {
                "sequence": sequence,
                "terrain": sample.name,
                "heightfield_sha256": sample.height_digest,
                "viewpoint": {
                    "name": variant,
                    "camera": camera.to_dict(),
                },
                "screen_error_threshold": screen_error_threshold,
                "seam_tolerance": seam_tolerance,
                "block_levels": [
                    {
                        "block_id": item["block_id"],
                        "level": item["level"],
                        "error_upper_bound": item["error_upper_bound"],
                        "budget_usage": item["budget_usage"],
                    }
                    for item in result.report["levels"]
                ],
                "error_upper_bound_max": result.report["lod_summary"][
                    "max_error_upper_bound"
                ],
                "seam_status": result.seam_result.summary(),
                "content_hash": result.combined_digest,
            }
            digest_payload = {key: value for key, value in payload.items() if key != "content_hash"}
            payload["trace_hash"] = hashlib.sha256(
                canonical_json_bytes(digest_payload)
            ).hexdigest()
            lines.append(canonical_json_bytes(payload) + b"\n")
            sequence += 1
    atomic_write_bytes(output, b"".join(lines))
    return output

