"""Shared deterministic DSP helpers (STFT, band energies, dBFS)."""

from __future__ import annotations

import numpy as np


def dbfs(power: float, floor: float = 1e-20) -> float:
    """RMS-equivalent level in dBFS for a mean-square power value."""
    return float(10.0 * np.log10(max(power, floor)))


def band_mask(fft_freqs: np.ndarray, band_hz: tuple[float, float]) -> np.ndarray:
    low, high = band_hz
    return (fft_freqs >= low) & (fft_freqs <= high)


def stft(signal: np.ndarray, n_fft: int = 4096, hop: int = 1024,
         sr: int | None = None):
    """Hann-windowed STFT; returns (frames, bins), freqs, window, hop."""
    window = np.hanning(n_fft)
    if len(signal) < n_fft:
        padded = np.zeros(n_fft)
        padded[:len(signal)] = signal
        signal = padded
    n_frames = 1 + (len(signal) - n_fft) // hop
    frames = np.lib.stride_tricks.sliding_window_view(
        signal, n_fft)[::hop][:n_frames]
    spectrum = np.fft.rfft(frames * window[None, :], n=n_fft, axis=1)
    freqs = np.fft.rfftfreq(n_fft, d=1.0 if sr is None else 1.0 / sr)
    return np.abs(spectrum) ** 2, freqs, window, hop


def frame_time_centers(n_fft: int, hop: int, sr: int,
                       n_frames: int) -> np.ndarray:
    return (np.arange(n_frames) * hop + n_fft / 2.0) / sr


def band_power_series(power_spec: np.ndarray, freqs: np.ndarray,
                      band_hz: tuple[float, float]) -> np.ndarray:
    mask = band_mask(freqs, band_hz)
    return power_spec[:, mask].sum(axis=1)


def weighted_center_frequency(power_spec: np.ndarray, freqs: np.ndarray,
                              band_hz: tuple[float, float],
                              frame_selector: np.ndarray) -> float:
    mask = band_mask(freqs, band_hz)
    selected = power_spec[frame_selector][:, mask]
    weights = selected.sum(axis=0)
    band_freqs = freqs[mask]
    return float(np.sum(band_freqs * weights) / max(np.sum(weights), 1e-20))


def segment_band_energy(signal: np.ndarray, start: int, end: int,
                        band_hz: tuple[float, float], sr: int,
                        n_fft: int = 4096, hop: int = 1024) -> float:
    """Physical mean-square power inside a frequency band.

    One-sided Hann periodograms are scaled by 2/(N*sum(w^2)) so that a unit
    variance white noise yields a full-band estimate of ~1; the returned
    value therefore sits on the same scale as ``mean(signal**2)`` (dBFS).
    """
    start = max(0, start)
    end = min(len(signal), end)
    if end - start < n_fft:
        return 0.0
    power_spec, freqs, _, _ = stft(signal[start:end], n_fft, hop, sr)
    mask = band_mask(freqs, band_hz)
    frame_power = power_spec[:, mask].sum(axis=1)
    window = np.hanning(n_fft)
    scale = 2.0 / (n_fft * np.sum(window ** 2))
    return float(scale * np.mean(frame_power))


def reference_band_floor(signal: np.ndarray,
                         blocked_intervals: list,
                         band_hz: tuple[float, float], sr: int,
                         n_fft: int = 4096, hop: int = 1024) -> float:
    """Median in-band power from frames outside every action interval."""
    power_spec, freqs, _, _ = stft(signal, n_fft, hop, sr)
    series = band_power_series(power_spec, freqs, band_hz)
    times = frame_time_centers(n_fft, hop, sr, len(series))
    window = np.hanning(n_fft)
    scale = 2.0 / (n_fft * np.sum(window ** 2))
    series = series * scale
    blocked = np.zeros(len(times), dtype=bool)
    for start_sample, end_sample in blocked_intervals:
        blocked |= (times >= start_sample / sr) & (times <= end_sample / sr)
    if np.all(blocked):
        return float(np.median(series))
    return float(np.median(series[~blocked]))
