"""Stable, deterministic JSON reports (no timestamps, no absolute paths)."""

from __future__ import annotations

import json
import os

import soundfile as sf

from ..manifest import Manifest
from ..processing.audioutil import dbfs, reference_band_floor, segment_band_energy


def _recompute_mic_report(manifest: Manifest, trace_results: list) -> list:
    """Recompute every energy figure from the WAVs actually on disk."""
    band = manifest.noise_band_hz
    detected = {
        (item["mic_name"], entry["segment_id"])
        for item in trace_results
        for entry in item["entries"]
        if entry["detected"]
    }
    reports = []
    for mic in manifest.mics:
        path = os.path.join(manifest.source_dir, mic.path)
        signal, sr = sf.read(path, dtype="float64")
        blocked = [(a.start_sample, a.end_sample) for a in manifest.actions]
        floor = reference_band_floor(signal, blocked, band, sr)
        segments = []
        for action in manifest.actions:
            energy = segment_band_energy(signal, action.start_sample,
                                         action.end_sample, band, sr)
            segments.append({
                "segment_id": action.segment_id,
                "noise_band_energy_dbfs": round(dbfs(energy), 3),
                "processed": (mic.name, action.segment_id) in detected,
                "residual_below_threshold": (
                    bool(dbfs(energy) <= manifest.residual_threshold_dbfs)
                    if (mic.name, action.segment_id) in detected else None),
            })
        reports.append({
            "mic_name": mic.name,
            "position": mic.position,
            "reference_floor_dbfs": round(dbfs(floor), 3),
            "segments": segments,
        })
    return reports


def build_report(manifest: Manifest, pipeline_result: dict,
                 elapsed_seconds: float) -> dict:
    denoise_section = []
    for result in pipeline_result["denoise_reports"]:
        denoise_section.append({
            "mic_name": result["mic_name"],
            "modified_interval_count": result["modified_interval_count"],
            "modified_sample_count": result["modified_sample_count"],
            "max_sample_delta": result["max_sample_delta"],
            "reference_floor_before_dbfs":
                result["reference_floor_before_dbfs"],
            "reference_floor_after_dbfs":
                result["reference_floor_after_dbfs"],
            "content_band": result["content_band"],
            "segment_metrics": result["segment_metrics"],
        })

    trace_section = []
    for mic_trace in pipeline_result["trace_results"]:
        trace_section.append({
            "mic_name": mic_trace["mic_name"],
            "position": mic_trace["position"],
            "reference_floor_dbfs": mic_trace["reference_floor_dbfs"],
            "detected_segments": mic_trace["detected_segments"],
            "false_positive_segments":
                mic_trace["false_positive_segments"],
            "entries": mic_trace["entries"],
        })

    master_report = pipeline_result["master_report"]
    loudness_error = abs(
        master_report["final_integrated_loudness_lufs"]
        - manifest.target_loudness_lufs)
    report = {
        "report_version": 1,
        "manifest_seed": manifest.seed,
        "sample_rate_hz": manifest.sample_rate,
        "noise_reference_band_hz": list(manifest.noise_band_hz),
        "tolerance": {
            "center_frequency_hz":
                manifest.center_frequency_tolerance_hz,
            "timing_ms": manifest.timing_tolerance_ms,
            "max_false_action_segments":
                manifest.max_false_action_segments,
            "residual_threshold_dbfs":
                manifest.residual_threshold_dbfs,
            "min_attenuation_db": manifest.min_attenuation_db,
            "max_content_loss_db": manifest.max_content_loss_db,
        },
        "noise_tracing": trace_section,
        "noise_reduction": denoise_section,
        "residual_check_from_wav": _recompute_mic_report(
            manifest, pipeline_result["trace_results"]),
        "mastering": {
            "target_loudness_lufs": manifest.target_loudness_lufs,
            "true_peak_limit_dbtp": manifest.true_peak_limit_dbtp,
            "input_loudness_lufs":
                master_report["input_loudness_lufs"],
            "normalization_gain_db":
                master_report["normalization_gain_db"],
            "pre_limiter_true_peak_dbtp":
                master_report["pre_limiter_true_peak_dbtp"],
            "limiter_lookahead_samples":
                master_report["limiter_lookahead_samples"],
            "limited_sample_count":
                master_report["limited_sample_count"],
            "limiter_max_attenuation_db":
                master_report["limiter_max_attenuation_db"],
            "final_integrated_loudness_lufs":
                master_report["final_integrated_loudness_lufs"],
            "final_true_peak_dbtp":
                master_report["final_true_peak_dbtp"],
            "loudness_error_lu": round(loudness_error, 3),
            "loudness_within_tolerance": bool(loudness_error <= 0.5),
            "true_peak_within_limit": bool(
                master_report["final_true_peak_dbtp"]
                <= manifest.true_peak_limit_dbtp),
            "total_samples": pipeline_result["total_samples"],
            "chunk_count": pipeline_result["chunk_count"],
        },
        "elapsed_seconds": round(float(elapsed_seconds), 6),
    }
    return report


def write_report(report: dict, path: str) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, sort_keys=False, indent=2,
                  ensure_ascii=False)
        handle.write("\n")


def write_recovery_report(path: str, manifest: Manifest,
                          snapshot: dict, elapsed_seconds: float) -> None:
    payload = {
        "status": "interrupted_resumable",
        "manifest_seed": manifest.seed,
        "completed_chunks": snapshot["completed_chunks"],
        "total_chunks": snapshot["total_chunks"],
        "chunk_samples": snapshot["chunk_samples"],
        "elapsed_seconds": round(float(elapsed_seconds), 6),
    }
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, sort_keys=False, indent=2,
                  ensure_ascii=False)
        handle.write("\n")
