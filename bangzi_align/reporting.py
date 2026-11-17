from __future__ import annotations

import json
import os
import tempfile
from pathlib import Path
from typing import Any

from .analysis import AlignmentPlan, decibels_ratio
from .manifest import Manifest
from .render import MasterBundle
from .wavio import PCM_HEADER_BYTES


def _rounded(value: float, digits: int = 8) -> float:
    if value == float("inf"):
        return 999.0
    if value == float("-inf"):
        return -999.0
    return round(float(value), digits)


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = (json.dumps(payload, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=path.name + ".", suffix=".tmp", dir=str(path.parent)
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory = os.open(str(path.parent), os.O_RDONLY)
        except OSError:
            directory = -1
        if directory >= 0:
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()


def build_master_report(
    manifest: Manifest,
    plan: AlignmentPlan,
    bundle: MasterBundle,
    *,
    output_label: str,
    blocks_done: int,
    total_blocks: int,
    block_samples: int,
    restarted: bool,
    interrupted: bool,
) -> dict[str, Any]:
    finalize = bundle.finalize
    segments: list[dict[str, Any]] = []
    for alignment, entry, stats in zip(plan.segments, plan.entries, bundle.segment_stats):
        segment = alignment.segment
        separation = entry.separation
        segments.append(
            {
                "index": alignment.index,
                "id": segment.id,
                "ban_style": segment.ban_style,
                "start_sample": segment.start_sample,
                "end_sample": segment.end_sample,
                "crossfade_samples": manifest.crossfade_samples,
                "crossfade_ms": _rounded(
                    1000.0 * manifest.crossfade_samples / manifest.sample_rate, 6
                ),
                "bangzi_offset_samples": alignment.bangzi_correction_samples,
                "channel_offsets_samples": {
                    channel.id: alignment.channel_shifts_samples[channel.id]
                    for channel in manifest.channels
                },
                "bangzi_aligned_hit_max_error_samples": alignment.max_abs_hit_error_samples,
                "bangzi_reference_count": len(segment.bangzi_reference_samples),
                "bangqiang_entry_reference_sample": entry.reference_sample,
                "bangqiang_entry_detected_sample": entry.detected_aligned_sample,
                "bangqiang_entry_error_samples": entry.error_samples,
                "separation_window_samples": [
                    separation.window_start_sample,
                    separation.window_end_sample,
                ],
                "overlap_energy_before": {
                    "laosheng": _rounded(separation.laosheng_energy_before),
                    "bangqiang": _rounded(separation.bangqiang_energy_before),
                    "bangqiang_share": _rounded(separation.bangqiang_share_before),
                },
                "overlap_energy_after": {
                    "laosheng": _rounded(separation.laosheng_energy_after),
                    "bangqiang": _rounded(separation.bangqiang_energy_after),
                    "bangqiang_share": _rounded(separation.bangqiang_share_after),
                },
                "overlap_energy_delta_db": {
                    "laosheng": _rounded(
                        decibels_ratio(
                            separation.laosheng_energy_after,
                            separation.laosheng_energy_before,
                        )
                    ),
                    "bangqiang": _rounded(
                        decibels_ratio(
                            separation.bangqiang_energy_after,
                            separation.bangqiang_energy_before,
                        )
                    ),
                },
                "overlap_modified_samples": separation.modified_samples,
                "master_lufs": _rounded(stats.lufs),
                "master_true_peak_dbtp": _rounded(stats.true_peak_dbtp),
            }
        )
    measured_lufs = finalize.measured_lufs
    measured_peak = finalize.measured_true_peak_dbtp
    report = {
        "schema": "bangzi-master-report/v1",
        "status": "interrupted" if interrupted else "complete",
        "manifest_sha256": manifest.raw_sha256,
        "seed": manifest.seed,
        "sample_rate": manifest.sample_rate,
        "bit_depth": manifest.bit_depth,
        "output": output_label,
        "total_samples": int(bundle.master.shape[0]),
        "longest_channel_samples": max(plan.aligned_lengths.values()),
        "alignment": {
            "max_abs_hit_error_samples": plan.max_abs_hit_error_samples,
            "max_abs_bangqiang_entry_error_samples": plan.max_abs_entry_error_samples,
            "cross_segment_offset_leak_samples": 0,
        },
        "crossfade": {
            "samples": manifest.crossfade_samples,
            "milliseconds": _rounded(
                1000.0 * manifest.crossfade_samples / manifest.sample_rate, 6
            ),
            "curve": "equal_power_sine_cosine",
        },
        "segments": segments,
        "loudness": {
            "standard": "ITU-R BS.1770-4",
            "target_lufs": _rounded(manifest.target_lufs),
            "measured_lufs": _rounded(measured_lufs),
            "deviation_lu": _rounded(measured_lufs - manifest.target_lufs),
            "target_tolerance_lu": 0.5,
            "loudness_compliant": abs(measured_lufs - manifest.target_lufs) <= 0.5,
            "true_peak_max_dbtp": _rounded(manifest.true_peak_max_dbtp),
            "measured_true_peak_dbtp": _rounded(measured_peak),
            "true_peak_compliant": measured_peak <= manifest.true_peak_max_dbtp + 1e-6,
            "normalization_gain_db": _rounded(finalize.gain_db),
            "limiter_triggered_samples": finalize.limiter_triggered_samples,
            "limiter_threshold_dbtp": _rounded(finalize.limiter_threshold_dbtp),
            "limiter_lookahead_samples": finalize.lookahead_samples,
            "limiter_delay_samples": 0,
            "limiter_length_preserved": True,
            "limiter_minimum_gain_db": _rounded(finalize.limiter_minimum_gain_db),
            "true_peak_before_limiting_dbtp": _rounded(
                finalize.peak_before_limiting_dbtp
            ),
            "true_peak_after_limiting_dbtp": _rounded(measured_peak),
            "true_peak_reduction_db": _rounded(finalize.peak_reduction_db),
            "iterations": finalize.iterations,
        },
        "render": {
            "block_samples": block_samples,
            "total_blocks": total_blocks,
            "blocks_committed": blocks_done,
            "resumed_from_snapshot": restarted,
        },
    }
    return report


def build_recovery_report(
    manifest: Manifest,
    plan: AlignmentPlan,
    *,
    output_label: str,
    block_samples: int,
    total_samples: int,
    total_blocks: int,
    blocks_done: int,
) -> dict[str, Any]:
    return {
        "schema": "bangzi-recovery-report/v1",
        "status": "interrupted",
        "manifest_sha256": manifest.raw_sha256,
        "output": output_label,
        "block_samples": block_samples,
        "total_samples": total_samples,
        "total_blocks": total_blocks,
        "blocks_committed": blocks_done,
        "next_block": blocks_done,
        "resume_required": blocks_done < total_blocks,
        "segment_results": [
            {
                "index": alignment.index,
                "id": alignment.segment.id,
                "bangzi_offset_samples": alignment.bangzi_correction_samples,
                "channel_offsets_samples": {
                    channel.id: alignment.channel_shifts_samples[channel.id]
                    for channel in manifest.channels
                },
                "bangqiang_entry_detected_sample": entry.detected_aligned_sample,
                "bangqiang_entry_error_samples": entry.error_samples,
            }
            for alignment, entry in zip(plan.segments, plan.entries)
        ],
    }


def total_blocks(total_samples: int, block_samples: int) -> int:
    return (int(total_samples) + int(block_samples) - 1) // int(block_samples)


def data_prefix_bytes(total_samples: int, block_samples: int, blocks_done: int, bytes_per_frame: int) -> int:
    samples = min(int(total_samples), int(blocks_done) * int(block_samples))
    return PCM_HEADER_BYTES + samples * int(bytes_per_frame)

