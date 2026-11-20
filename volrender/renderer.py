"""Deterministic, budgeted NumPy ray casting.

The renderer is intentionally free of wall-clock metadata and does not write
images.  A caller can inspect or persist a result only after a complete render.
"""

from __future__ import annotations

from dataclasses import dataclass, field
import hashlib
import json
import math
import threading
from typing import Any, Callable, Mapping

import numpy as np

from .budget import BudgetConfig, RenderPlan, plan_render
from .errors import BudgetExceededError, RenderCancelledError
from .transfer import TransferFunction
from .view import CameraView
from .volume import Volume


def _canonical(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


@dataclass
class RenderResult:
    pixels: np.ndarray
    opacity: np.ndarray
    width: int
    height: int
    step_size: float
    ray_batch: int
    early_term_alpha: float
    degradation_stages: list[str]
    stage_usage: list[dict[str, Any]]
    usage: dict[str, Any]
    error_summary: dict[str, Any]
    content_hash: str
    view_hash: str
    view_matrix: list[list[float]]
    control_points: list[list[float]]
    precision_mode: str
    budget: BudgetConfig
    backend: str = "numpy"

    def summary_dict(self) -> dict[str, Any]:
        return {
            "backend": self.backend,
            "budget": {
                "max_memory_bytes": self.budget.max_memory_bytes,
                "max_samples": self.budget.max_samples,
                "memory_used_bytes": self.usage["memory_used_bytes"],
                "samples_used": self.usage["samples_used"],
            },
            "content_hash": self.content_hash,
            "degradation_stages": list(self.degradation_stages),
            "early_term_alpha": self.early_term_alpha,
            "error": dict(self.error_summary),
            "height": self.height,
            "ray_batch": self.ray_batch,
            "stage_usage": [dict(item) for item in self.stage_usage],
            "step_size": self.step_size,
            "view_hash": self.view_hash,
            "width": self.width,
        }

    def snapshot_fields(self) -> dict[str, Any]:
        return {
            "budget": self.budget.to_dict(),
            "content_hash": self.content_hash,
            "control_points": [list(point) for point in self.control_points],
            "degradation_stages": list(self.degradation_stages),
            "precision_mode": self.precision_mode,
            "step_size": self.step_size,
            "view_hash": self.view_hash,
            "view_matrix": [list(row) for row in self.view_matrix],
        }


@dataclass
class _Geometry:
    origins: np.ndarray
    direction: np.ndarray
    t_enter: np.ndarray
    t_exit: np.ndarray
    box_half: np.ndarray
    max_steps: int
    world_step: float
    total_rays: int


class RayCaster:
    def __init__(
        self,
        volume: Volume,
        transfer_function: TransferFunction,
        *,
        precision_mode: str = "fp32",
    ) -> None:
        if precision_mode not in ("fp32", "fp64"):
            raise ValueError("precision_mode must be 'fp32' or 'fp64'")
        self.volume = volume
        self.tf = transfer_function
        self.precision_mode = precision_mode
        self._baseline_cache: dict[str, RenderResult] = {}
        self._cache_lock = threading.Lock()

    def _geometry(
        self,
        view: CameraView,
        width: int,
        height: int,
        step_size: float,
    ) -> _Geometry:
        if width <= 0 or height <= 0:
            raise ValueError("render dimensions must be positive")
        dims = np.asarray(self.volume.dims, dtype=np.float64)
        spacing = np.asarray(self.volume.spacing, dtype=np.float64)
        physical = dims * spacing
        max_extent = float(np.max(physical))
        box_half = physical / (2.0 * max_extent)
        direction = view.direction
        right = view.right
        up = view.up
        x = ((np.arange(width, dtype=np.float64) + 0.5) / width) * 2.0 - 1.0
        y = 1.0 - ((np.arange(height, dtype=np.float64) + 0.5) / height) * 2.0
        grid_y, grid_x = np.meshgrid(y, x, indexing="ij")
        plane_scale = 0.88 / view.zoom
        origins = (
            -direction * 2.0
            + right[None, None, :] * (grid_x[..., None] * plane_scale)
            + up[None, None, :] * (grid_y[..., None] * plane_scale)
        ).reshape(-1, 3)

        safe_direction = np.where(np.abs(direction) < 1e-12, 1e-12, direction)
        t0 = (-box_half[None, :] - origins) / safe_direction[None, :]
        t1 = (box_half[None, :] - origins) / safe_direction[None, :]
        t_near = np.minimum(t0, t1)
        t_far = np.maximum(t0, t1)
        t_enter = np.max(t_near, axis=1)
        t_exit = np.min(t_far, axis=1)
        t_exit = np.maximum(t_exit, t_enter)

        world_step = float(step_size) / float(np.max(self.volume.dims))
        if not math.isfinite(world_step) or world_step <= 0.0:
            raise ValueError("step_size must be finite and positive")
        max_path = float(np.sqrt(3.0))
        max_steps = max(1, int(math.ceil(max_path / world_step)) + 1)
        return _Geometry(
            origins=origins,
            direction=direction,
            t_enter=t_enter,
            t_exit=t_exit,
            box_half=box_half,
            max_steps=max_steps,
            world_step=world_step,
            total_rays=width * height,
        )

    def _estimate_samples(
        self, geometry: _Geometry, step_size: float, early_alpha: float
    ) -> int:
        world_step = step_size / float(np.max(self.volume.dims))
        path = np.maximum(0.0, geometry.t_exit - geometry.t_enter)
        counts = np.floor(path / world_step + 1e-9).astype(np.int64) + 1
        total = int(np.sum(counts))
        if early_alpha <= 0.8:
            # A conservative planning reduction for early termination.  The
            # runtime counter remains authoritative and is checked each batch.
            total = int(math.ceil(total * 0.75))
        return total

    def _estimate_memory(self, width: int, height: int, ray_batch: int) -> int:
        scale = 1 if self.precision_mode == "fp32" else 2
        output = width * height * (3 + 4) * scale  # RGB and opacity buffers
        batch_work = ray_batch * 64 * scale
        lut_and_constants = 4096
        return int(output + batch_work + lut_and_constants)

    def estimate_memory(self, width: int, height: int, ray_batch: int) -> int:
        return self._estimate_memory(width, height, ray_batch)

    def estimate_samples(
        self,
        view: CameraView,
        *,
        width: int,
        height: int,
        step_size: float,
        early_term_alpha: float = 0.98,
    ) -> int:
        geometry = self._geometry(view, width, height, step_size)
        return self._estimate_samples(geometry, step_size, early_term_alpha)

    def plan(
        self,
        view: CameraView,
        *,
        width: int,
        height: int,
        step_size: float,
        budget: BudgetConfig,
    ) -> RenderPlan:
        geometry = self._geometry(view, width, height, step_size)
        return plan_render(
            base_step_size=step_size,
            budget=budget,
            estimate_samples=lambda step, alpha: self._estimate_samples(geometry, step, alpha),
            estimate_memory=lambda batch: self._estimate_memory(width, height, batch),
        )

    @staticmethod
    def _trilinear(
        array: np.ndarray,
        positions: np.ndarray,
        box_half: np.ndarray,
    ) -> np.ndarray:
        # positions are x,y,z while SVOL arrays are indexed [z,y,x].
        dims = np.asarray(array.shape[::-1], dtype=np.float64)
        normalized = np.clip((positions + box_half[None, :]) / (2.0 * box_half[None, :]), 0.0, 1.0)
        coords = normalized * (dims[None, :] - 1.0)
        lower = np.floor(coords).astype(np.intp)
        upper = np.minimum(lower + 1, dims.astype(np.intp)[None, :] - 1)
        fraction = coords - lower.astype(np.float64)
        x0, y0, z0 = lower[:, 0], lower[:, 1], lower[:, 2]
        x1, y1, z1 = upper[:, 0], upper[:, 1], upper[:, 2]
        c000 = array[z0, y0, x0]
        c100 = array[z0, y0, x1]
        c010 = array[z0, y1, x0]
        c110 = array[z0, y1, x1]
        c001 = array[z1, y0, x0]
        c101 = array[z1, y0, x1]
        c011 = array[z1, y1, x0]
        c111 = array[z1, y1, x1]
        fx, fy, fz = fraction[:, 0], fraction[:, 1], fraction[:, 2]
        c00 = c000 * (1.0 - fx) + c100 * fx
        c10 = c010 * (1.0 - fx) + c110 * fx
        c01 = c001 * (1.0 - fx) + c101 * fx
        c11 = c011 * (1.0 - fx) + c111 * fx
        c0 = c00 * (1.0 - fy) + c10 * fy
        c1 = c01 * (1.0 - fy) + c11 * fy
        return c0 * (1.0 - fz) + c1 * fz

    def _content_hash(
        self,
        pixels: np.ndarray,
        *,
        width: int,
        height: int,
        step_size: float,
        early_alpha: float,
        view_hash: str,
        precision: str,
        transfer_function: TransferFunction,
    ) -> str:
        metadata = {
            "control_points": [point.as_list() for point in transfer_function.points],
            "early_term_alpha": early_alpha,
            "height": height,
            "precision_mode": precision,
            "step_size": step_size,
            "tf_digest": transfer_function.digest(),
            "view_hash": view_hash,
            "width": width,
        }
        digest = hashlib.sha256()
        digest.update(_canonical(metadata).encode("utf-8"))
        digest.update(b"\x00")
        digest.update(pixels.tobytes(order="C"))
        return digest.hexdigest()

    def _run(
        self,
        view: CameraView,
        *,
        width: int,
        height: int,
        base_step_size: float,
        budget: BudgetConfig,
        cancel_event: threading.Event | None = None,
        checkpoint: Callable[[str, Mapping[str, Any]], None] | None = None,
        compute_error: bool = True,
        force_no_degradation: bool = False,
        transfer_function: TransferFunction | None = None,
    ) -> RenderResult:
        # Freeze the TF used by this render.  An editor update in another
        # thread cannot change the committed pixels or their metadata.
        local_tf = transfer_function or TransferFunction(
            self.tf.points, resolution=self.tf.resolution
        )
        geometry = self._geometry(view, width, height, base_step_size)
        if force_no_degradation:
            plan = RenderPlan(
                step_size=float(base_step_size),
                early_term_alpha=budget.early_term_alpha,
                ray_batch=budget.ray_batch,
                stages=(),
                estimated_samples=self._estimate_samples(
                    geometry, base_step_size, budget.early_term_alpha
                ),
                estimated_memory_bytes=self._estimate_memory(width, height, budget.ray_batch),
            )
        else:
            plan = plan_render(
                base_step_size=base_step_size,
                budget=budget,
                estimate_samples=lambda step, alpha: self._estimate_samples(geometry, step, alpha),
                estimate_memory=lambda batch: self._estimate_memory(width, height, batch),
                cancel_event=cancel_event,
                stage_checkpoint=checkpoint,
            )

        dtype = np.float32 if self.precision_mode == "fp32" else np.float64
        color = np.zeros((geometry.total_rays, 3), dtype=dtype)
        transmittance = np.ones(geometry.total_rays, dtype=dtype)
        ray_indices = np.arange(geometry.total_rays, dtype=np.uint64)
        sample_jitter = (
            ((ray_indices * np.uint64(11400714819323198485)) & np.uint64(0xFFFFFFFF)).astype(
                np.float64
            )
            / float(0x100000000)
        )
        samples_used = 0
        early_cutoff = dtype(1.0 - plan.early_term_alpha)
        batch_size = min(plan.ray_batch, geometry.total_rays)
        geometry.world_step = plan.step_size / float(np.max(self.volume.dims))
        max_step = int(math.ceil(np.max(geometry.t_exit - geometry.t_enter) / geometry.world_step)) + 2
        max_step = min(max_step, geometry.max_steps)
        lut = local_tf.lut_float.astype(dtype, copy=False)

        for batch_start in range(0, geometry.total_rays, batch_size):
            if cancel_event is not None and cancel_event.is_set():
                raise RenderCancelledError(
                    "CANCELLED",
                    "渲染在光线批检查点被取消",
                    usage={"samples_used": samples_used, "stage": "ray_batch"},
                )
            batch_end = min(batch_start + batch_size, geometry.total_rays)
            origins = geometry.origins[batch_start:batch_end]
            t_enter = geometry.t_enter[batch_start:batch_end]
            t_exit = geometry.t_exit[batch_start:batch_end]
            local_count = batch_end - batch_start
            local_color = np.zeros((local_count, 3), dtype=dtype)
            local_t = np.ones(local_count, dtype=dtype)
            local_jitter = sample_jitter[batch_start:batch_end]
            valid = t_exit >= t_enter

            for sample_index in range(max_step):
                if cancel_event is not None and cancel_event.is_set():
                    raise RenderCancelledError(
                        "CANCELLED",
                        "渲染在采样检查点被取消",
                        usage={
                            "samples_used": samples_used,
                            "stage": "sample",
                            "ray_batch_start": batch_start,
                        },
                    )
                t = t_enter + (sample_index + local_jitter) * geometry.world_step
                active = (
                    valid
                    & (t <= t_exit + 1e-12)
                    & (local_t > early_cutoff)
                )
                count = int(np.count_nonzero(active))
                if count == 0:
                    break
                if samples_used + count > budget.max_samples and not force_no_degradation:
                    usage = {
                        "budget": budget.to_dict(),
                        "final_estimate": {
                            "memory_bytes": self._estimate_memory(width, height, batch_size),
                            "samples": samples_used + count,
                        },
                        "stage_usage": list(plan.stages),
                    }
                    raise BudgetExceededError(
                        "RUNTIME_SAMPLE_BUDGET_EXCEEDED",
                        f"运行时采样数 {samples_used + count} 超出预算 {budget.max_samples}",
                        usage=usage,
                    )
                samples_used += count
                positions = origins[active] + geometry.direction[None, :] * t[active, None]
                scalar_values = self._trilinear(self.volume.scalar, positions, geometry.box_half)
                rgba = lut[
                    np.rint(np.clip(scalar_values, 0.0, 1.0) * (local_tf.resolution - 1)).astype(
                        np.intp
                    )
                ]
                alpha = rgba[:, 3].astype(dtype, copy=False)
                local_color[active] += (
                    local_t[active, None] * alpha[:, None] * rgba[:, :3]
                )
                local_t[active] *= (1.0 - alpha)
                local_t = np.clip(local_t, 0.0, 1.0)

            color[batch_start:batch_end] = local_color
            transmittance[batch_start:batch_end] = local_t
            if checkpoint is not None:
                checkpoint(
                    "ray_batch",
                    {
                        "completed_rays": batch_end,
                        "samples_used": samples_used,
                        "total_rays": geometry.total_rays,
                    },
                )

        rgb = color + transmittance[:, None]
        pixels = np.rint(np.clip(rgb, 0.0, 1.0).reshape(height, width, 3) * 255.0).astype(np.uint8)
        opacity = (1.0 - transmittance).reshape(height, width).astype(np.float32)
        memory_used = self._estimate_memory(width, height, batch_size)
        degradation = plan.degradation_names
        usage = {
            "estimated_memory_bytes": plan.estimated_memory_bytes,
            "estimated_samples": plan.estimated_samples,
            "max_memory_bytes": budget.max_memory_bytes,
            "max_samples": budget.max_samples,
            "memory_used_bytes": memory_used,
            "resident_volume_bytes": self.volume.resident_bytes,
            "samples_used": samples_used,
            "sequence": list(budget.sequence),
        }
        content_hash = self._content_hash(
            pixels,
            width=width,
            height=height,
            step_size=plan.step_size,
            early_alpha=plan.early_term_alpha,
            view_hash=view.hash,
            precision=self.precision_mode,
            transfer_function=local_tf,
        )

        if not compute_error:
            error_summary = {
                "max_pixel_deviation": 0,
                "mean_pixel_deviation": 0.0,
                "opacity_deviation_bound": 0.0,
            }
        elif plan.stages:
            cache_key = _canonical(
                {
                    "height": height,
                    "precision": self.precision_mode,
                    "step": float(base_step_size),
                    "tf": local_tf.digest(),
                    "view": view.hash,
                    "volume": self.volume.volume_sha256,
                    "width": width,
                    "early_alpha": budget.early_term_alpha,
                }
            )
            with self._cache_lock:
                baseline = self._baseline_cache.get(cache_key)
            if baseline is None:
                baseline = self._run(
                    view,
                    width=width,
                    height=height,
                    base_step_size=base_step_size,
                    budget=BudgetConfig(
                        max_samples=10**18,
                        max_memory_bytes=10**18,
                        ray_batch=budget.ray_batch,
                        early_term_alpha=budget.early_term_alpha,
                    ),
                    cancel_event=cancel_event,
                    checkpoint=checkpoint,
                    compute_error=False,
                    force_no_degradation=True,
                    transfer_function=local_tf,
                )
                with self._cache_lock:
                    self._baseline_cache[cache_key] = baseline
            difference = np.abs(pixels.astype(np.int16) - baseline.pixels.astype(np.int16))
            ratio = plan.step_size / float(base_step_size)
            step_bound = 1.0 - math.exp(
                -max(0.0, ratio - 1.0) * max(0.05, local_tf.max_opacity) * 0.5
            )
            early_bound = max(0.0, budget.early_term_alpha - plan.early_term_alpha)
            # Union bound for two independent degradation sources.
            opacity_bound = 1.0 - (1.0 - step_bound) * (1.0 - early_bound)
            error_summary = {
                "max_pixel_deviation": int(np.max(difference)),
                "mean_pixel_deviation": float(np.mean(difference)),
                "opacity_deviation_bound": float(min(1.0, opacity_bound)),
            }
        else:
            error_summary = {
                "max_pixel_deviation": 0,
                "mean_pixel_deviation": 0.0,
                "opacity_deviation_bound": 0.0,
            }

        return RenderResult(
            pixels=pixels,
            opacity=opacity,
            width=width,
            height=height,
            step_size=plan.step_size,
            ray_batch=batch_size,
            early_term_alpha=plan.early_term_alpha,
            degradation_stages=degradation,
            stage_usage=[dict(item) for item in plan.stages],
            usage=usage,
            error_summary=error_summary,
            content_hash=content_hash,
            view_hash=view.hash,
            view_matrix=view.matrix_list,
            control_points=[point.as_list() for point in local_tf.points],
            precision_mode=self.precision_mode,
            budget=budget,
        )

    def render(
        self,
        view: CameraView,
        *,
        width: int,
        height: int,
        step_size: float,
        budget: BudgetConfig,
        cancel_event: threading.Event | None = None,
        checkpoint: Callable[[str, Mapping[str, Any]], None] | None = None,
    ) -> RenderResult:
        return self._run(
            view,
            width=width,
            height=height,
            base_step_size=step_size,
            budget=budget,
            cancel_event=cancel_event,
            checkpoint=checkpoint,
            compute_error=True,
        )


__all__ = ["RayCaster", "RenderResult"]
