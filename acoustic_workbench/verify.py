"""Three-path consistency checks, injection drills, and scale reports."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .analysis import FrameAnalyzer
from .checkpoint import remove_sqlite_checkpoint
from .codes import E_DIFF, E_TOL
from .config import AnalysisConfig, frame_count
from .errors import WorkbenchError, error
from .io import SampleRepository
from .io_export import export_json
from .models import FrameEstimate, InjectionConfig, RenderConfig, Sample
from .pipeline import AcousticPipeline, PipelineRun
from .render import make_block_plan
from .strategy import compare_preview_calibration, resolve_analysis_config, work_estimate


@dataclass
class ThreePathVerification:
    realtime: PipelineRun
    resumed: PipelineRun
    recomputed: PipelineRun
    realtime_vs_resume: dict[str, Any]
    realtime_vs_recompute: dict[str, Any]
    report: dict[str, Any] = field(default_factory=dict)


def _first_waveform_mismatch(
    expected: np.ndarray,
    actual: np.ndarray,
    *,
    block_size: int,
    total_frames: int,
) -> dict[str, Any] | None:
    if expected.shape == actual.shape:
        unequal = expected != actual
        if not np.any(unequal):
            return None
        channel, sample_position = np.unravel_index(int(np.argmax(unequal)), unequal.shape)
    else:
        common_channels = min(expected.shape[0], actual.shape[0])
        common_samples = min(expected.shape[1], actual.shape[1])
        for channel in range(common_channels):
            unequal = expected[channel, :common_samples] != actual[channel, :common_samples]
            if np.any(unequal):
                sample_position = int(np.argmax(unequal))
                break
        else:
            channel = 0
            sample_position = common_samples
    plan = make_block_plan(expected.shape[1], max(1, total_frames), block_size)
    block_index = min(len(plan.block_frame) - 1, sample_position // block_size)
    frame_index = plan.block_frame[block_index] if plan.block_frame else 0
    expected_len = expected.shape[1] if expected.ndim == 2 else 0
    return {
        "first_sample_flat_index": int(channel * expected_len + sample_position),
        "output_channel_index": int(channel),
        "sample_position": int(sample_position),
        "frame_index": int(frame_index),
        "block_index": int(block_index),
        "expected_shape": list(expected.shape),
        "actual_shape": list(actual.shape),
        "expected_value": float(expected[channel, sample_position])
        if channel < expected.shape[0] and sample_position < expected.shape[1]
        else None,
        "actual_value": float(actual[channel, sample_position])
        if channel < actual.shape[0] and sample_position < actual.shape[1]
        else None,
    }


def compare_run_results(
    expected: PipelineRun,
    actual: PipelineRun,
    *,
    block_size: int,
    total_frames: int,
    comparison_path: str | Path | None = None,
    label: str = "comparison",
) -> dict[str, Any]:
    if expected.render_result is None or actual.render_result is None:
        raise error(
            E_DIFF,
            "one of the runs has no rendered output",
            stage="verify.render_presence",
            context={
                "label": label,
                "expected_complete": expected.complete,
                "actual_complete": actual.complete,
            },
        )
    expected_output = expected.render_result.output
    actual_output = actual.render_result.output
    mismatch = _first_waveform_mismatch(
        expected_output,
        actual_output,
        block_size=block_size,
        total_frames=total_frames,
    )
    if mismatch is not None:
        context = {
            "label": label,
            **mismatch,
            "max_abs_difference": _max_abs_difference(expected_output, actual_output),
        }
        _write_comparison_context(comparison_path, context)
        exc = error(
            E_DIFF,
            "rendered outputs differ at the first mismatching sample",
            stage="verify.waveform_compare",
            frame_index=mismatch["frame_index"],
            context=context,
        )
        raise exc

    estimate_mismatch = _first_estimate_mismatch(
        expected.estimates,
        actual.estimates,
    )
    if estimate_mismatch is not None:
        context = {"label": label, **estimate_mismatch}
        _write_comparison_context(comparison_path, context)
        exc = error(
            E_DIFF,
            "frame delay or energy statistics differ",
            stage="verify.statistics_compare",
            frame_index=estimate_mismatch["frame_index"],
            context=context,
        )
        raise exc

    report = {
        "ok": True,
        "label": label,
        "sample_count": int(expected_output.shape[1]),
        "output_channels": int(expected_output.shape[0]),
        "output_sha256": hashlib.sha256(
            np.ascontiguousarray(expected_output).tobytes()
        ).hexdigest(),
        "frame_count": len(expected.estimates),
        "max_abs_difference": 0.0,
    }
    _write_comparison_context(comparison_path, report)
    return report


def _max_abs_difference(expected: np.ndarray, actual: np.ndarray) -> float:
    common_channels = min(expected.shape[0], actual.shape[0])
    common_samples = min(expected.shape[1], actual.shape[1])
    if common_channels == 0 or common_samples == 0:
        return 0.0
    return float(
        np.max(
            np.abs(
                expected[:common_channels, :common_samples]
                - actual[:common_channels, :common_samples]
            )
        )
    )


def _first_estimate_mismatch(
    expected: list[FrameEstimate],
    actual: list[FrameEstimate],
) -> dict[str, Any] | None:
    if len(expected) != len(actual):
        return {
            "frame_index": min(len(expected), len(actual)),
            "field": "frame_count",
            "expected": len(expected),
            "actual": len(actual),
        }
    fields = (
        "delay_samples",
        "direct_energy",
        "early_energy",
        "reverberant_energy",
        "drr_db",
        "direction_deg",
    )
    for left, right in zip(expected, actual, strict=True):
        if left.frame_index != right.frame_index:
            return {
                "frame_index": min(left.frame_index, right.frame_index),
                "field": "frame_index",
                "expected": left.frame_index,
                "actual": right.frame_index,
            }
        for field_name in fields:
            if getattr(left, field_name) != getattr(right, field_name):
                return {
                    "frame_index": left.frame_index,
                    "field": field_name,
                    "expected": getattr(left, field_name),
                    "actual": getattr(right, field_name),
                }
    return None


def _write_comparison_context(path: str | Path | None, context: dict[str, Any]) -> None:
    if path:
        export_json(path, context)


def run_three_path_verification(
    sample: Sample,
    analysis_config: AnalysisConfig,
    render_config: RenderConfig,
    workspace_dir: str | Path,
    *,
    interrupt_after: int | None = None,
) -> ThreePathVerification:
    root = Path(workspace_dir)
    root.mkdir(parents=True, exist_ok=True)
    realtime_path = root / "realtime.sqlite3"
    recovery_path = root / "recovery.sqlite3"
    for path in (
        realtime_path,
        recovery_path,
        Path(f"{realtime_path}-wal"),
        Path(f"{realtime_path}-shm"),
        Path(f"{recovery_path}-wal"),
        Path(f"{recovery_path}-shm"),
        root / "realtime_vs_resume.json",
        root / "realtime_vs_recompute.json",
        root / "three_path_report.json",
    ):
        path.unlink(missing_ok=True)
    total_frames = frame_count(
        sample.ir.shape[1],
        resolve_analysis_config(sample, analysis_config)[0].frame_length,
        resolve_analysis_config(sample, analysis_config)[0].hop_length,
    )
    if interrupt_after is None:
        interrupt_after = min(max(1, total_frames // 3), max(0, total_frames - 1))

    realtime_pipeline = AcousticPipeline(
        sample, analysis_config, render_config, realtime_path
    )
    try:
        realtime = realtime_pipeline.run()
    finally:
        realtime_pipeline.close()

    partial_pipeline = AcousticPipeline(sample, analysis_config, render_config, recovery_path)
    try:
        partial = partial_pipeline.run(stop_after=interrupt_after)
    finally:
        partial_pipeline.close()
    if partial.complete and total_frames > 1:
        raise error(
            E_DIFF,
            "recovery test did not create a genuine interruption checkpoint",
            stage="verify.interrupt_setup",
            context={"interrupt_after": interrupt_after, "total_frames": total_frames},
        )

    resumed_pipeline = AcousticPipeline(sample, analysis_config, render_config, recovery_path)
    try:
        resumed = resumed_pipeline.run()
    finally:
        resumed_pipeline.close()

    recompute_pipeline = AcousticPipeline(sample, analysis_config, render_config, recovery_path)
    try:
        recomputed = recompute_pipeline.full_recompute_from_checkpoint()
    finally:
        recompute_pipeline.close()

    realtime_vs_resume = compare_run_results(
        realtime,
        resumed,
        block_size=render_config.render_block_size,
        total_frames=total_frames,
        comparison_path=root / "realtime_vs_resume.json",
        label="realtime_vs_resume",
    )
    realtime_vs_recompute = compare_run_results(
        realtime,
        recomputed,
        block_size=render_config.render_block_size,
        total_frames=total_frames,
        comparison_path=root / "realtime_vs_recompute.json",
        label="realtime_vs_recompute",
    )
    report = {
        "ok": True,
        "sample_id": sample.id,
        "total_frames": total_frames,
        "interrupt_after": interrupt_after,
        "partial_committed_frames": partial.committed_frames,
        "realtime_vs_resume": realtime_vs_resume,
        "realtime_vs_recompute": realtime_vs_recompute,
        "checkpoint_paths": {
            "realtime": str(realtime_path),
            "recovery": str(recovery_path),
        },
    }
    export_json(root / "three_path_report.json", report)
    return ThreePathVerification(
        realtime=realtime,
        resumed=resumed,
        recomputed=recomputed,
        realtime_vs_resume=realtime_vs_resume,
        realtime_vs_recompute=realtime_vs_recompute,
        report=report,
    )


def run_injection_suite(
    repository: SampleRepository,
    analysis_config: AnalysisConfig,
    render_config: RenderConfig,
    workspace_dir: str | Path,
    *,
    retry_bad_frame: bool = True,
) -> dict[str, Any]:
    root = Path(workspace_dir)
    root.mkdir(parents=True, exist_ok=True)
    results: list[dict[str, Any]] = []
    for case in repository.injection_cases():
        case_id = str(case["id"])
        checkpoint = root / f"{case_id}.sqlite3"
        remove_sqlite_checkpoint(checkpoint)
        sample = None
        pipeline = None
        observed_code = None
        error_payload = None
        progress = None
        retry_ok = None
        try:
            sample = repository.load(
                str(case["sample_id"]),
                injection=InjectionConfig.from_dict(case.get("injection")),
            )
            pipeline = AcousticPipeline(sample, analysis_config, render_config, checkpoint)
            try:
                pipeline.run()
            except WorkbenchError as exc:
                observed_code = exc.code
                error_payload = exc.to_dict()
                progress = pipeline.progress()
                if exc.code == "E_FRAME" and retry_bad_frame:
                    sample.injection.bad_frames = []
                    retried = pipeline.retry_frame(int(exc.frame_index))
                    remaining = pipeline.run()
                    retry_ok = bool(remaining.complete or retried.complete)
        except WorkbenchError as exc:
            observed_code = exc.code
            error_payload = exc.to_dict()
        finally:
            if pipeline is not None:
                pipeline.close()
        results.append(
            {
                "id": case_id,
                "expected_code": case["expected_code"],
                "observed_code": observed_code,
                "ok": observed_code == case["expected_code"],
                "error": error_payload,
                "progress_at_failure": progress,
                "bad_frame_retry_completed": retry_ok,
            }
        )
    report = {
        "ok": all(item["ok"] for item in results),
        "cases": results,
    }
    export_json(root / "injection_report.json", report)
    return report


def run_scale_report(
    repository: SampleRepository,
    requested: AnalysisConfig,
) -> dict[str, Any]:
    analyzer = FrameAnalyzer()
    suites: list[dict[str, Any]] = []
    all_ok = True
    for suite in repository.suites():
        sample_ids = repository.sample_ids(suite["name"])
        if not sample_ids:
            continue
        representative = repository.load(sample_ids[0])
        effective, decision = resolve_analysis_config(representative, requested)
        frames = analyzer.analyze(representative, effective)
        work = work_estimate(representative, effective)
        try:
            comparison = compare_preview_calibration(
                representative,
                requested,
                analyzer=analyzer,
            )
            comparison_payload = {
                "ok": comparison["ok"],
                "max_delay_delta_samples": comparison["max_delay_delta_samples"],
                "max_energy_relative_delta": comparison["max_energy_relative_delta"],
                "max_drr_delta_db": comparison["max_drr_delta_db"],
                "comparison_frames": len(comparison["comparisons"]),
            }
        except WorkbenchError as exc:
            if exc.code != E_TOL:
                raise
            all_ok = False
            comparison_payload = exc.to_dict()
        suites.append(
            {
                "suite": suite["name"],
                "sample_count": len(sample_ids),
                "representative": representative.id,
                "ir_duration_s": representative.duration_s,
                "decision": decision.to_dict(),
                "work": work,
                "analyzed_frames": len(frames),
                "truth_direct_delay_max_abs_error_samples": _truth_delay_error(
                    representative, frames[0]
                )
                if frames
                else None,
                "mode_tolerance_check": comparison_payload,
            }
        )
    return {
        "ok": all_ok,
        "suites": suites,
        "total_generated_samples": sum(item["sample_count"] for item in suites),
    }


def _truth_delay_error(sample: Sample, first_frame: FrameEstimate) -> float:
    truth_channels = sample.truth.get("channels", [])
    if len(truth_channels) != len(first_frame.delay_samples):
        return float("nan")
    errors = [
        abs(float(channel["direct_delay_samples"]) - float(estimate))
        for channel, estimate in zip(
            truth_channels, first_frame.delay_samples, strict=True
        )
    ]
    return float(max(errors)) if errors else 0.0
