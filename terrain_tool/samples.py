"""确定性地形与补丁样本生成器。

全部基于固定种子的 :class:`numpy.random.Generator`，无外部数据、无网络。
产出多种地形（平原、丘陵、山脊、噪声场）与多批高程/材质补丁序列，
并记录每个样本的基线内容哈希，供测试与界面复用。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass

import numpy as np

from .binary_io import (
    KIND_HEIGHT,
    KIND_MATERIAL,
    HeightField,
    MaterialMap,
    Patch,
    save_heightfield,
    save_material,
    save_patch,
)
from .hashing import sha256_array

TERRAIN_TYPES = ("plain", "hills", "ridge", "noise")


def _terrain(kind: str, width: int, height: int, cell_size: float, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    xs = np.arange(width, dtype=np.float64)
    ys = np.arange(height, dtype=np.float64)
    gx, gy = np.meshgrid(xs, ys)
    if kind == "plain":
        field = 5.0 + 0.05 * gx + 0.03 * gy + rng.normal(0, 0.05, (height, width))
    elif kind == "hills":
        field = (
            20.0
            + 8.0 * np.sin(gx * 0.12) * np.cos(gy * 0.10)
            + 4.0 * np.sin((gx + gy) * 0.07)
            + rng.normal(0, 0.2, (height, width))
        )
    elif kind == "ridge":
        ridge = 30.0 * np.exp(-((gx - width * 0.5) ** 2) / (2 * (width * 0.08) ** 2))
        field = ridge + 0.1 * gy + rng.normal(0, 0.15, (height, width))
    else:  # noise
        field = rng.normal(0, 1.0, (height, width)).cumsum(axis=0).cumsum(axis=1)
        field = field / max(1.0, np.abs(field).max()) * 25.0
    return field.astype(np.float32)


def _materials(width: int, height: int, seed: int, palette_size: int) -> np.ndarray:
    rng = np.random.default_rng(seed + 7919)
    # 分带 + 少量噪点，保证可压缩且确定性
    bands = (np.arange(height)[:, None] // max(1, height // 4)) % palette_size
    tiles = np.broadcast_to(bands, (height, width)).copy()
    noise = rng.integers(0, palette_size, size=(height, width))
    mask = rng.random((height, width)) < 0.08
    tiles[mask] = noise[mask]
    return tiles.astype(np.uint8)


@dataclass
class SampleBundle:
    sample_dir: str
    terrain_type: str
    width: int
    height: int
    cell_size: float
    height_path: str
    material_path: str
    patch_paths: list[str]
    baseline: dict


def generate_sample(
    out_dir: str,
    terrain_type: str = "hills",
    width: int = 96,
    height: int = 80,
    cell_size: float = 1.0,
    chunk_size: int = 16,
    palette_size: int = 16,
    patch_count: int = 4,
    seed: int = 20261121,
) -> SampleBundle:
    if terrain_type not in TERRAIN_TYPES:
        raise ValueError(f"未知地形类型 {terrain_type}，可选 {TERRAIN_TYPES}")
    os.makedirs(out_dir, exist_ok=True)
    rng = np.random.default_rng(seed + 13)

    elevations = _terrain(terrain_type, width, height, cell_size, seed)
    materials = _materials(width, height, seed, palette_size)
    valid_rect = (0, 0, width, height)
    hf = HeightField(
        elevations=elevations,
        cell_size=cell_size,
        valid_rect=valid_rect,
        base_version=0,
    )
    mat = MaterialMap(indices=materials, base_version=0)
    height_path = os.path.join(out_dir, f"{terrain_type}.hgt")
    material_path = os.path.join(out_dir, f"{terrain_type}.mat")
    save_heightfield(height_path, hf)
    save_material(material_path, mat)

    baseline = {
        "terrain_type": terrain_type,
        "width": width,
        "height": height,
        "cell_size": cell_size,
        "chunk_size": chunk_size,
        "palette_size": palette_size,
        "height_hash": sha256_array(elevations),
        "material_hash": sha256_array(materials),
        "seed": seed,
        "patches": [],
    }

    patch_paths: list[str] = []
    margin = max(2, chunk_size // 2)
    for i in range(patch_count):
        seq = i + 1
        pw = int(rng.integers(margin, max(margin + 1, width // 3)))
        ph = int(rng.integers(margin, max(margin + 1, height // 3)))
        px = int(rng.integers(0, width - pw))
        py = int(rng.integers(0, height - ph))
        kind = KIND_HEIGHT if i % 2 == 0 else KIND_MATERIAL
        if kind == KIND_HEIGHT:
            amplitude = 2.0 + 4.0 * rng.random()
            data = (
                elevations[py : py + ph, px : px + pw]
                + rng.normal(0, 0.5, (ph, pw)) * amplitude
            ).astype(np.float32)
        else:
            data = rng.integers(0, palette_size, size=(ph, pw)).astype(np.uint8)
        patch = Patch(
            kind=kind,
            rect=(px, py, pw, ph),
            cell_size=cell_size,
            base_version=i,  # 顺序批次：第 i 批基于版本 i
            seq=seq,
            data=data,
        )
        patch_path = os.path.join(out_dir, f"{terrain_type}_patch_{seq:02d}.ptc")
        save_patch(patch_path, patch)
        patch_paths.append(patch_path)
        baseline["patches"].append(
            {
                "seq": seq,
                "kind": "height" if kind == KIND_HEIGHT else "material",
                "rect": [px, py, pw, ph],
                "base_version": i,
                "data_hash": sha256_array(data),
            }
        )

    baseline_path = os.path.join(out_dir, f"{terrain_type}_baseline.json")
    with open(baseline_path, "w", encoding="utf-8") as fh:
        json.dump(baseline, fh, sort_keys=True, indent=2)
    return SampleBundle(
        sample_dir=out_dir,
        terrain_type=terrain_type,
        width=width,
        height=height,
        cell_size=cell_size,
        height_path=height_path,
        material_path=material_path,
        patch_paths=patch_paths,
        baseline=baseline,
    )


def generate_all(out_root: str, **kwargs) -> dict[str, SampleBundle]:
    bundles: dict[str, SampleBundle] = {}
    for kind in TERRAIN_TYPES:
        bundles[kind] = generate_sample(
            os.path.join(out_root, kind), terrain_type=kind, **kwargs
        )
    index_path = os.path.join(out_root, "index.json")
    with open(index_path, "w", encoding="utf-8") as fh:
        json.dump(
            {k: {"dir": v.sample_dir, "baseline": v.baseline} for k, v in bundles.items()},
            fh,
            sort_keys=True,
            indent=2,
        )
    return bundles
