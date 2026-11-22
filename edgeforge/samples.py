from __future__ import annotations

from dataclasses import dataclass
import json
import os
from pathlib import Path
from typing import Iterable, Sequence

import numpy as np

from .hashing import sha256_array
from .image_io import write_png


GENERATOR_VERSION = "edgeforge.samples.v1"
DEFAULT_MASTER_SEED = 20261123
GENERATOR_RNG_ALGORITHM = "numpy.default_rng(PCG64)"


@dataclass(frozen=True)
class LineSpec:
    line_id: str
    start: tuple[float, float]
    end: tuple[float, float]
    thickness: int


@dataclass(frozen=True)
class CircleSpec:
    circle_id: str
    center: tuple[float, float]
    diameter_ratio: float
    thickness: int


@dataclass(frozen=True)
class SampleSpec:
    sample_id: str
    width: int
    height: int
    channels: int
    seed: int
    scenario: str
    noise_std: float
    low_contrast: bool
    base: float
    amplitude: float
    stroke: float
    lines: tuple[LineSpec, ...]
    circles: tuple[CircleSpec, ...]


SAMPLE_SPECS: tuple[SampleSpec, ...] = (
    SampleSpec(
        sample_id="low_contrast_gray_320x240",
        width=320,
        height=240,
        channels=1,
        seed=DEFAULT_MASTER_SEED + 1,
        scenario="低对比度单通道纹理",
        noise_std=4.0,
        low_contrast=True,
        base=103.0,
        amplitude=9.0,
        stroke=137.0,
        lines=(
            LineSpec("L1", (0.08, 0.24), (0.92, 0.36), 3),
            LineSpec("L2", (0.16, 0.82), (0.81, 0.15), 2),
            LineSpec("L3", (0.44, 0.10), (0.49, 0.91), 3),
        ),
        circles=(
            CircleSpec("C1", (0.72, 0.68), 0.24, 3),
            CircleSpec("C2", (0.27, 0.50), 0.17, 2),
        ),
    ),
    SampleSpec(
        sample_id="noisy_rgb_800x600",
        width=800,
        height=600,
        channels=3,
        seed=DEFAULT_MASTER_SEED + 2,
        scenario="高噪声三通道素材",
        noise_std=10.0,
        low_contrast=False,
        base=48.0,
        amplitude=24.0,
        stroke=218.0,
        lines=(
            LineSpec("L1", (0.07, 0.22), (0.91, 0.30), 4),
            LineSpec("L2", (0.12, 0.84), (0.75, 0.12), 3),
            LineSpec("L3", (0.38, 0.08), (0.57, 0.92), 4),
            LineSpec("L4", (0.20, 0.58), (0.88, 0.75), 3),
        ),
        circles=(
            CircleSpec("C1", (0.73, 0.66), 0.22, 4),
            CircleSpec("C2", (0.29, 0.40), 0.14, 3),
        ),
    ),
    SampleSpec(
        sample_id="mixed_gray_1600x1000",
        width=1600,
        height=1000,
        channels=1,
        seed=DEFAULT_MASTER_SEED + 3,
        scenario="中高分辨率单通道混合纹理",
        noise_std=7.0,
        low_contrast=False,
        base=57.0,
        amplitude=22.0,
        stroke=205.0,
        lines=(
            LineSpec("L1", (0.05, 0.20), (0.95, 0.30), 5),
            LineSpec("L2", (0.10, 0.85), (0.82, 0.10), 4),
            LineSpec("L3", (0.47, 0.05), (0.53, 0.95), 5),
            LineSpec("L4", (0.18, 0.66), (0.90, 0.80), 4),
        ),
        circles=(
            CircleSpec("C1", (0.76, 0.63), 0.23, 5),
            CircleSpec("C2", (0.27, 0.38), 0.15, 4),
        ),
    ),
    SampleSpec(
        sample_id="panorama_rgb_2048x1152",
        width=2048,
        height=1152,
        channels=3,
        seed=DEFAULT_MASTER_SEED + 4,
        scenario="宽幅三通道游戏场景素材",
        noise_std=6.5,
        low_contrast=False,
        base=43.0,
        amplitude=27.0,
        stroke=225.0,
        lines=(
            LineSpec("L1", (0.04, 0.18), (0.96, 0.27), 5),
            LineSpec("L2", (0.08, 0.87), (0.72, 0.08), 4),
            LineSpec("L3", (0.35, 0.06), (0.47, 0.94), 5),
            LineSpec("L4", (0.55, 0.13), (0.91, 0.86), 4),
            LineSpec("L5", (0.16, 0.62), (0.88, 0.72), 4),
        ),
        circles=(
            CircleSpec("C1", (0.80, 0.61), 0.19, 5),
            CircleSpec("C2", (0.24, 0.37), 0.13, 4),
        ),
    ),
)

SAMPLE_BY_ID = {spec.sample_id: spec for spec in SAMPLE_SPECS}
DEFAULT_SAMPLE_IDS = tuple(spec.sample_id for spec in SAMPLE_SPECS)


def _segment_mask(
    height: int,
    width: int,
    start: tuple[float, float],
    end: tuple[float, float],
    thickness: int,
) -> tuple[np.ndarray, np.ndarray]:
    x0 = start[0] * (width - 1)
    y0 = start[1] * (height - 1)
    x1 = end[0] * (width - 1)
    y1 = end[1] * (height - 1)
    radius = thickness / 2.0
    pad = int(np.ceil(radius)) + 2
    xa = max(0, int(np.floor(min(x0, x1))) - pad)
    xb = min(width, int(np.ceil(max(x0, x1))) + pad + 1)
    ya = max(0, int(np.floor(min(y0, y1))) - pad)
    yb = min(height, int(np.ceil(max(y0, y1))) + pad + 1)
    yy, xx = np.mgrid[ya:yb, xa:xb].astype(np.float32)
    vx = float(x1 - x0)
    vy = float(y1 - y0)
    denom = vx * vx + vy * vy
    if denom <= 1e-12:
        t = np.zeros_like(xx, dtype=np.float32)
    else:
        t = ((xx - x0) * vx + (yy - y0) * vy) / denom
        np.clip(t, 0.0, 1.0, out=t)
    px = x0 + t * vx
    py = y0 + t * vy
    local = (xx - px) ** 2 + (yy - py) ** 2 <= radius * radius
    mask = np.zeros((height, width), dtype=bool)
    mask[ya:yb, xa:xb] = local
    return mask, np.array([x0, y0, x1, y1], dtype=np.float32)


def _circle_mask(
    height: int,
    width: int,
    center: tuple[float, float],
    diameter_ratio: float,
    thickness: int,
) -> tuple[np.ndarray, np.ndarray, int]:
    cx = center[0] * (width - 1)
    cy = center[1] * (height - 1)
    diameter = max(4, int(round(diameter_ratio * min(width, height))))
    radius = diameter / 2.0
    stroke_radius = max(1.0, thickness / 2.0)
    pad = int(np.ceil(stroke_radius)) + 2
    xa = max(0, int(np.floor(cx - radius)) - pad)
    xb = min(width, int(np.ceil(cx + radius)) + pad + 1)
    ya = max(0, int(np.floor(cy - radius)) - pad)
    yb = min(height, int(np.ceil(cy + radius)) + pad + 1)
    yy, xx = np.mgrid[ya:yb, xa:xb].astype(np.float32)
    dist = np.sqrt((xx - cx) ** 2 + (yy - cy) ** 2)
    local = np.abs(dist - radius) <= stroke_radius
    mask = np.zeros((height, width), dtype=bool)
    mask[ya:yb, xa:xb] = local
    return mask, np.array([cx, cy], dtype=np.float32), diameter


def _background(spec: SampleSpec, rng: np.random.Generator) -> np.ndarray:
    height, width = spec.height, spec.width
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float32)
    texture = (
        np.sin((xx + spec.seed % 97) * 0.031)
        * np.cos((yy - spec.seed % 53) * 0.027)
        + 0.45 * np.sin((xx + yy) * 0.011)
    )
    texture = np.clip(texture / 1.45, -1.0, 1.0)
    base = spec.base + spec.amplitude * texture
    if spec.channels == 1:
        noise = rng.normal(0.0, spec.noise_std, size=(height, width))
        return np.clip(base + noise, 0, 255).astype(np.float32)[:, :, None]

    offsets = np.array([-7.0, 0.0, 9.0], dtype=np.float32)[None, None, :]
    color_wave = np.stack(
        [
            0.65 * np.sin((xx + 17) * 0.019),
            0.55 * np.cos((yy + 11) * 0.023),
            0.60 * np.sin((xx - yy + 7) * 0.017),
        ],
        axis=2,
    ).astype(np.float32)
    tint = np.clip(color_wave, -1.0, 1.0) * (spec.amplitude * 0.45)
    noise = rng.normal(
        0.0,
        spec.noise_std,
        size=(height, width, spec.channels),
    ).astype(np.float32)
    return np.clip(base[:, :, None] + offsets + tint + noise, 0, 255).astype(
        np.float32
    )


def _stroke_color(
    spec: SampleSpec,
    index: int,
) -> np.ndarray:
    if spec.channels == 1:
        return np.array([spec.stroke], dtype=np.float32)
    palette = np.array(
        [
            [1.00, 0.84, 0.38],
            [0.40, 0.91, 1.00],
            [1.00, 0.53, 0.56],
            [0.68, 1.00, 0.52],
            [0.90, 0.62, 1.00],
            [1.00, 0.94, 0.72],
        ],
        dtype=np.float32,
    )
    return palette[index % len(palette)] * spec.stroke


def generate_sample(
    spec: SampleSpec,
    out_root: str | Path,
) -> dict[str, object]:
    root = Path(out_root)
    sample_dir = root / spec.sample_id
    sample_dir.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(spec.seed)
    image = _background(spec, rng)
    truth_mask = np.zeros((spec.height, spec.width), dtype=bool)

    lines_payload: list[dict[str, object]] = []
    for index, line in enumerate(spec.lines):
        mask, endpoints = _segment_mask(
            spec.height,
            spec.width,
            line.start,
            line.end,
            line.thickness,
        )
        truth_mask |= mask
        color = _stroke_color(spec, index)
        image[mask] = color[None, :] if spec.channels > 1 else color[0]
        lines_payload.append(
            {
                "id": line.line_id,
                "start": [int(round(float(endpoints[0]))), int(round(float(endpoints[1])))],
                "end": [int(round(float(endpoints[2]))), int(round(float(endpoints[3])))],
                "thickness": line.thickness,
            }
        )

    circles_payload: list[dict[str, object]] = []
    for index, circle in enumerate(spec.circles, start=len(spec.lines)):
        mask, center, diameter = _circle_mask(
            spec.height,
            spec.width,
            circle.center,
            circle.diameter_ratio,
            circle.thickness,
        )
        truth_mask |= mask
        color = _stroke_color(spec, index)
        image[mask] = color[None, :] if spec.channels > 1 else color[0]
        circles_payload.append(
            {
                "id": circle.circle_id,
                "center": [int(round(float(center[0]))), int(round(float(center[1])))],
                "diameter": int(diameter),
                "thickness": circle.thickness,
            }
        )

    image_u8 = np.clip(image + 0.5, 0, 255).astype(np.uint8)
    if spec.channels == 1:
        image_u8 = image_u8[:, :, 0]
    truth_u8 = truth_mask.astype(np.uint8)

    image_path = sample_dir / "image.png"
    truth_path = sample_dir / "truth_mask.npy"
    meta_path = sample_dir / "meta.json"
    write_png(image_path, image_u8)
    np.save(truth_path, truth_u8, allow_pickle=False)

    image_hash = sha256_array(image_u8)
    truth_hash = sha256_array(truth_u8)
    meta: dict[str, object] = {
        "schema_version": 1,
        "sample_id": spec.sample_id,
        "width": spec.width,
        "height": spec.height,
        "channels": spec.channels,
        "seed": spec.seed,
        "master_seed": DEFAULT_MASTER_SEED,
        "rng_algorithm": GENERATOR_RNG_ALGORITHM,
        "scenario": spec.scenario,
        "noise_std": spec.noise_std,
        "low_contrast": spec.low_contrast,
        "image_file": "image.png",
        "image_sha256": image_hash,
        "truth_mask_file": "truth_mask.npy",
        "truth_mask_sha256": truth_hash,
        "generator_version": GENERATOR_VERSION,
        "truth": {
            "lines": lines_payload,
            "circles": circles_payload,
            "edge_coverage": float(truth_mask.mean()),
        },
    }
    temp_meta = meta_path.with_name(meta_path.name + ".tmp")
    temp_meta.write_text(
        json.dumps(meta, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    os.replace(temp_meta, meta_path)
    return meta


def generate_samples(
    out_root: str | Path,
    sample_ids: Sequence[str] | None = None,
) -> dict[str, object]:
    root = Path(out_root)
    root.mkdir(parents=True, exist_ok=True)
    requested = list(sample_ids) if sample_ids is not None else list(DEFAULT_SAMPLE_IDS)
    unknown = [sample_id for sample_id in requested if sample_id not in SAMPLE_BY_ID]
    if unknown:
        raise KeyError(f"unknown sample ids: {', '.join(unknown)}")

    for sample_id in requested:
        spec = SAMPLE_BY_ID[sample_id]
        meta_path = root / sample_id / "meta.json"
        expected = root / sample_id / "image.png"
        truth = root / sample_id / "truth_mask.npy"
        if not (meta_path.is_file() and expected.is_file() and truth.is_file()):
            generate_sample(spec, root)

    entries: list[dict[str, object]] = []
    for meta_path in sorted(root.glob("*/meta.json")):
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        entries.append(
            {
                "sample_id": meta["sample_id"],
                "width": meta["width"],
                "height": meta["height"],
                "channels": meta["channels"],
                "scenario": meta["scenario"],
                "path": meta["sample_id"],
                "image_sha256": meta["image_sha256"],
                "truth_mask_sha256": meta["truth_mask_sha256"],
            }
        )
    manifest: dict[str, object] = {
        "schema_version": 1,
        "generator_version": GENERATOR_VERSION,
        "master_seed": DEFAULT_MASTER_SEED,
        "seed_policy": "每个样本使用写死的独立种子，生成顺序不影响内容",
        "rng_algorithm": GENERATOR_RNG_ALGORITHM,
        "samples": entries,
    }
    manifest_path = root / "manifest.json"
    temp_manifest = manifest_path.with_name(manifest_path.name + ".tmp")
    temp_manifest.write_text(
        json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    os.replace(temp_manifest, manifest_path)
    return manifest


def ensure_samples(
    out_root: str | Path,
    sample_ids: Iterable[str] | None = None,
) -> dict[str, object]:
    return generate_samples(out_root, list(sample_ids) if sample_ids else None)

