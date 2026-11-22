from __future__ import annotations

from datetime import datetime, timezone
import json
from pathlib import Path
import shutil
import time
from typing import Any

import numpy as np

from .comparison import first_stage_difference, metrics_differences
from .config import PipelineConfig
from .errors import CancelledError, EdgeForgeError
from .hashing import sha256_array
from .loader import load_sample
from .pipeline import Pipeline, process_whole_image
from .samples import generate_samples
from .stages import BlockState, BlockStateMachine, Stage


VERIFY_SAMPLE = "low_contrast_gray_320x240"
RGB_SAMPLE = "noisy_rgb_800x600"
HIRES_SAMPLE = "panorama_rgb_2048x1152"


def run_verification(
    *,
    project_root: str | Path,
    samples_dir: str | Path | None = None,
    work_dir: str | Path | None = None,
    report_path: str | Path | None = None,
) -> dict[str, Any]:
    root = Path(project_root)
    samples_root = Path(samples_dir) if samples_dir else root / "samples"
    runs_root = Path(work_dir) if work_dir else root / "runs" / "verification"
    report_file = (
        Path(report_path)
        if report_path
        else root / "artifacts" / "verification.json"
    )
    started = time.perf_counter()
    report: dict[str, Any] = {
        "schema_version": 1,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "python": _python_version(),
        "samples_dir": str(samples_root),
        "work_dir": str(runs_root),
        "checks": [],
    }
    checks: list[dict[str, Any]] = report["checks"]
    try:
        manifest = generate_samples(
            samples_root,
            [VERIFY_SAMPLE, RGB_SAMPLE, HIRES_SAMPLE],
        )
        _record(
            checks,
            "sample_generation",
            True,
            {
                "manifest_samples": [
                    entry["sample_id"] for entry in manifest["samples"]
                ],
                "master_seed": manifest["master_seed"],
            },
        )
        sample = load_sample(samples_root, VERIFY_SAMPLE)
        rgb_sample = load_sample(samples_root, RGB_SAMPLE)
        order_ok = _check_load_order(samples_root, [VERIFY_SAMPLE, RGB_SAMPLE])
        _record(
            checks,
            "loader_order_independence",
            order_ok,
            {"sample_hashes": [sample.image_sha256, rgb_sample.image_sha256]},
        )
        _record(
            checks,
            "channel_shapes",
            sample.image.ndim == 2 and rgb_sample.image.ndim == 3,
            {
                "gray_shape": list(sample.image.shape),
                "rgb_shape": list(rgb_sample.image.shape),
                "gray_truth_coverage": float(sample.truth_mask.mean()),
                "rgb_truth_coverage": float(rgb_sample.truth_mask.mean()),
            },
        )

        config = PipelineConfig(
            tile_size=64,
            max_parallel=4,
            memory_budget_bytes=16 * 1024 * 1024,
        )
        serial = Pipeline(
            sample,
            config,
            execution_mode="serial",
        ).run()
        report["reference_config"] = config.describe()
        report["reference_block_count"] = len(serial.blocks)
        report["reference_stage_hashes"] = {
            stage.value: sha256_array(array)
            for stage, array in serial.stage_arrays.items()
        }
        report["reference_metrics"] = serial.stage_metrics
        live_dir = runs_root / "live"
        _reset_directory(live_dir)
        live = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=live_dir,
        ).run()
        live_diff = first_stage_difference(
            serial.stage_arrays,
            live.stage_arrays,
            live.blocks,
            config.tile_size,
        )
        live_metric_diff = metrics_differences(
            serial.stage_metrics,
            live.stage_metrics,
        )
        expected_commit_order = [
            (stage.value, block.block_id)
            for stage in Stage
            for block in live.blocks
        ]
        observed_commit_order = (
            [
                (entry["stage"], int(entry["block_id"]))
                for entry in live.checkpoint.commit_order
            ]
            if live.checkpoint is not None
            else []
        )
        _record(
            checks,
            "parallel_vs_serial",
            (
                live_diff is None
                and not live_metric_diff
                and observed_commit_order == expected_commit_order
            ),
            {
                "first_difference": live_diff,
                "metric_differences": live_metric_diff,
                "commits": live.checkpoint.next_seq if live.checkpoint else 0,
                "commit_order_stable": observed_commit_order == expected_commit_order,
                "queue_events": live.queue_events,
                "budget_peak_bytes": live.budget_peak_bytes,
            },
        )

        resume_dir = runs_root / "resume"
        _reset_directory(resume_dir)
        partial = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=resume_dir,
        )
        partial.run(stages_limit=3)
        partial_progress = partial.progress
        partial_commits = (
            partial.checkpoint.next_seq if partial.checkpoint is not None else 0
        )
        resumed = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=resume_dir,
        )
        resumed_commits = resumed.resumed_commits
        resumed_invalidated = resumed.snapshot_invalidated
        resumed.run()
        resume_diff = first_stage_difference(
            serial.stage_arrays,
            resumed.stage_arrays,
            resumed.blocks,
            config.tile_size,
        )
        resume_metric_diff = metrics_differences(
            serial.stage_metrics,
            resumed.stage_metrics,
        )
        _record(
            checks,
            "checkpoint_resume",
            (
                resume_diff is None
                and not resume_metric_diff
                and resumed_commits == partial_commits
                and not resumed_invalidated
            ),
            {
                "partial_progress": partial_progress,
                "partial_commits": partial_commits,
                "restored_commits": resumed_commits,
                "snapshot_invalidated": resumed_invalidated,
                "first_difference": resume_diff,
                "metric_differences": resume_metric_diff,
            },
        )

        partial_dir = runs_root / "partial-stage"
        _reset_directory(partial_dir)
        interrupted = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=partial_dir,
        )
        seen_commits = {"count": 0}

        def cancel_at_partial_stage(event: dict[str, Any]) -> None:
            if event.get("type") == "block_committed":
                seen_commits["count"] += 1
                if seen_commits["count"] == 25:
                    interrupted.control.cancel()

        interrupted.on_event = cancel_at_partial_stage
        cancel_error = None
        try:
            interrupted.run()
        except CancelledError as exc:
            cancel_error = exc.as_dict()
        partial_stage_commits = (
            interrupted.checkpoint.next_seq
            if interrupted.checkpoint is not None
            else 0
        )
        resumed_partial = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=partial_dir,
        )
        partial_restored = resumed_partial.resumed_commits
        resumed_partial.run()
        partial_diff = first_stage_difference(
            serial.stage_arrays,
            resumed_partial.stage_arrays,
            resumed_partial.blocks,
            config.tile_size,
        )
        partial_metric_diff = metrics_differences(
            serial.stage_metrics,
            resumed_partial.stage_metrics,
        )
        _record(
            checks,
            "resume_from_partially_committed_stage",
            (
                cancel_error is not None
                and cancel_error["code"] == "E_CANCEL"
                and 20 < partial_stage_commits < 40
                and partial_restored == partial_stage_commits
                and partial_diff is None
                and not partial_metric_diff
            ),
            {
                "cancel": cancel_error,
                "commits_before_exit": partial_stage_commits,
                "restored_commits": partial_restored,
                "first_difference": partial_diff,
                "metric_differences": partial_metric_diff,
            },
        )

        full = Pipeline(
            sample,
            config,
            execution_mode="parallel",
        ).run()
        full_diff = first_stage_difference(
            serial.stage_arrays,
            full.stage_arrays,
            full.blocks,
            config.tile_size,
        )
        full_metric_diff = metrics_differences(
            serial.stage_metrics,
            full.stage_metrics,
        )
        _record(
            checks,
            "full_recompute",
            full_diff is None and not full_metric_diff,
            {
                "first_difference": full_diff,
                "metric_differences": full_metric_diff,
            },
        )

        whole = process_whole_image(sample, config)
        whole_diff = first_stage_difference(
            serial.stage_arrays,
            whole,
            serial.blocks,
            config.tile_size,
        )
        _record(
            checks,
            "whole_image_vs_tiled",
            whole_diff is None,
            {"first_difference": whole_diff},
        )

        tamper_dir = runs_root / "tamper"
        _reset_directory(tamper_dir)
        tampered_run = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=tamper_dir,
        ).run(stages_limit=3)
        tamper_target = _tamper_first_record(tamper_dir)
        recovered = Pipeline(
            sample,
            config,
            execution_mode="parallel",
            run_dir=tamper_dir,
        )
        detected = recovered.snapshot_invalidated
        recovered.run()
        tamper_diff = first_stage_difference(
            serial.stage_arrays,
            recovered.stage_arrays,
            recovered.blocks,
            config.tile_size,
        )
        _record(
            checks,
            "checkpoint_tamper_invalidation",
            detected and recovered.resumed_commits == 0 and tamper_diff is None,
            {
                "tampered_file": str(tamper_target),
                "snapshot_invalidated": detected,
                "reason": recovered.snapshot_invalidation_reason,
                "restored_commits": recovered.resumed_commits,
                "first_difference": tamper_diff,
                "prior_commits": (
                    tampered_run.checkpoint.next_seq
                    if tampered_run.checkpoint is not None
                    else 0
                ),
            },
        )

        rgb_config = PipelineConfig(
            tile_size=128,
            max_parallel=4,
            thin_iterations=6,
            prune_length=8,
            memory_budget_bytes=24 * 1024 * 1024,
        )
        rgb_serial = Pipeline(
            rgb_sample,
            rgb_config,
            execution_mode="serial",
        ).run()
        rgb_parallel = Pipeline(
            rgb_sample,
            rgb_config,
            execution_mode="parallel",
        ).run()
        rgb_diff = first_stage_difference(
            rgb_serial.stage_arrays,
            rgb_parallel.stage_arrays,
            rgb_parallel.blocks,
            rgb_config.tile_size,
        )
        rgb_channel_ok = all(
            array.shape == rgb_sample.image.shape
            for array in rgb_parallel.stage_arrays.values()
        )
        _record(
            checks,
            "rgb_parallel_vs_serial",
            rgb_diff is None
            and rgb_channel_ok
            and not metrics_differences(
                rgb_serial.stage_metrics,
                rgb_parallel.stage_metrics,
            ),
            {
                "shape": list(rgb_sample.image.shape),
                "channel_consistent": rgb_channel_ok,
                "first_difference": rgb_diff,
            },
        )

        hires_sample = load_sample(samples_root, HIRES_SAMPLE)
        hires_config = PipelineConfig(
            tile_size=256,
            max_parallel=4,
            thin_iterations=6,
            prune_length=8,
            memory_budget_bytes=64 * 1024 * 1024,
        )
        hires = Pipeline(
            hires_sample,
            hires_config,
            execution_mode="parallel",
        ).run()
        hires_shape_ok = all(
            array.shape == hires_sample.image.shape
            for array in hires.stage_arrays.values()
        )
        _record(
            checks,
            "high_resolution_full_pipeline",
            (
                hires_shape_ok
                and hires.progress["committed"] == hires.progress["total"]
                and hires.progress["total"] == len(hires.blocks) * len(Stage)
            ),
            {
                "sample": HIRES_SAMPLE,
                "shape": list(hires_sample.image.shape),
                "blocks": len(hires.blocks),
                "committed": hires.progress["committed"],
                "stage_shapes_consistent": hires_shape_ok,
                "prune_coverage": hires.stage_metrics["prune"]["coverage"],
            },
        )

        queue_config = PipelineConfig(
            tile_size=128,
            max_parallel=4,
            memory_budget_bytes=200_000,
        )
        queued = Pipeline(
            sample,
            queue_config,
            execution_mode="parallel",
        ).run()
        queued_diff = first_stage_difference(
            serial.stage_arrays,
            queued.stage_arrays,
            queued.blocks,
            queue_config.tile_size,
        )
        _record(
            checks,
            "budget_blocking_queue",
            len(queued.queue_events) > 0 and queued_diff is None,
            {
                "queue_events": queued.queue_events[:8],
                "queue_event_count": len(queued.queue_events),
                "peak_bytes": queued.budget_peak_bytes,
                "budget_bytes": queue_config.memory_budget_bytes,
                "first_difference": queued_diff,
            },
        )

        errors = _error_code_checks(samples_root, sample, runs_root)
        report["error_codes"] = errors
        expected_codes = {
            "E_DECODE",
            "E_META",
            "E_PARA",
            "E_STATE",
            "E_BAND",
            "E_BUDGET",
        }
        observed_codes = {
            item["observed"].get("code")
            for item in errors
            if item["observed"] is not None
        }
        _record(
            checks,
            "error_code_coverage",
            expected_codes.issubset(observed_codes),
            {
                "expected": sorted(expected_codes),
                "observed": sorted(code for code in observed_codes if code),
            },
        )
    except Exception as exc:
        _record(
            checks,
            "verification_exception",
            False,
            {
                "exception": type(exc).__name__,
                "message": str(exc),
                "error_code": getattr(exc, "code", None),
            },
        )

    report["elapsed_seconds"] = time.perf_counter() - started
    report["ok"] = all(check["ok"] for check in checks) and bool(checks)
    report_file.parent.mkdir(parents=True, exist_ok=True)
    temp = report_file.with_name(report_file.name + ".tmp")
    temp.write_text(
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    temp.replace(report_file)
    return report


def _check_load_order(samples_root: Path, sample_ids: list[str]) -> bool:
    forward = {sample_id: load_sample(samples_root, sample_id) for sample_id in sample_ids}
    backward = {
        sample_id: load_sample(samples_root, sample_id)
        for sample_id in reversed(sample_ids)
    }
    return all(
        forward[sample_id].image_sha256 == backward[sample_id].image_sha256
        and forward[sample_id].truth_sha256 == backward[sample_id].truth_sha256
        for sample_id in sample_ids
    )


def _error_code_checks(
    samples_root: Path,
    sample: Any,
    runs_root: Path,
) -> list[dict[str, Any]]:
    checks: list[dict[str, Any]] = []

    corrupt_root = runs_root / "errors" / "decode"
    _reset_directory(corrupt_root)
    shutil.copytree(samples_root, corrupt_root / "samples")
    (corrupt_root / "samples" / VERIFY_SAMPLE / "image.png").write_bytes(b"broken-png")
    checks.append(
        _capture_error(
            "E_DECODE",
            lambda: load_sample(corrupt_root / "samples", VERIFY_SAMPLE),
        )
    )

    meta_root = runs_root / "errors" / "meta"
    _reset_directory(meta_root)
    shutil.copytree(samples_root, meta_root / "samples")
    meta_path = meta_root / "samples" / VERIFY_SAMPLE / "meta.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8"))
    meta["width"] = int(meta["width"]) + 1
    meta_path.write_text(json.dumps(meta, indent=2, sort_keys=True), encoding="utf-8")
    checks.append(
        _capture_error(
            "E_META",
            lambda: load_sample(meta_root / "samples", VERIFY_SAMPLE),
        )
    )

    checks.append(
        _capture_error(
            "E_PARA",
            lambda: PipelineConfig(
                low_threshold=50.0,
                high_threshold=10.0,
            ).validate(),
        )
    )

    def state_check() -> dict[str, Any]:
        machine = BlockStateMachine((0, 1))
        before = machine.cell(Stage.GRADIENT, 0).state
        try:
            machine.transition(Stage.GRADIENT, 0, BlockState.COMMITTED)
        except EdgeForgeError:
            after = machine.cell(Stage.GRADIENT, 0).state
            if before is not after or machine.committed != 0:
                raise AssertionError("非法迁移改变了状态或提交计数")
            raise

    checks.append(_capture_error("E_STATE", state_check))

    band_config = PipelineConfig(
        tile_size=64,
        band_override=1,
    )
    checks.append(
        _capture_error(
            "E_BAND",
            lambda: Pipeline(
                sample,
                band_config,
                execution_mode="serial",
            ).run(),
        )
    )

    budget_config = PipelineConfig(
        tile_size=256,
        thread_buffer_pixels=4096,
    )
    checks.append(
        _capture_error(
            "E_BUDGET",
            lambda: Pipeline(
                sample,
                budget_config,
                execution_mode="serial",
            ).run(),
        )
    )
    return checks


def _capture_error(label: str, action: Any) -> dict[str, Any]:
    try:
        action()
    except EdgeForgeError as exc:
        return {
            "expected": label,
            "observed": exc.as_dict(),
            "ok": exc.code == label,
        }
    except Exception as exc:
        return {
            "expected": label,
            "observed": {
                "code": "E_INTERNAL",
                "message": str(exc),
                "exception": type(exc).__name__,
            },
            "ok": False,
        }
    return {
        "expected": label,
        "observed": None,
        "ok": False,
    }


def _tamper_first_record(run_dir: Path) -> Path:
    records = sorted((run_dir / "records").glob("*.npz"))
    if not records:
        raise RuntimeError("no checkpoint records to tamper")
    target = records[0]
    with np.load(target, allow_pickle=False) as data:
        payload = {key: np.array(data[key], copy=True) for key in data.files}
    array = payload["array"]
    if array.dtype == np.uint8:
        array.reshape(-1)[0] = np.uint8(1 - int(array.reshape(-1)[0]))
    else:
        array.reshape(-1)[0] = array.reshape(-1)[0] + np.array(1.0, dtype=array.dtype)
    payload["array"] = array
    np.savez_compressed(target, **payload)
    return target


def _reset_directory(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _record(
    checks: list[dict[str, Any]],
    name: str,
    ok: bool,
    details: dict[str, Any],
) -> None:
    checks.append({"name": name, "ok": bool(ok), "details": details})


def _python_version() -> str:
    import platform

    return platform.python_version()
