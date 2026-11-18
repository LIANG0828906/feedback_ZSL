from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .manifest import LoadedAudio


@dataclass(frozen=True)
class PassaggioEvent:
    voice: str
    start_s: float
    end_s: float
    band_energy_drop_db: float

    def as_dict(self) -> dict[str, Any]:
        return {
            "voice": self.voice,
            "start_s": round(float(self.start_s), 9),
            "end_s": round(float(self.end_s), 9),
            "band_energy_drop_db": round(float(self.band_energy_drop_db), 6),
        }


@dataclass
class FrameFeatures:
    times_s: np.ndarray
    band_power: np.ndarray
    total_power: np.ndarray
    ratio: np.ndarray
    active: np.ndarray
    window_s: float
    hop_s: float


def _frame_features(
    left: np.ndarray,
    right: np.ndarray,
    sample_rate: int,
    band_hz: tuple[float, float],
    *,
    window_s: float = 0.040,
    hop_s: float = 0.005,
) -> FrameFeatures:
    length = min(len(left), len(right))
    window = max(32, int(round(window_s * sample_rate)))
    hop = max(1, int(round(hop_s * sample_rate)))
    if length < window:
        left = np.pad(left[:length], (0, window - length))
        right = np.pad(right[:length], (0, window - length))
        length = window
    frames_l = np.lib.stride_tricks.sliding_window_view(left[:length], window)[::hop]
    frames_r = np.lib.stride_tricks.sliding_window_view(right[:length], window)[::hop]
    taper = np.hanning(window).astype(np.float64)
    spectrum_l = np.fft.rfft(frames_l * taper, axis=1)
    spectrum_r = np.fft.rfft(frames_r * taper, axis=1)
    power = np.abs(spectrum_l) ** 2 + np.abs(spectrum_r) ** 2
    frequencies = np.fft.rfftfreq(window, 1.0 / sample_rate)
    band_mask = (frequencies >= band_hz[0]) & (frequencies <= band_hz[1])
    total_mask = (frequencies >= 50.0) & (frequencies <= min(8000.0, sample_rate / 2 - 1.0))
    band_power = np.sum(power[:, band_mask], axis=1)
    total_power = np.sum(power[:, total_mask], axis=1)
    ratio = band_power / np.maximum(total_power, 1e-24)
    max_total = float(np.max(total_power)) if total_power.size else 0.0
    active = total_power > max(max_total * 1.0e-4, 1e-16)
    times = (np.arange(len(frames_l), dtype=np.float64) * hop + window / 2.0) / sample_rate
    return FrameFeatures(
        times_s=times,
        band_power=band_power,
        total_power=total_power,
        ratio=ratio,
        active=active,
        window_s=window / sample_rate,
        hop_s=hop / sample_rate,
    )


def _smooth(values: np.ndarray, width: int = 3) -> np.ndarray:
    if values.size < 2 or width <= 1:
        return values
    kernel = np.ones(width, dtype=np.float64) / width
    padded = np.pad(values, (width // 2, width - 1 - width // 2), mode="edge")
    return np.convolve(padded, kernel, mode="valid")


def _rolling_max(values: np.ndarray, width: int) -> np.ndarray:
    width = max(1, min(width, len(values)))
    half = width // 2
    padded = np.pad(values, (half, width - 1 - half), mode="edge")
    windows = np.lib.stride_tricks.sliding_window_view(padded, width)
    return np.max(windows, axis=1)


def _runs_from_mask(
    mask: np.ndarray,
    *,
    max_gap_frames: int,
    min_duration_frames: int,
) -> list[tuple[int, int]]:
    indices = np.flatnonzero(mask)
    if indices.size == 0:
        return []
    runs: list[tuple[int, int]] = []
    start = int(indices[0])
    previous = int(indices[0])
    for index in indices[1:]:
        current = int(index)
        if current - previous - 1 > max_gap_frames:
            runs.append((start, previous))
            start = current
        previous = current
    runs.append((start, previous))
    return [
        (start, end)
        for start, end in runs
        if end - start + 1 >= min_duration_frames
    ]


def _energy_drop_for_bounds(
    features: FrameFeatures,
    start_s: float,
    end_s: float,
) -> float:
    inside = (features.times_s >= start_s) & (features.times_s <= end_s) & features.active
    before = (
        (features.times_s >= start_s - 0.32)
        & (features.times_s <= start_s - 0.04)
        & features.active
    )
    after = (
        (features.times_s >= end_s + 0.04)
        & (features.times_s <= end_s + 0.32)
        & features.active
    )
    side = np.concatenate([features.band_power[before], features.band_power[after]])
    if not np.any(inside) or side.size == 0:
        return 0.0
    baseline = float(np.median(side))
    event = float(np.median(features.band_power[inside]))
    return float(10.0 * np.log10(max(baseline, 1e-24) / max(event, 1e-24)))


def detect_passaggio_events(
    left: np.ndarray,
    right: np.ndarray,
    sample_rate: int,
    band_hz: tuple[float, float],
    *,
    voice: str = "tenor",
    threshold_drop_db: float = 7.0,
    max_gap_s: float = 0.055,
    min_duration_s: float = 0.10,
) -> list[PassaggioEvent]:
    features = _frame_features(left, right, sample_rate, band_hz)
    if not np.any(features.active):
        return []
    smoothed = _smooth(features.ratio, width=3)
    # A single global ratio is not valid across a changing note register: the
    # same voice can legitimately have a very different high-band ratio on E4
    # and A4. Compare each frame with the strongest nearby ratio instead. The
    # 1.8 s context survives a sub-second timbre drop but retains normal notes
    # on both sides of it.
    context_frames = max(3, int(round(1.8 / features.hop_s)))
    active_runs = _runs_from_mask(
        features.active,
        max_gap_frames=max(1, int(round(0.12 / features.hop_s))),
        min_duration_frames=max(1, int(round(0.08 / features.hop_s))),
    )
    candidate = np.zeros_like(features.active)
    for run_start, run_end in active_runs:
        segment = smoothed[run_start : run_end + 1]
        width = min(context_frames, len(segment))
        local_reference = _rolling_max(segment, width)
        threshold = local_reference * (10.0 ** (-threshold_drop_db / 20.0))
        candidate[run_start : run_end + 1] = segment < threshold
    runs = _runs_from_mask(
        candidate,
        max_gap_frames=max(1, int(round(max_gap_s / features.hop_s))),
        min_duration_frames=max(1, int(round(min_duration_s / features.hop_s))),
    )
    events: list[PassaggioEvent] = []
    for first, last in runs:
        # The analysis window is centered on each frame. Its first half can see
        # the incoming edge, so compensate that window bias at both boundaries.
        start_s = float(features.times_s[first] - features.window_s / 2.0)
        end_s = float(features.times_s[last] + features.window_s / 2.0)
        start_s = max(0.0, start_s)
        end_s = min(len(left) / sample_rate, end_s)
        if end_s - start_s < min_duration_s:
            continue
        drop_db = _energy_drop_for_bounds(features, start_s, end_s)
        events.append(
            PassaggioEvent(
                voice=voice,
                start_s=start_s,
                end_s=end_s,
                band_energy_drop_db=max(0.0, drop_db),
            )
        )
    return events


def detect_for_project(loaded: LoadedAudio) -> list[PassaggioEvent]:
    manifest = loaded.manifest
    tenor_ids = loaded.voice_track_ids("tenor")
    return detect_passaggio_events(
        loaded.tracks[tenor_ids[0]],
        loaded.tracks[tenor_ids[1]],
        manifest.sample_rate,
        manifest.transition_band_hz,
        voice="tenor",
    )


def false_positive_audit(loaded: LoadedAudio) -> dict[str, list[dict[str, Any]]]:
    manifest = loaded.manifest
    audit: dict[str, list[dict[str, Any]]] = {}
    for voice in manifest.voices:
        if voice.voice == "tenor":
            continue
        track_ids = [track.track_id for track in voice.tracks]
        events = detect_passaggio_events(
            loaded.tracks[track_ids[0]],
            loaded.tracks[track_ids[1]],
            manifest.sample_rate,
            manifest.transition_band_hz,
            voice=voice.voice,
        )
        audit[voice.voice] = [event.as_dict() for event in events]
    return audit

