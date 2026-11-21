"""Spectral material import, validation and normalization.

Reads local JSON material descriptions:

* ``spectral``    -- spectrophotometer export: wavelength samples, sample
  interval and reflectance values.
* ``srgb``        -- existing sRGB material (transfer-encoded RGB triple).
* ``display-p3``  -- existing Display-P3 material (transfer-encoded RGB triple).

Spectral curves are validated (strictly increasing wavelengths, non-zero
sample interval, reflectance within [0, 1]) and resampled onto the unified
wavelength grid.  RGB materials are spectrally uplifted onto the same grid
via a fixed three-basis least-squares reconstruction so that every material
shares the same observer color-matching functions and adaptation matrices
downstream.  The reconstruction is deterministic: it uses fixed reference
conditions (D65-like reference illuminant, 2-degree observer) and fixed
basis spectra.
"""

from __future__ import annotations

import json
import math

import numpy as np

from .canon import content_hash
from .errors import ErrorCode, SpectralStudioError

GRID_START_NM = 380.0
GRID_END_NM = 780.0
GRID_STEP_NM = 5.0

REFLECTANCE_MIN = 0.0
REFLECTANCE_MAX = 1.0

KIND_SPECTRAL = "spectral"
KIND_SRGB = "srgb"
KIND_DISPLAY_P3 = "display-p3"
KNOWN_KINDS = (KIND_SPECTRAL, KIND_SRGB, KIND_DISPLAY_P3)


def unified_grid() -> np.ndarray:
    count = int(round((GRID_END_NM - GRID_START_NM) / GRID_STEP_NM)) + 1
    return GRID_START_NM + GRID_STEP_NM * np.arange(count, dtype=np.float64)


def _fail(message, detail=None):
    raise SpectralStudioError(ErrorCode.E_SPECTRUM_INVALID, message, detail)


def load_material_document(source):
    """Load a material JSON document from a path or parse a dict directly."""
    if isinstance(source, dict):
        doc = source
    else:
        with open(source, "r", encoding="utf-8") as handle:
            doc = json.load(handle)
    if not isinstance(doc, dict):
        _fail("material document must be a JSON object")
    return doc


def validate_spectral_curve(wavelengths, interval_nm, reflectance):
    """Validate a spectral reflectance curve; returns numpy arrays."""
    wl = np.asarray(wavelengths, dtype=np.float64)
    refl = np.asarray(reflectance, dtype=np.float64)
    if wl.ndim != 1 or refl.ndim != 1 or wl.size != refl.size or wl.size < 2:
        _fail("wavelength/reflectance arrays must be 1-D, equal length >= 2",
              {"samples": int(wl.size)})
    if not (np.all(np.isfinite(wl)) and np.all(np.isfinite(refl))):
        _fail("wavelength/reflectance contain non-finite values")
    if interval_nm is None or not math.isfinite(float(interval_nm)) or float(interval_nm) <= 0.0:
        _fail("sample interval must be a positive finite number",
              {"interval_nm": interval_nm})
    diffs = np.diff(wl)
    if not np.all(diffs > 0.0):
        _fail("wavelengths must be strictly increasing",
              {"first_bad_index": int(np.argmax(diffs <= 0.0)) + 1})
    if np.any(refl < REFLECTANCE_MIN) or np.any(refl > REFLECTANCE_MAX):
        bad = int(np.argmax((refl < REFLECTANCE_MIN) | (refl > REFLECTANCE_MAX)))
        _fail("reflectance out of range [0, 1]",
              {"first_bad_index": bad, "value": float(refl[bad])})
    return wl, refl


def resample_to_grid(wavelengths, reflectance):
    """Linearly resample a validated curve onto the unified grid."""
    grid = unified_grid()
    resampled = np.interp(grid, wavelengths, reflectance,
                          left=float(reflectance[0]), right=float(reflectance[-1]))
    return grid, np.clip(resampled, REFLECTANCE_MIN, REFLECTANCE_MAX)


def _gaussian(x, mu, sigma_left, sigma_right):
    sigma = np.where(x < mu, sigma_left, sigma_right)
    return np.exp(-0.5 * ((x - mu) / sigma) ** 2)


def uplift_basis() -> np.ndarray:
    """Fixed smooth basis spectra (columns) for RGB spectral uplift."""
    grid = unified_grid()
    centers = np.linspace(400.0, 760.0, 9)
    return np.stack([np.exp(-0.5 * ((grid - c) / 28.0) ** 2)
                     for c in centers], axis=1)


def uplift_rgb_to_spectrum(xyz_target, reference_illuminant, reference_cmf):
    """Reconstruct a reflectance in [0,1] matching *xyz_target* under the
    fixed reference conditions.

    Deterministic iterative bounded least squares: saturated display colors
    cannot be matched exactly by a reflectance in [0, 1] (a physical
    constraint, not a numerical one), so chromaticity is matched closely
    and luminance is compressed toward the perfect-diffuser limit.
    """
    grid = unified_grid()
    basis = uplift_basis()
    norm = np.trapezoid(reference_illuminant * reference_cmf[:, 1], grid)
    weights = reference_illuminant[:, None] * reference_cmf / norm  # (N, 3)
    design = basis.T @ weights  # (3 bases, 3 xyz)
    coeff = np.zeros(basis.shape[1], dtype=np.float64)
    target = np.asarray(xyz_target, dtype=np.float64)
    for _ in range(6):
        residual = target - design.T @ coeff
        delta, *_ = np.linalg.lstsq(design.T, residual, rcond=None)
        coeff = np.maximum(coeff + delta, 0.0)
        raw = basis @ coeff
        peak = float(np.max(raw))
        if peak > 1.0:
            coeff *= 1.0 / peak
    spectrum = basis @ coeff
    return grid, np.clip(spectrum, REFLECTANCE_MIN, REFLECTANCE_MAX)


class MaterialSpec:
    """Validated, normalized material ready for the color pipeline."""

    def __init__(self, material_id, kind, grid, reflectance, source_desc):
        self.material_id = material_id
        self.kind = kind
        self.grid = np.asarray(grid, dtype=np.float64)
        self.reflectance = np.asarray(reflectance, dtype=np.float64)
        self.source_desc = source_desc  # canonical description of original params
        self.content_hash = content_hash({
            "material_id": material_id,
            "kind": kind,
            "grid": [float(v) for v in self.grid],
            "reflectance": [float(v) for v in self.reflectance],
            "source": source_desc,
        })

    @property
    def resampled_reference(self):
        """Resampled reference values on the unified grid (rounded)."""
        return [float(v) for v in self.reflectance]

    def to_params(self):
        return dict(self.source_desc)


def import_material(doc, reference_illuminant=None, reference_cmf=None):
    """Validate and normalize one material document into a MaterialSpec.

    RGB kinds require *reference_illuminant* and *reference_cmf* for the
    deterministic spectral uplift.
    """
    doc = load_material_document(doc)
    material_id = doc.get("id")
    kind = doc.get("kind")
    if not material_id or not isinstance(material_id, str):
        _fail("material document missing string 'id'")
    if kind not in KNOWN_KINDS:
        _fail("unknown material kind", {"kind": kind})

    if kind == KIND_SPECTRAL:
        spectral = doc.get("spectral")
        if not isinstance(spectral, dict):
            _fail("spectral material missing 'spectral' object")
        wl, refl = validate_spectral_curve(
            spectral.get("wavelengths_nm"),
            spectral.get("interval_nm"),
            spectral.get("reflectance"),
        )
        grid, resampled = resample_to_grid(wl, refl)
        source_desc = {
            "kind": kind,
            "spectral": {
                "wavelengths_nm": [float(v) for v in wl],
                "interval_nm": float(spectral["interval_nm"]),
                "reflectance": [float(v) for v in refl],
            },
        }
        return MaterialSpec(material_id, kind, grid, resampled, source_desc)

    # RGB kinds: decode + spectral uplift under fixed reference conditions.
    from .colorimetry import decode_transfer, rgb_to_xyz_matrix  # local import

    rgb = doc.get("rgb")
    if (not isinstance(rgb, (list, tuple)) or len(rgb) != 3
            or not all(isinstance(v, (int, float)) and math.isfinite(v) for v in rgb)):
        _fail("RGB material requires 'rgb' triple of finite numbers")
    if any(v < 0.0 or v > 1.0 for v in rgb):
        _fail("RGB values out of range [0, 1]", {"rgb": list(rgb)})
    if reference_illuminant is None or reference_cmf is None:
        _fail("RGB uplift requires reference illuminant and CMF")
    linear = decode_transfer(np.asarray(rgb, dtype=np.float64), kind)
    xyz = rgb_to_xyz_matrix(kind) @ linear
    grid, spectrum = uplift_rgb_to_spectrum(xyz, reference_illuminant, reference_cmf)
    source_desc = {"kind": kind, "rgb": [float(v) for v in rgb]}
    return MaterialSpec(material_id, kind, grid, spectrum, source_desc)
