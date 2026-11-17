from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any

from .analysis import AlignmentPlan
from .constants import E_RESUME_MISMATCH
from .dsp import FinalizeResult
from .errors import Diagnostic, ResumeMismatch
from .manifest import Manifest, SourceSet
from .reporting import total_blocks, write_json_atomic
from .wavio import wav_header


def build_snapshot(
    manifest: Manifest,
    sources: SourceSet,
    plan: AlignmentPlan,
    finalize: FinalizeResult,
    *,
    block_samples: int,
    blocks_done: int,
    prefix_sha256: str,
    output_label: str,
    bytes_committed: int,
) -> dict[str, Any]:
    blocks_total = total_blocks(plan.master_total_samples, block_samples)
    return {
        "schema": "bangzi-progress/v1",
        "manifest_sha256": manifest.raw_sha256,
        "input_sha256": {
            channel.id: sources.sha256[channel.id] for channel in manifest.channels
        },
        "block_samples": int(block_samples),
        "total_samples": int(plan.master_total_samples),
        "total_blocks": int(blocks_total),
        "blocks_done": int(blocks_done),
        "bangzi_offsets_samples": [
            alignment.bangzi_correction_samples for alignment in plan.segments
        ],
        "bangqiang_entries_samples": [
            entry.detected_aligned_sample for entry in plan.entries
        ],
        "segment_results": [
            {
                "index": alignment.index,
                "id": alignment.segment.id,
                "bangzi_correction_samples": alignment.bangzi_correction_samples,
                "channel_shifts_samples": {
                    channel.id: alignment.channel_shifts_samples[channel.id]
                    for channel in manifest.channels
                },
                "bangqiang_entry_reference_sample": entry.reference_sample,
                "bangqiang_entry_detected_sample": entry.detected_aligned_sample,
                "bangqiang_entry_error_samples": entry.error_samples,
            }
            for alignment, entry in zip(plan.segments, plan.entries)
        ],
        "render": {
            "gain_db": finalize.gain_db,
            "measured_lufs": finalize.measured_lufs,
            "measured_true_peak_dbtp": finalize.measured_true_peak_dbtp,
            "limiter_triggered_samples": finalize.limiter_triggered_samples,
        },
        "output": {
            "path": output_label,
            "prefix_sha256": prefix_sha256,
            "bytes_committed": int(bytes_committed),
            "complete": int(blocks_done) == int(blocks_total),
        },
    }


def read_snapshot(path: Path) -> dict[str, Any] | None:
    if not path.exists():
        return None
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return None
    if not isinstance(payload, dict) or payload.get("schema") != "bangzi-progress/v1":
        return None
    return payload


def _decision_mismatches(
    snapshot: dict[str, Any],
    manifest: Manifest,
    sources: SourceSet,
    plan: AlignmentPlan,
    finalize: FinalizeResult,
    block_samples: int,
    output_label: str,
) -> list[str]:
    mismatches: list[str] = []
    if snapshot.get("manifest_sha256") != manifest.raw_sha256:
        mismatches.append("manifest_sha256")
    expected_inputs = {
        channel.id: sources.sha256[channel.id] for channel in manifest.channels
    }
    if snapshot.get("input_sha256") != expected_inputs:
        mismatches.append("input_sha256")
    if snapshot.get("block_samples") != block_samples:
        mismatches.append("block_samples")
    if snapshot.get("total_samples") != plan.master_total_samples:
        mismatches.append("total_samples")
    expected_blocks = total_blocks(plan.master_total_samples, block_samples)
    if snapshot.get("total_blocks") != expected_blocks:
        mismatches.append("total_blocks")
    segments = snapshot.get("segment_results")
    expected_segments = [
        {
            "index": alignment.index,
            "id": alignment.segment.id,
            "bangzi_correction_samples": alignment.bangzi_correction_samples,
            "channel_shifts_samples": {
                channel.id: alignment.channel_shifts_samples[channel.id]
                for channel in manifest.channels
            },
            "bangqiang_entry_reference_sample": entry.reference_sample,
            "bangqiang_entry_detected_sample": entry.detected_aligned_sample,
            "bangqiang_entry_error_samples": entry.error_samples,
        }
        for alignment, entry in zip(plan.segments, plan.entries)
    ]
    if segments != expected_segments:
        mismatches.append("segment_results")
    render = snapshot.get("render")
    if not isinstance(render, dict):
        mismatches.append("render")
    else:
        if render.get("gain_db") != finalize.gain_db:
            mismatches.append("render.gain_db")
        if render.get("limiter_triggered_samples") != finalize.limiter_triggered_samples:
            mismatches.append("render.limiter_triggered_samples")
    output = snapshot.get("output")
    if not isinstance(output, dict) or output.get("path") != output_label:
        mismatches.append("output.path")
    return mismatches


def verify_resume(
    snapshot: dict[str, Any],
    manifest: Manifest,
    sources: SourceSet,
    plan: AlignmentPlan,
    finalize: FinalizeResult,
    *,
    block_samples: int,
    output_label: str,
    output_path: Path,
    bytes_per_frame: int,
) -> tuple[int, hashlib._Hash, bool]:
    mismatch = _decision_mismatches(
        snapshot,
        manifest,
        sources,
        plan,
        finalize,
        block_samples,
        output_label,
    )
    if mismatch:
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "快照决定与当前输入不一致: " + ",".join(mismatch),
                )
            ]
        )
    blocks_done = snapshot.get("blocks_done")
    total_blocks = snapshot.get("total_blocks")
    if (
        isinstance(blocks_done, bool)
        or not isinstance(blocks_done, int)
        or isinstance(total_blocks, bool)
        or not isinstance(total_blocks, int)
        or blocks_done < 0
        or blocks_done > total_blocks
    ):
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "快照块计数非法",
                )
            ]
        )
    output = snapshot["output"]
    prefix_sha = output.get("prefix_sha256")
    expected_bytes = int(output.get("bytes_committed", -1))
    if not isinstance(prefix_sha, str) or expected_bytes < 0:
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "快照输出前缀信息缺失",
                )
            ]
        )
    if not output_path.is_file() or output_path.stat().st_size < expected_bytes:
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "已提交母带前缀缺失或长度不足",
                )
            ]
        )
    expected_total_bytes = 44 + int(plan.master_total_samples) * bytes_per_frame
    if output_path.stat().st_size != expected_total_bytes:
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "母带文件长度与清单不符",
                )
            ]
        )
    expected_header = wav_header(
        manifest.sample_rate,
        manifest.bit_depth,
        2,
        plan.master_total_samples,
    )
    with output_path.open("rb") as handle:
        if handle.read(len(expected_header)) != expected_header:
            raise ResumeMismatch(
                [
                    Diagnostic(
                        E_RESUME_MISMATCH,
                        "resume",
                        "母带 WAV 头与当前清单不一致",
                    )
                ]
            )
    digest = hashlib.sha256()
    with output_path.open("rb") as handle:
        remaining = expected_bytes
        while remaining:
            chunk = handle.read(min(1024 * 1024, remaining))
            if not chunk:
                break
            digest.update(chunk)
            remaining -= len(chunk)
    if remaining != 0 or digest.hexdigest() != prefix_sha:
        raise ResumeMismatch(
            [
                Diagnostic(
                    E_RESUME_MISMATCH,
                    "resume",
                    "已提交母带前缀哈希不一致",
                )
            ]
        )
    complete = blocks_done == total_blocks
    return blocks_done, digest, complete


def discard_progress(snapshot_path: Path, output_path: Path) -> None:
    for path in (snapshot_path, output_path):
        try:
            path.unlink()
        except FileNotFoundError:
            pass
    try:
        directory = os.open(str(snapshot_path.parent), os.O_RDONLY)
    except OSError:
        directory = -1
    if directory >= 0:
        try:
            os.fsync(directory)
        finally:
            os.close(directory)


def commit_snapshot(path: Path, payload: dict[str, Any]) -> None:
    write_json_atomic(path, payload)

