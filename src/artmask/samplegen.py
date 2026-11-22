"""Deterministic local sample generation with pixel-level reference masks."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import SAMPLE_FORMAT_VERSION


@dataclass(frozen=True)
class SampleSpec:
    sample_id: str
    scene: str
    width: int
    height: int
    seed: int


DEFAULT_SAMPLES: tuple[SampleSpec, ...] = (
    SampleSpec("s01-color-blocks", "color_blocks", 384, 256, 1101),
    SampleSpec("s02-gradient", "gradient_background", 640, 480, 2202),
    SampleSpec("s03-low-contrast", "low_contrast", 960, 720, 3303),
    SampleSpec("s04-embedded-noise", "embedded_noise", 1280, 960, 4404),
    SampleSpec("s05-multi-overlap", "multi_overlap", 2048, 1536, 5505),
)


@dataclass(frozen=True)
class Shape:
    kind: str
    center: tuple[float, float]
    color: tuple[int, int, int]
    params: dict[str, Any]

    def mask(self, width: int, height: int) -> np.ndarray:
        yy, xx = np.mgrid[0:height, 0:width]
        cx, cy = self.center
        if self.kind == "ellipse":
            rx = float(self.params["rx"])
            ry = float(self.params["ry"])
            return ((xx - cx) / rx) ** 2 + ((yy - cy) / ry) ** 2 <= 1.0
        if self.kind == "rectangle":
            x = float(self.params["x"])
            y = float(self.params["y"])
            w = float(self.params["w"])
            h = float(self.params["h"])
            return (xx >= x) & (xx < x + w) & (yy >= y) & (yy < y + h)
        if self.kind == "polygon":
            points = np.asarray(self.params["points"], dtype=float)
            inside = np.zeros((height, width), dtype=bool)
            x0 = max(0, int(np.floor(points[:, 0].min())))
            x1 = min(width, int(np.ceil(points[:, 0].max())) + 1)
            y0 = max(0, int(np.floor(points[:, 1].min())))
            y1 = min(height, int(np.ceil(points[:, 1].max())) + 1)
            for py in range(y0, y1):
                for px in range(x0, x1):
                    if _point_in_polygon(px + 0.5, py + 0.5, points):
                        inside[py, px] = True
            return inside
        raise ValueError(f"unknown shape kind: {self.kind}")

    def analytic(self) -> dict[str, Any]:
        return {
            "kind": self.kind,
            "center": list(self.center),
            "color": list(self.color),
            **self.params,
        }


def _point_in_polygon(x: float, y: float, points: np.ndarray) -> bool:
    inside = False
    j = len(points) - 1
    for i in range(len(points)):
        xi, yi = points[i]
        xj, yj = points[j]
        if (yi > y) != (yj > y):
            x_cross = (xj - xi) * (y - yi) / (yj - yi + 1e-12) + xi
            if x < x_cross:
                inside = not inside
        j = i
    return inside


def _shape_layout(scene: str, width: int, height: int) -> list[Shape]:
    sx = width / 640.0
    sy = height / 480.0

    def p(x: float, y: float) -> tuple[float, float]:
        return (x * sx, y * sy)

    if scene == "color_blocks":
        return [
            Shape(
                "rectangle",
                p(119, 118),
                (204, 62, 78),
                {"x": 55 * sx, "y": 62 * sy, "w": 128 * sx, "h": 112 * sy},
            ),
            Shape(
                "ellipse",
                p(350, 150),
                (54, 173, 106),
                {"rx": 96 * sx, "ry": 84 * sy},
            ),
            Shape(
                "polygon",
                p(497.5, 332.5),
                (231, 177, 60),
                {
                    "points": [
                        [420 * sx, 300 * sy],
                        [525 * sx, 245 * sy],
                        [575 * sx, 382 * sy],
                        [468 * sx, 420 * sy],
                    ]
                },
            ),
            Shape(
                "rectangle",
                p(209, 342),
                (72, 120, 204),
                {"x": 150 * sx, "y": 304 * sy, "w": 118 * sx, "h": 76 * sy},
            ),
        ]
    if scene == "gradient_background":
        return [
            Shape(
                "ellipse",
                p(150, 160),
                (218, 102, 70),
                {"rx": 82 * sx, "ry": 68 * sy},
            ),
            Shape(
                "polygon",
                p(356.5, 140),
                (58, 176, 166),
                {
                    "points": [
                        [278 * sx, 175 * sy],
                        [350 * sx, 54 * sy],
                        [435 * sx, 112 * sy],
                        [415 * sx, 226 * sy],
                    ]
                },
            ),
            Shape(
                "rectangle",
                p(516, 330),
                (168, 96, 210),
                {"x": 462 * sx, "y": 284 * sy, "w": 108 * sx, "h": 92 * sy},
            ),
        ]
    if scene == "low_contrast":
        return [
            Shape(
                "ellipse",
                p(248, 210),
                (137, 146, 156),
                {"rx": 106 * sx, "ry": 92 * sy},
            ),
            Shape(
                "polygon",
                p(460, 329.5),
                (145, 132, 118),
                {
                    "points": [
                        [372 * sx, 360 * sy],
                        [446 * sx, 236 * sy],
                        [548 * sx, 270 * sy],
                        [540 * sx, 402 * sy],
                        [430 * sx, 423 * sy],
                    ]
                },
            ),
        ]
    if scene == "embedded_noise":
        return [
            Shape(
                "rectangle",
                p(193, 173),
                (215, 76, 90),
                {"x": 92 * sx, "y": 94 * sy, "w": 202 * sx, "h": 158 * sy},
            ),
            Shape(
                "ellipse",
                p(450, 188),
                (57, 186, 122),
                {"rx": 112 * sx, "ry": 84 * sy},
            ),
            Shape(
                "polygon",
                p(318.5, 363),
                (236, 196, 70),
                {
                    "points": [
                        [212 * sx, 342 * sy],
                        [318 * sx, 288 * sy],
                        [425 * sx, 350 * sy],
                        [370 * sx, 438 * sy],
                        [248 * sx, 432 * sy],
                    ]
                },
            ),
        ]
    if scene == "multi_overlap":
        return [
            Shape(
                "ellipse",
                p(292, 216),
                (211, 74, 92),
                {"rx": 218 * sx, "ry": 142 * sy},
            ),
            Shape(
                "rectangle",
                p(419, 260),
                (52, 150, 220),
                {"x": 300 * sx, "y": 154 * sy, "w": 238 * sx, "h": 212 * sy},
            ),
            Shape(
                "ellipse",
                p(510, 330),
                (234, 189, 63),
                {"rx": 100 * sx, "ry": 104 * sy},
            ),
            Shape(
                "polygon",
                p(253, 352.5),
                (77, 202, 169),
                {
                    "points": [
                        [146 * sx, 344 * sy],
                        [272 * sx, 270 * sy],
                        [360 * sx, 396 * sy],
                        [220 * sx, 435 * sy],
                    ]
                },
            ),
            Shape(
                "rectangle",
                p(131, 212),
                (179, 92, 205),
                {"x": 75 * sx, "y": 146 * sy, "w": 112 * sx, "h": 132 * sy},
            ),
        ]
    raise ValueError(f"unknown scene: {scene}")


def _background(
    scene: str, width: int, height: int, rng: np.random.Generator
) -> np.ndarray:
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    yn = yy / max(1, height - 1)
    xn = xx / max(1, width - 1)
    if scene == "color_blocks":
        base = np.array([42.0, 50.0, 64.0], dtype=np.float32)
        drift = np.array([11.0, 7.0, 5.0], dtype=np.float32)
        image = base + drift[None, None, :] * yn[..., None]
    elif scene == "low_contrast":
        base = np.array([112.0, 120.0, 130.0], dtype=np.float32)
        drift = np.array([14.0, 11.0, 8.0], dtype=np.float32)
        image = base + drift[None, None, :] * (
            0.5 * xn[..., None] + 0.5 * yn[..., None]
        )
    else:
        left = np.array([33.0, 44.0, 65.0], dtype=np.float32)
        right = np.array([94.0, 82.0, 65.0], dtype=np.float32)
        image = left + (right - left)[None, None, :] * (
            0.65 * xn[..., None] + 0.35 * yn[..., None]
        )
    noise_std = 9.0 if scene == "embedded_noise" else 1.8
    image += rng.normal(0.0, noise_std, size=image.shape)
    return np.clip(image, 0, 255).astype(np.uint8)


def _recommended_params(scene: str) -> dict[str, Any]:
    if scene == "low_contrast":
        return {
            "color_tolerance": 17.0,
            "gradient_threshold": 55.0,
            "confidence_threshold": 0.0,
            "morph_size": 3,
            "min_area": 20,
        }
    if scene == "embedded_noise":
        return {
            "color_tolerance": 32.0,
            "gradient_threshold": 150.0,
            "confidence_threshold": 0.0,
            "morph_size": 5,
            "min_area": 24,
        }
    if scene == "multi_overlap":
        return {
            "color_tolerance": 29.0,
            "gradient_threshold": 90.0,
            "confidence_threshold": 0.0,
            "morph_size": 3,
            "min_area": 30,
        }
    return {
        "color_tolerance": 28.0,
        "gradient_threshold": 85.0,
        "confidence_threshold": 0.0,
        "morph_size": 3,
        "min_area": 12,
    }


def _region_stats(labels: np.ndarray, region_id: int) -> dict[str, Any]:
    ys, xs = np.nonzero(labels == region_id)
    if len(xs) == 0:
        raise ValueError(f"empty region {region_id}")
    return {
        "id": region_id,
        "centroid": [float(xs.mean()), float(ys.mean())],
        "area": int(len(xs)),
        "bbox": [
            int(xs.min()),
            int(ys.min()),
            int(xs.max() - xs.min() + 1),
            int(ys.max() - ys.min() + 1),
        ],
    }


def generate_sample(spec: SampleSpec, root: Path) -> dict[str, Any]:
    root.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(spec.seed)
    image = _background(spec.scene, spec.width, spec.height, rng)
    labels = np.zeros((spec.height, spec.width), dtype=np.uint16)
    shapes = _shape_layout(spec.scene, spec.width, spec.height)
    for index, shape in enumerate(shapes, start=1):
        mask = shape.mask(spec.width, spec.height)
        image[mask] = np.asarray(shape.color, dtype=np.uint8)
        labels[mask] = index

    if spec.scene == "embedded_noise":
        for region_id in (1, 2, 3):
            ys, xs = np.nonzero(labels == region_id)
            if len(xs) == 0:
                continue
            count = max(1, len(xs) // 290)
            pick = rng.choice(len(xs), size=count, replace=False)
            delta = rng.integers(-22, 23, size=(count, 3))
            pixels = image[ys[pick], xs[pick]].astype(np.int16) + delta
            image[ys[pick], xs[pick]] = np.clip(pixels, 0, 255).astype(np.uint8)

    image = np.clip(
        image.astype(np.int16) + rng.integers(-1, 2, image.shape), 0, 255
    ).astype(np.uint8)

    regions = []
    for index, shape in enumerate(shapes, start=1):
        stats = _region_stats(labels, index)
        regions.append({**stats, "analytic": shape.analytic()})

    sample_dir = root / spec.sample_id
    sample_dir.mkdir(parents=True, exist_ok=True)
    image_path = sample_dir / "image.npy"
    mask_path = sample_dir / "truth_mask.npy"
    _atomic_npy(image_path, image)
    _atomic_npy(mask_path, labels)
    image_hash = _array_hash(image)
    mask_hash = _array_hash(labels)
    manifest = {
        "format_version": SAMPLE_FORMAT_VERSION,
        "sample_id": spec.sample_id,
        "scene": spec.scene,
        "seed": spec.seed,
        "width": spec.width,
        "height": spec.height,
        "channels": 3,
        "dtype": "uint8",
        "region_count": len(regions),
        "regions": regions,
        "image_file": "image.npy",
        "truth_mask_file": "truth_mask.npy",
        "image_hash": image_hash,
        "truth_mask_hash": mask_hash,
        "recommended_params": _recommended_params(spec.scene),
        "truth_note": (
            "Region metrics come from the visible pixel-level reference mask; "
            "analytic primitive parameters are included per region."
        ),
    }
    _atomic_json(sample_dir / "manifest.json", manifest)
    return {
        "sample_id": spec.sample_id,
        "scene": spec.scene,
        "width": spec.width,
        "height": spec.height,
        "region_count": len(regions),
        "directory": str(sample_dir.resolve()),
        "image_hash": image_hash,
    }


def generate_suite(
    root: Path, specs: tuple[SampleSpec, ...] = DEFAULT_SAMPLES
) -> dict[str, Any]:
    root = root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    samples = [generate_sample(spec, root) for spec in specs]
    index = {
        "format_version": SAMPLE_FORMAT_VERSION,
        "root": str(root),
        "sample_count": len(samples),
        "samples": samples,
        "generator": "artmask.samplegen",
        "deterministic": True,
    }
    _atomic_json(root / "manifest.json", index)
    return index


def _array_hash(array: np.ndarray) -> str:
    return hashlib.sha256(
        array.tobytes(order="C")
        + str(array.shape).encode()
        + str(array.dtype).encode()
    ).hexdigest()


def _atomic_npy(path: Path, array: np.ndarray) -> None:
    tmp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with tmp.open("wb") as handle:
        np.save(handle, array, allow_pickle=False)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)


def _atomic_json(path: Path, payload: Any) -> None:
    tmp = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    text = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True)
    with tmp.open("w", encoding="utf-8") as handle:
        handle.write(text)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)
