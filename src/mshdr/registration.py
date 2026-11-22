"""Deterministic subpixel band registration."""

from __future__ import annotations

from typing import Any

import numpy as np
from scipy import ndimage

from .errors import WorkbenchError, require


def validate_registration_offsets(
    offsets_px: np.ndarray,
    *,
    reference_band: int,
    max_shift_px: float,
) -> None:
    offsets = np.asarray(offsets_px, dtype=np.float64)
    require(
        offsets.ndim == 2 and offsets.shape[1] == 2,
        "E_META",
        "Registration offsets must have shape (band_count, 2)",
        field="truth.registration_offsets_px",
    )
    for band_index, pair in enumerate(offsets):
        dy, dx = float(pair[0]), float(pair[1])
        if not np.isfinite(dy) or not np.isfinite(dx):
            raise WorkbenchError(
                "E_REG",
                "Registration offset is not finite",
                field=f"truth.registration_offsets_px[{band_index}]",
                details={"band_pair": [int(reference_band), band_index], "measured_offset": [dy, dx]},
            )
        if max(abs(dy), abs(dx)) > float(max_shift_px) + 1.0e-12:
            raise WorkbenchError(
                "E_REG",
                "Registration offset exceeds the configured correction range",
                field=f"truth.registration_offsets_px[{band_index}]",
                details={
                    "band_pair": [int(reference_band), band_index],
                    "measured_offset": [dy, dx],
                    "max_registration_shift_px": float(max_shift_px),
                },
            )


def align_bands(
    bands: np.ndarray,
    offsets_px: np.ndarray,
    *,
    reference_band: int,
    interpolation_order: int,
    max_shift_px: float,
) -> tuple[np.ndarray, dict[str, Any]]:
    """Align band content to the reference band using the negative truth offset."""

    source = np.asarray(bands, dtype=np.float64)
    require(
        source.ndim == 3,
        "E_META",
        "Band image stack must have shape (band_count, height, width)",
        field="bands",
    )
    require(
        isinstance(reference_band, int) and 0 <= reference_band < source.shape[0],
        "E_META",
        "Reference band index is out of range",
        field="reference_band",
    )
    require(
        isinstance(interpolation_order, int) and 0 <= interpolation_order <= 5,
        "E_PARA",
        "interpolation_order must be an integer in [0, 5]",
        field="interpolation_order",
    )
    offsets = np.asarray(offsets_px, dtype=np.float64)
    require(
        offsets.shape == (source.shape[0], 2),
        "E_META",
        "Registration offsets must contain one pair per band",
        field="truth.registration_offsets_px",
    )
    validate_registration_offsets(offsets, reference_band=reference_band, max_shift_px=max_shift_px)

    aligned = np.empty_like(source, dtype=np.float64)
    for band_index in range(source.shape[0]):
        dy, dx = (float(-offsets[band_index, 0]), float(-offsets[band_index, 1]))
        aligned[band_index] = ndimage.shift(
            source[band_index],
            shift=(dy, dx),
            order=interpolation_order,
            mode="reflect",
            prefilter=interpolation_order > 1,
        )
    if aligned.shape[1:] != source[reference_band].shape:
        raise WorkbenchError(
            "E_REG",
            "Aligned band dimensions do not match the reference band",
            field="registration",
            details={"expected": list(source[reference_band].shape), "actual": list(aligned.shape[1:])},
        )
    return aligned, {
        "interpolation_order": interpolation_order,
        "reference_band": reference_band,
        "offsets_px": offsets.tolist(),
        "aligned_shape": list(aligned.shape),
    }

