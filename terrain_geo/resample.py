"""Inverse-mapped resampling with tile/overlap-band boundary guarantees.

Both whole-image and tiled paths call the same pixel kernel over contiguous
destination regions, so tiled output is bit-identical to the single-shot
output. Tiles carry an overlap band whose width is derived from the local
Jacobian of the inverse map and the interpolation kernel half-width; tiles
are merged by deterministic destination ownership (the natural tile grid),
never by blending, eliminating seam bias.
"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np

from .errors import GeoError, ErrorCode
from .geometry import numeric_jacobian

ORDER_HALF_WIDTH = {0: 0, 1: 1, 2: 2}
ORDER_NAME = {0: "nearest", 1: "bilinear", 2: "bicubic"}


@dataclass(frozen=True)
class ResampleConfig:
    """Resampling configuration.

    tile_size: destination tile edge (pixels); 0 forces single-shot.
    order: 0=nearest, 1=bilinear, 2=bicubic.
    max_overlap: hard cap of the expanded overlap band (pixels).
    max_pixels_per_thread: per-thread destination pixel buffer budget.
    """

    tile_size: int = 64
    order: int = 1
    max_overlap: int = 16
    max_pixels_per_thread: int = 4_000_000

    def validate(self, stage: str) -> None:
        if self.order not in ORDER_HALF_WIDTH:
            raise GeoError(ErrorCode.E_PARA,
                           f"unsupported interpolation order {self.order}",
                           stage=stage, field="order")
        if self.tile_size < 0 or not isinstance(self.tile_size, int):
            raise GeoError(ErrorCode.E_PARA, "tile_size must be >= 0",
                           stage=stage, field="tile_size")
        if self.max_overlap < 0:
            raise GeoError(ErrorCode.E_PARA, "max_overlap must be >= 0",
                           stage=stage, field="max_overlap")
        if self.max_pixels_per_thread <= 0:
            raise GeoError(ErrorCode.E_PARA,
                           "max_pixels_per_thread must be > 0",
                           stage=stage, field="max_pixels_per_thread")


def _bicubic_weight(t: np.ndarray) -> np.ndarray:
    """Catmull-Rom bicubic kernel (a = -0.5)."""
    t = np.abs(t)
    w = np.zeros_like(t)
    m1 = t <= 1.0
    m2 = (t > 1.0) & (t < 2.0)
    w[m1] = 1.5 * t[m1] ** 3 - 2.5 * t[m1] ** 2 + 1.0
    w[m2] = (-0.5 * t[m2] ** 3 + 2.5 * t[m2] ** 2
             - 4.0 * t[m2] + 2.0)
    return w


def _sample_nearest(image, sx, sy):
    h, w = image.shape[:2]
    ix = np.rint(sx).astype(np.int64)
    iy = np.rint(sy).astype(np.int64)
    valid = (ix >= 0) & (ix < w) & (iy >= 0) & (iy < h)
    ix_safe = np.clip(ix, 0, w - 1)
    iy_safe = np.clip(iy, 0, h - 1)
    out = np.zeros(sx.shape + (image.shape[2],), dtype=image.dtype)
    out[valid] = image[iy_safe[valid], ix_safe[valid]]
    return out


def _sample_bilinear(image, sx, sy):
    h, w = image.shape[:2]
    x0 = np.floor(sx).astype(np.int64)
    y0 = np.floor(sy).astype(np.int64)
    x1, y1 = x0 + 1, y0 + 1
    wx = (sx - x0)[..., None]
    wy = (sy - y0)[..., None]
    padded = np.pad(image, ((1, 1), (1, 1), (0, 0)),
                    mode="constant", constant_values=0).astype(np.float64)
    px0 = np.clip(x0 + 1, 0, w + 1)
    px1 = np.clip(x1 + 1, 0, w + 1)
    py0 = np.clip(y0 + 1, 0, h + 1)
    py1 = np.clip(y1 + 1, 0, h + 1)
    top = padded[py0, px0] * (1 - wx) + padded[py0, px1] * wx
    bot = padded[py1, px0] * (1 - wx) + padded[py1, px1] * wx
    out = top * (1 - wy) + bot * wy
    # Zero out samples whose footprint is fully outside the source frame.
    outside = (x1 < 0) | (x0 >= w) | (y1 < 0) | (y0 >= h)
    out[outside] = 0.0
    return out


def _sample_bicubic(image, sx, sy):
    h, w = image.shape[:2]
    ix = np.floor(sx).astype(np.int64)
    iy = np.floor(sy).astype(np.int64)
    fx = sx - ix
    fy = sy - iy
    padded = np.pad(image, ((2, 2), (2, 2), (0, 0)),
                    mode="constant", constant_values=0).astype(np.float64)
    acc = np.zeros(sx.shape + (image.shape[2],), dtype=np.float64)
    for m in range(-1, 3):
        wy = _bicubic_weight(fy - m)[..., None]
        row = np.zeros_like(acc)
        for n in range(-1, 3):
            wx = _bicubic_weight(fx - n)[..., None]
            yy = np.clip(iy + m + 2, 0, h + 3)
            xx = np.clip(ix + n + 2, 0, w + 3)
            row += padded[yy, xx] * wx
        acc += row * wy
    # A 4x4 footprint is valid only when its span intersects the frame.
    outside = (ix + 2 < 0) | (ix - 1 >= w) | (iy + 2 < 0) | (iy - 1 >= h)
    acc[outside] = 0.0
    return acc


def _sample(image, sx, sy, order):
    if order == 0:
        return _sample_nearest(image, sx, sy)
    if order == 1:
        return _sample_bilinear(image, sx, sy)
    return _sample_bicubic(image, sx, sy)


def _destination_grid(out_h, out_w):
    yy, xx = np.meshgrid(np.arange(out_h), np.arange(out_w), indexing="ij")
    return xx.astype(np.float64), yy.astype(np.float64)


def required_overlap(inverse_fn, out_h, out_w, order):
    """Conservative overlap (destination pixels) from local Jacobian.

    A destination displacement d maps to a source displacement J*d; the
    interpolation footprint needs half-width h source pixels, hence
    d >= h / max|J| along each axis.
    """
    half = ORDER_HALF_WIDTH[order]
    if half == 0:
        return 0
    # Probe a deterministic coarse lattice including frame borders.
    xs = np.linspace(0, max(out_w - 1, 0), min(out_w, 16))
    ys = np.linspace(0, max(out_h - 1, 0), min(out_h, 16))
    gx, gy = np.meshgrid(xs, ys)
    pts = np.column_stack([gx.ravel(), gy.ravel()])
    jac = numeric_jacobian(inverse_fn, pts)
    gain = np.max(np.sum(np.abs(jac), axis=1), axis=0)  # per x/y axis
    gain = np.maximum(gain, 1e-9)
    bands = np.ceil(half / gain).astype(np.int64)
    return int(np.max(bands))


def resample_region(image, inverse_fn, row0, row1, col0, col1, order):
    """Resample one contiguous destination region (half-open rows/cols)."""
    out_h = row1 - row0
    out_w = col1 - col0
    gx, gy = _destination_grid(out_h, out_w)
    dest = np.column_stack([(gx + col0).ravel(), (gy + row0).ravel()])
    src = inverse_fn(dest)
    sampled = _sample(image, src[:, 0], src[:, 1], order)
    return sampled.reshape(out_h, out_w, image.shape[2])


def resample_full(image, inverse_fn, out_h, out_w, order):
    squeezed = image.ndim == 2
    work = image[..., None] if squeezed else image
    out = resample_region(work, inverse_fn, 0, out_h, 0, out_w, order)
    return out[..., 0] if squeezed else out


def resample_tiled(image, inverse_fn, out_h, out_w, config: ResampleConfig,
                   stage: str, executor=None):
    """Tile-grid resampling with overlap bands; deterministic ownership merge."""
    config.validate(stage)
    order = config.order
    if config.tile_size == 0:
        return resample_full(image, inverse_fn, out_h, out_w, order)
    squeezed = image.ndim == 2
    if squeezed:
        image = image[..., None]
    need = required_overlap(inverse_fn, out_h, out_w, order)
    if need > config.max_overlap:
        raise GeoError(
            ErrorCode.E_BAND,
            f"required overlap {need}px exceeds max_overlap "
            f"{config.max_overlap} (order={ORDER_NAME[order]})",
            stage=stage, field="max_overlap",
            detail={"required": need, "max_overlap": config.max_overlap})
    h, w = image.shape[:2]
    tile_pixels = config.tile_size * config.tile_size
    if tile_pixels > config.max_pixels_per_thread:
        raise GeoError(
            ErrorCode.E_BAND,
            f"tile buffer {tile_pixels}px exceeds per-thread limit "
            f"{config.max_pixels_per_thread}",
            stage=stage, field="max_pixels_per_thread")
    channels = image.shape[2]
    output = np.zeros((out_h, out_w, channels), dtype=np.float64)
    row_edges = list(range(0, out_h, config.tile_size)) + [out_h]
    col_edges = list(range(0, out_w, config.tile_size)) + [out_w]
    jobs = []
    regions = []
    for r in range(len(row_edges) - 1):
        for c in range(len(col_edges) - 1):
            r0, r1 = row_edges[r], row_edges[r + 1]
            c0, c1 = col_edges[c], col_edges[c + 1]
            er0 = max(0, r0 - need)
            er1 = min(out_h, r1 + need)
            ec0 = max(0, c0 - need)
            ec1 = min(out_w, c1 + need)
            regions.append((r0, r1, c0, c1))
            jobs.append((er0, er1, ec0, ec1))

    def run(job):
        er0, er1, ec0, ec1 = job
        expanded = resample_region(image, inverse_fn, er0, er1, ec0, ec1, order)
        return expanded

    if executor is not None:
        results = list(executor.map(run, jobs))
    else:
        results = [run(job) for job in jobs]
    for (r0, r1, c0, c1), expanded in zip(regions, results):
        er0, er1, ec0, ec1 = [v for v in (
            max(0, r0 - need), min(out_h, r1 + need),
            max(0, c0 - need), min(out_w, c1 + need))]
        output[r0:r1, c0:c1] = expanded[r0 - er0:r1 - er0, c0 - ec0:c1 - ec0]
    if squeezed:
        return output[..., 0]
    return output


def cast_like_source(arr, source_dtype):
    """Match the resampled float/uint8 result to the source dtype convention."""
    if source_dtype == np.uint8:
        return np.clip(np.rint(arr), 0, 255).astype(np.uint8)
    return arr.astype(np.float64)
