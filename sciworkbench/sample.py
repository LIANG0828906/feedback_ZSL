"""Deterministic synthetic sequences and baseline diagnostic bundles."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Iterable, List, Optional, Tuple

import numpy as np

from .engine import Workbench
from .factors import ErrorModel, FactorSet, NoiseParams
from .model import load_sequence, write_sequence
from .util import canonical_json_bytes, canonical_json_text, write_text_atomic


@dataclass(frozen=True)
class SampleProfile:
    name: str
    width: int
    height: int
    frames: int
    bad_density: float
    noise_sigma: float
    seed: int
    block_size: int = 16
    frac_bits: int = 12

    def canonical_dict(self) -> dict:
        return {
            "name": self.name,
            "width": int(self.width),
            "height": int(self.height),
            "frames": int(self.frames),
            "bad_density": float(self.bad_density),
            "noise_sigma": float(self.noise_sigma),
            "seed": int(self.seed),
            "block_size": int(self.block_size),
            "frac_bits": int(self.frac_bits),
        }


PROFILES: Dict[str, SampleProfile] = {
    "micro": SampleProfile(
        name="micro_64x48x4",
        width=64,
        height=48,
        frames=4,
        bad_density=0.002,
        noise_sigma=1.0,
        seed=1001,
        block_size=16,
        frac_bits=12,
    ),
    "fast": SampleProfile(
        name="fast_128x96x3",
        width=128,
        height=96,
        frames=3,
        bad_density=0.008,
        noise_sigma=2.5,
        seed=2002,
        block_size=24,
        frac_bits=11,
    ),
    "wide": SampleProfile(
        name="wide_96x72x6",
        width=96,
        height=72,
        frames=6,
        bad_density=0.004,
        noise_sigma=1.8,
        seed=3003,
        block_size=16,
        frac_bits=13,
    ),
}


def generate_frames_and_factors(
    profile: SampleProfile,
) -> Tuple[np.ndarray, FactorSet]:
    rng = np.random.default_rng(profile.seed)
    width = profile.width
    height = profile.height
    frames = profile.frames
    yy, xx = np.mgrid[0:height, 0:width].astype(np.float64)
    xn = xx / max(1.0, width - 1.0)
    yn = yy / max(1.0, height - 1.0)

    background = 70.0 + 28.0 * xn + 11.0 * yn
    output = np.empty((frames, height, width), dtype=np.uint16)
    for frame in range(frames):
        phase = (frame + 1.0) / (frames + 1.0)
        cx1 = width * (0.22 + 0.55 * phase)
        cy1 = height * (0.35 + 0.25 * math_sin(phase * 3.0))
        cx2 = width * (0.78 - 0.35 * phase)
        cy2 = height * (0.65 - 0.20 * math_cos(phase * 2.3))
        spot1 = 210.0 * np.exp(
            -((xx - cx1) ** 2 + (yy - cy1) ** 2) / (2.0 * (width * 0.075) ** 2)
        )
        spot2 = 95.0 * np.exp(
            -((xx - cx2) ** 2 + (yy - cy2) ** 2) / (2.0 * (width * 0.050) ** 2)
        )
        illumination = 5.0 * np.sin(xx * 0.09 + frame * 0.7)
        noise = rng.normal(0.0, profile.noise_sigma, size=(height, width))
        frame_values = np.rint(
            background + spot1 + spot2 + illumination + noise
        )
        output[frame] = np.clip(frame_values, 0, 65535).astype(np.uint16)

    bad_count = int(round(profile.bad_density * width * height))
    bad_mask = np.zeros((height, width), dtype=np.bool_)
    if bad_count > 0:
        selected = rng.choice(width * height, size=bad_count, replace=False)
        for index in selected:
            y, x = divmod(int(index), width)
            bad_mask[y, x] = True
            output[:, y, x] = 0 if int(index) % 2 == 0 else 65535

    dark = (
        9.0
        + 2.5 * xn
        + 1.5 * yn
        + 0.8 * np.sin(xx * 0.07)
        + rng.normal(0.0, 0.12, size=(height, width))
    ).astype(np.float64)
    flat = (
        1.0
        - 0.075 * ((xn - 0.48) ** 2)
        - 0.055 * ((yn - 0.52) ** 2)
        + 0.012 * np.cos(xx * 0.04 + yy * 0.03)
        + rng.normal(0.0, 0.0015, size=(height, width))
    ).astype(np.float64)
    flat = np.clip(flat, 0.55, 1.25)
    block_rows = (height + profile.block_size - 1) // profile.block_size
    block_cols = (width + profile.block_size - 1) // profile.block_size
    gy, gx = np.mgrid[0:block_rows, 0:block_cols].astype(np.float64)
    block_gain = (
        0.94
        + 0.045 * gx / max(1.0, block_cols - 1.0)
        + 0.030 * gy / max(1.0, block_rows - 1.0)
        + rng.normal(0.0, 0.004, size=(block_rows, block_cols))
    )
    block_gain = np.clip(block_gain, 0.70, 1.30)
    factors = FactorSet(
        dark=dark,
        flat=flat,
        bad_mask=bad_mask,
        block_gain=block_gain,
        noise=NoiseParams(sigma=0.85, coeff_bits=max(8, profile.frac_bits - 1)),
        error=ErrorModel(
            dark_error=0.35,
            flat_rel_error=1.5e-4,
            flat_floor=1.0e-3,
            threshold=8.0,
        ),
        frac_bits=profile.frac_bits,
    )
    return output, factors


def math_sin(value: float) -> float:
    import math

    return math.sin(value)


def math_cos(value: float) -> float:
    import math

    return math.cos(value)


def generate_sample_bundle(
    directory,
    profile: SampleProfile,
    *,
    write_baseline: bool = True,
    include_mode_comparison: bool = True,
) -> dict:
    from .factors import save_factors

    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    frames, factors = generate_frames_and_factors(profile)
    sequence_path = root / "sequence.simg"
    sequence = write_sequence(sequence_path, frames)
    sequence = load_sequence(sequence_path, block_size=profile.block_size)
    factors.validate(sequence.structure)
    calibration_path = root / "factors.cal"
    save_factors(calibration_path, factors, sequence.structure)
    profile_path = root / "profile.json"
    write_text_atomic(
        profile_path, canonical_json_text(profile.canonical_dict())
    )
    if not write_baseline:
        return {
            "profile": profile.canonical_dict(),
            "sequence": str(sequence_path),
            "calibration": str(calibration_path),
        }
    workbench = Workbench(
        block_size=profile.block_size,
        snapshot_root=root / "snapshots",
    )
    workbench.initialize(sequence, factors, "fixed")
    result = workbench.process()
    result.write_outputs(root / "baseline")
    baseline = result.canonical_summary()
    baseline["profile"] = profile.canonical_dict()
    baseline["sequence_file_sha256"] = _file_sha256(sequence_path)
    baseline["calibration_file_sha256"] = _file_sha256(calibration_path)
    write_text_atomic(root / "baseline_error_summary.json", canonical_json_text(baseline))
    comparison = (
        workbench.compare_modes() if include_mode_comparison else None
    )
    if comparison is not None:
        write_text_atomic(
            root / "mode_comparison.json", canonical_json_text(comparison)
        )
    return {
        "profile": profile.canonical_dict(),
        "result": result,
        "comparison": comparison,
        "directory": str(root),
    }


def generate_matrix(
    directory,
    *,
    profiles: Optional[Iterable[SampleProfile]] = None,
    include_mode_comparison: bool = True,
) -> List[dict]:
    root = Path(directory)
    root.mkdir(parents=True, exist_ok=True)
    selected = list(profiles) if profiles is not None else list(PROFILES.values())
    bundle = []
    index = []
    for profile in selected:
        item = generate_sample_bundle(
            root / profile.name,
            profile,
            include_mode_comparison=include_mode_comparison,
        )
        bundle.append(item)
        result = item.get("result")
        comparison = item.get("comparison")
        index.append(
            {
                "name": profile.name,
                "profile": profile.canonical_dict(),
                "overall_digest": result.overall_digest if result else None,
                "mean_abs_mode_diff": (
                    comparison["mean_abs_diff"] if comparison else None
                ),
                "max_abs_mode_diff": (
                    comparison["max_abs_diff"] if comparison else None
                ),
                "untrusted_pixels": (
                    int(result.untrusted.sum()) if result else None
                ),
            }
        )
    write_text_atomic(
        root / "matrix.json",
        canonical_json_text({"schema": 1, "profiles": index}),
    )
    return bundle


def _file_sha256(path: Path) -> str:
    import hashlib

    digest = hashlib.sha256()
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()
