"""End-to-end acceptance runner using locally generated deterministic samples."""

from __future__ import annotations

import copy
import json
import platform
import shutil
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np
import PySide6

from .compare import compare_results
from .config import (
    ALGORITHM_VERSION,
    BudgetConfig,
    DenoiseConfig,
    PipelineConfig,
    RegistrationConfig,
)
from .engine import SequenceProcessor
from .errors import SciSeqError
from .export import export_with_recomputation
from .generator import DEFAULT_GROUPS, generate_all
from .loader import load_sequence
from .planner import plan_mode
from .registration import estimate_translation
from .state import FrameStateMachine, FrameStatus

QUALITY_THRESHOLDS = {
    "registration_max_error_px": 0.35,
    "residual_std_upper": 3.25,
    "edge_fidelity_lower": 0.78,
    "feature_center_max_error_px": 2.5,
    "noise_relative_error_mean": 0.20,
}


def _base_config(*, max_workers: int = 4, memory_mb: float = 2048.0) -> PipelineConfig:
    return PipelineConfig(
        reference_frame=0,
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=8.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=32, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=600.0,
            memory_budget_mb=memory_mb,
            max_workers=max_workers,
            frames_per_worker=2,
            preview_denoise_fraction=0.6,
            preview_mean_abs_limit=35.0,
        ),
    )


def _group_metrics(sequence: Any, result: Any) -> dict[str, Any]:
    errors = np.hypot(
        result.offsets[:, 0] - sequence.truth.shifts[:, 0],
        result.offsets[:, 1] - sequence.truth.shifts[:, 1],
    )
    quality = result.stats["quality_summary"]
    center_errors = [
        item["truth_feature_match"]["max_center_error_px"]
        for item in result.stats["frames"]
        if item["truth_feature_match"]["max_center_error_px"] is not None
    ]
    estimated_noise = np.asarray([item["noise_sigma"] for item in result.stats["frames"]])
    truth_noise = sequence.truth.noise_sigmas
    relative_noise = np.abs(estimated_noise - truth_noise) / np.maximum(truth_noise, 1e-12)
    return {
        "frames": sequence.frame_count,
        "shape": list(sequence.shape),
        "registration_max_error_px": float(errors.max()),
        "registration_mean_error_px": float(errors.mean()),
        "residual_std_max": float(quality["residual_std_max"]),
        "residual_std_mean": float(quality["residual_std_mean"]),
        "edge_fidelity_min": float(quality["edge_fidelity_min"]),
        "edge_fidelity_mean": float(quality["edge_fidelity_mean"]),
        "feature_center_max_error_px": float(max(center_errors)),
        "feature_count_total": int(result.stats["total_features"]),
        "noise_relative_error_mean": float(relative_noise.mean()),
        "auto_mode": result.decision["auto_selected_mode"],
        "parameter_version": result.parameter_version,
        "decision_hash": result.decision["decision_hash"],
    }


def _check_quality(metrics: dict[str, Any]) -> dict[str, bool]:
    checks = {
        "registration": metrics["registration_max_error_px"]
        <= QUALITY_THRESHOLDS["registration_max_error_px"],
        "residual": metrics["residual_std_max"] <= QUALITY_THRESHOLDS["residual_std_upper"],
        "edge": metrics["edge_fidelity_min"] >= QUALITY_THRESHOLDS["edge_fidelity_lower"],
        "feature_center": metrics["feature_center_max_error_px"]
        <= QUALITY_THRESHOLDS["feature_center_max_error_px"],
        "noise_estimate": metrics["noise_relative_error_mean"]
        <= QUALITY_THRESHOLDS["noise_relative_error_mean"],
    }
    if not all(checks.values()):
        failed = [name for name, passed in checks.items() if not passed]
        raise SciSeqError(
            "E_QUALITY",
            "acceptance quality threshold failed",
            field=",".join(failed),
            details={"metrics": metrics, "thresholds": QUALITY_THRESHOLDS},
        )
    return checks


def _registration_tolerance_curve(sequence: Any) -> dict[str, Any]:
    frame_indices = sorted(
        {
            max(1, int(round(index * (sequence.frame_count - 1) / 8.0)))
            for index in range(1, 9)
        }
    )
    reference = sequence.frame(sequence.reference_frame)
    curve: list[dict[str, Any]] = []
    for factor in (1, 2, 4, 6):
        residuals = []
        for index in frame_indices:
            estimate = estimate_translation(
                reference,
                sequence.frame(index),
                RegistrationConfig(window="none", upsample_factor=factor, max_shift=8.0),
                frame_index=index,
            )
            truth = sequence.truth.shifts[index]
            residuals.append(
                float(np.hypot(estimate.offset[0] - truth[0], estimate.offset[1] - truth[1]))
            )
        curve.append(
            {
                "upsample_factor": factor,
                "frames": frame_indices,
                "mean_residual_px": float(np.mean(residuals)),
                "max_residual_px": float(np.max(residuals)),
            }
        )
    means = [item["mean_residual_px"] for item in curve]
    monotonic = all(means[index + 1] <= means[index] + 1e-12 for index in range(len(means) - 1))
    if not monotonic:
        raise SciSeqError(
            "E_REG",
            "registration residual is not monotonic as precision tolerance tightens",
            field="upsample_factor",
            details={"mean_residuals": means},
        )
    return {"frames": frame_indices, "curve": curve, "monotonic_nonincreasing": monotonic}


def _consistency_group(
    sequence: Any,
    config: PipelineConfig,
    checkpoint_root: Path,
) -> tuple[Any, dict[str, Any]]:
    baseline = SequenceProcessor(sequence, config).run_full(parallel=True)
    started = time.perf_counter()
    incremental = SequenceProcessor(
        sequence,
        config,
        checkpoint_dir=checkpoint_root / f"{sequence.sequence_id}_incremental",
    ).run_incremental(parallel=True, reset=True)
    incremental_seconds = time.perf_counter() - started
    if not incremental.complete:
        raise SciSeqError("E_STATE", "incremental path did not complete", field="progress")
    incremental_report = compare_results(
        baseline,
        incremental.result,
        label="incremental",
    )

    resume_checkpoint = checkpoint_root / f"{sequence.sequence_id}_resume"
    pause_after = max(1, sequence.frame_count // 2)
    started = time.perf_counter()
    partial = SequenceProcessor(
        sequence,
        config,
        checkpoint_dir=resume_checkpoint,
    ).run_incremental(parallel=True, pause_after=pause_after, reset=True)
    if partial.complete or partial.committed_count != pause_after:
        raise SciSeqError(
            "E_STATE",
            "pause boundary did not stop at the requested frame",
            field="pause_after",
            details={"expected": pause_after, "actual": partial.committed_count},
        )
    resumed = SequenceProcessor(
        sequence,
        config,
        checkpoint_dir=resume_checkpoint,
    ).run_resume(parallel=True)
    resume_seconds = time.perf_counter() - started
    if not resumed.complete:
        raise SciSeqError("E_STATE", "resume path did not complete", field="progress")
    resume_report = compare_results(baseline, resumed.result, label="resume")

    started = time.perf_counter()
    serial = SequenceProcessor(sequence, config).run_full(parallel=False)
    serial_seconds = time.perf_counter() - started
    serial_report = compare_results(baseline, serial, label="serial")

    decisions_equal = (
        baseline.decision == incremental.result.decision
        and baseline.decision == resumed.result.decision
        and baseline.decision == serial.decision
    )
    if not decisions_equal:
        raise SciSeqError(
            "E_DIVERGE",
            "algorithm decisions differ between execution paths",
            field="decision",
        )
    return baseline, {
        "incremental": incremental_report,
        "resume": resume_report,
        "parallel_vs_serial": serial_report,
        "decisions_identical": decisions_equal,
        "pause_frame": pause_after,
        "incremental_seconds": incremental_seconds,
        "resume_seconds": resume_seconds,
        "serial_seconds": serial_seconds,
        "snapshot_validation": resumed.snapshot_reason,
    }


def _exercise_error_codes(
    samples_root: Path,
    baseline: Any,
    sequence: Any,
    group_name: str,
) -> dict[str, Any]:
    results: dict[str, Any] = {}
    temp_root = Path(tempfile.mkdtemp(prefix="sciseq_acceptance_", dir=samples_root.parent))
    try:
        copied = temp_root / f"{group_name}_copy"
        shutil.copytree(samples_root / group_name, copied)
        manifest_path = copied / "manifest.json"
        original_manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        missing_manifest = dict(original_manifest)
        missing_manifest.pop("frame_count")
        manifest_path.write_text(json.dumps(missing_manifest), encoding="utf-8")
        try:
            load_sequence(copied)
        except SciSeqError as exc:
            results["E_META"] = exc.to_dict()

        manifest_path.write_text(json.dumps(original_manifest), encoding="utf-8")
        corrupt_path = copied / original_manifest["frame_files"][2]
        corrupt_path.write_bytes(b"not-a-numpy-frame")
        try:
            load_sequence(copied)
        except SciSeqError as exc:
            results["E_DECODE"] = exc.to_dict()
    finally:
        shutil.rmtree(temp_root, ignore_errors=True)

    try:
        DenoiseConfig(block_size=2).validate(sequence.shape)
    except SciSeqError as exc:
        results["E_PARA"] = exc.to_dict()

    state = FrameStateMachine(0)
    try:
        state.transition(FrameStatus.COMMITTED)
    except SciSeqError as exc:
        results["E_STATE"] = exc.to_dict()
        results["E_STATE"]["state_after_error"] = state.status.value

    large_path = samples_root / "large"
    large = load_sequence(large_path) if large_path.exists() else sequence
    target = next(
        index
        for index in range(1, large.frame_count)
        if float(np.hypot(*large.truth.shifts[index])) > 0.8
    )
    try:
        estimate_translation(
            large.frame(0),
            large.frame(target),
            RegistrationConfig(window="none", upsample_factor=5, max_shift=0.4),
            frame_index=target,
        )
    except SciSeqError as exc:
        results["E_REG"] = exc.to_dict()

    corrupted = copy.deepcopy(baseline)
    corrupted.denoised[2, 17, 29] += np.float32(1.0)
    try:
        compare_results(baseline, corrupted, label="corrupted")
    except SciSeqError as exc:
        results["E_DIVERGE"] = exc.to_dict()

    impossible = PipelineConfig(
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=8.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=32, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=0.1,
            memory_budget_mb=16.0,
            max_workers=4,
            frames_per_worker=2,
        ),
    )
    try:
        plan_mode(large.frame_count, large.shape, impossible)
    except SciSeqError as exc:
        results["E_BUDGET"] = exc.to_dict()

    required = {"E_META", "E_DECODE", "E_PARA", "E_STATE", "E_REG", "E_DIVERGE", "E_BUDGET"}
    missing = sorted(required - set(results))
    if missing:
        raise SciSeqError(
            "E_QUALITY",
            "some structured error codes were not exercised",
            field="error_codes",
            details={"missing": missing},
        )
    return results


def run_acceptance(
    samples_root: Path | str,
    output_path: Path | str,
    *,
    groups: list[str] | None = None,
    regenerate: bool = False,
) -> dict[str, Any]:
    samples = Path(samples_root)
    selected = groups or [group.name for group in DEFAULT_GROUPS]
    group_specs = [group for group in DEFAULT_GROUPS if group.name in selected]
    if regenerate:
        generate_all(samples, groups=group_specs, overwrite=True)
    missing_groups = [group.name for group in group_specs if not (samples / group.name).exists()]
    if missing_groups:
        generate_all(samples, groups=[group for group in group_specs if group.name in missing_groups])

    checkpoint_root = Path("artifacts/checkpoints")
    checkpoint_root.mkdir(parents=True, exist_ok=True)
    export_root = Path("artifacts/preview_export")
    if export_root.exists():
        shutil.rmtree(export_root)

    config = _base_config()
    representative_name = "small" if "small" in selected else selected[0]
    report: dict[str, Any] = {
        "status": "running",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": sys.version.split()[0],
            "numpy": np.__version__,
            "pyside6": PySide6.__version__,
            "platform": platform.platform(),
            "algorithm_version": ALGORITHM_VERSION,
        },
        "quality_thresholds": QUALITY_THRESHOLDS,
        "groups": {},
    }
    baseline_small = None
    small_sequence = None
    overall_start = time.perf_counter()
    for spec in group_specs:
        sequence = load_sequence(samples / spec.name)
        started = time.perf_counter()
        baseline, consistency = _consistency_group(sequence, config, checkpoint_root)
        full_seconds = time.perf_counter() - started
        metrics = _group_metrics(sequence, baseline)
        checks = _check_quality(metrics)
        tolerance_curve = _registration_tolerance_curve(sequence)
        report["groups"][spec.name] = {
            "source": str((samples / spec.name).resolve()),
            "manifest": {
                "seed": sequence.manifest["seed"],
                "frames": sequence.frame_count,
                "shape": list(sequence.shape),
                "max_truth_shift": sequence.manifest["max_truth_shift"],
            },
            "metrics": metrics,
            "quality_checks": checks,
            "registration_tolerance_curve": tolerance_curve,
            "consistency": consistency,
            "full_pipeline_seconds": full_seconds,
        }
        if spec.name == representative_name:
            baseline_small = baseline
            small_sequence = sequence

    if baseline_small is None or small_sequence is None:
        small_sequence = load_sequence(samples / representative_name)
        baseline_small = SequenceProcessor(small_sequence, config).run_full(parallel=True)

    preview_config = PipelineConfig(
        reference_frame=0,
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=8.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=32, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=600.0,
            memory_budget_mb=60.0,
            max_workers=4,
            frames_per_worker=2,
            preview_denoise_fraction=0.6,
            preview_mean_abs_limit=35.0,
        ),
    )
    preview_processor = SequenceProcessor(small_sequence, preview_config)
    preview_decision = preview_processor.plan()
    preview_started = time.perf_counter()
    preview = preview_processor.run_preview()
    preview_seconds = time.perf_counter() - preview_started
    export_summary = export_with_recomputation(
        preview_processor,
        preview,
        export_root / "small",
    )
    report["preview_gate"] = {
        "mode": preview_decision.mode,
        "downsample_factor": preview_decision.downsample_factor,
        "denoise_quota": preview_decision.preview_denoise_quota,
        "estimated_memory_mb": preview_decision.estimated_memory_mb,
        "memory_budget_mb": preview_decision.memory_budget_mb,
        "preview_seconds": preview_seconds,
        "preview_deviation": export_summary["preview_comparison"],
        "export_path": str(export_root / "small"),
        "export_mode": export_summary["mode"],
    }

    large_sequence_path = samples / "large"
    large_sequence = (
        load_sequence(large_sequence_path)
        if large_sequence_path.exists()
        else small_sequence
    )
    large_budget_config = PipelineConfig(
        reference_frame=0,
        registration=RegistrationConfig(window="none", upsample_factor=20, max_shift=8.0),
        denoise=DenoiseConfig(filter="gaussian", block_size=32, strength=1.0),
        budget=BudgetConfig(
            time_budget_s=5.0,
            memory_budget_mb=512.0,
            max_workers=4,
            frames_per_worker=2,
            preview_denoise_fraction=0.5,
            preview_mean_abs_limit=35.0,
        ),
    )
    large_processor = SequenceProcessor(large_sequence, large_budget_config)
    large_decision = large_processor.plan()
    large_started = time.perf_counter()
    large_preview = large_processor.run_preview()
    large_elapsed = time.perf_counter() - large_started
    if large_elapsed > large_budget_config.budget.time_budget_s:
        raise SciSeqError(
            "E_BUDGET",
            "large preview exceeded its declared wall-clock budget",
            field="budget.time_budget_s",
            details={
                "elapsed_seconds": large_elapsed,
                "time_budget_s": large_budget_config.budget.time_budget_s,
            },
        )
    report["large_budget_preview"] = {
        "mode": large_decision.mode,
        "downsample_factor": large_decision.downsample_factor,
        "denoise_quota": large_decision.preview_denoise_quota,
        "estimated_seconds": large_decision.estimated_selected_seconds,
        "estimated_memory_mb": large_decision.estimated_memory_mb,
        "actual_seconds": large_elapsed,
        "time_budget_s": large_budget_config.budget.time_budget_s,
        "memory_budget_mb": large_budget_config.budget.memory_budget_mb,
        "within_time_budget": True,
        "preview_shape": list(large_preview.shape),
    }

    report["error_codes"] = _exercise_error_codes(
        samples,
        baseline_small,
        small_sequence,
        representative_name,
    )
    report["status"] = "passed"
    report["elapsed_seconds"] = time.perf_counter() - overall_start
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(report, indent=2, sort_keys=True, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    return report

