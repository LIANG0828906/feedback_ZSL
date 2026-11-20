"""Scriptable deterministic rendering and fault-injection demonstrations."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .budget import BudgetConfig
from .errors import BusinessError, RenderCancelledError
from .injection import (
    INJECTIONS,
    load_with_injection,
    make_transfer_function,
    render_with_injection,
)
from .pngio import colorize_error, write_png
from .renderer import RayCaster, RenderResult
from .samples import sample_specs
from .snapshot import SnapshotManager, save_snapshot, stale_fields
from .transfer import DEFAULT_CONTROL_POINTS, TransferFunction
from .view import CameraView
from .volume import Volume


def trace_line(event: dict[str, Any]) -> str:
    return json.dumps(
        event,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    )


class TraceWriter:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lines: list[str] = []

    def append(self, event: dict[str, Any]) -> None:
        self._lines.append(trace_line(event))

    def flush(self) -> None:
        self.path.write_text("\n".join(self._lines) + ("\n" if self._lines else ""), encoding="utf-8")


def load_event(volume: Volume) -> dict[str, Any]:
    return {
        "content_hash": volume.volume_sha256,
        "dims": list(volume.dims),
        "event": "load",
        "mask_type": volume.header.mask_type_name,
        "normalization": volume.normalization,
        "scalar_type": volume.header.scalar_type_name,
        "source": "local synthetic SVOL",
        "spacing": list(volume.spacing),
        "window": [volume.header.window_width, volume.header.window_level],
    }


def tf_event(tf: TransferFunction) -> dict[str, Any]:
    return {
        "control_points": [point.as_list() for point in tf.points],
        "event": "tf",
        "interval_error_bounds": tf.interval_error_bounds,
        "lut_digest": tf.digest(),
    }


def render_event(result: RenderResult, tag: str) -> dict[str, Any]:
    return {
        "budget": {
            "max_memory_bytes": result.budget.max_memory_bytes,
            "max_samples": result.budget.max_samples,
            "memory_used_bytes": result.usage["memory_used_bytes"],
            "samples_used": result.usage["samples_used"],
            "resident_volume_bytes": result.usage["resident_volume_bytes"],
        },
        "content_hash": result.content_hash,
        "degradation_stages": result.degradation_stages,
        "early_term_alpha": result.early_term_alpha,
        "error": result.error_summary,
        "event": "render",
        "height": result.height,
        "ray_batch": result.ray_batch,
        "stage_usage": result.stage_usage,
        "step_size": result.step_size,
        "tag": tag,
        "view_hash": result.view_hash,
        "width": result.width,
    }


def _auto_image_size(volume: Volume) -> int:
    return 256 if max(volume.dims) <= 128 else 192


def _budget_fractions(
    raycaster: RayCaster,
    view: CameraView,
    *,
    image_size: int,
    step_size: float,
    ray_batch: int,
) -> list[BudgetConfig]:
    ample = BudgetConfig(
        max_samples=10**18,
        max_memory_bytes=10**18,
        ray_batch=ray_batch,
    )
    huge = raycaster.plan(
        view,
        width=image_size,
        height=image_size,
        step_size=step_size,
        budget=ample,
    )
    base_estimate = max(1, huge.estimated_samples)
    factor = 4.0
    increased_estimate = max(1, raycaster._estimate_samples(  # deterministic planning helper
        raycaster._geometry(view, image_size, image_size, step_size),
        step_size * factor,
        0.98,
    ))
    lowered_estimate = max(1, raycaster._estimate_samples(
        raycaster._geometry(view, image_size, image_size, step_size),
        step_size * factor,
        0.70,
    ))
    # Place each budget just below the preceding stage's estimate and above
    # the following stage's estimate.  The runtime counter remains the final
    # authority and is still checked on every ray batch.
    stage1_samples = max(1, (base_estimate + increased_estimate) // 2)
    stage2_samples = max(1, (increased_estimate + lowered_estimate) // 2)
    stage1 = BudgetConfig(
        max_samples=stage1_samples,
        max_memory_bytes=10**18,
        ray_batch=ray_batch,
    )
    stage2 = BudgetConfig(
        max_samples=stage2_samples,
        max_memory_bytes=10**18,
        ray_batch=ray_batch,
    )
    fixed_memory = image_size * image_size * 7 + 4096
    reduced_memory = fixed_memory + 1024 * 64 + 4096
    stage3 = BudgetConfig(
        max_samples=stage2_samples,
        max_memory_bytes=reduced_memory,
        ray_batch=ray_batch,
    )
    return [ample, stage1, stage2, stage3]


def render_budget_trajectory(
    *,
    volume_path: str | Path,
    output_dir: str | Path,
    tier: str | None = None,
    seed: int = 20261121,
) -> dict[str, Any]:
    """Run baseline + three degradation budgets and write deterministic artifacts."""

    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    trace = TraceWriter(out / "trace.jsonl")
    volume = load_with_injection(volume_path, None)
    trace.append(load_event(volume))
    tf = make_transfer_function(DEFAULT_CONTROL_POINTS)
    trace.append(tf_event(tf))
    view = CameraView()
    image_size = _auto_image_size(volume)
    step_size = 1.0 if max(volume.dims) <= 128 else 0.7
    raycaster = RayCaster(volume, tf)

    baseline_budget = BudgetConfig(
        max_samples=10**18,
        max_memory_bytes=10**18,
        ray_batch=8192,
    )
    baseline = raycaster.render(
        view,
        width=image_size,
        height=image_size,
        step_size=step_size,
        budget=baseline_budget,
    )
    budgets = _budget_fractions(
        raycaster,
        view,
        image_size=image_size,
        step_size=step_size,
        ray_batch=8192,
    )
    results: list[RenderResult] = []
    for index, budget in enumerate(budgets):
        result = raycaster.render(
            view,
            width=image_size,
            height=image_size,
            step_size=step_size,
            budget=budget,
        )
        results.append(result)
        trace.append(render_event(result, f"budget[{index}]"))
        if index == 0:
            # The first render is the ample-budget reference; its error fields
            # are zero by construction.
            assert result.usage["samples_used"] == baseline.usage["samples_used"]

    final = results[-1]
    repeat = raycaster.render(
        view,
        width=image_size,
        height=image_size,
        step_size=step_size,
        budget=budgets[-1],
    )
    repeat_summary = repeat.summary_dict()
    final_summary = final.summary_dict()
    repeat_identical = bool(
        np.array_equal(final.pixels, repeat.pixels)
        and repeat_summary == final_summary
        and np.array_equal(final.opacity, repeat.opacity)
    )
    trace.append({"event": "repeat_check", "identical": repeat_identical})

    write_png(out / "render.png", final.pixels)
    maximum = max(1, int(np.max(np.abs(final.pixels.astype(np.int16) - baseline.pixels.astype(np.int16)))))
    difference = np.max(
        np.abs(final.pixels.astype(np.int16) - baseline.pixels.astype(np.int16)),
        axis=2,
    )
    write_png(out / "error_heatmap.png", colorize_error(difference, max_error=maximum))

    manager = SnapshotManager()
    manager.current_result = final
    manager.commit(out / "snapshot.json", final)
    loaded = manager.load(out / "snapshot.json")
    view_changed = CameraView(yaw_deg=40.0, pitch_deg=24.0)
    modified = final.snapshot_fields()
    modified["view_matrix"] = view_changed.matrix_list
    modified["control_points"] = [[0.0, 0.0, 0.0, 0.0, 0.0], [1.0, 1.0, 1.0, 1.0, 1.0]]
    modified["budget"] = {**modified["budget"], "max_samples": 1234}
    stale_detected = stale_fields(loaded.payload, modified)
    trace.append(
        {
            "content_hash": final.content_hash,
            "event": "snapshot",
            "path": "snapshot.json",
            "stale_fields": stale_detected,
            "version": loaded.payload["version"],
        }
    )
    trace.append(
        {
            "event": "done",
            "images": ["render.png", "error_heatmap.png"],
            "repeat_identical": repeat_identical,
            "stages": final.degradation_stages,
            "tier": tier or "auto",
        }
    )
    trace.flush()
    return {
        "tier": tier,
        "results": results,
        "baseline": baseline,
        "repeat_identical": repeat_identical,
        "trace": trace.path,
        "snapshot": out / "snapshot.json",
    }


def run_one_injection(
    *,
    injection: str,
    volume_path: str | Path,
    output_dir: str | Path,
) -> dict[str, Any]:
    out = Path(output_dir)
    out.mkdir(parents=True, exist_ok=True)
    if not injection:
        raise ValueError("injection name is required")
    trace = TraceWriter(out / "trace.jsonl")
    try:
        if injection in ("dim_mismatch", "window_nonfinite", "truncated_voxels"):
            volume = load_with_injection(volume_path, injection)
            trace.append(load_event(volume))
            tf = make_transfer_function(DEFAULT_CONTROL_POINTS)
            trace.append(tf_event(tf))
        else:
            volume = load_with_injection(volume_path, None)
            trace.append(load_event(volume))
            tf = make_transfer_function(DEFAULT_CONTROL_POINTS, injection=injection)
            trace.append(tf_event(tf))
            if injection == "budget_overflow":
                ray = RayCaster(volume, tf)
                render_with_injection(
                    ray,
                    CameraView(),
                    width=64,
                    height=64,
                    step_size=1.0,
                    budget=BudgetConfig(),
                    injection=injection,
                )
    except BusinessError as exc:
        trace.append(exc.to_dict())
        trace.flush()
        return {"injection": injection, "error": exc.to_dict(), "trace": trace.path}
    # No PNG/snapshot write exists in this function; an injected path can only
    # leave a trace.
    trace.flush()
    return {"injection": injection, "error": None, "trace": trace.path}


def run_all_injections(
    *,
    volume_path: str | Path,
    root_output: str | Path,
) -> list[dict[str, Any]]:
    root = Path(root_output)
    outputs: list[dict[str, Any]] = []
    for injection in INJECTIONS:
        result = run_one_injection(
            injection=injection,
            volume_path=volume_path,
            output_dir=root / f"demo_inj_{injection}",
        )
        outputs.append(result)
    return outputs


def generate_baseline_artifacts(
    *,
    volume_path: str | Path,
    output_dir: str | Path,
    tier: str,
    seed: int,
) -> dict[str, Any]:
    from .samples import write_baseline_metadata

    out = Path(output_dir)
    volume = load_with_injection(volume_path, None)
    tf = make_transfer_function(DEFAULT_CONTROL_POINTS)
    spec = sample_specs()[tier]
    result = RayCaster(volume, tf).render(
        CameraView(),
        width=int(spec["baseline_image"]),
        height=int(spec["baseline_image"]),
        step_size=float(spec["baseline_step"]),
        budget=BudgetConfig(
            max_samples=10**18,
            max_memory_bytes=10**18,
            ray_batch=8192,
        ),
    )
    write_png(out / "baseline.png", result.pixels)
    metadata = write_baseline_metadata(
        out / "baseline_hash.json",
        tier=tier,
        dims=volume.dims,
        spacing=volume.spacing,
        seed=seed,
        volume_sha256=volume.volume_sha256,
        normalization=volume.normalization,
        step_size=float(spec["baseline_step"]),
        image_size=int(spec["baseline_image"]),
        content_hash=result.content_hash,
    )
    return metadata


__all__ = [
    "TraceWriter",
    "generate_baseline_artifacts",
    "load_event",
    "render_budget_trajectory",
    "render_event",
    "run_all_injections",
    "run_one_injection",
    "tf_event",
    "trace_line",
]
