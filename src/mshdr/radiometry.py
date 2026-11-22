"""Inverse camera response linearization and HDR exposure fusion."""

from __future__ import annotations

from typing import Any

import numpy as np

from .errors import WorkbenchError, require


def validate_exposure_sequence(exposure_times: np.ndarray) -> None:
    times = np.asarray(exposure_times, dtype=np.float64)
    require(times.ndim == 1 and times.size >= 1, "E_META", "Exposure times must be a non-empty vector", field="exposures")
    for index, value in enumerate(times):
        if not np.isfinite(value) or value <= 0.0:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure times must be finite and positive",
                field=f"exposures.frames[{index}].exposure_time",
                details={"index": index, "exposure_time": float(value)},
            )
    for index in range(1, times.size):
        if times[index] <= times[index - 1]:
            raise WorkbenchError(
                "E_ORDER",
                "Exposure times must be strictly increasing in frame order",
                field=f"exposures.frames[{index}].exposure_time",
                details={
                    "previous_index": index - 1,
                    "previous_exposure_time": float(times[index - 1]),
                    "index": index,
                    "exposure_time": float(times[index]),
                },
            )


def linearize_and_fuse(
    normalized_frames: np.ndarray,
    exposure_metadata: dict[str, Any],
    exposure_weights: tuple[float, ...] | list[float] | np.ndarray,
    *,
    bit_depth: int,
) -> tuple[np.ndarray, dict[str, Any]]:
    """Invert the declared gamma response and merge radiance samples."""

    frames = np.asarray(normalized_frames, dtype=np.float64)
    require(
        frames.ndim == 3,
        "E_META",
        "Exposure stack must have shape (frame_count, height, width)",
        field="exposures.frames",
    )
    frame_count = frames.shape[0]
    weights = np.asarray(exposure_weights, dtype=np.float64)
    require(
        weights.shape == (frame_count,),
        "E_PARA",
        "exposure_weights length must equal frame_count",
        field="exposure_weights",
        expected=frame_count,
        actual=int(weights.size),
    )
    require(np.all(np.isfinite(weights)) and np.all(weights >= 0.0), "E_PARA", "Invalid exposure weights", field="exposure_weights")
    require(float(weights.sum()) > 0.0, "E_PARA", "Exposure weight sum must be positive", field="exposure_weights")

    times = np.asarray([frame["exposure_time"] for frame in exposure_metadata["frames"]], dtype=np.float64)
    require(times.size == frame_count, "E_META", "Exposure frame count mismatch", field="exposures.frames")
    validate_exposure_sequence(times)
    response = exposure_metadata["response"]
    require(response.get("model") == "gamma", "E_META", "Unsupported camera response model", field="exposures.response.model")
    gamma = float(response["gamma"])
    black_level = float(response["black_level"]) / float((1 << int(bit_depth)) - 1)
    saturation = float(response["saturation"]) / float((1 << int(bit_depth)) - 1)
    full_well = float(response["full_well_exposure_radiance"])
    require(np.isfinite(gamma) and gamma > 0.0, "E_META", "Invalid response gamma", field="exposures.response.gamma")
    require(saturation > black_level, "E_META", "Invalid response black/saturation range", field="exposures.response")
    require(np.isfinite(full_well) and full_well > 0.0, "E_META", "Invalid full-well scale", field="exposures.response.full_well_exposure_radiance")

    clipped = np.clip(frames, black_level, saturation)
    encoded = (clipped - black_level) / (saturation - black_level)
    one_code = 1.0 / float((1 << int(bit_depth)) - 1)
    valid = (encoded > one_code) & (encoded < 1.0 - one_code)
    radiance_samples = np.power(np.maximum(encoded, 0.0), gamma) * full_well / times[:, None, None]

    weight_map = weights[:, None, None] * valid.astype(np.float64)
    weight_sum = weight_map.sum(axis=0)
    no_valid = weight_sum <= 0.0
    fused = np.empty(frames.shape[1:], dtype=np.float64)
    np.divide(
        np.sum(radiance_samples * weight_map, axis=0),
        weight_sum,
        out=fused,
        where=~no_valid,
    )
    if np.any(no_valid):
        # All exposures are saturated or black at these pixels. Choose the
        # least-clipped frame deterministically rather than filling silently.
        encoded_distance = np.minimum(encoded, 1.0 - encoded)
        fallback = np.argmin(encoded_distance, axis=0)
        fallback_values = np.take_along_axis(
            radiance_samples,
            fallback[None, :, :],
            axis=0,
        )[0]
        fused[no_valid] = fallback_values[no_valid]

    fused = np.maximum(fused, 0.0)
    stats = {
        "frame_count": frame_count,
        "valid_sample_fraction": float(np.mean(valid)),
        "fallback_pixel_count": int(np.count_nonzero(no_valid)),
        "weighted_exposure_times": times.tolist(),
        "exposure_weights": weights.tolist(),
        "fused_min": float(fused.min()),
        "fused_max": float(fused.max()),
    }
    return fused, stats

