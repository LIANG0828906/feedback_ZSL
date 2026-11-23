"""Global exposure compensation estimated from overlap statistics."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import cv2
import numpy as np

from .config import ExposureConfig
from .dataset import Dataset
from .errors import ErrorCode, WorkbenchError
from .geometry import invert_homography, warp_to_canvas


@dataclass(frozen=True)
class ImageExposure:
    gain: float
    offset: float
    overlap_pixels: int
    fit_rms: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "gain": self.gain,
            "offset": self.offset,
            "overlap_pixels": self.overlap_pixels,
            "fit_rms": self.fit_rms,
        }


@dataclass(frozen=True)
class ExposureModel:
    images: dict[int, ImageExposure]
    clipped_pixels: dict[int, int]
    max_overflow: dict[int, float]

    def to_dict(self) -> dict[str, Any]:
        return {
            "images": {
                str(index): image.to_dict() for index, image in sorted(self.images.items())
            },
            "clipped_pixels": {
                str(index): count for index, count in sorted(self.clipped_pixels.items())
            },
            "max_overflow": {
                str(index): value for index, value in sorted(self.max_overflow.items())
            },
        }


def _robust_linear_fit(x: np.ndarray, y: np.ndarray) -> tuple[float, float, float]:
    design = np.column_stack([x, np.ones(len(x), dtype=np.float64)])
    weights = np.ones(len(x), dtype=np.float64)
    gain = 1.0
    offset = 0.0
    for _ in range(4):
        weighted_design = design * np.sqrt(weights)[:, None]
        weighted_target = y * np.sqrt(weights)
        try:
            gain, offset = np.linalg.lstsq(weighted_design, weighted_target, rcond=None)[0]
        except np.linalg.LinAlgError:
            break
        residual = y - (gain * x + offset)
        median = float(np.median(residual))
        mad = float(np.median(np.abs(residual - median)))
        scale = max(1.4826 * mad, 1.0)
        next_weights = (np.abs(residual - median) <= 2.5 * scale).astype(np.float64)
        if np.count_nonzero(next_weights) < max(30, len(x) // 4):
            break
        if np.array_equal(weights, next_weights):
            break
        weights = next_weights
    residual = y - (gain * x + offset)
    rms = float(np.sqrt(np.mean(residual**2))) if len(residual) else 0.0
    return float(gain), float(offset), rms


def estimate_exposure(
    dataset: Dataset,
    homographies_to_canvas: dict[int, np.ndarray],
    canvas_size: tuple[int, int],
    config: ExposureConfig,
) -> ExposureModel:
    """Fit image-to-base gain and offset from overlap grayscale samples."""

    config.validate()
    if dataset.base_index not in homographies_to_canvas:
        raise WorkbenchError(ErrorCode.HOMO, "base image is not registered")
    base_image = dataset.read_image(dataset.base_index, color=True)
    base_homography = homographies_to_canvas[dataset.base_index]
    _, base_mask = warp_to_canvas(
        base_image,
        base_homography,
        canvas_size,
        interpolation=cv2.INTER_LINEAR,
    )
    estimates: dict[int, ImageExposure] = {
        dataset.base_index: ImageExposure(1.0, 0.0, int(np.count_nonzero(base_mask)), 0.0)
    }
    clipped: dict[int, int] = {dataset.base_index: 0}
    overflow: dict[int, float] = {dataset.base_index: 0.0}

    for index in sorted(dataset.images):
        if index == dataset.base_index:
            continue
        meta = dataset.images[index]
        image = dataset.read_image(index, color=True)
        image_homography = homographies_to_canvas[index]
        warped, mask = warp_to_canvas(
            image,
            image_homography,
            canvas_size,
            interpolation=cv2.INTER_LINEAR,
        )
        base_to_image = invert_homography(image_homography) @ base_homography
        image_size = (meta.width, meta.height)
        base_in_image = cv2.warpPerspective(
            base_image,
            base_to_image,
            image_size,
            flags=cv2.INTER_LINEAR,
            borderMode=cv2.BORDER_CONSTANT,
            borderValue=(0, 0, 0),
        )
        base_in_image_mask = cv2.warpPerspective(
            np.full(base_image.shape[:2], 255, dtype=np.uint8),
            base_to_image,
            image_size,
            flags=cv2.INTER_NEAREST,
            borderMode=cv2.BORDER_CONSTANT,
            borderValue=0,
        )
        reference, _ = warp_to_canvas(
            base_in_image,
            image_homography,
            canvas_size,
            interpolation=cv2.INTER_LINEAR,
        )
        reference_valid, _ = warp_to_canvas(
            base_in_image_mask,
            image_homography,
            canvas_size,
            interpolation=cv2.INTER_NEAREST,
        )
        overlap = mask & base_mask & (reference_valid > 0)
        ys, xs = np.nonzero(overlap)
        if len(xs) < 100:
            raise WorkbenchError(
                ErrorCode.HOMO,
                "registered image has too little overlap with the base exposure reference",
                filename=dataset.images[index].filename,
                field="exposure.overlap",
                details={"overlap_pixels": int(len(xs))},
            )
        stride = max(1, int(config.sample_stride))
        sample_indices = np.arange(0, len(xs), stride)
        x = warped[ys[sample_indices], xs[sample_indices], :].astype(np.float64)
        y = reference[ys[sample_indices], xs[sample_indices], :].astype(np.float64)
        x = x.reshape(-1)
        y = y.reshape(-1)
        non_saturated = (x > 1.0) & (x < 254.0) & (y > 1.0) & (y < 254.0)
        if int(np.count_nonzero(non_saturated)) >= 50:
            x_fit = x[non_saturated]
            y_fit = y[non_saturated]
            gain, offset, rms = _robust_linear_fit(x_fit, y_fit)
        else:
            gain = float(np.mean(y) / max(np.mean(x), 1e-6))
            offset = float(np.mean(y) - gain * np.mean(x))
            rms = float(np.sqrt(np.mean((y - (gain * x + offset)) ** 2)))
        if not np.isfinite(gain) or not np.isfinite(offset):
            raise WorkbenchError(
                ErrorCode.CLIP,
                "exposure fit produced non-finite coefficients",
                filename=dataset.images[index].filename,
                field="exposure",
            )
        if gain < config.gain_min or gain > config.gain_max:
            raise WorkbenchError(
                ErrorCode.PARA,
                f"estimated gain {gain:.6g} is outside configured range",
                filename=dataset.images[index].filename,
                field="exposure.gain",
                details={
                    "estimated": gain,
                    "minimum": config.gain_min,
                    "maximum": config.gain_max,
                },
            )
        if offset < config.offset_min or offset > config.offset_max:
            raise WorkbenchError(
                ErrorCode.PARA,
                f"estimated offset {offset:.6g} is outside configured range",
                filename=dataset.images[index].filename,
                field="exposure.offset",
                details={
                    "estimated": offset,
                    "minimum": config.offset_min,
                    "maximum": config.offset_max,
                },
            )
        estimates[index] = ImageExposure(gain, offset, int(len(xs)), rms)

        full_image = dataset.read_image(index, color=True)
        corrected = full_image.astype(np.float64) * gain + offset
        below = np.maximum(0.0 - corrected, 0.0)
        above = np.maximum(corrected - 255.0, 0.0)
        outside = (below > 0) | (above > 0)
        clipped[index] = int(np.count_nonzero(outside))
        overflow[index] = float(np.max(np.maximum(below, above))) if outside.any() else 0.0
        if clipped[index] / max(full_image.size, 1) > 0.05:
            raise WorkbenchError(
                ErrorCode.CLIP,
                "exposure compensation would clip more than 5% of image pixels",
                filename=dataset.images[index].filename,
                field="exposure.overflow",
                details={
                    "clipped_pixels": clipped[index],
                    "total_pixels": int(full_image.size),
                    "max_overflow": overflow[index],
                },
            )
    return ExposureModel(estimates, clipped, overflow)


def apply_exposure(
    image: np.ndarray,
    exposure: ImageExposure,
    config: ExposureConfig,
    *,
    strict: bool = False,
) -> tuple[np.ndarray, int, float]:
    """Apply a compensation model and return pixels plus overflow statistics."""

    config.validate()
    if (
        not np.isfinite(exposure.gain)
        or not np.isfinite(exposure.offset)
        or exposure.gain < config.gain_min
        or exposure.gain > config.gain_max
        or exposure.offset < config.offset_min
        or exposure.offset > config.offset_max
    ):
        raise WorkbenchError(
            ErrorCode.PARA,
            "exposure coefficients are outside configured bounds",
            field="exposure",
        )
    adjusted = image.astype(np.float64) * exposure.gain + exposure.offset
    if not np.all(np.isfinite(adjusted)):
        raise WorkbenchError(
            ErrorCode.CLIP,
            "exposure compensation overflowed to non-finite values",
            field="exposure",
        )
    below = np.maximum(0.0 - adjusted, 0.0)
    above = np.maximum(adjusted - 255.0, 0.0)
    excess = np.maximum(below, above)
    clipped = int(np.count_nonzero(excess > 0))
    max_overflow = float(np.max(excess)) if clipped else 0.0
    if strict and max_overflow > config.overflow_tolerance:
        raise WorkbenchError(
            ErrorCode.CLIP,
            "exposure compensation exceeded the output range",
            field="exposure.overflow",
            details={
                "clipped_pixels": clipped,
                "max_overflow": max_overflow,
                "tolerance": config.overflow_tolerance,
            },
        )
    clipped_image = np.clip(np.rint(adjusted), 0, 255).astype(np.uint8)
    return clipped_image, clipped, max_overflow

