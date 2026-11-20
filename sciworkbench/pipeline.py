"""Deterministic fixed/float operators and per-pixel error propagation."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Dict, List, Optional, Sequence, Tuple

import numpy as np

from .errors import E_ERROR_ATTR, E_MODE_INVALID, WorkbenchError
from .factors import STEP_NAMES, FactorSet, NoiseParams
from .model import BlockRef, SequenceStructure
from .util import canonical_json_bytes, sha256_bytes


MODE_FIXED = "fixed"
MODE_FLOAT = "float"
VALID_MODES = (MODE_FIXED, MODE_FLOAT)


def validate_mode(mode: str) -> str:
    if mode not in VALID_MODES:
        raise WorkbenchError(
            E_MODE_INVALID, "mode must be 'fixed' or 'float'"
        )
    return mode


def quantization_step(frac_bits: int) -> float:
    return math.ldexp(1.0, -int(frac_bits))


def quantize(values: np.ndarray, frac_bits: int) -> np.ndarray:
    """Round half away from zero to a binary fixed-point grid."""

    step = quantization_step(frac_bits)
    scaled = np.asarray(values, dtype=np.float64) / step
    rounded = np.sign(scaled) * np.floor(np.abs(scaled) + 0.5)
    return rounded * step


def _maybe_quantize(values: np.ndarray, frac_bits: Optional[int]) -> np.ndarray:
    if frac_bits is None:
        return np.asarray(values, dtype=np.float64)
    return quantize(values, frac_bits)


def _frame_array(array: np.ndarray, frame_index: int) -> np.ndarray:
    if array.ndim == 2:
        return array
    return array[frame_index]


def _gaussian_kernel(params: NoiseParams) -> np.ndarray:
    if params.sigma == 0.0:
        return np.asarray([[0.0, 0.0, 0.0], [0.0, 1.0, 0.0], [0.0, 0.0, 0.0]])
    coords = np.arange(-1, 2, dtype=np.float64)
    yy, xx = np.meshgrid(coords, coords, indexing="ij")
    weights = np.exp(-(xx * xx + yy * yy) / (2.0 * params.sigma * params.sigma))
    return weights / np.sum(weights)


def filter_kernels(params: NoiseParams) -> Tuple[np.ndarray, np.ndarray, np.ndarray]:
    ideal = _gaussian_kernel(params)
    fixed = quantize(ideal, params.coeff_bits)
    coefficient_error = np.abs(fixed - ideal)
    return ideal, fixed, coefficient_error


def _bilinear_nodes(
    grid: np.ndarray, width: int, height: int
) -> Tuple[np.ndarray, ...]:
    rows, cols = grid.shape
    if rows == 1:
        gy = np.zeros(height, dtype=np.float64)
    else:
        gy = np.linspace(0.0, rows - 1, height, dtype=np.float64)
    if cols == 1:
        gx = np.zeros(width, dtype=np.float64)
    else:
        gx = np.linspace(0.0, cols - 1, width, dtype=np.float64)
    y0 = np.floor(gy).astype(np.int64)
    x0 = np.floor(gx).astype(np.int64)
    y0 = np.clip(y0, 0, max(0, rows - 1))
    x0 = np.clip(x0, 0, max(0, cols - 1))
    y1 = np.clip(y0 + 1, 0, rows - 1)
    x1 = np.clip(x0 + 1, 0, cols - 1)
    ty = (gy - y0)[:, None]
    tx = (gx - x0)[None, :]
    return y0, y1, x0, x1, ty, tx


def sample_bilinear(grid: np.ndarray, width: int, height: int) -> np.ndarray:
    y0, y1, x0, x1, ty, tx = _bilinear_nodes(grid, width, height)
    top = grid[y0[:, None], x0[None, :]] * (1.0 - tx) + grid[
        y0[:, None], x1[None, :]
    ] * tx
    bottom = grid[y1[:, None], x0[None, :]] * (1.0 - tx) + grid[
        y1[:, None], x1[None, :]
    ] * tx
    return top * (1.0 - ty) + bottom * ty


def bilinear_error_bound(grid: np.ndarray, width: int, height: int) -> np.ndarray:
    """Conservative curvature bound for bilinear interpolation.

    In normalised cell coordinates the interpolation error is bounded by
    ``(|g_xx| + |g_yy|) / 8``.  Central second differences of the control grid
    are used as the discrete curvature estimate.
    """

    rows, cols = grid.shape
    curvature_y = np.zeros((rows, cols), dtype=np.float64)
    curvature_x = np.zeros((rows, cols), dtype=np.float64)
    with np.errstate(over="ignore", invalid="ignore"):
        if rows >= 3:
            d2y = grid[2:, :] - 2.0 * grid[1:-1, :] + grid[:-2, :]
            curvature_y[1:-1, :] = np.abs(d2y)
            curvature_y[0, :] = curvature_y[1, :]
            curvature_y[-1, :] = curvature_y[-2, :]
        if cols >= 3:
            d2x = grid[:, 2:] - 2.0 * grid[:, 1:-1] + grid[:, :-2]
            curvature_x[:, 1:-1] = np.abs(d2x)
            curvature_x[:, 0] = curvature_x[:, 1]
            curvature_x[:, -1] = curvature_x[:, -2]
    y0, y1, x0, x1, _, _ = _bilinear_nodes(grid, width, height)
    local_y = np.maximum.reduce(
        (
            curvature_y[y0[:, None], x0[None, :]],
            curvature_y[y0[:, None], x1[None, :]],
            curvature_y[y1[:, None], x0[None, :]],
            curvature_y[y1[:, None], x1[None, :]],
        )
    )
    local_x = np.maximum.reduce(
        (
            curvature_x[y0[:, None], x0[None, :]],
            curvature_x[y0[:, None], x1[None, :]],
            curvature_x[y1[:, None], x0[None, :]],
            curvature_x[y1[:, None], x1[None, :]],
        )
    )
    return 0.125 * (local_y + local_x)


@dataclass(frozen=True)
class StepSummary:
    step: str
    pixel_count: int
    max_abs_error: float
    max_error_bound: float
    sum_error_bound: float
    mean_error_bound: float

    def canonical_dict(self) -> dict:
        return {
            "step": self.step,
            "pixel_count": int(self.pixel_count),
            "max_abs_error": float(self.max_abs_error),
            "max_error_bound": float(self.max_error_bound),
            "sum_error_bound": float(self.sum_error_bound),
            "mean_error_bound": float(self.mean_error_bound),
        }

    @classmethod
    def from_dict(cls, value: dict) -> "StepSummary":
        return cls(
            step=str(value["step"]),
            pixel_count=int(value["pixel_count"]),
            max_abs_error=float(value["max_abs_error"]),
            max_error_bound=float(value["max_error_bound"]),
            sum_error_bound=float(value["sum_error_bound"]),
            mean_error_bound=float(value["mean_error_bound"]),
        )


def _summaries_from_arrays(
    fixed_stages: Sequence[np.ndarray],
    float_stages: Sequence[np.ndarray],
    bounds: Sequence[np.ndarray],
) -> Tuple[StepSummary, ...]:
    summaries: List[StepSummary] = []
    for index, name in enumerate(STEP_NAMES):
        with np.errstate(invalid="ignore", over="ignore"):
            actual = np.abs(fixed_stages[index] - float_stages[index])
        bound = np.asarray(bounds[index], dtype=np.float64)
        count = int(bound.size)
        total = float(np.sum(bound, dtype=np.float64))
        summaries.append(
            StepSummary(
                step=name,
                pixel_count=count,
                max_abs_error=float(np.max(actual)) if count else 0.0,
                max_error_bound=float(np.max(bound)) if count else 0.0,
                sum_error_bound=total,
                mean_error_bound=(total / count) if count else 0.0,
            )
        )
    return tuple(summaries)


@dataclass(frozen=True)
class PathRun:
    stages: Tuple[np.ndarray, ...]
    contributions: np.ndarray
    stage_contributions: Tuple[np.ndarray, ...]
    error_bound: np.ndarray


@dataclass
class FrameContext:
    dark: np.ndarray
    flat: np.ndarray
    bad_mask: np.ndarray
    gain: np.ndarray
    gain_error: np.ndarray
    kernel_ideal: np.ndarray
    kernel_fixed: np.ndarray
    coefficient_error: np.ndarray


class BlockResult:
    """One committed output block and its diagnostic payload."""

    def __init__(
        self,
        *,
        frame_index: int,
        block_index: int,
        ordinal: int,
        shape: Tuple[int, int],
        pixels: np.ndarray,
        error_bound: np.ndarray,
        attribution: np.ndarray,
        step_summaries: Sequence[StepSummary],
        fixed_float_max_abs: float,
        fixed_float_abs_sum: float,
        fixed_float_pixel_count: int,
        factor_versions: Optional[Dict[str, str]] = None,
    ) -> None:
        self.frame_index = int(frame_index)
        self.block_index = int(block_index)
        self.ordinal = int(ordinal)
        self.shape = (int(shape[0]), int(shape[1]))
        self.pixels = np.ascontiguousarray(pixels, dtype="<f8")
        self.error_bound = np.ascontiguousarray(error_bound, dtype="<f8")
        self.attribution = np.ascontiguousarray(attribution, dtype=np.int8)
        if self.pixels.shape != self.shape or self.error_bound.shape != self.shape:
            raise ValueError("block pixel/error shapes do not match")
        if self.attribution.shape != self.shape:
            raise ValueError("block attribution shape does not match")
        self.pixels.setflags(write=False)
        self.error_bound.setflags(write=False)
        self.attribution.setflags(write=False)
        self.step_summaries = tuple(step_summaries)
        self.fixed_float_max_abs = float(fixed_float_max_abs)
        self.fixed_float_abs_sum = float(fixed_float_abs_sum)
        self.fixed_float_pixel_count = int(fixed_float_pixel_count)
        self.factor_versions = dict(factor_versions or {})
        self.content_hash = self._compute_content_hash()

    def _content_material(self) -> dict:
        return {
            "frame": self.frame_index,
            "block": self.block_index,
            "shape": list(self.shape),
            "pixels": sha256_bytes(self.pixels.tobytes(order="C")),
            "error_bound": sha256_bytes(self.error_bound.tobytes(order="C")),
            "attribution": sha256_bytes(self.attribution.tobytes(order="C")),
            "step_summaries": [item.canonical_dict() for item in self.step_summaries],
            "fixed_float_max_abs": self.fixed_float_max_abs,
            "fixed_float_abs_sum": self.fixed_float_abs_sum,
            "fixed_float_pixel_count": self.fixed_float_pixel_count,
        }

    def _compute_content_hash(self) -> str:
        return sha256_bytes(canonical_json_bytes(self._content_material()))

    @property
    def pixel_count(self) -> int:
        return int(self.pixels.size)

    def untrusted(self, threshold: float) -> np.ndarray:
        return np.asarray(self.error_bound > threshold, dtype=np.bool_)

    def max_error_bound(self) -> float:
        return float(np.max(self.error_bound)) if self.error_bound.size else 0.0

    def with_factor_versions(self, versions: Dict[str, str]) -> "BlockResult":
        return BlockResult(
            frame_index=self.frame_index,
            block_index=self.block_index,
            ordinal=self.ordinal,
            shape=self.shape,
            pixels=self.pixels,
            error_bound=self.error_bound,
            attribution=self.attribution,
            step_summaries=self.step_summaries,
            fixed_float_max_abs=self.fixed_float_max_abs,
            fixed_float_abs_sum=self.fixed_float_abs_sum,
            fixed_float_pixel_count=self.fixed_float_pixel_count,
            factor_versions=versions,
        )

    def canonical_dict(self) -> dict:
        return {
            "frame": self.frame_index,
            "block": self.block_index,
            "shape": list(self.shape),
            "pixels_sha256": sha256_bytes(self.pixels.tobytes(order="C")),
            "error_bound_sha256": sha256_bytes(
                self.error_bound.tobytes(order="C")
            ),
            "attribution_sha256": sha256_bytes(
                self.attribution.tobytes(order="C")
            ),
            "max_error_bound": self.max_error_bound(),
            "content_hash": self.content_hash,
            "factor_versions": self.factor_versions,
        }


class BlockComputer:
    """Runs both precision paths so every block carries a live reference delta."""

    def __init__(
        self,
        structure: SequenceStructure,
        factors: FactorSet,
        mode: str,
        sequence,
    ) -> None:
        validate_mode(mode)
        factors.validate(structure)
        self.structure = structure
        self.factors = factors
        self.mode = mode
        self.sequence = sequence
        self._contexts: Dict[int, FrameContext] = {}
        self.kernel_ideal, self.kernel_fixed, self.coefficient_error = filter_kernels(
            factors.noise
        )
        self.gain_image = sample_bilinear(
            factors.block_gain, structure.width, structure.height
        )
        self.gain_error = bilinear_error_bound(
            factors.block_gain, structure.width, structure.height
        )

    def frame_context(self, frame_index: int) -> FrameContext:
        context = self._contexts.get(frame_index)
        if context is not None:
            return context
        factors = self.factors
        context = FrameContext(
            dark=_frame_array(factors.dark, frame_index),
            flat=_frame_array(factors.flat, frame_index),
            bad_mask=_frame_array(factors.bad_mask, frame_index),
            gain=self.gain_image,
            gain_error=self.gain_error,
            kernel_ideal=self.kernel_ideal,
            kernel_fixed=self.kernel_fixed,
            coefficient_error=self.coefficient_error,
        )
        self._contexts[frame_index] = context
        return context

    def _run_path(
        self,
        raw: np.ndarray,
        dark: np.ndarray,
        flat: np.ndarray,
        bad_mask: np.ndarray,
        gain: np.ndarray,
        gain_error: np.ndarray,
        kernel: np.ndarray,
        coefficient_error: np.ndarray,
        *,
        fixed: bool,
    ) -> PathRun:
        factors = self.factors
        frac_bits = factors.frac_bits if fixed else None
        qerror = quantization_step(factors.frac_bits) * 0.5 if fixed else 0.0
        shape = raw.shape
        contributions = np.zeros((len(STEP_NAMES),) + shape, dtype=np.float64)
        stage_contributions: List[np.ndarray] = []

        with np.errstate(over="ignore", divide="ignore", invalid="ignore"):
            stage1 = _maybe_quantize(raw.astype(np.float64) - dark, frac_bits)
            contributions[0] += factors.error.dark_error + qerror
            stage_contributions.append(contributions.copy())

            denominator = np.maximum(np.abs(flat), factors.error.flat_floor)
            denominator_lower = np.maximum(
                factors.error.flat_floor,
                denominator * (1.0 - factors.error.flat_rel_error),
            )
            stage2 = _maybe_quantize(stage1 / denominator, frac_bits)
            contributions = contributions / denominator_lower[None, :, :]
            contributions[1] += (
                np.abs(stage1)
                * factors.error.flat_rel_error
                / denominator_lower
                + qerror
            )
            stage_contributions.append(contributions.copy())

            stage3 = stage2.copy()
            repair_contributions = contributions.copy()
            bad_positions = np.argwhere(bad_mask)
            height, width = bad_mask.shape
            for y, x in bad_positions:
                neighbors: List[Tuple[int, int]] = []
                for dy in (-1, 0, 1):
                    for dx in (-1, 0, 1):
                        if dy == 0 and dx == 0:
                            continue
                        ny = int(y) + dy
                        nx = int(x) + dx
                        if 0 <= ny < height and 0 <= nx < width and not bad_mask[ny, nx]:
                            neighbors.append((ny, nx))
                if not neighbors:
                    for dy in (-1, 0, 1):
                        for dx in (-1, 0, 1):
                            if dy == 0 and dx == 0:
                                continue
                            ny = int(y) + dy
                            nx = int(x) + dx
                            if 0 <= ny < height and 0 <= nx < width:
                                neighbors.append((ny, nx))
                if not neighbors:
                    continue
                ny = np.asarray([item[0] for item in neighbors], dtype=np.int64)
                nx = np.asarray([item[1] for item in neighbors], dtype=np.int64)
                neighbor_values = stage2[ny, nx]
                neighbor_contribs = contributions[:, ny, nx]
                stage3[int(y), int(x)] = _maybe_quantize(
                    np.asarray([np.mean(neighbor_values)]), frac_bits
                )[0]
                repair_contributions[:, int(y), int(x)] = np.mean(
                    neighbor_contribs, axis=1
                )
                span = float(np.max(neighbor_values) - np.min(neighbor_values))
                repair_contributions[2, int(y), int(x)] += 0.5 * span + qerror
            contributions = repair_contributions
            stage_contributions.append(contributions.copy())

            stage4 = _maybe_quantize(stage3 * gain, frac_bits)
            contributions = contributions * np.abs(gain)[None, :, :]
            contributions[3] += np.abs(stage3) * gain_error + qerror
            stage_contributions.append(contributions.copy())

            padded_values = np.pad(stage4, 1, mode="edge")
            padded_contribs = np.pad(
                contributions,
                ((0, 0), (1, 1), (1, 1)),
                mode="edge",
            )
            filtered = np.zeros_like(stage4)
            output_contribs = np.zeros_like(contributions)
            coefficient_term = np.zeros_like(stage4)
            for ky in range(3):
                for kx in range(3):
                    values = padded_values[ky : ky + height, kx : kx + width]
                    filtered += kernel[ky, kx] * values
                    output_contribs += (
                        np.abs(kernel[ky, kx])
                        * padded_contribs[
                            :, ky : ky + height, kx : kx + width
                        ]
                    )
                    coefficient_term += (
                        np.abs(values) * coefficient_error[ky, kx]
                    )
            stage5 = _maybe_quantize(filtered, frac_bits)
            output_contribs[4] += coefficient_term + qerror
            stage_contributions.append(output_contribs.copy())

        stages = (stage1, stage2, stage3, stage4, stage5)
        bound = np.sum(output_contribs, axis=0)
        return PathRun(
            stages=stages,
            contributions=output_contribs,
            stage_contributions=tuple(stage_contributions),
            error_bound=bound,
        )

    def run_block(self, block: BlockRef) -> BlockResult:
        structure = self.structure
        context = self.frame_context(block.frame_index)
        pad = 2
        wy0 = max(0, block.y0 - pad)
        wy1 = min(structure.height, block.y1 + pad)
        wx0 = max(0, block.x0 - pad)
        wx1 = min(structure.width, block.x1 + pad)
        raw = self._raw_work(block.frame_index, wy0, wy1, wx0, wx1)
        dark = context.dark[wy0:wy1, wx0:wx1]
        flat = context.flat[wy0:wy1, wx0:wx1]
        bad = context.bad_mask[wy0:wy1, wx0:wx1]
        gain = context.gain[wy0:wy1, wx0:wx1]
        gain_error = context.gain_error[wy0:wy1, wx0:wx1]

        fixed_run = self._run_path(
            raw,
            dark,
            flat,
            bad,
            gain,
            gain_error,
            context.kernel_fixed,
            context.coefficient_error,
            fixed=True,
        )
        float_run = self._run_path(
            raw,
            dark,
            flat,
            bad,
            gain,
            gain_error,
            context.kernel_ideal,
            np.zeros_like(context.coefficient_error),
            fixed=False,
        )

        cy0 = block.y0 - wy0
        cy1 = block.y1 - wy0
        cx0 = block.x0 - wx0
        cx1 = block.x1 - wx0
        core = (slice(cy0, cy1), slice(cx0, cx1))

        fixed_stages = tuple(stage[core] for stage in fixed_run.stages)
        float_stages = tuple(stage[core] for stage in float_run.stages)
        chosen_run = fixed_run if self.mode == MODE_FIXED else float_run
        step_bounds = []
        for prefix in chosen_run.stage_contributions:
            step_bounds.append(np.sum(prefix[:, core[0], core[1]], axis=0))
        summaries = _summaries_from_arrays(
            fixed_stages,
            float_stages,
            tuple(step_bounds),
        )

        final_pixels = np.asarray(chosen_run.stages[-1][core], dtype=np.float64)
        final_error = np.asarray(chosen_run.error_bound[core], dtype=np.float64)
        final_contribs = chosen_run.contributions[:, core[0], core[1]]
        finite = np.isfinite(final_pixels) & np.isfinite(final_error) & np.all(
            np.isfinite(final_contribs), axis=0
        )
        if not np.all(finite):
            local_y, local_x = np.argwhere(~finite)[0]
            raise WorkbenchError(
                E_ERROR_ATTR,
                "non-finite result cannot be attributed to an error node",
                frame=block.frame_index,
                block=block.block_index,
                pixel=(block.y0 + int(local_y), block.x0 + int(local_x)),
            )
        attribution = np.full(final_pixels.shape, -1, dtype=np.int8)
        untrusted = final_error > self.factors.error.threshold
        if np.any(untrusted):
            maxima = np.max(final_contribs, axis=0)
            if np.any(untrusted & ((maxima <= 0.0) | ~np.isfinite(maxima))):
                local_y, local_x = np.argwhere(
                    untrusted & ((maxima <= 0.0) | ~np.isfinite(maxima))
                )[0]
                raise WorkbenchError(
                    E_ERROR_ATTR,
                    "untrusted pixel has no positive contribution node",
                    frame=block.frame_index,
                    block=block.block_index,
                    pixel=(block.y0 + int(local_y), block.x0 + int(local_x)),
                )
            attribution[untrusted] = np.argmax(
                final_contribs[:, untrusted], axis=0
            ).astype(np.int8)

        with np.errstate(invalid="ignore", over="ignore"):
            actual = np.abs(
                fixed_run.stages[-1][core] - float_run.stages[-1][core]
            )
        return BlockResult(
            frame_index=block.frame_index,
            block_index=block.block_index,
            ordinal=block.ordinal,
            shape=block.shape,
            pixels=final_pixels,
            error_bound=final_error,
            attribution=attribution,
            step_summaries=summaries,
            fixed_float_max_abs=float(np.max(actual)) if actual.size else 0.0,
            fixed_float_abs_sum=float(np.sum(actual, dtype=np.float64)),
            fixed_float_pixel_count=int(actual.size),
            factor_versions=self.factors.block_factor_versions(
                structure, block.frame_index, block.block_index
            ),
        )

    def _raw_work(
        self,
        frame_index: int,
        y0: int,
        y1: int,
        x0: int,
        x1: int,
    ) -> np.ndarray:
        return np.asarray(
            self.sequence.pixels[frame_index, y0:y1, x0:x1], dtype=np.float64
        )
