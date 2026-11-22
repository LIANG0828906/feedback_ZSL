"""Deterministic local image samples with machine-readable geometry truth."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .imaging import save_png
from .stages import DEFAULT_PARAMS


@dataclass(frozen=True)
class SampleDefinition:
    filename: str
    mode: str
    width: int
    height: int
    seed: int
    title: str
    palette: tuple[tuple[int, int, int], tuple[int, int, int]]
    noise_sigma: float
    alpha_floor: int


SAMPLE_DEFINITIONS: tuple[SampleDefinition, ...] = (
    SampleDefinition(
        filename="forest_gray_320x240.png",
        mode="L",
        width=320,
        height=240,
        seed=1042,
        title="森林遗迹（单通道）",
        palette=((18, 42, 35), (122, 166, 112)),
        noise_sigma=7.5,
        alpha_floor=255,
    ),
    SampleDefinition(
        filename="spark_la_480x270.png",
        mode="LA",
        width=480,
        height=270,
        seed=2087,
        title="等离子火花（灰度+透明）",
        palette=((12, 20, 42), (224, 180, 88)),
        noise_sigma=5.0,
        alpha_floor=132,
    ),
    SampleDefinition(
        filename="neon_rgb_640x360.png",
        mode="RGB",
        width=640,
        height=360,
        seed=3159,
        title="霓虹街区（三通道）",
        palette=((7, 12, 34), (240, 72, 148)),
        noise_sigma=6.0,
        alpha_floor=255,
    ),
    SampleDefinition(
        filename="portrait_rgba_512x512.png",
        mode="RGBA",
        width=512,
        height=512,
        seed=4211,
        title="全息档案（四通道）",
        palette=((15, 34, 58), (88, 226, 206)),
        noise_sigma=4.5,
        alpha_floor=138,
    ),
)


def _channel_count(mode: str) -> int:
    return {"L": 1, "LA": 2, "RGB": 3, "RGBA": 4}[mode]


def _luminance(rgb: np.ndarray) -> np.ndarray:
    return (
        rgb[..., 0].astype(np.float32) * 0.2126
        + rgb[..., 1].astype(np.float32) * 0.7152
        + rgb[..., 2].astype(np.float32) * 0.0722
    )


def _circle_mask(
    xx: np.ndarray,
    yy: np.ndarray,
    cx: float,
    cy: float,
    radius: float,
) -> np.ndarray:
    return (xx - cx) ** 2 + (yy - cy) ** 2 <= radius**2


def _rect_mask(
    xx: np.ndarray,
    yy: np.ndarray,
    x0: float,
    y0: float,
    x1: float,
    y1: float,
) -> np.ndarray:
    return (xx >= x0) & (xx <= x1) & (yy >= y0) & (yy <= y1)


def _line_mask(
    xx: np.ndarray,
    yy: np.ndarray,
    x0: float,
    y0: float,
    x1: float,
    y1: float,
    width: float,
) -> np.ndarray:
    vx, vy = x1 - x0, y1 - y0
    length_squared = vx * vx + vy * vy
    if length_squared == 0:
        return _circle_mask(xx, yy, x0, y0, width / 2.0)
    projection = ((xx - x0) * vx + (yy - y0) * vy) / length_squared
    projection = np.clip(projection, 0.0, 1.0)
    nearest_x = x0 + projection * vx
    nearest_y = y0 + projection * vy
    return (xx - nearest_x) ** 2 + (yy - nearest_y) ** 2 <= (width / 2.0) ** 2


def _objects_for(defn: SampleDefinition) -> list[dict[str, Any]]:
    short = min(defn.width, defn.height)
    cx = defn.width * 0.5
    cy = defn.height * 0.5
    objects: list[dict[str, Any]] = [
        {
            "id": "subject_ring",
            "type": "circle",
            "cx": round(cx, 3),
            "cy": round(cy, 3),
            "radius": round(short * 0.235, 3),
            "color": [247, 214, 102],
            "role": "subject",
        },
        {
            "id": "core",
            "type": "circle",
            "cx": round(cx + short * 0.035, 3),
            "cy": round(cy - short * 0.025, 3),
            "radius": round(short * 0.105, 3),
            "color": [92, 238, 220],
            "role": "subject",
        },
        {
            "id": "diagonal",
            "type": "line",
            "x0": round(defn.width * 0.12, 3),
            "y0": round(defn.height * 0.18, 3),
            "x1": round(defn.width * 0.88, 3),
            "y1": round(defn.height * 0.82, 3),
            "width": round(max(4.0, short * 0.018), 3),
            "color": [235, 72, 120],
            "role": "calibration",
        },
        {
            "id": "frame_left",
            "type": "rect",
            "x0": round(defn.width * 0.055, 3),
            "y0": round(defn.height * 0.09, 3),
            "x1": round(defn.width * 0.115, 3),
            "y1": round(defn.height * 0.91, 3),
            "color": [128, 176, 255],
            "role": "calibration",
        },
        {
            "id": "frame_right",
            "type": "rect",
            "x0": round(defn.width * 0.885, 3),
            "y0": round(defn.height * 0.09, 3),
            "x1": round(defn.width * 0.945, 3),
            "y1": round(defn.height * 0.91, 3),
            "color": [128, 176, 255],
            "role": "calibration",
        },
        {
            "id": "detail_block",
            "type": "rect",
            "x0": round(defn.width * 0.67, 3),
            "y0": round(defn.height * 0.66, 3),
            "x1": round(defn.width * 0.84, 3),
            "y1": round(defn.height * 0.84, 3),
            "color": [248, 245, 232],
            "role": "detail",
        },
    ]
    objects.append(
        {
            "id": "alpha_cutout" if defn.mode in {"LA", "RGBA"} else "marker",
            "type": "circle",
            "cx": round(defn.width * 0.22, 3),
            "cy": round(defn.height * 0.73, 3),
            "radius": round(short * 0.072, 3),
            "color": [255, 255, 255],
            "role": "alpha" if defn.mode in {"LA", "RGBA"} else "marker",
        }
    )
    return objects


def _mask_for(
    obj: dict[str, Any],
    xx: np.ndarray,
    yy: np.ndarray,
) -> np.ndarray:
    object_type = obj["type"]
    if object_type == "circle":
        return _circle_mask(xx, yy, obj["cx"], obj["cy"], obj["radius"])
    if object_type == "rect":
        return _rect_mask(xx, yy, obj["x0"], obj["y0"], obj["x1"], obj["y1"])
    if object_type == "line":
        return _line_mask(
            xx,
            yy,
            obj["x0"],
            obj["y0"],
            obj["x1"],
            obj["y1"],
            obj["width"],
        )
    raise ValueError(f"未知几何类型: {object_type}")


def _render(defn: SampleDefinition) -> tuple[np.ndarray, dict[str, Any]]:
    rng = np.random.default_rng(defn.seed)
    yy, xx = np.mgrid[0 : defn.height, 0 : defn.width]
    xn = xx / max(defn.width - 1, 1)
    yn = yy / max(defn.height - 1, 1)
    low = np.array(defn.palette[0], dtype=np.float32)
    high = np.array(defn.palette[1], dtype=np.float32)
    gradient = (0.22 + 0.55 * xn + 0.23 * yn)[..., None]
    rgb = low[None, None, :] + (high - low)[None, None, :] * gradient
    rgb += (
        22.0
        * np.sin(xn * np.pi * 7.0 + yn * 2.3)[..., None]
        * np.array([0.35, 0.8, 1.0], dtype=np.float32)[None, None, :]
    )
    checker = ((xx // 24 + yy // 24) % 2).astype(np.float32)[..., None]
    rgb += checker * np.array([4.0, 6.0, 8.0], dtype=np.float32)
    rgb -= (
        ((xn - 0.5) ** 2 + (yn - 0.5) ** 2)[..., None]
        * np.array([35.0, 22.0, 12.0], dtype=np.float32)
    )

    objects = _objects_for(defn)
    for obj in objects:
        mask = _mask_for(obj, xx, yy)
        color = np.array(obj["color"], dtype=np.float32)
        if obj["role"] in {"calibration", "detail"}:
            blend = 0.88
        else:
            blend = 0.72
        rgb[mask] = rgb[mask] * (1.0 - blend) + color[None, :] * blend
        if obj["id"] in {"subject_ring", "detail_block"}:
            edge = mask & ~np.roll(mask, 2, axis=0) & ~np.roll(mask, -2, axis=0)
            rgb[edge] = np.clip(rgb[edge] + 28.0, 0, 255)

    rgb += rng.normal(0.0, defn.noise_sigma, size=rgb.shape).astype(np.float32)
    rgb = np.clip(np.rint(rgb), 0, 255).astype(np.uint8)

    alpha = np.full((defn.height, defn.width), 255, dtype=np.float32)
    if defn.mode in {"LA", "RGBA"}:
        alpha = (
            defn.alpha_floor
            + (255 - defn.alpha_floor) * (0.35 + 0.65 * yn)
        ).astype(np.float32)
        alpha += (
            np.sin(xn * np.pi * 3.0)[...,] * 14.0
        ).astype(np.float32)
        for obj in objects:
            if obj["role"] == "alpha":
                mask = _mask_for(obj, xx, yy)
                alpha[mask] = 62.0
                inner = _circle_mask(
                    xx,
                    yy,
                    obj["cx"],
                    obj["cy"],
                    obj["radius"] * 0.55,
                )
                alpha[inner] = 220.0
        alpha = np.clip(np.rint(alpha), 0, 255).astype(np.uint8)

    if defn.mode == "L":
        array = np.rint(_luminance(rgb)).astype(np.uint8)
    elif defn.mode == "LA":
        array = np.stack([np.rint(_luminance(rgb)).astype(np.uint8), alpha], axis=2)
    elif defn.mode == "RGB":
        array = rgb
    else:
        array = np.dstack([rgb, alpha])
    array = np.ascontiguousarray(array)

    truth = {
        "schema": 1,
        "title": defn.title,
        "seed": defn.seed,
        "coordinate_system": "origin_top_left_x_right_y_down",
        "width": defn.width,
        "height": defn.height,
        "mode": defn.mode,
        "channels": _channel_count(defn.mode),
        "has_alpha": defn.mode in {"LA", "RGBA"},
        "objects": objects,
        "noise_sigma": defn.noise_sigma,
    }
    return array, truth


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def _atomic_write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp")
    temporary.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    temporary.replace(path)


def parameter_sequences() -> dict[str, Any]:
    baseline = {
        stage: dict(values) for stage, values in DEFAULT_PARAMS.items()
    }
    contrast = {
        stage: dict(values) for stage, values in DEFAULT_PARAMS.items()
    }
    contrast["local_contrast"] = {
        "tile_size": 24,
        "clip_limit": 3.2,
        "blend": 0.9,
    }
    sharpen = {
        stage: dict(values) for stage, values in DEFAULT_PARAMS.items()
    }
    sharpen["sharpen"] = {"radius": 2, "amount": 1.25, "threshold": 1}
    sharpen["local_contrast"] = {
        "tile_size": 40,
        "clip_limit": 2.4,
        "blend": 0.7,
    }
    return {
        "schema": 1,
        "stage_order": ["denoise", "sharpen", "local_contrast"],
        "scenarios": [
            {
                "name": "baseline",
                "description": "全阶段默认参数",
                "params": baseline,
            },
            {
                "name": "contrast_only",
                "description": "仅调整局部对比度，用于验证只重算末端阶段",
                "changed_stage": "local_contrast",
                "params": contrast,
            },
            {
                "name": "sharpen_and_downstream",
                "description": "调整锐化，用于验证锐化及局部对比度失效",
                "changed_stage": "sharpen",
                "params": sharpen,
            },
        ],
    }


def generate_samples(
    output_dir: str | Path,
    *,
    definitions: Iterable[SampleDefinition] = SAMPLE_DEFINITIONS,
) -> dict[str, Any]:
    destination = Path(output_dir)
    destination.mkdir(parents=True, exist_ok=True)
    samples: list[dict[str, Any]] = []
    for defn in definitions:
        array, truth = _render(defn)
        image_path = destination / defn.filename
        save_png(array, defn.mode, image_path)
        truth_path = destination / f"{image_path.stem}.truth.json"
        _atomic_write_json(truth_path, truth)
        samples.append(
            {
                "file": defn.filename,
                "truth_file": truth_path.name,
                "title": defn.title,
                "mode": defn.mode,
                "width": defn.width,
                "height": defn.height,
                "channels": _channel_count(defn.mode),
                "seed": defn.seed,
                "sha256": _sha256(image_path),
                "recommended_params": {
                    stage: dict(values)
                    for stage, values in DEFAULT_PARAMS.items()
                },
            }
        )
    manifest = {
        "schema": 1,
        "generator": "filter_studio.samples.generate_samples",
        "samples": samples,
    }
    _atomic_write_json(destination / "manifest.json", manifest)
    _atomic_write_json(destination / "params_sequences.json", parameter_sequences())
    return manifest


def load_manifest(directory: str | Path = "samples") -> dict[str, Any]:
    path = Path(directory) / "manifest.json"
    return json.loads(path.read_text(encoding="utf-8"))


def load_truth(directory: str | Path, sample: dict[str, Any]) -> dict[str, Any]:
    path = Path(directory) / sample["truth_file"]
    return json.loads(path.read_text(encoding="utf-8"))


__all__ = [
    "SAMPLE_DEFINITIONS",
    "generate_samples",
    "load_manifest",
    "load_truth",
    "parameter_sequences",
]
