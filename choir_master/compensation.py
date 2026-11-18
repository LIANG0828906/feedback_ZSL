from __future__ import annotations

from typing import Any

import numpy as np

from .detection import (
    PassaggioEvent,
    _energy_drop_for_bounds,
    _frame_features,
)
from .manifest import LoadedAudio
from .util import linear_to_db


def _raised_cosine_envelope(
    length: int,
    sample_rate: int,
    ramp_s: float = 0.040,
) -> np.ndarray:
    envelope = np.ones(length, dtype=np.float64)
    ramp = min(max(1, int(round(ramp_s * sample_rate))), max(1, length // 2))
    envelope[:ramp] = 0.5 - 0.5 * np.cos(np.linspace(0.0, np.pi, ramp, endpoint=False))
    envelope[length - ramp :] = (
        0.5
        + 0.5
        * np.cos(np.linspace(0.0, np.pi, ramp, endpoint=False))
    )
    return envelope


def _bandpass(signal: np.ndarray, sample_rate: int, band_hz: tuple[float, float]) -> np.ndarray:
    n = len(signal)
    if n == 0:
        return signal.copy()
    frequencies = np.fft.rfftfreq(n, 1.0 / sample_rate)
    low, high = band_hz
    mask = np.zeros_like(frequencies)
    inside = (frequencies >= low) & (frequencies <= high)
    mask[inside] = 1.0
    edge = min(80.0, max(20.0, (high - low) * 0.08))
    rising = (frequencies >= low) & (frequencies < low + edge)
    falling = (frequencies > high - edge) & (frequencies <= high)
    if np.any(rising):
        mask[rising] = 0.5 - 0.5 * np.cos(
            np.pi * (frequencies[rising] - low) / max(edge, 1e-12)
        )
    if np.any(falling):
        mask[falling] = 0.5 + 0.5 * np.cos(
            np.pi * (frequencies[falling] - (high - edge)) / max(edge, 1e-12)
        )
    return np.fft.irfft(np.fft.rfft(signal) * mask, n=n)


def _pair_features(
    left: np.ndarray,
    right: np.ndarray,
    loaded: LoadedAudio,
):
    return _frame_features(
        left,
        right,
        loaded.manifest.sample_rate,
        loaded.manifest.transition_band_hz,
    )


def _band_gain_for_interval(
    bandpass_tracks: list[np.ndarray],
    start: int,
    end: int,
    sample_rate: int,
) -> tuple[float, float, float, int, int]:
    guard = int(round(0.02 * sample_rate))
    side = int(round(0.30 * sample_rate))
    ramp = int(round(0.04 * sample_rate))
    core_start = min(end, start + ramp + guard)
    core_end = max(core_start, end - ramp - guard)
    baseline_pieces: list[np.ndarray] = []
    event_pieces: list[np.ndarray] = []
    for track in bandpass_tracks:
        before_start = max(0, start - side)
        before_end = max(before_start, start - guard)
        after_start = min(len(track), end + guard)
        after_end = min(len(track), end + side)
        if before_end > before_start:
            baseline_pieces.append(track[before_start:before_end])
        if after_end > after_start:
            baseline_pieces.append(track[after_start:after_end])
        if core_end > core_start:
            event_pieces.append(track[core_start:core_end])
    if not baseline_pieces or not event_pieces:
        return 1.0, 0.0, 0.0, start, start
    baseline = np.concatenate(baseline_pieces)
    event = np.concatenate(event_pieces)
    baseline_power = float(np.mean(baseline**2))
    event_power = float(np.mean(event**2))
    if event_power <= 1e-24 or baseline_power <= event_power:
        return 1.0, baseline_power, event_power, core_start, core_end
    gain = float(np.clip(np.sqrt(baseline_power / event_power), 1.0, 100.0))
    return gain, baseline_power, event_power, core_start, core_end


def apply_tenor_compensation(
    loaded: LoadedAudio,
    events: list[PassaggioEvent],
) -> tuple[dict[str, np.ndarray], dict[str, Any]]:
    manifest = loaded.manifest
    sample_rate = manifest.sample_rate
    tenor_ids = loaded.voice_track_ids("tenor")
    originals = [np.asarray(loaded.tracks[track_id], dtype=np.float64) for track_id in tenor_ids]
    length = max(len(track) for track in originals)
    if any(len(track) != length for track in originals):
        originals = [
            np.pad(track, (0, length - len(track))) if len(track) < length else track[:length]
            for track in originals
        ]
    compensated = [track.copy() for track in originals]
    bandpass_tracks = [
        _bandpass(track, sample_rate, manifest.transition_band_hz) for track in originals
    ]
    interval_reports: list[dict[str, Any]] = []
    overall_max_modification = 0.0

    for event_index, event in enumerate(events):
        start = max(0, int(round(event.start_s * sample_rate)))
        end = min(length, int(round(event.end_s * sample_rate)))
        if end <= start:
            continue
        current_bandpass = [track[start:end] for track in bandpass_tracks]
        gain, baseline_power, pre_event_power, core_start, core_end = (
            _band_gain_for_interval(bandpass_tracks, start, end, sample_rate)
        )
        envelope = _raised_cosine_envelope(end - start, sample_rate)
        pre_features = _pair_features(originals[0], originals[1], loaded)
        pre_drop = _energy_drop_for_bounds(pre_features, event.start_s, event.end_s)
        post_drop = pre_drop
        interval_max_modification = 0.0

        for _ in range(4):
            for track_index, track in enumerate(compensated):
                if gain > 1.0:
                    track[start:end] = originals[track_index][start:end] + (
                        (gain - 1.0) * envelope * current_bandpass[track_index]
                    )
            interval_max_modification = max(
                float(np.max(np.abs(compensated[track_index][start:end] - originals[track_index][start:end])))
                for track_index in range(2)
            )
            post_features = _pair_features(compensated[0], compensated[1], loaded)
            post_drop = _energy_drop_for_bounds(post_features, event.start_s, event.end_s)
            if post_drop <= 0.75:
                break
            gain *= 10.0 ** (post_drop / 20.0)

        updated_drop = post_drop
        post_event_power = pre_event_power
        if core_end > core_start:
            post_pieces = []
            for track_index in range(2):
                local_start = core_start - start
                local_end = core_end - start
                post_pieces.append(
                    (
                        1.0
                        + (gain - 1.0) * envelope[local_start:local_end]
                    )
                    * current_bandpass[track_index][local_start:local_end]
                )
            post_event_power = float(
                np.mean(np.concatenate(post_pieces) ** 2)
            )
        overall_max_modification = max(overall_max_modification, interval_max_modification)
        interval_reports.append(
            {
                "index": event_index,
                "voice": "tenor",
                "start_s": round(float(event.start_s), 9),
                "end_s": round(float(event.end_s), 9),
                "band_hz": list(manifest.transition_band_hz),
                "pre_band_drop_db": round(float(max(0.0, pre_drop)), 6),
                "post_band_drop_db": round(float(max(0.0, updated_drop)), 6),
                "improvement_db": round(float(max(0.0, pre_drop - updated_drop)), 6),
                "max_abs_modification": round(float(interval_max_modification), 12),
                "max_modification_dbfs": round(
                    linear_to_db(interval_max_modification), 6
                ),
                "correction_band_gain": round(float(gain), 9),
                "baseline_band_rms": round(float(np.sqrt(baseline_power)), 12),
                "pre_compensation_band_rms": round(
                    float(np.sqrt(pre_event_power)), 12
                ),
                "post_compensation_band_rms": round(float(np.sqrt(post_event_power)), 12),
            }
        )

    outside_mask = np.ones(length, dtype=bool)
    for event in events:
        start = max(0, int(round(event.start_s * sample_rate)))
        end = min(length, int(round(event.end_s * sample_rate)))
        outside_mask[start:end] = False
    outside_unchanged = all(
        np.array_equal(compensated[index][outside_mask], originals[index][outside_mask])
        for index in range(2)
    )
    min_improvement = min(
        (item["improvement_db"] for item in interval_reports), default=None
    )
    report = {
        "voice": "tenor",
        "band_hz": list(manifest.transition_band_hz),
        "intervals": interval_reports,
        "maximum_modification": round(float(overall_max_modification), 12),
        "maximum_modification_dbfs": round(
            linear_to_db(overall_max_modification), 6
        ),
        "outside_detected_intervals_bit_exact": bool(outside_unchanged),
        "minimum_improvement_db": (
            round(float(min_improvement), 6) if min_improvement is not None else None
        ),
        "required_improvement_db": 8.0,
        "improvement_requirement_met": bool(
            min_improvement is None or min_improvement >= 8.0
        ),
    }
    return {tenor_ids[0]: compensated[0], tenor_ids[1]: compensated[1]}, report

