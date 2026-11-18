from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from scipy import signal

from .generate import bandpass_deterministic, lowpass_deterministic
from .project import Project, TrackSpec


@dataclass
class LeakageEstimate:
    channel: str
    delay_samples: int
    delay_ms: float
    gain_linear: float
    gain_db: float
    pause_seconds: float
    energy_before_db: float
    energy_after_db: float
    reduction_db: float
    direct_peak_loss_db: float
    suppressed: np.ndarray

    def report(self) -> dict[str, object]:
        return {
            "channel": self.channel,
            "delay_samples": int(self.delay_samples),
            "delay_ms": round(float(self.delay_ms), 9),
            "gain_linear": float(self.gain_linear),
            "gain_db": round(float(self.gain_db), 9),
            "pause_seconds": round(float(self.pause_seconds), 6),
            "leak_energy_before_dbfs": round(float(self.energy_before_db), 6),
            "leak_energy_after_dbfs": round(float(self.energy_after_db), 6),
            "reduction_db": round(float(self.reduction_db), 6),
            "direct_peak_loss_db": round(float(self.direct_peak_loss_db), 6),
        }


def _frame_rms(x: np.ndarray, frame: int, hop: int) -> np.ndarray:
    if x.size == 0:
        return np.zeros(0, dtype=np.float64)
    count = 1 + max(0, (x.size - frame) // hop)
    if count <= 0:
        return np.asarray([np.sqrt(np.mean(x * x) + 1.0e-30)])
    result = np.empty(count, dtype=np.float64)
    for index in range(count):
        segment = x[index * hop : index * hop + frame]
        result[index] = np.sqrt(np.mean(segment * segment) + 1.0e-30)
    return result


def _pause_mask(x: np.ndarray, sample_rate: int) -> tuple[np.ndarray, float]:
    frame = max(64, int(round(0.020 * sample_rate)))
    hop = max(32, int(round(0.010 * sample_rate)))
    highpassed = bandpass_deterministic(
        x, sample_rate, 150.0, min(8000.0, sample_rate * 0.45), order=3
    )
    rms = _frame_rms(highpassed, frame, hop)
    if rms.size == 0:
        return np.ones(x.size, dtype=bool), 0.0
    peak = float(np.max(rms))
    if peak <= 1.0e-10:
        return np.ones(x.size, dtype=bool), x.size / sample_rate
    threshold = max(float(np.percentile(rms, 12)) * 8.0, peak * 10.0 ** (-32.0 / 20.0))
    active_frames = rms > threshold
    expanded = active_frames.copy()
    for shift in (1, 2, 3):
        if shift < expanded.size:
            expanded[shift:] |= active_frames[:-shift]
            expanded[:-shift] |= active_frames[shift:]
    mask = np.ones(x.size, dtype=bool)
    for index, is_active in enumerate(expanded):
        if is_active:
            start = max(0, index * hop - hop // 2)
            end = min(x.size, index * hop + frame + hop // 2)
            mask[start:end] = False
    minimum_hole = int(0.060 * sample_rate)
    if minimum_hole > 1 and mask.any():
        padded = np.concatenate(([False], mask, [False]))
        changes = np.flatnonzero(padded[1:] != padded[:-1])
        cleaned = np.zeros_like(mask)
        for start, end in zip(changes[0::2], changes[1::2]):
            if end - start >= minimum_hole:
                cleaned[start:end] = True
        mask = cleaned
    return mask, float(np.count_nonzero(mask)) / sample_rate


def _shift_right(x: np.ndarray, delay: int) -> np.ndarray:
    result = np.zeros_like(x)
    if delay <= 0:
        result[:] = x
    elif delay < x.size:
        result[delay:] = x[: x.size - delay]
    return result


def _band_energy_db(x: np.ndarray, mask: np.ndarray, sample_rate: int, cutoff: float) -> float:
    if not np.any(mask):
        return -200.0
    filtered = lowpass_deterministic(x, sample_rate, cutoff)
    energy = float(np.mean(filtered[mask] ** 2) + 1.0e-30)
    return 10.0 * np.log10(energy)


def estimate_and_suppress_actor(
    track: TrackSpec,
    audio: np.ndarray,
    reference: np.ndarray,
    project: Project,
) -> LeakageEstimate:
    sample_rate = project.sample_rate
    cutoff = project.leakage_lowpass_hz
    max_delay = int(round(project.leakage_max_delay_ms * sample_rate / 1000.0))
    pause_mask, pause_seconds = _pause_mask(audio, sample_rate)
    reference_band = lowpass_deterministic(reference, sample_rate, cutoff)
    actor_band = lowpass_deterministic(audio, sample_rate, cutoff)

    delay = 0
    gain = 0.0
    if pause_seconds >= 0.08 and max_delay > 0:
        masked_actor = np.where(pause_mask, actor_band, 0.0)
        correlation = signal.correlate(
            masked_actor, reference_band, mode="full", method="fft"
        )
        lags = signal.correlation_lags(
            masked_actor.size, reference_band.size, mode="full"
        )
        window = np.flatnonzero(np.abs(lags) <= max_delay)
        if window.size:
            best = int(window[np.argmax(np.abs(correlation[window]))])
            delay = int(lags[best])
        delayed_reference = _shift_right(reference_band, delay)
        denominator = float(
            np.dot(delayed_reference[pause_mask], delayed_reference[pause_mask])
        )
        if denominator > 1.0e-20:
            gain = float(
                np.dot(actor_band[pause_mask], delayed_reference[pause_mask])
                / denominator
            )
            if abs(gain) < 1.0e-6:
                gain = 0.0
                delay = 0
    delay = int(np.clip(delay, -max_delay, max_delay))
    reference_component = gain * _shift_right(reference_band, delay)
    suppressed = audio - reference_component

    energy_before = _band_energy_db(audio, pause_mask, sample_rate, cutoff)
    energy_after = _band_energy_db(suppressed, pause_mask, sample_rate, cutoff)
    reduction = max(0.0, energy_before - energy_after)
    active_mask = ~pause_mask
    if np.any(active_mask):
        raw_direct = bandpass_deterministic(
            audio, sample_rate, 150.0, min(9000.0, sample_rate * 0.45)
        )
        clean_direct = bandpass_deterministic(
            suppressed, sample_rate, 150.0, min(9000.0, sample_rate * 0.45)
        )
        raw_peak = float(np.max(np.abs(raw_direct[active_mask])) + 1.0e-30)
        clean_peak = float(np.max(np.abs(clean_direct[active_mask])) + 1.0e-30)
        direct_peak_loss = max(0.0, 20.0 * np.log10(raw_peak / clean_peak))
    else:
        direct_peak_loss = 0.0

    return LeakageEstimate(
        channel=track.name,
        delay_samples=delay,
        delay_ms=delay * 1000.0 / sample_rate,
        gain_linear=gain,
        gain_db=20.0 * np.log10(abs(gain)) if abs(gain) > 1.0e-15 else -200.0,
        pause_seconds=pause_seconds,
        energy_before_db=energy_before,
        energy_after_db=energy_after,
        reduction_db=reduction,
        direct_peak_loss_db=direct_peak_loss,
        suppressed=suppressed,
    )


def suppress_all_actors(
    project: Project,
    tracks: dict[str, np.ndarray],
) -> dict[str, LeakageEstimate]:
    reference = tracks[project.reference_channel]
    estimates: dict[str, LeakageEstimate] = {}
    for track in project.tracks:
        if track.source_type != "actor":
            continue
        estimates[track.name] = estimate_and_suppress_actor(
            track, tracks[track.name], reference, project
        )
    return estimates

