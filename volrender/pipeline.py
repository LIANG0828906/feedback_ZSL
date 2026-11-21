"""Deterministic sample/artifact pipeline and scripted preview scenario."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np

from .buffers import BufferStore
from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .preview import PreviewController
from .renderer import RenderResult, render_offscreen
from .snapshot import SnapshotManager, create_snapshot, load_snapshot, save_snapshot
from .synthetic import DEFAULT_LEVELS, DEFAULT_SEED, baseline_parameters, generate_samples
from .trace import DeterministicTrace
from .transfer import ControlPoint, TransferFunction, validate_control_points
from .volume import map_window


def project_directories(root: str | Path) -> dict[str, Path]:
    root_path = Path(root)
    directories = {
        "root": root_path,
        "samples": root_path / "samples",
        "params": root_path / "params",
        "buffers": root_path / "buffers",
        "snapshots": root_path / "snapshots",
        "traces": root_path / "traces",
        "reports": root_path / "reports",
    }
    for directory in directories.values():
        directory.mkdir(parents=True, exist_ok=True)
    return directories


def _write_json(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, ensure_ascii=True, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )


def generate_project_assets(
    root: str | Path,
    *,
    sizes: tuple[int, ...] = DEFAULT_LEVELS,
    seed: int = DEFAULT_SEED,
) -> dict[str, Any]:
    directories = project_directories(root)
    records = generate_samples(directories["samples"], sizes=sizes, seed=seed)
    buffer_store = BufferStore(directories["buffers"])
    baselines: dict[str, Any] = {}
    for record in records:
        from .volume import load_volume

        volume_path = directories["samples"] / str(record["path"])
        volume = load_volume(volume_path)
        params = baseline_parameters(volume)
        result = render_offscreen(volume, params)
        stored = buffer_store.publish(result, params)
        snapshot = create_snapshot(
            params,
            result,
            color_buffer_name=params.parameter_hash,
            mask_coverage_hash=str(stored.manifest.get("mask_coverage_hash", "")),
        )
        snapshot_name = f"baseline_{volume.dims[0]}.json"
        snapshot_path = directories["snapshots"] / snapshot_name
        save_snapshot(snapshot, snapshot_path)
        params_name = f"baseline_{volume.dims[0]}.json"
        _write_json(directories["params"] / params_name, params.to_snapshot_dict())

        trace_name = f"baseline_{volume.dims[0]}.jsonl"
        with DeterministicTrace(directories["traces"] / trace_name) as trace:
            trace.record(
                "sample_generated",
                seed=seed,
                dims=list(volume.dims),
                spacing=list(volume.spacing),
                scalar_type=volume.header.scalar_type,
                density_sha256=volume.density_sha256,
                mask_sha256=volume.mask_sha256,
                content_sha256=volume.content_sha256,
            )
            _, reference = map_window(volume.density, params.window_center, params.window_width)
            trace.record_mapping(
                "window_mapping",
                window_center=params.window_center,
                window_width=params.window_width,
                reference=reference.to_dict(),
            )
            trace.record_render(
                "baseline_render",
                params,
                result,
                mask_coverage_pixels=int(np.count_nonzero(result.mask_coverage))
                if result.mask_coverage is not None
                else 0,
            )
        record["baseline_buffer_hash"] = result.buffer_hash
        record["baseline_snapshot"] = snapshot_name
        record["baseline_trace"] = trace_name
        record["baseline_buffer_name"] = params.parameter_hash
        baselines[str(volume.dims[0])] = {
            "parameter_hash": params.parameter_hash,
            "buffer_hash": result.buffer_hash,
            "snapshot": snapshot_name,
            "trace": trace_name,
        }

    _write_json(
        directories["samples"] / "samples.json",
        {
            "generator": "volrender.synthetic",
            "generator_version": 1,
            "seed": seed,
            "levels": records,
        },
    )
    report = {
        "generator": "volrender.pipeline",
        "generator_version": 1,
        "seed": seed,
        "levels": baselines,
    }
    _write_json(directories["reports"] / "baseline.json", report)
    return report


def run_demo(root: str | Path, *, size: int = 64) -> dict[str, Any]:
    """Run a fixed, timestamp-free interaction sequence for reproducibility checks."""

    directories = project_directories(root)
    from .volume import load_volume

    volume_path = directories["samples"] / f"synthetic_{size}x{size}x{size}.vol"
    if not volume_path.is_file():
        generate_project_assets(root, sizes=(size,))
    volume = load_volume(volume_path)
    params0 = baseline_parameters(volume)
    store = BufferStore(directories["buffers"])
    stored0 = store.load_verified(params0.parameter_hash, expected_parameter_hash=params0.parameter_hash)
    result0 = stored0.result
    preview = PreviewController()
    preview.commit(params0, result0)

    trace_path = directories["traces"] / f"demo_{size}.jsonl"
    events: list[dict[str, Any]] = []
    with DeterministicTrace(trace_path) as trace:
        trace.record_render("preview_loaded", params0, result0)

        delta = volume.compare_windows(
            params0.window_center,
            params0.window_width,
            params0.window_center + 140.0,
            params0.window_width * 1.18,
        )
        trace.record_mapping(
            "window_mapping_delta",
            old=delta.old.to_dict(),
            new=delta.new.to_dict(),
            max_abs_difference=delta.max_abs_difference,
            affected_voxels=delta.affected_voxels,
            total_voxels=delta.total_voxels,
        )
        params1 = params0.with_changes(
            window_center=params0.window_center + 140.0,
            window_width=params0.window_width * 1.18,
        )
        preview.set_pending(params1)
        trace.record(
            "preview_pending",
            displayed_parameter_hash=preview.displayed_parameter_hash,
            displayed_buffer_hash=preview.displayed_buffer_hash,
            pending_parameter_hash=params1.parameter_hash,
            pending_is_uncommitted=True,
        )
        result1 = render_offscreen(volume, params1)
        store.publish(result1, params1)
        preview.commit(params1, result1)
        trace.record_render("preview_committed_after_window_edit", params1, result1)
        events.append(preview.state())

        controls2 = tuple(
            ControlPoint(
                point.position,
                min(1.0, point.opacity * 1.15),
                point.color,
            )
            for point in params1.control_points
        )
        params2 = params1.with_changes(control_points=validate_control_points(controls2))
        result2 = render_offscreen(volume, params2)
        store.publish(result2, params2)
        preview.set_pending(params2)
        preview.commit(params2, result2)
        trace.record_render("preview_committed_after_tf_edit", params2, result2)

        editor = TransferFunction(params2.control_points)
        revision_before = editor.revision
        try:
            editor.update(
                (
                    params2.control_points[0],
                    params2.control_points[2],
                    params2.control_points[2],
                )
            )
        except VolumeRenderError as exc:
            trace.record(
                "tf_update_rejected",
                code=exc.code.value,
                field=exc.field,
                revision_before=revision_before,
                revision_after=editor.revision,
                lut_retained=True,
            )
        else:
            raise AssertionError("invalid transfer function was accepted")

        no_mask_params = params2.with_changes(mask_enabled=False, mask_hash="")
        result_masked = render_offscreen(volume, params2)
        result_no_mask = render_offscreen(volume, no_mask_params)
        changed_pixels = np.any(result_masked.color != result_no_mask.color, axis=2)
        coverage = (
            result_masked.mask_coverage
            if result_masked.mask_coverage is not None
            else np.zeros(changed_pixels.shape, dtype=bool)
        )
        outside = int(np.count_nonzero(changed_pixels & ~coverage))
        trace.record_render(
            "mask_consistency_render",
            params2,
            result_masked,
            changed_pixels=int(np.count_nonzero(changed_pixels)),
            changed_outside_mask_rays=outside,
            mask_coverage_pixels=int(np.count_nonzero(coverage)),
        )
        if outside != 0:
            raise AssertionError("mask overlay changed pixels outside mask rays")

        latest_snapshot_path = directories["snapshots"] / f"demo_latest_{size}.json"
        stored_latest = store.load_verified(params2.parameter_hash)
        latest_snapshot = create_snapshot(
            params2,
            result_masked,
            color_buffer_name=params2.parameter_hash,
            mask_coverage_hash=str(stored_latest.manifest.get("mask_coverage_hash", "")),
        )
        save_snapshot(latest_snapshot, latest_snapshot_path)
        loaded = load_snapshot(latest_snapshot_path)
        trace.record(
            "snapshot_saved_and_loaded",
            parameter_hash=loaded.parameter_hash,
            color_buffer_hash=loaded.color_buffer_hash,
            snapshot_id=loaded.snapshot_id,
        )

        requested = params2.with_changes(step_size=params2.step_size * 1.1)
        manager = SnapshotManager(store)
        outcome = manager.restore(
            loaded,
            requested,
            volume,
            directories["snapshots"] / f"demo_recovered_{size}.json",
        )
        trace.record_render(
            "snapshot_restore_rerender",
            requested,
            outcome.result,
            reused_buffer=outcome.reused_buffer,
            rerendered=outcome.rerendered,
            changed_fields=list(outcome.changed_fields),
        )
        events.append(preview.state())

    report = {
        "trace": trace_path.name,
        "events": events,
        "latest_parameter_hash": params2.parameter_hash,
        "latest_buffer_hash": result_masked.buffer_hash,
        "snapshot": latest_snapshot_path.name,
        "restore_changed_fields": list(outcome.changed_fields),
    }
    _write_json(directories["reports"] / f"demo_{size}.json", report)
    return report


def load_parameters(path: str | Path) -> RenderParameters:
    payload = json.loads(Path(path).read_text(encoding="utf-8"))
    return RenderParameters.from_snapshot_dict(payload)
