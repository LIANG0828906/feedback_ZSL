from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .dsp import detect_onset, shift_audio, stable_float
from .errors import warning
from .manifest import Manifest


@dataclass
class AlignedStem:
    role: str
    name: str
    group: str
    audio: np.ndarray
    reference: np.ndarray
    detected: bool
    detected_sample: int | None
    reference_sample: int
    shift_samples: int
    aligned_onset_sample: int | None
    aligned_onset_seconds: float | None
    alignment_error_samples: int | None
    aligned_frames: int


@dataclass
class AlignmentResult:
    stems: dict[str, AlignedStem]
    warnings: list[dict[str, Any]]
    master_frames: int
    params: dict[str, Any]

    def report(self) -> dict[str, Any]:
        tracks = []
        for stem in self.stems.values():
            tracks.append(
                {
                    "channel": stem.name,
                    "group": stem.group,
                    "detected": stem.detected,
                    "detected_onset_sample": stem.detected_sample,
                    "detected_onset_seconds": (
                        stable_float(stem.detected_sample / max(1, self.params["sample_rate"]))
                        if stem.detected_sample is not None
                        else None
                    ),
                    "reference_sample": stem.reference_sample,
                    "shift_samples": stem.shift_samples,
                    "offset_samples": stem.shift_samples,
                    "aligned_onset_sample": stem.aligned_onset_sample,
                    "aligned_onset_seconds": (
                        stable_float(stem.aligned_onset_seconds)
                        if stem.aligned_onset_seconds is not None
                        else None
                    ),
                    "alignment_error_samples": stem.alignment_error_samples,
                    "aligned_frames": stem.aligned_frames,
                    "aligned_duration_seconds": stable_float(
                        stem.aligned_frames / max(1, self.params["sample_rate"])
                    ),
                }
            )
        return {
            "tracks": tracks,
            "master_frames": self.master_frames,
            "warnings": self.warnings,
        }


def align_stems(
    manifest: Manifest,
    stems: dict[str, np.ndarray],
    references: dict[str, np.ndarray],
) -> AlignmentResult:
    aligned: dict[str, AlignedStem] = {}
    warnings: list[dict[str, Any]] = []
    params_tracks: list[dict[str, Any]] = []
    for track in manifest.tracks:
        source = np.asarray(stems[track.role], dtype=np.float64)
        reference = np.asarray(references[track.role], dtype=np.float64)
        detected_sample = detect_onset(source, manifest.sample_rate)
        reference_sample = int(round(track.onset_reference_seconds * manifest.sample_rate))
        if detected_sample is None:
            shift = 0
            detected = False
            warning_item = warning(
                "DETECTION_FAILED",
                "起音瞬态检测失败，该分轨保持原始位置且不参与对齐",
                channel=track.name,
                group=track.group,
            )
            warnings.append(warning_item)
        else:
            shift = reference_sample - detected_sample
            detected = True
        shifted_audio = shift_audio(source, shift)
        shifted_reference = shift_audio(reference, shift)
        if len(shifted_audio) == 0:
            warning_item = warning(
                "DETECTION_FAILED",
                "对齐平移后分轨为空，该分轨保持原始位置",
                channel=track.name,
                group=track.group,
            )
            warnings.append(warning_item)
            shift = 0
            detected = False
            shifted_audio = source
            shifted_reference = reference
        final_onset = detect_onset(shifted_audio, manifest.sample_rate)
        if detected and final_onset is None:
            warnings.append(
                warning(
                    "DETECTION_FAILED",
                    "平移后无法复核起音位置",
                    channel=track.name,
                    group=track.group,
                )
            )
        alignment_error = (
            abs(int(final_onset) - reference_sample) if final_onset is not None else None
        )
        if alignment_error is not None and alignment_error > 1:
            warnings.append(
                warning(
                    "ALIGNMENT_TOLERANCE_EXCEEDED",
                    "对齐后起音误差超过 1 个采样",
                    channel=track.name,
                    group=track.group,
                    alignment_error_samples=alignment_error,
                )
            )
        aligned[track.role] = AlignedStem(
            role=track.role,
            name=track.name,
            group=track.group,
            audio=shifted_audio,
            reference=shifted_reference,
            detected=detected,
            detected_sample=detected_sample,
            reference_sample=reference_sample,
            shift_samples=shift,
            aligned_onset_sample=final_onset,
            aligned_onset_seconds=(
                float(final_onset) / manifest.sample_rate if final_onset is not None else None
            ),
            alignment_error_samples=alignment_error,
            aligned_frames=len(shifted_audio),
        )
        params_tracks.append(
            {
                "role": track.role,
                "channel": track.name,
                "group": track.group,
                "detected": detected,
                "detected_sample": detected_sample,
                "reference_sample": reference_sample,
                "shift_samples": shift,
                "aligned_frames": len(shifted_audio),
            }
        )
    master_frames = max((len(stem.audio) for stem in aligned.values()), default=0)
    params = {
        "sample_rate": manifest.sample_rate,
        "master_frames": master_frames,
        "tracks": params_tracks,
    }
    return AlignmentResult(stems=aligned, warnings=warnings, master_frames=master_frames, params=params)


def realign_from_params(
    manifest: Manifest,
    stems: dict[str, np.ndarray],
    references: dict[str, np.ndarray],
    params: dict[str, Any],
) -> AlignmentResult:
    by_role = {item["role"]: item for item in params.get("tracks", [])}
    if set(by_role) != {track.role for track in manifest.tracks}:
        raise ValueError("快照中的对齐通道集合与清单不一致")
    aligned: dict[str, AlignedStem] = {}
    warnings: list[dict[str, Any]] = []
    for track in manifest.tracks:
        item = by_role[track.role]
        shift = int(item["shift_samples"])
        detected = bool(item["detected"])
        audio = shift_audio(stems[track.role], shift)
        reference = shift_audio(references[track.role], shift)
        final_onset = detect_onset(audio, manifest.sample_rate)
        error = abs(final_onset - int(item["reference_sample"])) if final_onset is not None else None
        if not detected:
            warnings.append(
                warning(
                    "DETECTION_FAILED",
                    "起音瞬态检测失败，该分轨保持原始位置且不参与对齐",
                    channel=track.name,
                    group=track.group,
                )
            )
        aligned[track.role] = AlignedStem(
            role=track.role,
            name=track.name,
            group=track.group,
            audio=audio,
            reference=reference,
            detected=detected,
            detected_sample=item.get("detected_sample"),
            reference_sample=int(item["reference_sample"]),
            shift_samples=shift,
            aligned_onset_sample=final_onset,
            aligned_onset_seconds=(
                float(final_onset) / manifest.sample_rate if final_onset is not None else None
            ),
            alignment_error_samples=error,
            aligned_frames=len(audio),
        )
    master_frames = max((len(stem.audio) for stem in aligned.values()), default=0)
    return AlignmentResult(stems=aligned, warnings=warnings, master_frames=master_frames, params=params)

