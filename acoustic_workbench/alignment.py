from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import E_ALIGN_FAIL, E_WAV_INVALID, fail
from .geometry import distance
from .model import Scene


@dataclass
class AlignmentResult:
    aligned: np.ndarray
    direct_indices: dict[str, int]
    aligned_direct_indices: dict[str, int]
    shifts: dict[str, int]
    gains: dict[str, float]
    reference_index: int
    reference_peak: float
    geometry_gain: float
    direct_distance_m: float

    def metadata(self) -> dict[str, Any]:
        return {
            "direct_indices": self.direct_indices,
            "aligned_direct_indices": self.aligned_direct_indices,
            "shifts_samples": self.shifts,
            "gains": self.gains,
            "reference_index": self.reference_index,
            "reference_peak": self.reference_peak,
            "geometry_gain": self.geometry_gain,
            "direct_distance_m": self.direct_distance_m,
            "truth_source": "raw_wav_sample_index",
        }


def first_threshold_sample(channel: np.ndarray, threshold: float) -> int | None:
    candidates = np.flatnonzero(np.abs(channel) > threshold)
    if candidates.size == 0:
        return None
    return int(candidates[0])


def _shift_channel(channel: np.ndarray, shift: int) -> np.ndarray:
    output = np.zeros_like(channel)
    if shift == 0:
        output[:] = channel
    elif shift > 0:
        output[shift:] = channel[: channel.size - shift]
    else:
        output[:shift] = channel[-shift:]
    return output


def align_impulse_responses(
    samples: np.ndarray,
    scene: Scene,
    listener_id: str,
) -> AlignmentResult:
    if samples.ndim != 2 or samples.shape[0] != len(scene.channel_layout):
        fail(
            E_WAV_INVALID,
            "impulse response channel count does not match 5.1 layout",
            shape=list(samples.shape),
            layout=scene.channel_layout,
        )
    if samples.shape[1] < 2:
        fail(E_ALIGN_FAIL, "impulse response is too short for alignment")
    threshold = float(scene.alignment.get("threshold", 0.02))
    max_shift = int(scene.alignment.get("max_shift_samples", 8192))
    if threshold <= 0.0 or not math.isfinite(threshold):
        fail(E_ALIGN_FAIL, "alignment threshold must be positive and finite")

    layout = scene.channel_layout
    reference_channel = scene.reference_channel
    reference_position = layout.index(reference_channel)
    direct_indices: dict[str, int] = {}
    peaks: dict[str, float] = {}
    missing: list[str] = []
    for index, channel_name in enumerate(layout):
        detected = first_threshold_sample(samples[index], threshold)
        if detected is None:
            missing.append(channel_name)
            continue
        direct_indices[channel_name] = detected
        peaks[channel_name] = float(abs(samples[index, detected]))
    if missing:
        fail(
            E_ALIGN_FAIL,
            "direct sound was not detected in every channel",
            listener_id=listener_id,
            missing_channels=missing,
            threshold=threshold,
        )
    if reference_channel not in direct_indices:
        fail(
            E_ALIGN_FAIL,
            "reference channel direct sound was not detected",
            listener_id=listener_id,
            reference_channel=reference_channel,
        )

    reference_index = direct_indices[reference_channel]
    reference_peak = peaks[reference_channel]
    if reference_peak <= 0.0:
        fail(E_ALIGN_FAIL, "reference direct-sound peak is zero")

    listener = scene.listener(listener_id)
    direct_distance = max(0.05, distance(scene.source, listener))
    propagation_gain = 1.0 / (1.0 + 0.025 * direct_distance)
    shifts: dict[str, int] = {}
    gains: dict[str, float] = {}
    aligned = np.zeros_like(samples, dtype=np.float64)
    aligned_indices: dict[str, int] = {}
    for index, channel_name in enumerate(layout):
        shift = reference_index - direct_indices[channel_name]
        if abs(shift) > max_shift:
            fail(
                E_ALIGN_FAIL,
                "required alignment shift exceeds configured limit",
                listener_id=listener_id,
                channel=channel_name,
                shift_samples=shift,
                max_shift_samples=max_shift,
            )
        gain = reference_peak / peaks[channel_name]
        if not math.isfinite(gain) or gain <= 0.0:
            fail(
                E_ALIGN_FAIL,
                "invalid gain normalization factor",
                listener_id=listener_id,
                channel=channel_name,
            )
        shifts[channel_name] = shift
        gains[channel_name] = gain
        aligned[index] = _shift_channel(samples[index] * gain, shift)
        aligned_indices[channel_name] = direct_indices[channel_name] + shift

    aligned *= propagation_gain
    if not np.all(np.isfinite(aligned)):
        fail(E_ALIGN_FAIL, "aligned impulse response contains non-finite samples")

    peak = float(np.max(np.abs(aligned)))
    if peak > 0.98:
        aligned *= 0.98 / peak

    return AlignmentResult(
        aligned=np.ascontiguousarray(aligned),
        direct_indices=direct_indices,
        aligned_direct_indices=aligned_indices,
        shifts=shifts,
        gains=gains,
        reference_index=reference_index,
        reference_peak=reference_peak,
        geometry_gain=propagation_gain,
        direct_distance_m=direct_distance,
    )

