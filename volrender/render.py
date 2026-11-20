"""NumPy volume ray casting, lighting, error propagation, and precision modes."""

from __future__ import annotations

import hashlib
import json
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .camera import build_rays, default_view_matrix, validate_view_matrix
from .errors import (
    E_INPUT_INVALID,
    E_LIGHT_DRIFT,
    E_MODE_INVALID,
    E_TF_INVALID,
    VolRenderError,
)
from .sampling import sample_trilinear
from .transfer import (
    PRECISION_MODES,
    ControlPoint,
    LUTBundle,
    TransferFunction,
    opacity_error_at,
    sample_lut,
)
from .volume import MappingChange, VolumeData


def canonical_json_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")


def compute_state_hash(state: Mapping[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(state)).hexdigest()


def _validate_step(step: float) -> float:
    value = float(step)
    if not np.isfinite(value) or value <= 0.0:
        raise VolRenderError(E_INPUT_INVALID, "ray step must be positive and finite")
    return value


def _validate_threshold(threshold: float) -> float:
    value = float(threshold)
    if not np.isfinite(value) or value <= 0.0:
        raise VolRenderError(
            E_INPUT_INVALID,
            "error threshold must be positive and finite",
        )
    return value


@dataclass(frozen=True)
class ErrorSummary:
    threshold: float
    mean_error: float
    max_error: float
    p95_error: float
    untrusted_pixels: int
    total_pixels: int
    trusted_fraction: float
    converged: bool
    status: str

    def as_dict(self) -> dict[str, object]:
        return {
            "threshold": self.threshold,
            "mean_error": self.mean_error,
            "max_error": self.max_error,
            "p95_error": self.p95_error,
            "untrusted_pixels": self.untrusted_pixels,
            "total_pixels": self.total_pixels,
            "trusted_fraction": self.trusted_fraction,
            "converged": self.converged,
            "status": self.status,
        }


@dataclass(frozen=True)
class RenderResult:
    rgba8: np.ndarray
    linear_rgb: np.ndarray
    alpha: np.ndarray
    error_map: np.ndarray
    untrusted: np.ndarray
    error_summary: ErrorSummary
    content_hash: str
    state_hash: str
    mode: str
    state: dict[str, Any]
    elapsed_ms: float

    def as_dict(self, *, include_errors: bool = True) -> dict[str, Any]:
        result: dict[str, Any] = {
            "content_hash": self.content_hash,
            "state_hash": self.state_hash,
            "mode": self.mode,
            "size": [int(self.rgba8.shape[1]), int(self.rgba8.shape[0])],
            "elapsed_ms": self.elapsed_ms,
            "error_summary": self.error_summary.as_dict(),
            "state": self.state,
        }
        if include_errors:
            result["untrusted_pixels"] = int(np.count_nonzero(self.untrusted))
        return result


@dataclass(frozen=True)
class LightDriftReport:
    reference_step: float
    fine_step: float
    tolerance: float
    compared_pixels: int
    mean_abs_diff: float
    p95_abs_diff: float
    max_abs_diff: float
    passed: bool

    def as_dict(self) -> dict[str, object]:
        return {
            "reference_step": self.reference_step,
            "fine_step": self.fine_step,
            "tolerance": self.tolerance,
            "compared_pixels": self.compared_pixels,
            "mean_abs_diff": self.mean_abs_diff,
            "p95_abs_diff": self.p95_abs_diff,
            "max_abs_diff": self.max_abs_diff,
            "passed": self.passed,
        }


@dataclass(frozen=True)
class PrecisionComparison:
    float_result: RenderResult
    fixed_result: RenderResult
    mean_abs_pixel_diff: float
    rms_pixel_diff: float
    max_abs_pixel_diff: float
    changed_pixel_fraction: float
    float_max_error: float
    fixed_max_error: float
    float_untrusted_pixels: int
    fixed_untrusted_pixels: int
    semantics_unchanged: bool

    def as_dict(self) -> dict[str, object]:
        return {
            "mean_abs_pixel_diff": self.mean_abs_pixel_diff,
            "rms_pixel_diff": self.rms_pixel_diff,
            "max_abs_pixel_diff": self.max_abs_pixel_diff,
            "changed_pixel_fraction": self.changed_pixel_fraction,
            "float_max_error": self.float_max_error,
            "fixed_max_error": self.fixed_max_error,
            "float_untrusted_pixels": self.float_untrusted_pixels,
            "fixed_untrusted_pixels": self.fixed_untrusted_pixels,
            "semantics_unchanged": self.semantics_unchanged,
        }


@dataclass(frozen=True)
class ModeSwitchReport:
    mode: str
    result: RenderResult
    comparison: PrecisionComparison

    def as_dict(self) -> dict[str, object]:
        return {
            "mode": self.mode,
            "result": self.result.as_dict(),
            "comparison": self.comparison.as_dict(),
        }


class VolumeRenderer:
    """Calibrated NumPy renderer with explicit state and error observability."""

    def __init__(
        self,
        *,
        volume: VolumeData | None,
        transfer: TransferFunction | None,
        mode: str = "float",
        view_matrix: np.ndarray | None = None,
        step: float = 1.0,
        image_size: tuple[int, int] = (96, 96),
        error_threshold: float = 1.0e-3,
        fov_degrees: float = 38.0,
        lut_resolution: int = 65536,
        fixed_bits: int = 12,
        fixed_color_bits: int = 12,
        lighting_tolerance: float = 0.20,
    ) -> None:
        if mode not in PRECISION_MODES:
            raise VolRenderError(
                E_MODE_INVALID,
                "precision mode must be 'float' or 'fixed'",
                details={"mode": mode},
            )
        if volume is None or transfer is None:
            raise VolRenderError(
                E_MODE_INVALID,
                "renderer is not initialized with a volume and transfer function",
            )
        self.volume = volume
        self.transfer = transfer
        self.mode = mode
        self.step = _validate_step(step)
        self.image_size = self._validate_image_size(image_size)
        self.error_threshold = _validate_threshold(error_threshold)
        self.fov_degrees = self._validate_fov(fov_degrees)
        self.lut_resolution = int(lut_resolution)
        self.fixed_bits = int(fixed_bits)
        self.fixed_color_bits = int(fixed_color_bits)
        self.lighting_tolerance = self._validate_tolerance(lighting_tolerance)
        self.view_matrix = (
            validate_view_matrix(view_matrix)
            if view_matrix is not None
            else default_view_matrix(volume.world_extent)
        )
        self._lut = self.transfer.build_lut(
            self.mode,
            resolution=self.lut_resolution,
            fixed_bits=self.fixed_bits,
            fixed_color_bits=self.fixed_color_bits,
        )
        self._last_good_result: RenderResult | None = None
        self._offscreen_result: RenderResult | None = None

    @staticmethod
    def _validate_image_size(image_size: Sequence[int]) -> tuple[int, int]:
        if len(image_size) != 2:
            raise VolRenderError(E_INPUT_INVALID, "image size must be (width, height)")
        width, height = int(image_size[0]), int(image_size[1])
        if width <= 0 or height <= 0:
            raise VolRenderError(E_INPUT_INVALID, "image size must be positive")
        if width * height > 4_000_000:
            raise VolRenderError(E_INPUT_INVALID, "image size is too large")
        return width, height

    @staticmethod
    def _validate_fov(value: float) -> float:
        fov = float(value)
        if not np.isfinite(fov) or not 1.0 < fov < 179.0:
            raise VolRenderError(E_INPUT_INVALID, "field of view must be in (1, 179)")
        return fov

    @staticmethod
    def _validate_tolerance(value: float) -> float:
        tolerance = float(value)
        if not np.isfinite(tolerance) or tolerance < 0.0:
            raise VolRenderError(E_INPUT_INVALID, "lighting tolerance must be non-negative")
        return tolerance

    @property
    def lut(self) -> LUTBundle:
        return self._lut

    @property
    def last_good_result(self) -> RenderResult | None:
        return self._last_good_result

    def _build_bundle(self, mode: str) -> LUTBundle:
        if mode not in PRECISION_MODES:
            raise VolRenderError(
                E_MODE_INVALID,
                "precision mode must be 'float' or 'fixed'",
                details={"mode": mode},
            )
        return self.transfer.build_lut(
            mode,
            resolution=self.lut_resolution,
            fixed_bits=self.fixed_bits,
            fixed_color_bits=self.fixed_color_bits,
        )

    def state_dict(
        self,
        *,
        mode: str | None = None,
        view_matrix: np.ndarray | None = None,
        step: float | None = None,
        error_threshold: float | None = None,
        image_size: tuple[int, int] | None = None,
        fov_degrees: float | None = None,
    ) -> dict[str, Any]:
        return {
            "volume_id": self.volume.volume_id,
            "mapping_fingerprint": self.volume.mapping_fingerprint(),
            "view_matrix": np.asarray(
                self.view_matrix if view_matrix is None else view_matrix,
                dtype=np.float64,
            ).tolist(),
            "step": float(self.step if step is None else step),
            "fov_degrees": float(
                self.fov_degrees if fov_degrees is None else fov_degrees
            ),
            "image_size": list(self.image_size if image_size is None else image_size),
            "transfer_points": self.transfer.as_list(),
            "precision_mode": self.mode if mode is None else mode,
            "window_width": self.volume.window_width,
            "window_level": self.volume.window_level,
            "spacing": list(self.volume.spacing),
            "error_threshold": float(
                self.error_threshold
                if error_threshold is None
                else error_threshold
            ),
            "lut_resolution": self.lut_resolution,
            "fixed_bits": self.fixed_bits,
            "fixed_color_bits": self.fixed_color_bits,
            "lighting_tolerance": self.lighting_tolerance,
        }

    def state_hash(self) -> str:
        return compute_state_hash(self.state_dict())

    def set_transfer_points(
        self,
        points: Iterable[
            ControlPoint | Mapping[str, float] | Sequence[float]
        ],
    ) -> TransferFunction:
        """Replace the TF transactionally; invalid edits keep the old LUT live."""

        try:
            candidate = TransferFunction(points)
            candidate_lut = candidate.build_lut(
                self.mode,
                resolution=self.lut_resolution,
                fixed_bits=self.fixed_bits,
                fixed_color_bits=self.fixed_color_bits,
            )
        except VolRenderError as exc:
            if exc.code == E_TF_INVALID:
                raise
            raise VolRenderError(E_TF_INVALID, exc.message, details=exc.details) from exc
        except Exception as exc:
            raise VolRenderError(
                E_TF_INVALID,
                "transfer function edit is invalid",
            ) from exc
        self.transfer = candidate
        self._lut = candidate_lut
        return candidate

    def set_transfer(self, transfer: TransferFunction) -> LUTBundle:
        candidate_lut = transfer.build_lut(
            self.mode,
            resolution=self.lut_resolution,
            fixed_bits=self.fixed_bits,
            fixed_color_bits=self.fixed_color_bits,
        )
        self.transfer = transfer
        self._lut = candidate_lut
        return candidate_lut

    def set_window(self, width: float, level: float) -> MappingChange:
        return self.volume.set_window(width, level)

    def set_spacing(self, spacing: Iterable[float]) -> MappingChange:
        return self.volume.set_spacing(spacing)

    def set_step(self, step: float) -> float:
        self.step = _validate_step(step)
        return self.step

    def set_error_threshold(self, threshold: float) -> float:
        self.error_threshold = _validate_threshold(threshold)
        return self.error_threshold

    def set_view_matrix(self, view_matrix: np.ndarray) -> np.ndarray:
        self.view_matrix = validate_view_matrix(view_matrix)
        return self.view_matrix

    def release_offscreen(self) -> None:
        """Drop the transient render buffer while retaining the last good result."""

        self._offscreen_result = None

    def _publish(self, result: RenderResult) -> RenderResult:
        self._offscreen_result = result
        self._last_good_result = result
        return result

    def render(
        self,
        *,
        step: float | None = None,
        view_matrix: np.ndarray | None = None,
        error_threshold: float | None = None,
    ) -> RenderResult:
        chosen_step = self.step if step is None else _validate_step(step)
        chosen_view = (
            self.view_matrix
            if view_matrix is None
            else validate_view_matrix(view_matrix)
        )
        chosen_threshold = (
            self.error_threshold
            if error_threshold is None
            else _validate_threshold(error_threshold)
        )
        try:
            result = self._render_with(
                mode=self.mode,
                bundle=self._lut,
                view_matrix=chosen_view,
                step=chosen_step,
                threshold=chosen_threshold,
            )
            return self._publish(result)
        except Exception:
            self.release_offscreen()
            raise

    def resume_render(self) -> RenderResult:
        """Continue from a restored snapshot while reusing the compiled LUT."""

        return self.render()

    def compare_precision(
        self,
        *,
        step: float | None = None,
        view_matrix: np.ndarray | None = None,
        error_threshold: float | None = None,
    ) -> PrecisionComparison:
        chosen_step = self.step if step is None else _validate_step(step)
        chosen_view = (
            self.view_matrix
            if view_matrix is None
            else validate_view_matrix(view_matrix)
        )
        chosen_threshold = (
            self.error_threshold
            if error_threshold is None
            else _validate_threshold(error_threshold)
        )
        float_bundle = self._build_bundle("float")
        fixed_bundle = self._build_bundle("fixed")
        try:
            float_result = self._render_with(
                mode="float",
                bundle=float_bundle,
                view_matrix=chosen_view,
                step=chosen_step,
                threshold=chosen_threshold,
            )
            fixed_result = self._render_with(
                mode="fixed",
                bundle=fixed_bundle,
                view_matrix=chosen_view,
                step=chosen_step,
                threshold=chosen_threshold,
            )
        except Exception:
            self.release_offscreen()
            raise
        return self._compare_results(float_result, fixed_result)

    @staticmethod
    def _compare_results(
        float_result: RenderResult,
        fixed_result: RenderResult,
    ) -> PrecisionComparison:
        left = float_result.rgba8.astype(np.float64)
        right = fixed_result.rgba8.astype(np.float64)
        diff = np.abs(left - right)
        changed = np.any(diff != 0.0, axis=-1)
        return PrecisionComparison(
            float_result=float_result,
            fixed_result=fixed_result,
            mean_abs_pixel_diff=float(np.mean(diff)),
            rms_pixel_diff=float(np.sqrt(np.mean(diff * diff))),
            max_abs_pixel_diff=float(np.max(diff)),
            changed_pixel_fraction=float(np.mean(changed)),
            float_max_error=float_result.error_summary.max_error,
            fixed_max_error=fixed_result.error_summary.max_error,
            float_untrusted_pixels=float_result.error_summary.untrusted_pixels,
            fixed_untrusted_pixels=fixed_result.error_summary.untrusted_pixels,
            semantics_unchanged=(
                float_result.state["transfer_points"]
                == fixed_result.state["transfer_points"]
            ),
        )

    def set_precision(self, mode: str) -> ModeSwitchReport:
        """Switch LUT precision transactionally and compare both paths."""

        if mode not in PRECISION_MODES:
            raise VolRenderError(
                E_MODE_INVALID,
                "precision mode must be 'float' or 'fixed'",
                details={"mode": mode},
            )
        if self.volume is None or self.transfer is None:
            raise VolRenderError(E_MODE_INVALID, "renderer is not initialized")

        float_bundle = self._build_bundle("float")
        fixed_bundle = self._build_bundle("fixed")
        chosen_bundle = float_bundle if mode == "float" else fixed_bundle
        try:
            float_result = self._render_with(
                mode="float",
                bundle=float_bundle,
                view_matrix=self.view_matrix,
                step=self.step,
                threshold=self.error_threshold,
            )
            fixed_result = self._render_with(
                mode="fixed",
                bundle=fixed_bundle,
                view_matrix=self.view_matrix,
                step=self.step,
                threshold=self.error_threshold,
            )
        except Exception:
            self.release_offscreen()
            raise
        comparison = self._compare_results(float_result, fixed_result)
        self.mode = mode
        self._lut = chosen_bundle
        chosen_result = float_result if mode == "float" else fixed_result
        self._publish(chosen_result)
        return ModeSwitchReport(
            mode=mode,
            result=chosen_result,
            comparison=comparison,
        )

    def _render_with(
        self,
        *,
        mode: str,
        bundle: LUTBundle,
        view_matrix: np.ndarray,
        step: float,
        threshold: float,
    ) -> RenderResult:
        start_time = time.perf_counter()
        width, height = self.image_size
        rays = build_rays(
            view_matrix,
            self.volume.world_extent,
            (width, height),
            fov_degrees=self.fov_degrees,
        )
        spacing = np.asarray(self.volume.spacing, dtype=np.float64)
        shape_xyz = np.asarray(self.volume.shape_xyz, dtype=np.float64)
        half_extent = 0.5 * np.asarray(self.volume.world_extent, dtype=np.float64)
        step_world = float(step * np.mean(spacing))
        max_span = float(np.max(np.where(rays.valid, rays.t_far - rays.t_near, 0.0)))
        max_steps = max(1, int(np.ceil(max_span / step_world)) + 1)

        transmittance = np.ones((height, width), dtype=np.float64)
        transmittance_error = np.zeros_like(transmittance)
        linear_rgb = np.zeros((height, width, 3), dtype=np.float64)
        error_map = np.zeros_like(transmittance)
        light_direction = np.array([-0.40, -0.58, 0.71], dtype=np.float64)
        light_direction /= np.linalg.norm(light_direction)
        volume_values = self.volume.windowed

        for sample_index in range(max_steps):
            t = rays.t_near + (sample_index + 0.5) * step_world
            active = rays.valid & (t < rays.t_far)
            if not np.any(active):
                break
            world = rays.origin + t[..., None] * rays.direction
            coords = (world + half_extent) / spacing - 0.5
            scalar = sample_trilinear(volume_values, coords)
            opacity_lut, color_lut = sample_lut(bundle, scalar)
            opacity_step = self._opacity_for_step(opacity_lut, step)

            normal = self._normals_at(coords)
            diffuse = np.clip(normal @ light_direction, 0.0, 1.0)
            shade = 0.25 + 0.75 * diffuse
            shaded_color = color_lut * shade[..., None]

            opacity_upper = opacity_error_at(self.transfer, scalar, bundle)
            one_minus = np.clip(1.0 - opacity_lut, 1.0e-12, 1.0)
            opacity_derivative = step * np.power(one_minus, step - 1.0)
            opacity_error_step = np.clip(opacity_upper * opacity_derivative, 0.0, 1.0)

            active_f = active.astype(np.float64)
            alpha = np.clip(opacity_step, 0.0, 1.0) * active_f
            d_alpha = opacity_error_step * active_f
            contribution_weight = transmittance * alpha

            error_increment = (
                (transmittance_error * alpha + transmittance * d_alpha)[..., None]
                * shaded_color
                + contribution_weight[..., None] * bundle.color_quant_step
            )
            error_map += np.max(error_increment, axis=-1)
            linear_rgb += contribution_weight[..., None] * shaded_color

            next_transmittance = transmittance * (1.0 - alpha)
            next_transmittance_error = (
                transmittance_error * (1.0 - alpha)
                + transmittance * d_alpha
            )
            transmittance = next_transmittance
            transmittance_error = next_transmittance_error

        alpha_map = np.clip(1.0 - transmittance, 0.0, 1.0)
        untrusted = error_map > threshold
        rgba8 = np.zeros((height, width, 4), dtype=np.uint8)
        trusted_mask = ~untrusted
        rgb8 = np.floor(np.clip(linear_rgb, 0.0, 1.0) * 255.0 + 0.5).astype(np.uint8)
        alpha8 = np.floor(np.clip(alpha_map, 0.0, 1.0) * 255.0 + 0.5).astype(np.uint8)
        rgba8[trusted_mask, :3] = rgb8[trusted_mask]
        rgba8[trusted_mask, 3] = alpha8[trusted_mask]
        rgba8[untrusted] = 0

        content_hash = self._content_hash(rgba8, error_map, untrusted)
        summary = self._error_summary(error_map, threshold, untrusted)
        state = self.state_dict(
            mode=mode,
            view_matrix=view_matrix,
            step=step,
            error_threshold=threshold,
        )
        return RenderResult(
            rgba8=rgba8,
            linear_rgb=linear_rgb,
            alpha=alpha_map,
            error_map=error_map,
            untrusted=untrusted,
            error_summary=summary,
            content_hash=content_hash,
            state_hash=compute_state_hash(state),
            mode=mode,
            state=state,
            elapsed_ms=(time.perf_counter() - start_time) * 1000.0,
        )

    @staticmethod
    def _opacity_for_step(opacity: np.ndarray, step: float) -> np.ndarray:
        base = np.clip(opacity, 0.0, 1.0)
        return 1.0 - np.power(1.0 - base, step)

    def _normals_at(self, coords: np.ndarray) -> np.ndarray:
        volume = self.volume.windowed
        sx, sy, sz = self.volume.spacing
        dx = (
            sample_trilinear(volume, coords + np.array([1.0, 0.0, 0.0]))
            - sample_trilinear(volume, coords + np.array([-1.0, 0.0, 0.0]))
        ) / (2.0 * sx)
        dy = (
            sample_trilinear(volume, coords + np.array([0.0, 1.0, 0.0]))
            - sample_trilinear(volume, coords + np.array([0.0, -1.0, 0.0]))
        ) / (2.0 * sy)
        dz = (
            sample_trilinear(volume, coords + np.array([0.0, 0.0, 1.0]))
            - sample_trilinear(volume, coords + np.array([0.0, 0.0, -1.0]))
        ) / (2.0 * sz)
        norm = np.sqrt(dx * dx + dy * dy + dz * dz)
        normal = np.zeros_like(coords)
        nonzero = norm > 1.0e-12
        denom = np.where(nonzero, norm, 1.0)
        normal[..., 0] = np.where(nonzero, dx / denom, 0.0)
        normal[..., 1] = np.where(nonzero, dy / denom, 0.0)
        normal[..., 2] = np.where(nonzero, dz / denom, 1.0)
        return normal

    @staticmethod
    def _content_hash(
        rgba8: np.ndarray,
        error_map: np.ndarray,
        untrusted: np.ndarray,
    ) -> str:
        digest = hashlib.sha256()
        digest.update(np.ascontiguousarray(rgba8).tobytes())
        digest.update(np.ascontiguousarray(error_map, dtype="<f4").tobytes())
        digest.update(np.packbits(untrusted, axis=None).tobytes())
        return digest.hexdigest()

    @staticmethod
    def _error_summary(
        error_map: np.ndarray,
        threshold: float,
        untrusted: np.ndarray,
    ) -> ErrorSummary:
        flattened = error_map.reshape(-1)
        untrusted_pixels = int(np.count_nonzero(untrusted))
        total = int(flattened.size)
        trusted_fraction = 1.0 - (untrusted_pixels / total if total else 0.0)
        converged = untrusted_pixels == 0
        return ErrorSummary(
            threshold=float(threshold),
            mean_error=float(np.mean(flattened)),
            max_error=float(np.max(flattened)),
            p95_error=float(np.percentile(flattened, 95)),
            untrusted_pixels=untrusted_pixels,
            total_pixels=total,
            trusted_fraction=float(trusted_fraction),
            converged=converged,
            status="converged" if converged else "low_confidence",
        )

    def audit_lighting_drift(
        self,
        *,
        tolerance: float | None = None,
        fine_step: float | None = None,
        raise_on_fail: bool = True,
    ) -> LightDriftReport:
        """Compare the same pixels at the current and half-size ray steps."""

        declared_tolerance = (
            self.lighting_tolerance if tolerance is None else self._validate_tolerance(tolerance)
        )
        reference_step = self.step
        detailed_step = (
            _validate_step(fine_step) if fine_step is not None else 0.5 * reference_step
        )
        coarse = self._render_with(
            mode=self.mode,
            bundle=self._lut,
            view_matrix=self.view_matrix,
            step=reference_step,
            threshold=self.error_threshold,
        )
        fine = self._render_with(
            mode=self.mode,
            bundle=self._lut,
            view_matrix=self.view_matrix,
            step=detailed_step,
            threshold=self.error_threshold,
        )
        compared = (coarse.alpha > 0.01) | (fine.alpha > 0.01)
        compared_count = int(np.count_nonzero(compared))
        if compared_count == 0:
            report = LightDriftReport(
                reference_step=reference_step,
                fine_step=detailed_step,
                tolerance=declared_tolerance,
                compared_pixels=0,
                mean_abs_diff=0.0,
                p95_abs_diff=0.0,
                max_abs_diff=0.0,
                passed=True,
            )
            return report
        diff = np.abs(coarse.linear_rgb[compared] - fine.linear_rgb[compared])
        max_diff = float(np.max(diff))
        report = LightDriftReport(
            reference_step=reference_step,
            fine_step=detailed_step,
            tolerance=declared_tolerance,
            compared_pixels=compared_count,
            mean_abs_diff=float(np.mean(diff)),
            p95_abs_diff=float(np.percentile(diff, 95)),
            max_abs_diff=max_diff,
            passed=max_diff <= declared_tolerance,
        )
        if not report.passed and raise_on_fail:
            raise VolRenderError(
                E_LIGHT_DRIFT,
                "lighting drift exceeds the declared tolerance",
                details=report.as_dict(),
            )
        return report

    def save_snapshot(
        self,
        path: str | Path,
        *,
        result: RenderResult | None = None,
        expected_state_hash: str | None = None,
    ) -> Path:
        from .snapshot import save_snapshot

        chosen = result if result is not None else self._last_good_result
        if chosen is None:
            raise VolRenderError(E_INPUT_INVALID, "no successful render to snapshot")
        return save_snapshot(path, chosen, expected_state_hash=expected_state_hash)

    def restore_snapshot(self, snapshot: Any, *, rerender: bool = False) -> Any:
        from .snapshot import restore_snapshot

        restored = restore_snapshot(self, snapshot)
        if rerender:
            return self.resume_render()
        return restored
