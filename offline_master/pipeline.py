"""End-to-end offline mastering pipeline with resumable block rendering."""

from __future__ import annotations

import json
import os
import signal
from dataclasses import dataclass

import numpy as np
import soundfile as sf

from .alignment import align_tracks
from .codes import BusinessError, ErrorCode
from .loudness import comply, integrated_loudness, true_peak_db
from .manifest import load_manifest, validate_manifest
from .phase import phase_correct_groups
from .spatial import (
    StreamingBinauralPanner,
    azimuth_accuracy_report,
    params_for_angles,
    stability_report,
)
SNAPSHOT_NAME = "snapshot.json"
RECOVERY_NAME = "recovery_report.json"


def _atomic_write(path: str, payload: bytes) -> None:
    temporary = f"{path}.tmp"
    with open(temporary, "wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _write_json(path: str, payload: dict) -> None:
    _atomic_write(path, json.dumps(payload, indent=2, sort_keys=True).encode("utf-8"))


@dataclass
class RenderInterruption:
    requested: bool = False


def _ensure_stems(manifest: dict, base_dir: str) -> None:
    """Synthesize any missing/declared stem; existing files are left untouched."""
    from .synthesis import synthesize_stem

    for track in manifest["tracks"]:
        path = os.path.join(base_dir, track["file"])
        if not os.path.isfile(path):
            synthesize_stem(
                track,
                manifest["sample_rate"],
                manifest["bit_depth"],
                manifest["duration_s"],
                base_dir,
            )


def _load_or_restart(snapshot_dir: str, digest: str, param_hash_payload: dict):
    snapshot_path = os.path.join(snapshot_dir, SNAPSHOT_NAME)
    if not os.path.isfile(snapshot_path):
        return 0, [], None
    with open(snapshot_path, "r", encoding="utf-8") as handle:
        snapshot = json.load(handle)
    if (
        snapshot.get("manifest_digest") != digest
        or snapshot.get("param_digest") != _param_digest(param_hash_payload)
    ):
        for name in os.listdir(snapshot_dir):
            os.remove(os.path.join(snapshot_dir, name))
        raise BusinessError(
            ErrorCode.E_RESUME_MISMATCH,
            [
                {
                    "file": os.path.join(snapshot_dir, SNAPSHOT_NAME),
                    "reason": "resume_state_mismatch",
                    "code": ErrorCode.E_RESUME_MISMATCH,
                }
            ],
            "snapshot does not match current manifest/parameters; restarted",
        )
    completed = int(snapshot["completed_blocks"])
    blocks = [
        np.load(os.path.join(snapshot_dir, f"block_{index:06d}.npy"))
        for index in range(completed)
    ]
    return completed, blocks, snapshot


def _param_digest(payload: dict) -> str:
    import hashlib

    return hashlib.sha256(
        json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()


def render_spatial_blocks(
    corrected: list[np.ndarray],
    tracks: list[dict],
    manifest: dict,
    snapshot_dir: str,
    strong_section_s: tuple[float, float],
    block_sleep_s: float = 0.0,
) -> tuple[np.ndarray, dict, list[dict], list[dict], str | None]:
    """Render block-by-block, committing an atomic snapshot after each block."""
    sr = manifest["sample_rate"]
    frame_samples = int(round(manifest["frame_s"] * sr))
    block_frames = int(manifest["block_frames"])
    block_samples = frame_samples * block_frames
    total_samples = corrected[0].shape[0]
    angles = manifest["angles"]

    per_track_params = [
        params_for_angles(tuple(track["anchor"]), angles, sr) for track in tracks
    ]
    stability_rows = [
        stability_report(rows, manifest["stability_threshold_db"])
        for rows in per_track_params
    ]
    azimuth_rows = [azimuth_accuracy_report(rows) for rows in per_track_params]

    interruption = RenderInterruption()
    previous_handler = signal.getsignal(signal.SIGINT)

    def _handler(signum, frame):  # pragma: no cover - exercised via tests
        interruption.requested = True

    signal.signal(signal.SIGINT, _handler)
    os.makedirs(snapshot_dir, exist_ok=True)

    param_payload = {
        "alignment_ready_sha": _array_digest(corrected),
        "anchors": [track["anchor"] for track in tracks],
        "frame_s": manifest["frame_s"],
        "block_frames": block_frames,
        "angles": angles,
    }
    completed, prior_blocks, prior_snapshot = _load_or_restart(
        snapshot_dir, manifest["digest"], param_payload
    )
    # Each block is rendered independently with a zero-padded context prefix
    # feeding the variable delay lines, so resuming from a committed block
    # yields byte-identical output to a single uninterrupted run.
    context_samples = 512

    total_blocks = int(np.ceil(total_samples / block_samples))
    blocks: list[np.ndarray] = list(prior_blocks)
    interrupted_after: int | None = None
    try:
        for block_index in range(total_blocks):
            if block_index < completed:
                continue
            start = block_index * block_samples
            end = min(total_samples, start + block_samples)
            mix = np.zeros((end - start, 2), dtype=np.float64)
            for track_index, signal_data in enumerate(corrected):
                prefix_start = max(0, start - context_samples)
                context = signal_data[prefix_start:start]
                block = np.concatenate(
                    [
                        np.zeros(context_samples - context.shape[0]),
                        context,
                        signal_data[start:end],
                    ]
                )
                prefix = block.shape[0] - (end - start)
                panner = StreamingBinauralPanner(sr)
                rendered = np.zeros((block.shape[0], 2), dtype=np.float64)
                for frame in range(block_frames):
                    f0 = prefix + frame * frame_samples
                    f1 = min(block.shape[0], f0 + frame_samples)
                    if f1 <= f0:
                        break
                    angle_index = min(
                        len(angles) - 1,
                        (start + frame * frame_samples) // frame_samples,
                    )
                    start_params = per_track_params[track_index][angle_index]
                    next_index = min(len(angles) - 1, angle_index + 1)
                    end_params = per_track_params[track_index][next_index]
                    rendered[f0:f1] += panner.process_block(
                        block[f0:f1], start_params, end_params
                    )
                mix += rendered[prefix:]
            blocks.append(mix)
            np.save(
                os.path.join(snapshot_dir, f"block_{block_index:06d}.npy"),
                mix,
            )
            snapshot = {
                "version": 1,
                "completed_blocks": block_index + 1,
                "total_blocks": total_blocks,
                "manifest_digest": manifest["digest"],
                "param_digest": _param_digest(param_payload),
                "manifest_summary": {
                    "sample_rate": sr,
                    "duration_s": manifest["duration_s"],
                    "track_files": [t["file"] for t in tracks],
                    "angles": angles,
                    "strong_section_s": list(strong_section_s),
                },
            }
            _write_json(os.path.join(snapshot_dir, SNAPSHOT_NAME), snapshot)
            if block_sleep_s:
                import time

                time.sleep(block_sleep_s)
            if interruption.requested:
                interrupted_after = block_index + 1
                break
    finally:
        signal.signal(signal.SIGINT, previous_handler)

    recovery_path = None
    if interrupted_after is not None:
        recovery_path = os.path.join(snapshot_dir, RECOVERY_NAME)
        _write_json(
            recovery_path,
            {
                "status": "INTERRUPTED_SNAPSHOT_COMMITTED",
                "completed_blocks": interrupted_after,
                "total_blocks": total_blocks,
                "resume": "re-run the same command; rendering resumes from snapshot",
            },
        )
        return (
            np.concatenate(blocks, axis=0) if blocks else np.zeros((0, 2)),
            {"interrupted": True, "completed_blocks": interrupted_after},
            stability_rows,
            azimuth_rows,
            recovery_path,
        )

    spatial_mix = np.concatenate(blocks, axis=0)
    return spatial_mix, {"interrupted": False}, stability_rows, azimuth_rows, None


def _array_digest(arrays: list[np.ndarray]) -> list[str]:
    import hashlib

    return [
        hashlib.sha256(np.ascontiguousarray(a).tobytes()).hexdigest() for a in arrays
    ]


def render_project(manifest_path: str, output_dir: str,
                   block_sleep_s: float = 0.0) -> dict:
    """Full pipeline entry point. Returns the structured report dictionary."""
    base_dir = os.path.dirname(os.path.abspath(manifest_path))
    raw_manifest = load_manifest(manifest_path)
    manifest = validate_manifest(raw_manifest, base_dir)
    os.makedirs(output_dir, exist_ok=True)
    snapshot_dir = os.path.join(output_dir, "snapshot")

    _ensure_stems(manifest, base_dir)
    # Re-validate synthesized files so rate/depth/channel guarantees are enforced.
    validate_manifest(raw_manifest, base_dir)

    sr = manifest["sample_rate"]
    master_samples = int(round(manifest["duration_s"] * sr))
    warnings: list[dict] = []
    aligned, alignment_records, detection_warnings = align_tracks(
        manifest["tracks"], sr, base_dir, manifest["lip_points"],
        master_samples, warnings,
    )

    strong_section = tuple(raw_manifest.get(
        "strong_section_s", [1.2, 3.6]
    ))
    corrected, pair_reports, phase_summary = phase_correct_groups(
        manifest["tracks"], aligned, sr, strong_section, alignment_records
    )
    transient_displacement = _transient_displacement_check(
        aligned, corrected, sr, manifest["lip_points"], manifest["tracks"]
    )

    spatial_mix, interrupt_info, stability_rows, azimuth_rows, recovery_path = (
        render_spatial_blocks(
            corrected,
            manifest["tracks"],
            manifest,
            snapshot_dir,
            strong_section,
            block_sleep_s=block_sleep_s,
        )
    )

    if interrupt_info.get("interrupted"):
        return {
            "status": "INTERRUPTED",
            "warnings": warnings,
            "recovery_report": recovery_path,
            "interrupt": interrupt_info,
        }

    pre_loudness = integrated_loudness(spatial_mix, sr)
    pre_tp = true_peak_db(spatial_mix, sr)
    master, compliance = comply(
        spatial_mix, sr, manifest["target_lufs"], manifest["true_peak_db"]
    )
    master = master[:master_samples]

    master_path = os.path.join(output_dir, "master.wav")
    sf.write(master_path, master, sr, subtype="PCM_16")

    report = {
        "status": "OK",
        "manifest_digest": manifest["digest"],
        "sample_rate": sr,
        "master_samples": int(master.shape[0]),
        "master_path": master_path,
        "warnings": warnings,
        "alignment": alignment_records,
        "phase_pairs": pair_reports,
        "phase_summary": phase_summary,
        "transient_displacement": transient_displacement,
        "stability": [
            {"file": track["file"], **row}
            for track, row in zip(manifest["tracks"], stability_rows)
        ],
        "azimuth_accuracy": [
            {"file": track["file"], "frames": rows}
            for track, rows in zip(manifest["tracks"], azimuth_rows)
        ],
        "loudness": {
            "integrated_loudness_before_limiter_db": pre_loudness,
            "true_peak_before_limiter_db": pre_tp,
            **compliance,
        },
    }
    report_path = os.path.join(output_dir, "report.json")
    _write_json(report_path, report)
    return report


def _transient_displacement_check(before: list[np.ndarray],
                                  after: list[np.ndarray], sr: int,
                                  lip_points: list[float],
                                  tracks: list[dict]) -> list[dict]:
    from .alignment import detect_onset

    rows = []
    for index, (track, b, a) in enumerate(zip(tracks, before, after)):
        ref_s = lip_points[index % len(lip_points)]
        onset_before = detect_onset(b, sr, ref_s)
        onset_after = detect_onset(a, sr, ref_s)
        if onset_before is None or onset_after is None:
            displacement = 0
        else:
            displacement = abs(onset_after - onset_before)
        rows.append(
            {
                "file": track["file"],
                "onset_before_phase_correction": onset_before,
                "onset_after_phase_correction": onset_after,
                "displacement_samples": int(displacement),
                "within_one_sample": bool(displacement <= 1),
            }
        )
    return rows
