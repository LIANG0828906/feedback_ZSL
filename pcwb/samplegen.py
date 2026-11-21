"""确定性样本生成器。

相同种子生成逐字节一致的点云文件：坐标由 RandomState(seed) 产生，
颜色为坐标的确定性函数。生成 10 万点级局部碎片与 500 万点级整件模型，
并输出基线版本哈希（内容哈希 = 文件点数据区的 SHA-256）。
"""

from __future__ import annotations

import json
import os

import numpy as np

from .pcformat import PointCloud, write

BBOX_MIN = np.array([0.0, 0.0, 0.0], dtype=np.float32)
BBOX_MAX = np.array([100.0, 100.0, 100.0], dtype=np.float32)

SAMPLES = {
    "fragment_100k": {"count": 100_000, "seed": 20261122},
    "full_5m": {"count": 5_000_000, "seed": 20261123},
}


def _colors_from_points(points: np.ndarray) -> np.ndarray:
    span = (BBOX_MAX - BBOX_MIN).astype(np.float32)
    t = (points - BBOX_MIN) / span
    rgb = np.floor(t * 255.0).astype(np.int32)
    return np.clip(rgb, 0, 255).astype(np.uint8)


def generate_cloud(count: int, seed: int) -> PointCloud:
    rng = np.random.RandomState(seed)
    centers = np.array([
        [20.0, 30.0, 40.0], [60.0, 25.0, 70.0], [80.0, 60.0, 30.0],
        [35.0, 75.0, 55.0], [50.0, 50.0, 85.0], [15.0, 60.0, 15.0],
        [70.0, 80.0, 75.0], [45.0, 15.0, 20.0],
    ], dtype=np.float64)
    cluster = rng.randint(0, centers.shape[0], size=count)
    spread = 2.0 + (cluster % 3).astype(np.float64)
    pts = centers[cluster] + rng.standard_normal((count, 3)) * spread[:, None]
    shell_count = count // 20
    if shell_count > 0:
        side = int(np.ceil(shell_count ** (1.0 / 3.0)))
        lin = np.linspace(0.0, 100.0, side, dtype=np.float64)
        gx, gy, gz = np.meshgrid(lin, lin, lin, indexing="ij")
        grid = np.stack([gx.ravel(), gy.ravel(), gz.ravel()], axis=1)[:shell_count]
        pts[:shell_count] = grid
    pts = np.clip(pts, BBOX_MIN, BBOX_MAX).astype(np.float32)
    colors = _colors_from_points(pts)
    return PointCloud(pts, colors)


def generate_samples(samples_dir: str) -> dict:
    """生成全部样本与基线哈希清单，返回 {名称: 哈希}。幂等：已存在则复用。"""
    os.makedirs(samples_dir, exist_ok=True)
    manifest_path = os.path.join(samples_dir, "baseline_hashes.json")
    hashes = {}
    for name, spec in SAMPLES.items():
        path = os.path.join(samples_dir, f"{name}.pcwb")
        if os.path.exists(path) and os.path.exists(manifest_path):
            with open(manifest_path, "r", encoding="utf-8") as fh:
                hashes = json.load(fh)
            if name in hashes:
                continue
        cloud = generate_cloud(spec["count"], spec["seed"])
        hashes[name] = write(path, cloud)
    with open(manifest_path, "w", encoding="utf-8") as fh:
        json.dump(hashes, fh, indent=2, sort_keys=True)
        fh.write("\n")
    return hashes
