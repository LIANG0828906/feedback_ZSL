"""End-to-end deterministic renderer, recovery and reporting."""

from __future__ import annotations

import hashlib
import io
import os
import signal
import time
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .errors import (
    atomic_write_bytes,
    display_path,
    write_json,
)
from .manifest import (
    input_digest,
    load_manifest,
    manifest_digest,
    validate_audio_and_load,
    validate_manifest_document,
)
from .mastering import channel_peaks, master_bus
from .process import mix_stereo, process_channels
from .state import (
    alignment_parameters,
    assemble_blocks,
    commit_snapshot,
    compare_snapshot,
    discard_state,
    load_snapshot,
    new_snapshot,
    reflection_parameters,
    remove_state,
    validate_committed_blocks,
    write_block,
)


class InterruptFlag:
    def __init__(self) -> None:
        self.requested = False

    def handler(self, _signum: int, _frame: Any) -> None:
        self.requested = True


def encode_wav_bytes(
    stereo: np.ndarray,
    sample_rate: int,
    bit_depth: int,
) -> bytes:
    buffer = io.BytesIO()
    sf.write(
        buffer,
        np.asarray(stereo, dtype=np.float32),
        sample_rate,
        format="WAV",
        subtype=f"PCM_{bit_depth}",
    )
    return buffer.getvalue()


def _write_error_report(
    report_path: str | Path,
    errors: list[dict[str, Any]],
    warnings: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    report = {
        "status": "error",
        "schema_version": 1,
        "hard_errors": errors,
        "warnings": warnings or [],
        "master_created": False,
    }
    write_json(report_path, report)
    return report


def _public_channel_report(
    channel: dict[str, Any],
    contribution: float,
) -> dict[str, Any]:
    reflection = dict(channel["reflection"])
    return {
        "channel": channel["name"],
        "instrument": channel["instrument"],
        "file": channel["file"],
        "declared_start_offset_samples": channel[
            "declared_start_offset_samples"
        ],
        "fallback_alignment_samples": channel["fallback_alignment_samples"],
        "alignment_offset_samples": channel["alignment_offset_samples"],
        "alignment_status": channel["alignment_status"],
        "detected_onsets_before_alignment": channel[
            "detected_onsets_before_alignment"
        ],
        "onsets_after_alignment": channel["onsets_after_alignment"],
        "post_alignment_deviations_samples": channel[
            "post_alignment_deviations_samples"
        ],
        "max_abs_deviation_samples": channel["max_abs_deviation_samples"],
        "mix_weights": channel["mix_weights"],
        "contribution_percent": float(contribution),
        "reflection": reflection,
    }


def _resume_mismatch_event(
    block: int,
    differences: dict[str, Any],
) -> dict[str, Any]:
    return {
        "code": "E_RESUME_MISMATCH",
        "channel": None,
        "message": "committed snapshot no longer matches current input or "
        "determined parameters; old progress discarded",
        "block": int(block),
        "difference_fields": differences,
    }


def _find_first_resume_difference(
    expected: np.ndarray,
    actual: np.ndarray,
    block_frames: int,
    sample_rate: int,
    bit_depth: int,
) -> tuple[int | None, dict[str, Any]]:
    expected_frames = int(expected.shape[0])
    actual_frames = int(actual.shape[0])
    fields: dict[str, Any] = {
        "expected_frames": expected_frames,
        "actual_frames": actual_frames,
        "expected_wav_sha256": hashlib.sha256(
            encode_wav_bytes(expected, sample_rate, bit_depth)
        ).hexdigest(),
        "actual_wav_sha256": hashlib.sha256(
            encode_wav_bytes(actual, sample_rate, bit_depth)
        ).hexdigest(),
    }
    common = min(expected_frames, actual_frames)
    mismatch_sample = None
    if common:
        unequal = np.flatnonzero(
            np.any(
                expected[:common].astype(np.float64)
                != actual[:common].astype(np.float64),
                axis=1,
            )
        )
        if unequal.size:
            mismatch_sample = int(unequal[0])
    block = (
        mismatch_sample // block_frames
        if mismatch_sample is not None
        else (0 if expected_frames != actual_frames else None)
    )
    if block is not None:
        start = block * block_frames
        end_expected = min(expected_frames, start + block_frames)
        end_actual = min(actual_frames, start + block_frames)
        fields["frame_offset"] = start
        fields["expected_block_sha256"] = hashlib.sha256(
            expected[start:end_expected].astype("<f4").tobytes()
        ).hexdigest()
        fields["actual_block_sha256"] = hashlib.sha256(
            actual[start:end_actual].astype("<f4").tobytes()
        ).hexdigest()
        fields["expected_block_frames"] = int(end_expected - start)
        fields["actual_block_frames"] = int(end_actual - start)
    return block, fields


def _write_interrupted_reports(
    report_target: str | Path,
    recovery_target: str | Path,
    snapshot: dict[str, Any],
    resume_count: int,
) -> dict[str, Any]:
    report = {
        "status": "interrupted",
        "schema_version": 1,
        "exit_code": 0,
        "hard_errors": [],
        "warnings": [],
        "master_created": False,
        "manifest_digest": snapshot["manifest_digest"],
        "input_digest": snapshot["input_digest"],
        "completed_blocks": int(snapshot["completed_blocks"]),
        "frames_done": int(snapshot["frames_done"]),
        "total_frames": int(snapshot["total_frames"]),
        "total_blocks": int(snapshot["total_blocks"]),
        "resume_count": int(resume_count),
        "alignment_offsets": snapshot["alignment_offsets"],
        "reflection_params": snapshot["reflection_params"],
        "next_action": "rerun render to resume from the committed snapshot",
    }
    write_json(report_target, report)
    write_json(recovery_target, report)
    return report


def render(
    manifest_path: str | Path,
    output_path: str | Path,
    report_path: str | Path,
    state_dir: str | Path,
    recovery_report_path: str | Path,
    fresh: bool = False,
) -> tuple[int, dict[str, Any]]:
    interrupt = InterruptFlag()
    previous_sigint = signal.signal(signal.SIGINT, interrupt.handler)
    previous_sigterm = signal.signal(signal.SIGTERM, interrupt.handler)
    try:
        return _render_impl(
            manifest_path=manifest_path,
            output_path=output_path,
            report_path=report_path,
            state_dir=state_dir,
            recovery_report_path=recovery_report_path,
            fresh=fresh,
            interrupt=interrupt,
        )
    finally:
        signal.signal(signal.SIGINT, previous_sigint)
        signal.signal(signal.SIGTERM, previous_sigterm)


def _render_impl(
    manifest_path: str | Path,
    output_path: str | Path,
    report_path: str | Path,
    state_dir: str | Path,
    recovery_report_path: str | Path,
    fresh: bool,
    interrupt: InterruptFlag,
) -> tuple[int, dict[str, Any]]:
    Path(report_path).unlink(missing_ok=True)
    Path(recovery_report_path).unlink(missing_ok=True)
    document, load_errors = load_manifest(manifest_path)
    if load_errors:
        report = _write_error_report(report_path, load_errors)
        return 2, report
    assert document is not None

    structural_errors = validate_manifest_document(document)
    if structural_errors:
        report = _write_error_report(report_path, structural_errors)
        return 2, report

    loaded_channels, audio_errors = validate_audio_and_load(
        document, manifest_path
    )
    if audio_errors:
        report = _write_error_report(report_path, audio_errors)
        return 2, report

    manifest_digest_value = manifest_digest(document)
    input_digest_value = input_digest(
        manifest_digest_value, loaded_channels
    )
    state_path = Path(state_dir)
    resume_events: list[dict[str, Any]] = []
    recovery_attempts = 0
    blocks_reused = 0
    snapshot: dict[str, Any] | None = None

    if fresh and state_path.exists():
        discard_state(state_path)

    previous_snapshot, snapshot_error = load_snapshot(state_path)
    if previous_snapshot is not None or snapshot_error is not None:
        recovery_attempts = 1
    if snapshot_error is not None:
        resume_events.append(
            _resume_mismatch_event(
                int((previous_snapshot or {}).get("completed_blocks", 0)),
                snapshot_error,
            )
        )
        discard_state(state_path)
    elif previous_snapshot is not None:
        early_differences = compare_snapshot(
            previous_snapshot,
            manifest_digest_value,
            input_digest_value,
            previous_snapshot.get("alignment_offsets"),
            previous_snapshot.get("reflection_params"),
        )
        # Only input identity can be compared before signal analysis. Parameter
        # comparison is repeated after deterministic detection.
        early_differences = {
            key: value
            for key, value in early_differences.items()
            if key in ("manifest_digest", "input_digest")
        }
        if early_differences:
            resume_events.append(
                _resume_mismatch_event(
                    int(previous_snapshot.get("completed_blocks", 0)),
                    early_differences,
                )
            )
            discard_state(state_path)
        else:
            snapshot = previous_snapshot

    if interrupt.requested:
        # No deterministic parameters exist yet. Return a recoverable,
        # successful interruption report rather than a hard error.
        report = {
            "status": "interrupted",
            "schema_version": 1,
            "exit_code": 0,
            "hard_errors": [],
            "warnings": [],
            "master_created": False,
            "completed_blocks": 0,
            "resume_count": 0,
        }
        write_json(report_path, report)
        write_json(recovery_report_path, report)
        return 0, report

    processed_channels, warnings = process_channels(
        loaded_channels, document
    )
    alignment_params = alignment_parameters(processed_channels)
    reflection_params = reflection_parameters(processed_channels)

    if snapshot is not None:
        differences = compare_snapshot(
            snapshot,
            manifest_digest_value,
            input_digest_value,
            alignment_params,
            reflection_params,
        )
        if differences:
            resume_events.append(
                _resume_mismatch_event(
                    int(snapshot.get("completed_blocks", 0)),
                    differences,
                )
            )
            discard_state(state_path)
            snapshot = None
        else:
            snapshot_valid, snapshot_difference = validate_committed_blocks(
                snapshot,
                state_path,
                int(max(channel["aligned_compensated"].size for channel in processed_channels)),
                int(document["block_frames"]),
            )
            if not snapshot_valid:
                resume_events.append(
                    _resume_mismatch_event(
                        int(snapshot.get("completed_blocks", 0)),
                        snapshot_difference,
                    )
                )
                discard_state(state_path)
                snapshot = None
            else:
                blocks_reused = int(snapshot["completed_blocks"])

    resume_count = (
        int(snapshot["resume_count"]) + 1 if snapshot is not None else 0
    )
    bus, contributions, total_frames = mix_stereo(processed_channels)
    master, loudness_report = master_bus(
        bus,
        int(document["sample_rate"]),
        float(document["target_loudness_lufs"]),
        float(document.get("true_peak_limit_dbtp", -1.0)),
    )
    master_float32 = np.asarray(master, dtype=np.float32)
    if master_float32.shape[0] != total_frames:
        raise RuntimeError("limiter changed the master frame count")

    block_frames = int(document["block_frames"])
    total_blocks = (total_frames + block_frames - 1) // block_frames
    if snapshot is None:
        discard_state(state_path)
        snapshot = new_snapshot(
            document=document,
            manifest_digest=manifest_digest_value,
            input_digest=input_digest_value,
            processing_alignment=alignment_params,
            processing_reflection=reflection_params,
            block_frames=block_frames,
            total_frames=total_frames,
            resume_count=0,
        )
        resume_count = 0
    else:
        snapshot["resume_count"] = resume_count
    commit_snapshot(state_path, snapshot)

    if interrupt.requested:
        report = _write_interrupted_reports(
            report_path, recovery_report_path, snapshot, resume_count
        )
        return 0, report

    kill_after_text = os.environ.get("WUCHANG_TEST_KILL_AFTER_BLOCKS")
    kill_after = (
        int(kill_after_text)
        if kill_after_text and kill_after_text.isdigit()
        else None
    )
    delay_text = os.environ.get("WUCHANG_TEST_BLOCK_DELAY_MS", "0")
    try:
        block_delay = max(0.0, float(delay_text) / 1000.0)
    except ValueError:
        block_delay = 0.0

    completed = int(snapshot["completed_blocks"])
    for index in range(completed, total_blocks):
        start = index * block_frames
        end = min(total_frames, start + block_frames)
        block = master_float32[start:end]
        entry = write_block(state_path, index, block)
        snapshot["blocks"].append(entry)
        snapshot["completed_blocks"] = index + 1
        snapshot["frames_done"] = end
        snapshot["resume_count"] = resume_count
        commit_snapshot(state_path, snapshot)
        if block_delay > 0.0:
            time.sleep(block_delay)
        if interrupt.requested:
            report = _write_interrupted_reports(
                report_path, recovery_report_path, snapshot, resume_count
            )
            return 0, report
        if kill_after is not None and index + 1 == kill_after:
            os._exit(137)

    assembled = assemble_blocks(snapshot, state_path)
    final_wav = encode_wav_bytes(
        assembled, int(document["sample_rate"]), int(document["bit_depth"])
    )
    expected_wav = encode_wav_bytes(
        master_float32, int(document["sample_rate"]), int(document["bit_depth"])
    )

    diff_block: int | None = None
    diff_fields: dict[str, Any] = {}
    if resume_count > 0 and final_wav != expected_wav:
        diff_block, diff_fields = _find_first_resume_difference(
            master_float32,
            assembled,
            block_frames,
            int(document["sample_rate"]),
            int(document["bit_depth"]),
        )
        if diff_block is not None:
            diff_fields = {
                "expected_wav_sha256": hashlib.sha256(expected_wav).hexdigest(),
                "actual_wav_sha256": hashlib.sha256(final_wav).hexdigest(),
                **diff_fields,
            }
        difference_error = {
            "code": "E_RESUME_DIFFERENCE",
            "channel": None,
            "message": "resumed master differs from a one-shot deterministic "
            "render",
            "block": diff_block,
            "difference_fields": diff_fields,
        }
        report = {
            "status": "resume_difference",
            "schema_version": 1,
            "hard_errors": [difference_error],
            "warnings": warnings,
            "master_created": False,
            "resume_difference": {
                "block": diff_block,
                "difference_fields": diff_fields,
            },
        }
        write_json(report_path, report)
        diff_path = Path(report_path).with_name(
            Path(report_path).stem + ".diff.json"
        )
        write_json(diff_path, report)
        target = Path(output_path)
        if target.exists():
            target.unlink()
        return 3, report

    atomic_write_bytes(Path(output_path), final_wav)
    master_sha = hashlib.sha256(final_wav).hexdigest()
    report = {
        "status": "ok",
        "schema_version": 1,
        "hard_errors": [],
        "warnings": warnings,
        "manifest": {
            "digest": manifest_digest_value,
            "input_digest": input_digest_value,
            "sample_rate": int(document["sample_rate"]),
            "bit_depth": int(document["bit_depth"]),
            "block_frames": block_frames,
        },
        "resume": {
            "resume_count": int(resume_count),
            "recovery_attempts": int(recovery_attempts),
            "blocks_reused": int(blocks_reused),
            "events": resume_events,
        },
        "channels": [
            _public_channel_report(channel, contribution)
            for channel, contribution in zip(processed_channels, contributions)
        ],
        "master": {
            "file": display_path(output_path, Path.cwd()),
            "channels": 2,
            "sample_rate": int(document["sample_rate"]),
            "bit_depth": int(document["bit_depth"]),
            "frames": int(frames := assembled.shape[0]),
            "duration_seconds": float(frames / int(document["sample_rate"])),
            "sha256": master_sha,
            "peaks_dbfs": channel_peaks(assembled),
        },
        "loudness": loudness_report,
        "render": {
            "blocks_total": int(total_blocks),
            "blocks_completed": int(snapshot["completed_blocks"]),
            "resume_count": int(resume_count),
            "fixed_block_frames": block_frames,
            "resume_master_byte_identical": bool(
                resume_count == 0 or final_wav == expected_wav
            ),
            "state_discarded_after_success": True,
        },
    }
    write_json(report_path, report)
    remove_state(state_path)
    return 0, report

