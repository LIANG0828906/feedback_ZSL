"""本地确定性样本生成器。

所有随机性来自显式传入的 ``numpy.random.Generator`` 种子；同一种子重复
运行产出逐字节一致的 PNG 与真值清单。真值几何直接在生成期由栅格化
标签掩码统计，避免“解析公式 vs 像素口径”不一致。
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Dict, List, Sequence, Tuple

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.utils.hashing import sha256_array, sha256_json
from segworkbench.utils.pngcodec import write_png

IMAGE_WIDTH = 256
IMAGE_HEIGHT = 256
CHANNELS = 3
BLOCK_COLS = 2
BLOCK_ROWS = 2
BLOCK_WIDTH = IMAGE_WIDTH // BLOCK_COLS
BLOCK_HEIGHT = IMAGE_HEIGHT // BLOCK_ROWS


@dataclass(frozen=True)
class ShapeSpec:
    """区域形状规格，坐标均为图像级像素坐标，原点在左上角。"""

    region_id: str
    block_id: str
    shape: str  # "rect" | "circle"
    color: Tuple[int, int, int]
    params: Dict[str, float]
    low_contrast: bool = False


def block_origin(block_id: str) -> Tuple[int, int]:
    index = int(block_id[1:])
    row, col = divmod(index, BLOCK_COLS)
    return col * BLOCK_WIDTH, row * BLOCK_HEIGHT


def _gradient_background(width: int, height: int, base: int, slope_x: float, slope_y: float) -> np.ndarray:
    grid_x = np.arange(width, dtype=np.float32)
    grid_y = np.arange(height, dtype=np.float32)
    plane = base + grid_x[None, :] * slope_x + grid_y[:, None] * slope_y
    values = np.clip(np.rint(plane), 0, 255).astype(np.uint8)
    return np.repeat(values[:, :, None], CHANNELS, axis=2)


def _rasterize_shape(shape: ShapeSpec) -> np.ndarray:
    mask = np.zeros((IMAGE_HEIGHT, IMAGE_WIDTH), dtype=bool)
    ox, oy = block_origin(shape.block_id)
    if shape.shape == "rect":
        x0 = ox + int(shape.params["x0_local"])
        y0 = oy + int(shape.params["y0_local"])
        x1 = ox + int(shape.params["x1_local"])
        y1 = oy + int(shape.params["y1_local"])
        mask[y0:y1, x0:x1] = True
    elif shape.shape == "circle":
        cx = ox + float(shape.params["cx_local"])
        cy = oy + float(shape.params["cy_local"])
        radius = float(shape.params["radius"])
        yy, xx = np.mgrid[0:IMAGE_HEIGHT, 0:IMAGE_WIDTH]
        mask = (xx - cx) ** 2 + (yy - cy) ** 2 <= radius ** 2
    else:  # pragma: no cover - 由参数校验提前拦截
        raise fail(ErrorCode.E_PARA, f"未知形状: {shape.shape}")
    return mask


def _mask_geometry(mask: np.ndarray) -> Dict[str, object]:
    ys, xs = np.nonzero(mask)
    if xs.size == 0:
        raise fail(ErrorCode.E_REGION, "空区域无法计算真值几何")
    return {
        "mask_pixel_count": int(xs.size),
        "centroid_xy": [float(xs.mean()), float(ys.mean())],
        "bbox_xyxy": [int(xs.min()), int(ys.min()), int(xs.max()), int(ys.max())],
    }


def _default_shapes(sample_key: str) -> List[ShapeSpec]:
    if sample_key == "sample_01_blocks":
        colors = [(228, 48, 48), (48, 96, 228), (48, 180, 84), (232, 188, 40)]
        rects = [(18, 18, 108, 104), (20, 16, 110, 108), (16, 22, 104, 106), (24, 20, 100, 100)]
        return [
            ShapeSpec(f"r{idx:02d}", f"b{idx:02d}", "rect", colors[idx],
                      dict(zip(("x0_local", "y0_local", "x1_local", "y1_local"), rects[idx])))
            for idx in range(4)
        ]
    if sample_key == "sample_02_circles":
        colors = [(210, 70, 70), (80, 130, 220), (110, 110, 120), (240, 150, 60)]
        specs = []
        for idx in range(4):
            specs.append(
                ShapeSpec(
                    f"r{idx:02d}", f"b{idx:02d}", "circle", colors[idx],
                    {"cx_local": 64.0, "cy_local": 64.0, "radius": 42.0 - idx * 4.0},
                    low_contrast=(idx == 2),
                )
            )
        return specs
    if sample_key == "sample_03_mixed":
        specs = [
            ShapeSpec("r00", "b00", "rect", (220, 60, 60),
                      {"x0_local": 20, "y0_local": 20, "x1_local": 96, "y1_local": 92}),
            ShapeSpec("r01", "b01", "circle", (60, 110, 220),
                      {"cx_local": 64.0, "cy_local": 60.0, "radius": 34.0}),
            ShapeSpec("r02", "b02", "circle", (90, 170, 110),
                      {"cx_local": 62.0, "cy_local": 66.0, "radius": 26.0}),
            ShapeSpec("r03", "b03", "rect", (225, 180, 50),
                      {"x0_local": 28, "y0_local": 26, "x1_local": 88, "y1_local": 96}),
        ]
        return specs
    raise fail(ErrorCode.E_PARA, f"未知样本键: {sample_key}")


def generate_sample(sample_key: str, output_dir: str, noise_seed: int, noise_amplitude: int = 12) -> Dict[str, object]:
    """生成一张确定性样本图像与其真值清单。

    :returns: 清单 dict（同时写入 ``<output_dir>/<sample_key>.json``）。
    """
    rng = np.random.default_rng(noise_seed)
    image = _gradient_background(IMAGE_WIDTH, IMAGE_HEIGHT, base=46, slope_x=0.08, slope_y=0.05)
    label = np.zeros((IMAGE_HEIGHT, IMAGE_WIDTH), dtype=np.uint8)
    shapes = _default_shapes(sample_key)
    region_records: List[Dict[str, object]] = []
    for shape in shapes:
        shape_mask = _rasterize_shape(shape)
        label[shape_mask] = 1
        image[shape_mask] = np.array(shape.color, dtype=np.uint8)
        region_records.append(
            {
                "region_id": shape.region_id,
                "block_id": shape.block_id,
                "shape": shape.shape,
                "params": shape.params,
                "color": list(shape.color),
                "low_contrast": shape.low_contrast,
                "truth": _mask_geometry(shape_mask),
            }
        )
    # 噪声只作用于背景，区域颜色保持均匀（低对比度样本靠区域颜色设计）。
    background_mask = label == 0
    noise = rng.integers(-noise_amplitude, noise_amplitude + 1, size=image.shape, dtype=np.int16)
    noisy = np.clip(image.astype(np.int16) + noise, 0, 255).astype(np.uint8)
    image = np.where(background_mask[:, :, None], noisy, image)

    os.makedirs(output_dir, exist_ok=True)
    image_path = os.path.join(output_dir, f"{sample_key}.png")
    write_png(image_path, image)
    image_hash = sha256_array(image)
    manifest = {
        "schema_version": "sample-1.0.0",
        "sample_id": sample_key,
        "image": {
            "filename": f"{sample_key}.png",
            "width": IMAGE_WIDTH,
            "height": IMAGE_HEIGHT,
            "channels": CHANNELS,
            "sha256": image_hash,
            "noise_seed": noise_seed,
            "noise_amplitude": noise_amplitude,
        },
        "block_grid": {
            "rows": BLOCK_ROWS,
            "cols": BLOCK_COLS,
            "block_width": BLOCK_WIDTH,
            "block_height": BLOCK_HEIGHT,
        },
        "regions": region_records,
    }
    manifest["truth_fingerprint"] = sha256_json(
        [{"region_id": r["region_id"], "block_id": r["block_id"], "truth": r["truth"]} for r in region_records]
    )
    manifest_path = os.path.join(output_dir, f"{sample_key}.json")
    with open(manifest_path, "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, ensure_ascii=False, indent=2, sort_keys=True)
    return manifest


SAMPLE_SPECS: Sequence[Tuple[str, int]] = (
    ("sample_01_blocks", 1101),
    ("sample_02_circles", 1202),
    ("sample_03_mixed", 1303),
)


def generate_all(output_dir: str) -> List[Dict[str, object]]:
    return [generate_sample(key, output_dir, seed) for key, seed in SAMPLE_SPECS]
