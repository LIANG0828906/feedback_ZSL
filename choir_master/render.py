from __future__ import annotations

import math
import os
import shlex
import signal
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .compensation import apply_tenor_compensation
from .detection import (
    PassaggioEvent,
    detect_for_project,
    false_positive_audit,
)
from .errors import MasterError
from .loudness import (
    energy_fractions,
    finalize_loudness,
    integrated_loudness,
    true_peak_dbtp,
)
from .manifest import LoadedAudio, load_and_validate_audio
from .snapshot import (
    build_snapshot,
    discard_snapshot,
    load_resume_state,
    save_snapshot,
)
from .util import (
    atomic_write_json,
    linear_to_db,
    sha256_file,
)


class InterruptFlag:
    def __init__(self) -> None:
        self.triggered = False
        self._previous: Any = None

    def __enter__(self) -> "InterruptFlag":
        if hasattr(signal, "SIGINT"):
            self._previous = signal.getsignal(signal.SIGINT)
            signal.signal(signal.SIGINT, self._handle)
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        if hasattr(signal, "SIGINT") and self._previous is not None:
            signal.signal(signal.SIGINT, self._previous)

    def _handle(self, signum: int, frame: Any) -> None:
        self.triggered = True


def _padded(signal: np.ndarray, length: int) -> np.ndarray:
    array = np.asarray(signal, dtype=np.float64)
    if len(array) == length:
        return array
    if len(array) > length:
        return array[:length]
    return np.pad(array, (0, length - len(array)))


def _event_dicts(events: list[PassaggioEvent]) -> list[dict[str, Any]]:
    return [event.as_dict() for event in events]


def _voice_mono_tracks(
    loaded: LoadedAudio,
    overrides: dict[str, np.ndarray],
) -> dict[str, np.ndarray]:
    total = loaded.total_samples
    result: dict[str, np.ndarray] = {}
    for voice in loaded.manifest.voices:
        first_id, second_id = [track.track_id for track in voice.tracks]
        first = overrides.get(first_id, loaded.tracks[first_id])
        second = overrides.get(second_id, loaded.tracks[second_id])
        result[voice.voice] = 0.5 * (
            _padded(first, total) + _padded(second, total)
        )
    return result


def _voice_energies(mono_tracks: dict[str, np.ndarray], loaded: LoadedAudio) -> dict[str, float]:
    energies: dict[str, float] = {}
    for voice in loaded.manifest.voices:
        mono = mono_tracks[voice.voice]
        left_weight, right_weight = voice.downmix_weights
        energies[voice.voice] = float(
            np.sum((mono * left_weight) ** 2) + np.sum((mono * right_weight) ** 2)
        )
    return energies


def _make_stems(
    mono_tracks: dict[str, np.ndarray],
    loaded: LoadedAudio,
    gains: dict[str, float],
) -> dict[str, np.ndarray]:
    total = loaded.total_samples
    stems: dict[str, np.ndarray] = {}
    for voice in loaded.manifest.voices:
        mono = mono_tracks[voice.voice]
        gain = gains[voice.voice]
        left_weight, right_weight = voice.downmix_weights
        stems[voice.voice] = np.column_stack(
            (mono * gain * left_weight, mono * gain * right_weight)
        )
        if stems[voice.voice].shape != (total, 2):
            raise AssertionError("stem shape mismatch")
    return stems


def _mix_block(
    stems: dict[str, np.ndarray],
    start: int,
    end: int,
) -> np.ndarray:
    result = np.zeros((end - start, 2), dtype=np.float64)
    for stem in stems.values():
        result += stem[start:end]
    return result


def _write_master_pcm(
    path: Path,
    audio: np.ndarray,
    sample_rate: int,
    bit_depth: int,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}.wav")
    subtype = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}[bit_depth]
    sf.write(
        str(temporary),
        np.asarray(audio, dtype=np.float64),
        sample_rate,
        subtype=subtype,
        format="WAV",
    )
    os.replace(temporary, path)
    try:
        directory_fd = os.open(path.parent, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def _correction_report(
    loaded: LoadedAudio,
    pre_energies: dict[str, float],
    gains: dict[str, float],
) -> dict[str, Any]:
    total = sum(pre_energies.values())
    pre = {
        voice.voice: pre_energies[voice.voice] / total
        for voice in loaded.manifest.voices
    }
    post_total = sum((gains[voice] ** 2) * pre_energies[voice] for voice in gains)
    post = {
        voice.voice: (gains[voice.voice] ** 2) * pre_energies[voice.voice] / post_total
        for voice in loaded.manifest.voices
    }
    target = {
        voice.voice: voice.target_energy_ratio for voice in loaded.manifest.voices
    }
    return {
        "target_energy_ratio": target,
        "pre_correction_energy_ratio": pre,
        "correction_coefficients": {
            voice: {
                "linear_gain": float(gains[voice]),
                "gain_db": round(float(linear_to_db(gains[voice])), 6),
            }
            for voice in sorted(gains)
        },
        "post_correction_energy_ratio": post,
        "max_abs_error": max(abs(post[voice] - target[voice]) for voice in target),
        "downmix_weights": {
            voice.voice: list(voice.downmix_weights)
            for voice in loaded.manifest.voices
        },
    }


def _resume_command(
    manifest_path: Path,
    output_path: Path,
    snapshot_path: Path,
    report_path: Path,
) -> str:
    return shlex.join(
        [
            "python",
            "-m",
            "choir_master",
            "render",
            "--manifest",
            str(manifest_path),
            "--output",
            str(output_path),
            "--snapshot",
            str(snapshot_path),
            "--report",
            str(report_path),
        ]
    )


def render_project(
    *,
    manifest_path: str | Path,
    output_path: str | Path,
    snapshot_path: str | Path,
    report_path: str | Path,
    recovery_report_path: str | Path | None = None,
    fresh: bool = False,
    interrupt: InterruptFlag | None = None,
) -> dict[str, Any]:
    interrupt = interrupt or InterruptFlag()
    manifest_file = Path(manifest_path).expanduser().resolve()
    output_file = Path(output_path).expanduser().resolve()
    snapshot_file = Path(snapshot_path).expanduser().resolve()
    report_file = Path(report_path).expanduser().resolve()
    recovery_file = (
        Path(recovery_report_path).expanduser().resolve()
        if recovery_report_path is not None
        else report_file.with_name(report_file.stem + ".recovery.json")
    )
    if fresh:
        discard_snapshot(snapshot_file)

    loaded = load_and_validate_audio(manifest_file)
    manifest = loaded.manifest
    total_samples = loaded.total_samples
    if total_samples <= 0:
        raise MasterError("E_SILENT_MASTER", "All input tracks are empty.")
    block_size = manifest.block_size
    total_blocks = int(math.ceil(total_samples / block_size))

    events = detect_for_project(loaded)
    if not events:
        raise MasterError(
            "E_PASSAGGIO_NOT_FOUND",
            "No passaggio-region timbre discontinuity was detected in the tenor tracks.",
            path=str(manifest_file),
            details={"band_hz": list(manifest.transition_band_hz)},
        )
    event_dicts = _event_dicts(events)

    compensated_tracks, compensation_report = apply_tenor_compensation(loaded, events)
    if not compensation_report["outside_detected_intervals_bit_exact"]:
        raise MasterError(
            "E_COMPENSATION_BOUNDARY_VIOLATION",
            "Compensation modified tenor samples outside the confirmed intervals.",
        )
    if not compensation_report["improvement_requirement_met"]:
        raise MasterError(
            "E_COMPENSATION_INSUFFICIENT",
            "Band-energy discontinuity improvement is below 8 dB.",
            details=compensation_report,
        )

    mono_tracks = _voice_mono_tracks(loaded, compensated_tracks)
    pre_energies = _voice_energies(mono_tracks, loaded)
    total_energy = sum(pre_energies.values())
    if total_energy <= 1e-30:
        raise MasterError("E_SILENT_MASTER", "The downmixed source has no energy.")
    gains = {
        voice.voice: math.sqrt(
            voice.target_energy_ratio * total_energy / pre_energies[voice.voice]
        )
        for voice in manifest.voices
    }
    balance_report = _correction_report(loaded, pre_energies, gains)
    if balance_report["max_abs_error"] > 0.02:
        raise MasterError(
            "E_BALANCE_FAILED",
            "Voice balancing did not reach the requested energy ratios.",
            details=balance_report,
        )
    stems = _make_stems(mono_tracks, loaded, gains)

    snapshot: dict[str, Any] | None = None
    resumed = snapshot_file.is_file()
    if resumed:
        snapshot = load_resume_state(
            snapshot_file,
            manifest_digest=manifest.digest,
            input_digests=loaded.path_digests,
            block_size=block_size,
            sample_rate=manifest.sample_rate,
            total_samples=total_samples,
            confirmed_intervals=event_dicts,
            voice_corrections=gains,
        )
        pre_master = snapshot["_pre_master"]
        completed_blocks = int(snapshot["completed_blocks"])
    else:
        pre_master = np.zeros((0, 2), dtype=np.float64)
        completed_blocks = 0

    def persist(stage: str) -> dict[str, Any]:
        state = build_snapshot(
            manifest_digest=manifest.digest,
            input_digests=loaded.path_digests,
            block_size=block_size,
            sample_rate=manifest.sample_rate,
            total_samples=total_samples,
            completed_blocks=completed_blocks,
            confirmed_intervals=event_dicts,
            voice_corrections=gains,
            pre_master=pre_master,
            stage=stage,
        )
        save_snapshot(snapshot_file, state)
        return state

    if pre_master.shape != (min(completed_blocks * block_size, total_samples), 2):
        raise AssertionError("resume master length is inconsistent")

    # Commit an initial state after analysis so an interrupt before the first
    # block still leaves a complete, resumable snapshot and recovery report.
    if not resumed:
        persist("rendering")

    def interrupted_result(message: str) -> dict[str, Any]:
        recovery = {
            "status": "interrupted",
            "code": "E_INTERRUPTED_RESUMABLE",
            "message": message,
            "manifest": str(manifest_file),
            "manifest_digest": manifest.digest,
            "snapshot": str(snapshot_file),
            "completed_blocks": completed_blocks,
            "total_blocks": total_blocks,
            "confirmed_intervals": event_dicts,
            "voice_corrections": gains,
            "resume_command": _resume_command(
                manifest_file, output_file, snapshot_file, report_file
            ),
        }
        atomic_write_json(recovery_file, recovery)
        return {
            "status": "interrupted",
            "exit_code": 0,
            "recovery_report": str(recovery_file),
            "snapshot": str(snapshot_file),
            "completed_blocks": completed_blocks,
        }

    interrupted = False
    while completed_blocks < total_blocks:
        if interrupt.triggered:
            interrupted = True
            break
        block_index = completed_blocks
        start = block_index * block_size
        end = min(total_samples, start + block_size)
        block = _mix_block(stems, start, end)
        pre_master = np.concatenate([pre_master, block], axis=0)
        completed_blocks += 1
        persist("rendering")
        if interrupt.triggered:
            interrupted = True
            break

    if interrupted:
        return interrupted_result(
            "Ctrl+C was handled after committing the current block."
        )

    if interrupt.triggered:
        return interrupted_result(
            "Ctrl+C was handled after committing the final block."
        )

    loudness = finalize_loudness(
        pre_master,
        manifest.sample_rate,
        manifest.target_loudness_lufs,
        manifest.true_peak_limit_dbtp,
    )
    if interrupt.triggered:
        return interrupted_result(
            "Ctrl+C was handled after block rendering; loudness finalization will resume from the snapshot."
        )
    if loudness.audio.shape[0] != total_samples:
        raise MasterError(
            "E_SAMPLE_COUNT_CHANGED",
            "Master processing changed the total sample count.",
            details={
                "expected": total_samples,
                "actual": int(loudness.audio.shape[0]),
            },
        )
    _write_master_pcm(
        output_file,
        loudness.audio,
        manifest.sample_rate,
        manifest.bit_depth,
    )
    quantized, quantized_rate = sf.read(
        str(output_file), dtype="float64", always_2d=True
    )
    if quantized_rate != manifest.sample_rate or quantized.shape != (total_samples, 2):
        raise MasterError(
            "E_MASTER_VERIFY_FAILED",
            "Written master does not have the expected format or sample count.",
            path=str(output_file),
        )
    final_lufs = integrated_loudness(quantized, quantized_rate)
    final_tp = true_peak_dbtp(quantized)
    if abs(final_lufs - manifest.target_loudness_lufs) > 0.5:
        raise MasterError(
            "E_LOUDNESS_TARGET_MISSED",
            "Quantized master misses the requested integrated loudness tolerance.",
            path=str(output_file),
            details={
                "target_lufs": manifest.target_loudness_lufs,
                "measured_lufs": final_lufs,
            },
        )
    if final_tp > manifest.true_peak_limit_dbtp + 0.01:
        raise MasterError(
            "E_TRUE_PEAK_EXCEEDED",
            "Quantized master exceeds the declared true-peak limit.",
            path=str(output_file),
            details={
                "limit_dbtp": manifest.true_peak_limit_dbtp,
                "measured_dbtp": final_tp,
            },
        )

    final_ratios = energy_fractions(stems, loudness.limiter.gain)
    target_ratios = {
        voice.voice: voice.target_energy_ratio for voice in manifest.voices
    }
    final_max_error = max(
        abs(final_ratios[voice] - target_ratios[voice]) for voice in target_ratios
    )
    if final_max_error > 0.02:
        raise MasterError(
            "E_FINAL_BALANCE_FAILED",
            "True-peak limiting changed final voice proportions beyond the allowed tolerance.",
            details={
                "final_energy_ratio": final_ratios,
                "target_energy_ratio": target_ratios,
                "max_abs_error": final_max_error,
            },
        )

    audit = false_positive_audit(loaded)
    false_positive_count = sum(len(items) for items in audit.values())
    if false_positive_count:
        raise MasterError(
            "E_DETECTION_FALSE_POSITIVE",
            "Passaggio detection reported events in voices without injected transitions.",
            details={"audit": audit},
        )

    persist("finalized")
    report = {
        "status": "ok",
        "manifest": str(manifest_file),
        "manifest_digest": manifest.digest,
        "master": {
            "path": str(output_file),
            "sha256": sha256_file(output_file),
            "sample_rate": manifest.sample_rate,
            "bit_depth": manifest.bit_depth,
            "channels": 2,
            "samples": int(quantized.shape[0]),
            "duration_s": float(quantized.shape[0] / manifest.sample_rate),
        },
        "detection": {
            "voice": "tenor",
            "band_hz": list(manifest.transition_band_hz),
            "event_count": len(events),
            "intervals": event_dicts,
            "false_positive_audit": audit,
            "false_positive_count": false_positive_count,
        },
        "voice_intervals": {
            voice.voice: event_dicts if voice.voice == "tenor" else []
            for voice in manifest.voices
        },
        "compensation": compensation_report,
        "balance": balance_report,
        "loudness": {
            "target_lufs": manifest.target_loudness_lufs,
            "measured_lufs": round(float(final_lufs), 6),
            "delta_lu": round(float(final_lufs - manifest.target_loudness_lufs), 6),
            "true_peak_limit_dbtp": manifest.true_peak_limit_dbtp,
            "measured_true_peak_dbtp": round(float(final_tp), 6),
            "limiter_triggered_samples": loudness.limiter.triggered_samples,
            "limiter_lookahead_samples": loudness.limiter.lookahead_samples,
            "limiter_delay_compensated": loudness.limiter.delay_compensated,
            "master_gain_db": round(float(loudness.master_gain_db), 6),
            "sample_count_preserved": bool(quantized.shape[0] == total_samples),
            "master_samples": int(quantized.shape[0]),
        },
        "final_energy_ratio": final_ratios,
        "final_energy_target": target_ratios,
        "final_energy_max_abs_error": round(float(final_max_error), 9),
        "alignment": {
            "max_time_shift_samples": 0,
            "sample_alignment_preserved": True,
            "total_samples": int(total_samples),
        },
        "resume": {
            "resumed_from_snapshot": bool(resumed),
            "snapshot": str(snapshot_file),
            "completed_blocks": completed_blocks,
            "total_blocks": total_blocks,
            "block_size": block_size,
        },
    }
    atomic_write_json(report_file, report)
    return {
        "status": "ok",
        "exit_code": 0,
        "report": str(report_file),
        "master": str(output_file),
        "master_sha256": report["master"]["sha256"],
        "measured_lufs": report["loudness"]["measured_lufs"],
        "measured_true_peak_dbtp": report["loudness"]["measured_true_peak_dbtp"],
        "final_energy_max_abs_error": report["final_energy_max_abs_error"],
    }

