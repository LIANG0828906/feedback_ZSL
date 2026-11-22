"""Deterministic scientific imaging sequence sample generator."""

from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .models import FeatureSpec

GENERATOR_VERSION = "sample-generator-1.0.0"
SCHEMA_VERSION = "1.0"


@dataclass(frozen=True)
class GroupSpec:
    name: str
    height: int
    width: int
    frame_count: int
    seed: int
    noise_min: float
    noise_max: float
    max_shift: float


DEFAULT_GROUPS: tuple[GroupSpec, ...] = (
    GroupSpec("small", 320, 384, 24, 1101, 3.0, 7.0, 3.2),
    GroupSpec("medium", 512, 640, 56, 2202, 5.0, 11.0, 4.2),
    GroupSpec("large", 768, 1024, 96, 3303, 7.0, 16.0, 5.0),
)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, sort_keys=True, allow_nan=False)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _atomic_npy(path: Path, array: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    with temporary.open("wb") as handle:
        np.save(handle, array, allow_pickle=False)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def fourier_shift(image: np.ndarray, dx: float, dy: float) -> np.ndarray:
    """Translate image content by (dx, dy) pixels using a circular Fourier shift."""

    height, width = image.shape
    fy = np.fft.fftfreq(height)[:, None]
    fx = np.fft.fftfreq(width)[None, :]
    phase = np.exp(-2j * np.pi * (fx * float(dx) + fy * float(dy)))
    shifted = np.fft.ifft2(np.fft.fft2(image) * phase).real
    return shifted.astype(np.float32, copy=False)


def _distance_to_segment(
    x: np.ndarray,
    y: np.ndarray,
    start: tuple[float, float],
    end: tuple[float, float],
) -> np.ndarray:
    x0, y0 = start
    x1, y1 = end
    vx = x1 - x0
    vy = y1 - y0
    length_sq = vx * vx + vy * vy
    projection = np.clip(((x - x0) * vx + (y - y0) * vy) / length_sq, 0.0, 1.0)
    nearest_x = x0 + projection * vx
    nearest_y = y0 + projection * vy
    return np.hypot(x - nearest_x, y - nearest_y)


def _make_base(
    height: int,
    width: int,
    rng: np.random.Generator,
) -> tuple[np.ndarray, tuple[FeatureSpec, ...]]:
    x = np.arange(width, dtype=np.float64)[None, :]
    y = np.arange(height, dtype=np.float64)[:, None]
    background = 100.0 + 1.8 * np.sin(2.0 * np.pi * x / width) + 1.1 * np.cos(
        2.0 * np.pi * y / height
    )
    image = np.broadcast_to(background, (height, width)).copy()

    jitter = min(0.012 * min(height, width), 10.0)
    spot_layout = ((0.20, 0.25), (0.73, 0.29), (0.36, 0.73))
    features: list[FeatureSpec] = []

    for index, (fx, fy) in enumerate(spot_layout):
        cx = float(fx * width + rng.uniform(-jitter, jitter))
        cy = float(fy * height + rng.uniform(-jitter, jitter))
        sigma = float(rng.uniform(2.8, 5.6))
        amplitude = float(rng.uniform(220.0, 360.0))
        image += amplitude * np.exp(-((x - cx) ** 2 + (y - cy) ** 2) / (2.0 * sigma**2))
        features.append(
            FeatureSpec(
                feature_id=f"spot_{index}",
                kind="spot",
                center=(cx, cy),
                sigma=sigma,
                amplitude=amplitude,
            )
        )

    line_layout = (
        ((0.58, 0.62), (0.84, 0.52)),
        ((0.14, 0.51), (0.28, 0.88)),
    )
    for index, (start_fraction, end_fraction) in enumerate(line_layout):
        start = (
            float(start_fraction[0] * width + rng.uniform(-jitter, jitter)),
            float(start_fraction[1] * height + rng.uniform(-jitter, jitter)),
        )
        end = (
            float(end_fraction[0] * width + rng.uniform(-jitter, jitter)),
            float(end_fraction[1] * height + rng.uniform(-jitter, jitter)),
        )
        sigma = float(rng.uniform(1.35, 2.15))
        amplitude = float(rng.uniform(160.0, 250.0))
        distance = _distance_to_segment(x, y, start, end)
        image += amplitude * np.exp(-(distance**2) / (2.0 * sigma**2))
        center = ((start[0] + end[0]) / 2.0, (start[1] + end[1]) / 2.0)
        features.append(
            FeatureSpec(
                feature_id=f"line_{index}",
                kind="line",
                center=center,
                sigma=sigma,
                amplitude=amplitude,
                endpoints=(start, end),
            )
        )

    return image.astype(np.float32), tuple(features)


def _make_shifts(
    frame_count: int,
    max_shift: float,
    rng: np.random.Generator,
) -> np.ndarray:
    shifts = np.zeros((frame_count, 2), dtype=np.float64)
    for index in range(1, frame_count):
        phase = 2.0 * np.pi * index / max(frame_count - 1, 1)
        dx = 0.72 * max_shift * math.sin(1.15 * phase) + rng.normal(0.0, 0.06 * max_shift)
        dy = 0.64 * max_shift * math.cos(0.83 * phase) + rng.normal(0.0, 0.06 * max_shift)
        radius = math.hypot(dx, dy)
        limit = 0.92 * max_shift
        if radius > limit:
            dx *= limit / radius
            dy *= limit / radius
        shifts[index] = (dx, dy)
    return shifts


def _feature_frame_coordinates(
    features: Iterable[FeatureSpec],
    dx: float,
    dy: float,
) -> dict[str, tuple[float, float]]:
    return {
        feature.feature_id: (
            float(feature.center[0] + dx),
            float(feature.center[1] + dy),
        )
        for feature in features
    }


def generate_group(
    output_root: Path | str,
    spec: GroupSpec,
    *,
    overwrite: bool = False,
) -> Path:
    group_root = Path(output_root) / spec.name
    manifest_path = group_root / "manifest.json"
    if manifest_path.exists() and not overwrite:
        raise FileExistsError(f"sample already exists: {group_root}")

    group_root.mkdir(parents=True, exist_ok=True)
    frames_dir = group_root / "frames"
    frames_dir.mkdir(parents=True, exist_ok=True)
    rng = np.random.default_rng(spec.seed)

    clean_base, features = _make_base(spec.height, spec.width, rng)
    clean_base_path = group_root / "clean_base.npy"
    _atomic_npy(clean_base_path, clean_base)

    shifts = _make_shifts(spec.frame_count, spec.max_shift, rng)
    noise_sigmas = np.zeros(spec.frame_count, dtype=np.float64)
    frame_files: list[str] = []
    frame_hashes: list[str] = []
    frame_features: list[dict[str, tuple[float, float]]] = []

    for index in range(spec.frame_count):
        fraction = index / max(spec.frame_count - 1, 1)
        noise_sigma = spec.noise_min + (spec.noise_max - spec.noise_min) * fraction
        noise_sigma *= 1.0 + rng.normal(0.0, 0.025)
        noise_sigmas[index] = noise_sigma
        dx, dy = float(shifts[index, 0]), float(shifts[index, 1])
        clean_frame = fourier_shift(clean_base, dx, dy)
        noisy = clean_frame + rng.normal(0.0, noise_sigma, clean_frame.shape).astype(np.float32)
        noisy = np.ascontiguousarray(noisy, dtype=np.float32)

        relative_path = Path("frames") / f"frame_{index:06d}.npy"
        frame_path = group_root / relative_path
        _atomic_npy(frame_path, noisy)
        frame_files.append(relative_path.as_posix())
        frame_hashes.append(_sha256_file(frame_path))
        frame_features.append(_feature_frame_coordinates(features, dx, dy))

    truth = {
        "schema_version": SCHEMA_VERSION,
        "sequence_id": spec.name,
        "reference_frame": 0,
        "shifts": [
            {"dx": float(shift[0]), "dy": float(shift[1])} for shift in shifts
        ],
        "noise_sigmas": [float(value) for value in noise_sigmas],
        "noise_model": "per-frame additive Gaussian read noise",
        "features": [
            {
                "feature_id": feature.feature_id,
                "kind": feature.kind,
                "center": [float(feature.center[0]), float(feature.center[1])],
                "sigma": float(feature.sigma),
                "amplitude": float(feature.amplitude),
                "endpoints": (
                    [
                        [float(feature.endpoints[0][0]), float(feature.endpoints[0][1])],
                        [float(feature.endpoints[1][0]), float(feature.endpoints[1][1])],
                    ]
                    if feature.endpoints is not None
                    else None
                ),
            }
            for feature in features
        ],
        "frame_features": [
            {
                feature_id: [float(center[0]), float(center[1])]
                for feature_id, center in frame_map.items()
            }
            for frame_map in frame_features
        ],
        "clean_base_file": "clean_base.npy",
        "clean_base_sha256": _sha256_file(clean_base_path),
    }
    truth_path = group_root / "truth.json"
    _atomic_json(truth_path, truth)

    metadata = {
        "schema_version": SCHEMA_VERSION,
        "generator_version": GENERATOR_VERSION,
        "sequence_id": spec.name,
        "seed": spec.seed,
        "height": spec.height,
        "width": spec.width,
        "dtype": "float32",
        "frame_count": spec.frame_count,
        "reference_frame": 0,
        "frame_files": frame_files,
        "frame_sha256": frame_hashes,
        "truth_file": "truth.json",
        "truth_sha256": _sha256_file(truth_path),
        "clean_base_file": "clean_base.npy",
        "clean_base_sha256": truth["clean_base_sha256"],
        "noise_min": float(spec.noise_min),
        "noise_max": float(spec.noise_max),
        "max_truth_shift": float(spec.max_shift),
    }
    _atomic_json(manifest_path, metadata)
    return group_root


def generate_all(
    output_root: Path | str,
    *,
    groups: Iterable[GroupSpec] = DEFAULT_GROUPS,
    overwrite: bool = False,
) -> list[Path]:
    return [generate_group(output_root, group, overwrite=overwrite) for group in groups]

