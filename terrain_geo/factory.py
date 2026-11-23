"""Build typed stage objects from the deterministic sample descriptors."""

from __future__ import annotations

import numpy as np

from .resample import ResampleConfig
from .stages import (
    AffineStage,
    HomographyStage,
    RadialCorrectionStage,
)

STAGE_CLASSES = {
    "homography": HomographyStage,
    "radial_correction": RadialCorrectionStage,
    "affine": AffineStage,
}


def build_stages(specs: list, resample: ResampleConfig | None = None) -> list:
    cfg = resample or ResampleConfig()
    stages = []
    for spec in specs:
        cls = STAGE_CLASSES[spec["stage_id"]]
        params = _to_numpy(spec["params"])
        stages.append(cls(
            stage_id=spec["stage_id"],
            params=params,
            param_version=spec["param_version"],
            resample=ResampleConfig(
                tile_size=cfg.tile_size,
                order=cfg.order,
                max_overlap=cfg.max_overlap,
                max_pixels_per_thread=cfg.max_pixels_per_thread),
        ))
    return stages


def _to_numpy(params: dict) -> dict:
    out = dict(params)
    if "H" in out:
        out["H"] = np.asarray(out["H"], dtype=np.float64)
    return out
