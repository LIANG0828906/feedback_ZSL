"""Spectral material import, validation and normalisation (requirement 1)."""
from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .config import GridSpec
from .errors import SpectrumInvalid

REFLECTANCE_MIN = 0.0
REFLECTANCE_MAX = 1.0

MATERIAL_TYPES = ("spectral", "srgb", "display-p3")


@dataclass
class Material:
    material_id: str
    material_index: int
    type: str
    # spectral materials: reflectance resampled on the unified grid
    reflectance: np.ndarray | None = None
    norm_reference: float | None = None
    # rgb materials: linearised RGB in their native primaries
    rgb: tuple | None = None
    raw: dict | None = None


def _validate_curve(material_id: str, wavelengths, values) -> None:
    if len(wavelengths) != len(values):
        raise SpectrumInvalid(
            "wavelength/value length mismatch",
            material_id=material_id,
            sample_index=min(len(wavelengths), len(values)),
        )
    if len(wavelengths) < 2:
        raise SpectrumInvalid("curve needs at least 2 samples", material_id=material_id, sample_index=0)
    wl = np.asarray(wavelengths, dtype=np.float64)
    val = np.asarray(values, dtype=np.float64)
    diffs = np.diff(wl)
    bad = np.nonzero(diffs <= 0)[0]
    if bad.size:
        idx = int(bad[0]) + 1
        raise SpectrumInvalid(
            "wavelengths not strictly increasing",
            material_id=material_id,
            sample_index=idx,
            wavelength_nm=float(wl[idx]),
        )
    low = np.nonzero(val < REFLECTANCE_MIN)[0]
    high = np.nonzero(val > REFLECTANCE_MAX)[0]
    if low.size:
        idx = int(low[0])
        raise SpectrumInvalid(
            "reflectance below range",
            material_id=material_id,
            sample_index=idx,
            wavelength_nm=float(wl[idx]),
            value=float(val[idx]),
        )
    if high.size:
        idx = int(high[0])
        raise SpectrumInvalid(
            "reflectance above range",
            material_id=material_id,
            sample_index=idx,
            wavelength_nm=float(wl[idx]),
            value=float(val[idx]),
        )


def resample_to_grid(material_id: str, wavelengths, values, grid: GridSpec):
    """Resample onto the unified grid; missing segments -> E_SPECTRUM_INVALID."""
    _validate_curve(material_id, wavelengths, values)
    wl = np.asarray(wavelengths, dtype=np.float64)
    val = np.asarray(values, dtype=np.float64)
    grid_wl = grid.start_nm + grid.step_nm * np.arange(grid.sample_count, dtype=np.float64)
    # Locate the first grid point not covered by the source curve (缺段).
    uncovered = np.nonzero((grid_wl < wl[0]) | (grid_wl > wl[-1]))[0]
    if uncovered.size:
        idx = int(uncovered[0])
        raise SpectrumInvalid(
            "curve does not cover unified grid (missing segment)",
            material_id=material_id,
            sample_index=idx,
            wavelength_nm=float(grid_wl[idx]),
        )
    resampled = np.interp(grid_wl, wl, val)
    norm_reference = float(np.max(resampled))
    return resampled, norm_reference


def _srgb_transfer_decode(c: np.ndarray) -> np.ndarray:
    return np.where(c <= 0.04045, c / 12.92, ((c + 0.055) / 1.055) ** 2.4)


def load_material(material_index: int, raw: dict, grid: GridSpec) -> Material:
    material_id = str(raw.get("id", f"m{material_index}"))
    mtype = raw.get("type")
    if mtype not in MATERIAL_TYPES:
        raise SpectrumInvalid("unknown material type", material_id=material_id, sample_index=0, type=mtype)
    if mtype == "spectral":
        reflectance, norm_ref = resample_to_grid(
            material_id, raw["wavelengths_nm"], raw["reflectance"], grid
        )
        return Material(
            material_id=material_id,
            material_index=material_index,
            type=mtype,
            reflectance=reflectance,
            norm_reference=norm_ref,
            raw=raw,
        )
    color = np.asarray(raw["color"], dtype=np.float64)
    if color.shape != (3,) or np.any(color < 0.0) or np.any(color > 1.0):
        bad = int(np.nonzero((color < 0.0) | (color > 1.0))[0][0]) if color.shape == (3,) else 0
        raise SpectrumInvalid(
            "rgb material color out of range",
            material_id=material_id,
            sample_index=bad,
        )
    linear = _srgb_transfer_decode(color)  # both sRGB and Display-P3 use the sRGB transfer
    return Material(
        material_id=material_id,
        material_index=material_index,
        type=mtype,
        rgb=tuple(float(x) for x in linear),
        raw=raw,
    )


def load_materials(path, grid: GridSpec) -> list[Material]:
    raw_list = json.loads(Path(path).read_text(encoding="utf-8"))
    return [load_material(i, raw, grid) for i, raw in enumerate(raw_list)]
