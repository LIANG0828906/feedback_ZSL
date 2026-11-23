"""Deterministic local sample generator (fixed seed, no network).

Outputs under the work directory::

    samples/terrain_gray.png     single-channel terrain surface
    samples/terrain_rgb.png      three-channel terrain surface
    samples/stages.json          stage parameters + versions
    samples/truth.json           control-point ground truth, distortion info
"""

from __future__ import annotations

import json
import os

import numpy as np

from .geometry import (
    affine_forward,
    apply_homography,
    distort_points,
    undistort_points,
)
from .io_utils import save_image

SEED = 20261123
SRC_H, SRC_W = 192, 256
OUT_H, OUT_W = 160, 200


def _terrain_rgb(seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    yy, xx = np.meshgrid(np.linspace(0, 1, SRC_H),
                         np.linspace(0, 1, SRC_W), indexing="ij")
    bands = []
    for channel in range(3):
        field = np.zeros_like(xx)
        for k in range(6):
            fx = rng.uniform(1.0, 6.0)
            fy = rng.uniform(1.0, 6.0)
            phase = rng.uniform(0, 2 * np.pi)
            amp = rng.uniform(0.08, 0.22)
            field += amp * np.sin(2 * np.pi * fx * xx + phase) * \
                np.cos(2 * np.pi * fy * yy + phase * 0.7)
        ridges = np.clip(
            60 * np.sin(8 * xx + 5 * yy) + 40 * np.cos(11 * xx - 3 * yy),
            -80, 80)
        base = 90 + channel * 25
        band = base + field * 255 + ridges
        bands.append(np.clip(band, 0, 255))
    img = np.stack(bands, axis=-1).astype(np.uint8)
    # Deterministic fine grain so resampling differences are visible.
    grain = rng.integers(0, 12, size=img.shape, dtype=np.int16)
    return np.clip(img.astype(np.int16) + grain, 0, 255).astype(np.uint8)


def build_stage_params():
    # Stage 1: mild perspective homography mapping the source frame inward.
    H = np.array([
        [0.82, 0.06, 18.0],
        [-0.05, 0.88, 14.0],
        [-0.00035, 0.00022, 1.0],
    ], dtype=np.float64)
    homography = {
        "stage_id": "homography",
        "param_version": "h-1.0.0",
        "params": {
            "H": H.tolist(),
            "output_size": [SRC_H, SRC_W],
        },
    }
    # Stage 2: radial distortion correction in the same frame dimensions.
    radial = {
        "stage_id": "radial_correction",
        "param_version": "r-1.0.0",
        "params": {
            "center": [SRC_W * 0.5, SRC_H * 0.52],
            "radius": float(min(SRC_H, SRC_W) * 0.62),
            "k1": 0.18,
            "k2": 0.04,
            "output_size": [SRC_H, SRC_W],
        },
    }
    # Stage 3: scale + translate to the final orthophoto size.
    sx = OUT_W / SRC_W
    sy = OUT_H / SRC_H
    affine = {
        "stage_id": "affine",
        "param_version": "a-1.0.0",
        "params": {
            "scale": [sx, sy],
            "translate": [0.0, 0.0],
            "output_size": [OUT_H, OUT_W],
        },
    }
    return [homography, radial, affine], (H, radial["params"], affine["params"])


def _control_points(n: int = 5, margin: float = 26.0):
    # Points are defined in the final frame and back-projected to the source,
    # guaranteeing they stay inside every frame.
    xs = np.linspace(margin, OUT_W - margin, n)
    ys = np.linspace(margin, OUT_H - margin, n)
    gx, gy = np.meshgrid(xs, ys)
    return np.column_stack([gx.ravel(), gy.ravel()])


def generate(workdir: str):
    out_dir = os.path.join(workdir, "samples")
    os.makedirs(out_dir, exist_ok=True)
    rgb = _terrain_rgb(SEED)
    gray = np.asarray(
        np.rint(0.299 * rgb[..., 0] + 0.587 * rgb[..., 1]
                + 0.114 * rgb[..., 2]),
        dtype=np.uint8)
    rgb_path = os.path.join(out_dir, "terrain_rgb.png")
    gray_path = os.path.join(out_dir, "terrain_gray.png")
    save_image(rgb_path, rgb)
    save_image(gray_path, gray)
    stage_specs, (H, radial_p, affine_p) = build_stage_params()
    final_pts = _control_points()
    # Back-project final -> undistorted radial frame -> distorted frame -> src.
    undist = (final_pts - np.asarray(affine_p["translate"])) / \
        np.asarray(affine_p["scale"])
    distorted = distort_points(
        undist, radial_p["center"], radial_p["radius"],
        radial_p["k1"], radial_p["k2"])
    Hinv = np.linalg.inv(H)
    source_pts = apply_homography(Hinv, distorted)
    # Forward analytic truth at every stage boundary.
    after_h = apply_homography(H, source_pts)
    after_r = undistort_points(
        after_h, radial_p["center"], radial_p["radius"],
        radial_p["k1"], radial_p["k2"])
    after_a = affine_forward(after_r, affine_p["scale"],
                             affine_p["translate"])
    truth = {
        "seed": SEED,
        "points_source": source_pts.tolist(),
        "points_final": final_pts.tolist(),
        "points_by_stage": {
            "homography": after_h.tolist(),
            "radial_correction": after_r.tolist(),
            "affine": after_a.tolist(),
        },
        "distortion": {
            "center": list(radial_p["center"]),
            "radius": radial_p["radius"],
            "k1": radial_p["k1"],
            "k2": radial_p["k2"],
        },
        "control_point_ids": list(range(len(final_pts))),
    }
    with open(os.path.join(out_dir, "stages.json"), "w",
              encoding="utf-8") as fh:
        json.dump({"seed": SEED, "stages": stage_specs}, fh, indent=2)
    with open(os.path.join(out_dir, "truth.json"), "w",
              encoding="utf-8") as fh:
        json.dump(truth, fh, indent=2)
    return {
        "rgb_path": rgb_path,
        "gray_path": gray_path,
        "stages_path": os.path.join(out_dir, "stages.json"),
        "truth_path": os.path.join(out_dir, "truth.json"),
        "stage_specs": stage_specs,
        "truth": truth,
    }


def load_sample_bundle(workdir: str):
    with open(os.path.join(workdir, "samples", "stages.json"),
              encoding="utf-8") as fh:
        specs = json.load(fh)
    with open(os.path.join(workdir, "samples", "truth.json"),
              encoding="utf-8") as fh:
        truth = json.load(fh)
    return specs, truth
