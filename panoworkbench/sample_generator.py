"""Deterministic local panorama sample generation.

The generator creates a rich synthetic master scene, samples overlapping
rectified views through known homographies, and records exact truth data.
No network access or external image assets are used.
"""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from .errors import ErrorCode, WorkbenchError
from .geometry import (
    compute_canvas_geometry,
    invert_homography,
    normalize_homography,
    rotation_matrix,
    scale_matrix,
    translation_matrix,
)


@dataclass(frozen=True)
class DatasetSpec:
    name: str
    image_count: int
    width: int
    height: int
    max_rotation_deg: float
    scale_min: float = 0.94
    scale_max: float = 1.06
    center_radius_fraction: float = 0.22


DEFAULT_SPECS: tuple[DatasetSpec, ...] = (
    DatasetSpec("tiny", 3, 560, 420, 5.0),
    DatasetSpec("small", 6, 720, 540, 7.0),
    DatasetSpec("medium", 12, 760, 570, 9.0),
    DatasetSpec("large", 24, 640, 480, 10.0),
    DatasetSpec("highres", 4, 1800, 1200, 5.0),
)


def _make_master_scene(width: int, height: int, rng: np.random.Generator) -> np.ndarray:
    """Create a dense, multi-scale deterministic BGR texture."""

    yy, xx = np.mgrid[0:height, 0:width]
    base = np.empty((height, width, 3), dtype=np.float32)
    base[..., 0] = 32 + 55 * (xx / max(width - 1, 1)) + 22 * np.sin(yy / 67.0)
    base[..., 1] = 46 + 50 * (yy / max(height - 1, 1)) + 18 * np.cos(xx / 83.0)
    base[..., 2] = 58 + 35 * np.sin((xx + yy) / 101.0)
    base += rng.normal(0.0, 7.0, size=base.shape).astype(np.float32)
    base = np.clip(base, 0, 255).astype(np.uint8)

    grid = 34
    for y in range(0, height, grid):
        color = (55, 55, 55)
        cv2.line(base, (0, y), (width, y), color, 1, cv2.LINE_8)
    for x in range(0, width, grid):
        cv2.line(base, (x, 0), (x, height), (48, 48, 48), 1, cv2.LINE_8)

    palette = [
        (32, 205, 236),
        (230, 120, 44),
        (70, 210, 118),
        (212, 86, 190),
        (250, 230, 60),
        (30, 80, 240),
        (240, 240, 240),
        (20, 20, 20),
    ]
    for index in range(max(120, width * height // 6200)):
        cx = int(rng.integers(0, width))
        cy = int(rng.integers(0, height))
        radius = int(rng.integers(7, max(12, min(width, height) // 24)))
        color = palette[int(rng.integers(0, len(palette)))]
        if index % 4 == 0:
            points = np.column_stack(
                [
                    cx + rng.integers(-radius, radius + 1, size=5),
                    cy + rng.integers(-radius, radius + 1, size=5),
                ]
            ).astype(np.int32)
            cv2.fillPoly(base, [points], color, cv2.LINE_AA)
            cv2.polylines(base, [points], True, (245, 245, 245), 2, cv2.LINE_AA)
        elif index % 4 == 1:
            cv2.circle(base, (cx, cy), radius, color, -1, cv2.LINE_AA)
            cv2.circle(base, (cx, cy), max(2, radius // 2), (245, 245, 245), 2, cv2.LINE_AA)
        elif index % 4 == 2:
            rect = (cx, cy, radius * 2, max(8, radius))
            cv2.rectangle(base, rect, color, -1, cv2.LINE_AA)
            cv2.rectangle(base, rect, (250, 250, 250), 2, cv2.LINE_AA)
        else:
            end = (
                int(np.clip(cx + rng.integers(-80, 81), 0, width - 1)),
                int(np.clip(cy + rng.integers(-80, 81), 0, height - 1)),
            )
            cv2.line(base, (cx, cy), end, color, int(rng.integers(1, 5)), cv2.LINE_AA)

    for index in range(max(35, width * height // 25000)):
        x = int(rng.integers(0, max(1, width - 100)))
        y = int(rng.integers(18, max(19, height - 10)))
        text = f"S{index:04d}"
        color = tuple(int(v) for v in palette[int(rng.integers(0, len(palette)))])
        cv2.putText(
            base,
            text,
            (x, y),
            cv2.FONT_HERSHEY_SIMPLEX,
            float(rng.uniform(0.35, 0.8)),
            color,
            1,
            cv2.LINE_AA,
        )

    blurred = cv2.GaussianBlur(base, (0, 0), 2.0)
    base = cv2.addWeighted(base, 0.82, blurred, 0.18, 0.0)
    return base


def _view_homography(
    width: int,
    height: int,
    center_x: float,
    center_y: float,
    rotation_deg: float,
    scale: float,
    projective_x: float,
    projective_y: float,
) -> np.ndarray:
    to_origin = translation_matrix(-width / 2.0, -height / 2.0)
    project = np.array(
        [
            [1.0, 0.0, 0.0],
            [0.0, 1.0, 0.0],
            [projective_x, projective_y, 1.0],
        ],
        dtype=np.float64,
    )
    h = (
        translation_matrix(center_x, center_y)
        @ project
        @ rotation_matrix(rotation_deg)
        @ scale_matrix(scale)
        @ to_origin
    )
    return normalize_homography(h)


def _overlap_correspondences(
    base_width: int,
    base_height: int,
    image_width: int,
    image_height: int,
    homography_image_to_base: np.ndarray,
    *,
    max_points: int = 25,
) -> list[dict[str, list[float]]]:
    inverse = invert_homography(homography_image_to_base)
    samples: list[dict[str, list[float]]] = []
    for y in np.linspace(5, base_height - 5, 9):
        for x in np.linspace(5, base_width - 5, 9):
            base_point = np.array([[x, y]], dtype=np.float64)
            homogeneous = np.array([[x, y, 1.0]]) @ inverse.T
            local = homogeneous[0, :2] / homogeneous[0, 2]
            if (
                1 <= local[0] < image_width - 1
                and 1 <= local[1] < image_height - 1
            ):
                forward = np.array([[local[0], local[1], 1.0]]) @ homography_image_to_base.T
                world = forward[0, :2] / forward[0, 2]
                if np.linalg.norm(world - base_point[0]) < 1e-6:
                    samples.append(
                        {
                            "base_point": [round(float(x), 8), round(float(y), 8)],
                            "image_point": [
                                round(float(local[0]), 8),
                                round(float(local[1]), 8),
                            ],
                        }
                    )
            if len(samples) >= max_points:
                return samples
    return samples


def _write_png(path: Path, image: np.ndarray) -> str:
    path.parent.mkdir(parents=True, exist_ok=True)
    ok, encoded = cv2.imencode(".png", image, [cv2.IMWRITE_PNG_COMPRESSION, 3])
    if not ok:
        raise WorkbenchError(
            ErrorCode.IO, "failed to encode generated PNG", filename=str(path)
        )
    path.write_bytes(encoded.tobytes())
    return hashlib.sha256(encoded.tobytes()).hexdigest()


def generate_dataset(
    output_dir: str | Path,
    spec: DatasetSpec,
    *,
    seed: int,
    overwrite: bool = True,
) -> dict[str, Any]:
    """Generate one deterministic dataset and return its manifest."""

    root = Path(output_dir)
    if root.exists() and any(root.iterdir()) and not overwrite:
        raise WorkbenchError(
            ErrorCode.IO, "dataset directory is not empty", filename=str(root)
        )
    root.mkdir(parents=True, exist_ok=True)
    images_dir = root / "images"
    images_dir.mkdir(parents=True, exist_ok=True)

    dataset_seed = int(seed) + sum((index + 1) * ord(c) for index, c in enumerate(spec.name))
    rng = np.random.default_rng(dataset_seed)

    pad = max(190, int(min(spec.width, spec.height) * 0.42)) + 80
    master_w = spec.width + 2 * pad
    master_h = spec.height + 2 * pad
    master = _make_master_scene(master_w, master_h, rng)

    homographies: dict[int, np.ndarray] = {}
    sizes: dict[int, tuple[int, int]] = {}
    generated_images: list[tuple[int, np.ndarray, dict[str, Any]]] = []
    base_center_x = spec.width / 2.0
    base_center_y = spec.height / 2.0
    golden_angle = math.pi * (3.0 - math.sqrt(5.0))

    for index in range(spec.image_count):
        width = spec.width + int(rng.integers(-10, 11))
        height = spec.height + int(rng.integers(-8, 9))
        if index == 0:
            width, height = spec.width, spec.height
            homography = np.eye(3, dtype=np.float64)
            gain, offset = 1.0, 0.0
            rotation = 0.0
            scale = 1.0
        else:
            radial = spec.center_radius_fraction * min(spec.width, spec.height)
            radius = radial * float(rng.uniform(0.25, 1.0))
            angle = (index - 1) * golden_angle + float(rng.uniform(-0.34, 0.34))
            center_x = base_center_x + radius * math.cos(angle)
            center_y = base_center_y + radius * math.sin(angle)
            rotation = float(rng.uniform(-spec.max_rotation_deg, spec.max_rotation_deg))
            scale = float(rng.uniform(spec.scale_min, spec.scale_max))
            projective_x = float(rng.uniform(-0.018, 0.018)) / max(width, height)
            projective_y = float(rng.uniform(-0.018, 0.018)) / max(width, height)
            homography = _view_homography(
                width,
                height,
                center_x,
                center_y,
                rotation,
                scale,
                projective_x,
                projective_y,
            )
            gain = float(rng.uniform(0.88, 1.12))
            offset = float(rng.uniform(-12.0, 12.0))

        master_to_local = invert_homography(homography) @ translation_matrix(-pad, -pad)
        local = cv2.warpPerspective(
            master,
            master_to_local,
            (width, height),
            flags=cv2.INTER_LINEAR,
            borderMode=cv2.BORDER_CONSTANT,
            borderValue=(0, 0, 0),
        )
        coverage = cv2.warpPerspective(
            np.full(master.shape[:2], 255, dtype=np.uint8),
            master_to_local,
            (width, height),
            flags=cv2.INTER_NEAREST,
            borderMode=cv2.BORDER_CONSTANT,
            borderValue=0,
        )
        if int(np.min(coverage)) < 255:
            raise WorkbenchError(
                ErrorCode.IO,
                f"generated view {index} extends outside master scene",
                field="master_padding",
            )
        adjusted = local.astype(np.float32) * gain + offset
        local = np.clip(np.rint(adjusted), 0, 255).astype(np.uint8)

        filename = f"images/view_{index:03d}.png"
        homographies[index] = homography
        sizes[index] = (width, height)
        generated_images.append(
            (
                index,
                local,
                {
                    "id": index,
                    "filename": filename,
                    "width": width,
                    "height": height,
                    "channels": int(local.shape[2]),
                    "homography_to_base": homography.tolist(),
                    "synthetic_exposure": {"gain": gain, "offset": offset},
                    "synthetic_rotation_deg": rotation,
                    "synthetic_scale": scale,
                },
            )
        )

    canvas = compute_canvas_geometry(homographies, sizes)
    image_entries: list[dict[str, Any]] = []
    for index, image, entry in generated_images:
        sha = _write_png(root / entry["filename"], image)
        entry["sha256"] = sha
        if index == 0:
            entry["overlap_with_base"] = []
        else:
            entry["overlap_with_base"] = _overlap_correspondences(
                spec.width,
                spec.height,
                entry["width"],
                entry["height"],
                homographies[index],
            )
        image_entries.append(entry)

    manifest: dict[str, Any] = {
        "schema_version": 1,
        "generator": "panoworkbench.sample_generator",
        "dataset_name": spec.name,
        "seed": dataset_seed,
        "spec": asdict(spec),
        "base_image": 0,
        "image_count": len(image_entries),
        "canvas": {
            "width": canvas.width,
            "height": canvas.height,
            "origin_x": canvas.origin_x,
            "origin_y": canvas.origin_y,
        },
        "images": image_entries,
    }
    manifest_path = root / "dataset.json"
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    return manifest


def generate_all(
    output_root: str | Path,
    *,
    seed: int = 20261123,
    specs: tuple[DatasetSpec, ...] = DEFAULT_SPECS,
    overwrite: bool = True,
) -> dict[str, dict[str, Any]]:
    root = Path(output_root)
    root.mkdir(parents=True, exist_ok=True)
    manifests: dict[str, dict[str, Any]] = {}
    for spec in specs:
        manifests[spec.name] = generate_dataset(
            root / spec.name, spec, seed=seed, overwrite=overwrite
        )
    return manifests

