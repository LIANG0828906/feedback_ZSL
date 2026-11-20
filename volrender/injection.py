"""External fault-injection adapters.

The production loader, transfer-function and renderer modules are not patched.
Injections alter a private copy of bytes or a candidate configuration before
those modules are called.
"""

from __future__ import annotations

from dataclasses import dataclass
import math
from pathlib import Path
import struct
from typing import Any, Iterable, Sequence

from .budget import BudgetConfig
from .errors import BusinessError
from .renderer import RayCaster
from .transfer import ControlPoint, TransferFunction
from .volume import load_volume, parse_header


INJECTIONS = (
    "dim_mismatch",
    "window_nonfinite",
    "tf_out_of_range",
    "truncated_voxels",
    "budget_overflow",
)


@dataclass(frozen=True)
class InjectionContext:
    name: str
    injection_point: str


def _context(name: str, phase: str) -> InjectionContext:
    return InjectionContext(name=name, injection_point=f"{phase}_entry:{name}")


def inject_volume_bytes(data: bytes, injection: str) -> bytes:
    if injection not in ("dim_mismatch", "window_nonfinite", "truncated_voxels"):
        raise ValueError(f"not a volume injection: {injection}")
    header = parse_header(data)
    altered = bytearray(data)
    if injection == "dim_mismatch":
        dim_x = header.dims[0]
        if dim_x <= 1:
            raise ValueError("dim_mismatch requires dim_x > 1")
        struct.pack_into("<I", altered, 8, dim_x - 1)
    elif injection == "window_nonfinite":
        struct.pack_into("<d", altered, 48, math.inf)
    elif injection == "truncated_voxels":
        scalar_itemsize = header.scalar_dtype.itemsize
        cut = header.header_size + max(1, header.scalar_nbytes - scalar_itemsize * 7)
        cut = min(cut, len(altered) - 1)
        altered = altered[:cut]
    return bytes(altered)


def load_with_injection(
    volume_path: str | Path,
    injection: str | None,
) -> Any:
    if injection is None:
        return load_volume(volume_path)
    if injection not in ("dim_mismatch", "window_nonfinite", "truncated_voxels"):
        return load_volume(volume_path)
    context = _context(injection, "load")
    try:
        data = Path(volume_path).read_bytes()
        mutated = inject_volume_bytes(data, injection)
        return load_volume(mutated, source_name=f"{volume_path}#inject={injection}")
    except BusinessError as exc:
        exc.inject = context.name
        exc.injection_point = context.injection_point
        raise


def inject_control_points(
    points: Iterable[ControlPoint | Sequence[float]] | None,
    injection: str | None,
) -> list[ControlPoint | Sequence[float]]:
    candidate: list[ControlPoint | Sequence[float]] = list(points or [])
    if injection == "tf_out_of_range":
        candidate.append((1.5, 1.0, 0.0, 0.0, 1.0))
    return candidate


def make_transfer_function(
    points: Iterable[ControlPoint | Sequence[float]] | None,
    *,
    injection: str | None = None,
    resolution: int = 256,
) -> TransferFunction:
    context = _context(injection, "config") if injection else None
    try:
        return TransferFunction(
            inject_control_points(points, injection),
            resolution=resolution,
        )
    except BusinessError as exc:
        if context is not None:
            exc.inject = context.name
            exc.injection_point = context.injection_point
        raise


def make_budget(
    budget: BudgetConfig,
    *,
    injection: str | None = None,
) -> BudgetConfig:
    if injection == "budget_overflow":
        # One sample is intentionally impossible for a complete image.
        return BudgetConfig(
            max_samples=1,
            max_memory_bytes=budget.max_memory_bytes,
            ray_batch=budget.ray_batch,
            reduced_ray_batch=budget.reduced_ray_batch,
            increase_step_factor=budget.increase_step_factor,
            early_term_alpha=budget.early_term_alpha,
            degraded_early_term_alpha=budget.degraded_early_term_alpha,
            sequence=budget.sequence,
        )
    return budget


def render_with_injection(
    raycaster: RayCaster,
    view: Any,
    *,
    width: int,
    height: int,
    step_size: float,
    budget: BudgetConfig,
    injection: str | None,
) -> Any:
    context = _context(injection, "config") if injection else None
    try:
        selected = make_budget(budget, injection=injection)
        return raycaster.render(
            view,
            width=width,
            height=height,
            step_size=step_size,
            budget=selected,
        )
    except BusinessError as exc:
        if context is not None:
            exc.inject = context.name
            exc.injection_point = context.injection_point
        raise


__all__ = [
    "INJECTIONS",
    "InjectionContext",
    "inject_control_points",
    "inject_volume_bytes",
    "load_with_injection",
    "make_budget",
    "make_transfer_function",
    "render_with_injection",
]

