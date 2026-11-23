"""Seam-guided multi-band panorama blending and seam metrics."""

from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Callable

import cv2
import numpy as np

from .config import BlendConfig, ExposureConfig
from .dataset import Dataset
from .exposure import ExposureModel, ImageExposure, apply_exposure
from .geometry import CanvasGeometry, warp_to_canvas
from .seam import (
    SeamPath,
    apply_seam_partition,
    fallback_straight_seam,
    find_minimum_cost_seam,
)


@dataclass(frozen=True)
class WarpedSource:
    image: np.ndarray
    mask: np.ndarray
    clipped_pixels: int
    max_overflow: float


class WarpedSourceCache:
    def __init__(
        self,
        dataset: Dataset,
        homographies_to_canvas: dict[int, np.ndarray],
        exposure_model: ExposureModel,
        exposure_config: ExposureConfig,
        canvas_size: tuple[int, int],
        capacity: int,
    ) -> None:
        self.dataset = dataset
        self.homographies_to_canvas = homographies_to_canvas
        self.exposure_model = exposure_model
        self.exposure_config = exposure_config
        self.canvas_size = canvas_size
        self.capacity = max(1, capacity)
        self._cache: OrderedDict[int, WarpedSource] = OrderedDict()
        self.peak_buffered_images = 0

    def get(self, index: int) -> WarpedSource:
        if index in self._cache:
            value = self._cache.pop(index)
            self._cache[index] = value
            return value
        image = self.dataset.read_image(index, color=True)
        exposure = self.exposure_model.images.get(index, ImageExposure(1.0, 0.0, 0, 0.0))
        corrected, clipped, overflow = apply_exposure(
            image, exposure, self.exposure_config, strict=False
        )
        warped, mask = warp_to_canvas(
            corrected,
            self.homographies_to_canvas[index],
            self.canvas_size,
            interpolation=cv2.INTER_LINEAR,
        )
        value = WarpedSource(warped, mask, clipped, overflow)
        self._cache[index] = value
        self.peak_buffered_images = max(self.peak_buffered_images, len(self._cache))
        while len(self._cache) > self.capacity:
            self._cache.popitem(last=False)
        return value


@dataclass(frozen=True)
class BlendResult:
    panorama: np.ndarray
    owner: np.ndarray
    coverage: np.ndarray
    seams: tuple[tuple[str, SeamPath], ...]
    seam_gradient_p95: float
    seam_gradient_max: float
    clipped_pixels: dict[int, int]
    max_overflow: dict[int, float]
    peak_buffered_images: int

    def summary(self) -> dict[str, Any]:
        return {
            "seam_count": len(self.seams),
            "seam_gradient_p95": self.seam_gradient_p95,
            "seam_gradient_max": self.seam_gradient_max,
            "peak_buffered_images": self.peak_buffered_images,
            "clipped_pixels": {
                str(key): value for key, value in sorted(self.clipped_pixels.items())
            },
            "max_overflow": {
                str(key): value for key, value in sorted(self.max_overflow.items())
            },
        }


def _pyramid_levels(size: tuple[int, int], requested: int) -> int:
    levels = max(1, int(requested))
    smallest = min(size)
    while levels > 1 and smallest / (2 ** (levels - 1)) < 8:
        levels -= 1
    return levels


def _gaussian_pyramid(image: np.ndarray, levels: int) -> list[np.ndarray]:
    pyramid = [image]
    for _ in range(1, levels):
        pyramid.append(cv2.pyrDown(pyramid[-1]))
    return pyramid


def _laplacian_pyramid(
    gaussian: list[np.ndarray],
) -> list[np.ndarray]:
    laplacian: list[np.ndarray] = []
    for level in range(len(gaussian) - 1):
        upsampled = cv2.pyrUp(
            gaussian[level + 1], dstsize=(gaussian[level].shape[1], gaussian[level].shape[0])
        )
        laplacian.append(gaussian[level] - upsampled)
    laplacian.append(gaussian[-1])
    return laplacian


def _multi_band_blend(
    owner: np.ndarray,
    source_getter: Callable[[int], WarpedSource],
    image_indices: list[int],
    size: tuple[int, int],
    levels: int,
) -> np.ndarray:
    height, width = size[1], size[0]
    lap_accum = [
        np.zeros((max(1, height // (2**level)), max(1, width // (2**level)), 3), dtype=np.float32)
        for level in range(levels)
    ]
    weight_accum = [
        np.zeros((max(1, height // (2**level)), max(1, width // (2**level))), dtype=np.float32)
        for level in range(levels)
    ]
    coarse_accum = np.zeros_like(lap_accum[-1])
    coarse_weight = np.zeros_like(weight_accum[-1])

    for index in image_indices:
        source = source_getter(index)
        mask = (owner == index).astype(np.float32)
        if not np.any(mask):
            continue
        image_float = source.image.astype(np.float32) / 255.0
        image_pyramid = _gaussian_pyramid(image_float, levels)
        mask_pyramid = _gaussian_pyramid(mask, levels)
        laplacian = _laplacian_pyramid(image_pyramid)
        for level in range(levels):
            mask_level = mask_pyramid[level]
            if level == levels - 1:
                shape = coarse_accum.shape[:2]
                if mask_level.shape != shape:
                    mask_level = cv2.resize(
                        mask_level, (shape[1], shape[0]), interpolation=cv2.INTER_AREA
                    )
                padded = image_pyramid[level]
                if padded.shape[:2] != shape:
                    padded = cv2.resize(
                        padded, (shape[1], shape[0]), interpolation=cv2.INTER_AREA
                    )
                coarse_accum += padded * mask_level[..., None]
                coarse_weight += mask_level
            else:
                shape = lap_accum[level].shape[:2]
                if mask_level.shape != shape:
                    mask_level = cv2.resize(
                        mask_level, (shape[1], shape[0]), interpolation=cv2.INTER_AREA
                    )
                lap = laplacian[level]
                if lap.shape[:2] != shape:
                    lap = cv2.resize(lap, (shape[1], shape[0]), interpolation=cv2.INTER_AREA)
                lap_accum[level] += lap * mask_level[..., None]
                weight_accum[level] += mask_level

    epsilon = np.float32(1e-7)
    current = coarse_accum / np.maximum(coarse_weight[..., None], epsilon)
    for level in range(levels - 2, -1, -1):
        target_size = (lap_accum[level].shape[1], lap_accum[level].shape[0])
        current = cv2.pyrUp(current, dstsize=target_size)
        current = current + lap_accum[level] / np.maximum(
            weight_accum[level][..., None], epsilon
        )
    current = np.clip(np.rint(current * 255.0), 0, 255).astype(np.uint8)
    if current.shape[:2] != (height, width):
        current = cv2.resize(current, (width, height), interpolation=cv2.INTER_LINEAR)
    return current


def _seam_gradient_metrics(
    panorama: np.ndarray, seams: tuple[tuple[str, SeamPath], ...]
) -> tuple[float, float]:
    if not seams:
        return 0.0, 0.0
    gray = cv2.cvtColor(panorama, cv2.COLOR_BGR2GRAY).astype(np.float32)
    values: list[float] = []
    height, width = gray.shape
    for _, seam in seams:
        for x, y in seam.points_xy:
            if seam.orientation == "vertical":
                if x - 2 < 0 or x + 2 >= width:
                    continue
                left_difference = float(gray[y, x - 1] - gray[y, x - 2])
                right_difference = float(gray[y, x + 2] - gray[y, x + 1])
            else:
                if y - 2 < 0 or y + 2 >= height:
                    continue
                left_difference = float(gray[y - 1, x] - gray[y - 2, x])
                right_difference = float(gray[y + 2, x] - gray[y + 1, x])
            values.append(abs(right_difference - left_difference))
    if not values:
        return 0.0, 0.0
    array = np.asarray(values, dtype=np.float64)
    return float(np.percentile(array, 95)), float(np.max(array))


def compose_panorama(
    dataset: Dataset,
    homographies_to_canvas: dict[int, np.ndarray],
    geometry: CanvasGeometry,
    exposure_model: ExposureModel,
    config: BlendConfig,
    exposure_config: ExposureConfig,
    *,
    max_cached_images: int,
    pair_id_by_image: dict[int, str],
) -> BlendResult:
    """Partition ownership by seams, then blend with a Laplacian pyramid."""

    config.validate()
    size = (geometry.width, geometry.height)
    cache = WarpedSourceCache(
        dataset,
        homographies_to_canvas,
        exposure_model,
        exposure_config,
        size,
        max_cached_images,
    )
    ordered = [dataset.base_index] + [
        index for index in sorted(dataset.images) if index != dataset.base_index
    ]
    owner = np.full((geometry.height, geometry.width), -1, dtype=np.int16)
    coverage = np.zeros((geometry.height, geometry.width), dtype=np.uint16)
    owner_color = np.zeros((geometry.height, geometry.width, 3), dtype=np.uint8)
    seams: list[tuple[str, SeamPath]] = []
    source_stats: dict[int, WarpedSource] = {}

    for index in ordered:
        source = cache.get(index)
        source_stats[index] = source
        mask = source.mask
        existing_mask = (owner >= 0) & mask
        if np.any(existing_mask):
            seam = find_minimum_cost_seam(
                owner_color, source.image, owner >= 0, mask, config
            )
            if seam is None:
                seam = fallback_straight_seam(owner >= 0, mask)
            if seam is not None:
                pair_id = pair_id_by_image.get(index, f"image_{index}")
                seams.append((pair_id, seam))
                apply_seam_partition(owner, index, mask, seam)
            else:
                apply_seam_partition(owner, index, mask, None)
        else:
            apply_seam_partition(owner, index, mask, None)
        coverage += mask.astype(np.uint16)
        owned = owner == index
        owner_color[owned] = source.image[owned]

    levels = _pyramid_levels(size, config.pyramid_levels)
    panorama = _multi_band_blend(
        owner,
        cache.get,
        ordered,
        size,
        levels,
    )

    for index in ordered:
        source = cache.get(index)
        single = (owner == index) & (coverage == 1)
        panorama[single] = source.image[single]

    seam_gradient_p95, seam_gradient_max = _seam_gradient_metrics(
        panorama, tuple(seams)
    )
    if seam_gradient_p95 > config.seam_gradient_limit:
        raise ValueError(
            f"seam gradient p95 {seam_gradient_p95:.3f} exceeds limit "
            f"{config.seam_gradient_limit:.3f}"
        )
    clipped = {
        index: source.clipped_pixels for index, source in source_stats.items()
    }
    overflow = {
        index: source.max_overflow for index, source in source_stats.items()
    }
    return BlendResult(
        panorama=panorama,
        owner=owner,
        coverage=coverage,
        seams=tuple(seams),
        seam_gradient_p95=seam_gradient_p95,
        seam_gradient_max=seam_gradient_max,
        clipped_pixels=clipped,
        max_overflow=overflow,
        peak_buffered_images=cache.peak_buffered_images,
    )
