from __future__ import annotations

import math

import numpy as np
from scipy.signal import get_window

from .bands import Band


def frame_starts(length: int, frame_size: int, overlap: float) -> list[int]:
    hop = max(1, int(round(frame_size * (1.0 - overlap))))
    if length <= frame_size:
        return [0]
    starts = list(range(0, length - frame_size + 1, hop))
    final = length - frame_size
    if starts[-1] != final:
        starts.append(final)
    return starts


def _window(name: str, length: int) -> np.ndarray:
    if name == "rectangular":
        return np.ones(length, dtype=np.float64)
    return get_window(name, length, fftbins=True).astype(np.float64)


def analyse_channel(
    aligned: np.ndarray,
    sample_rate: int,
    quantization_step: float,
    bands: tuple[Band, ...],
    frame_size: int,
    overlap: float,
    window_name: str,
    segmentation_safety: float,
) -> dict[str, object]:
    n = int(frame_size)
    starts = frame_starts(aligned.size, n, overlap)
    window = _window(window_name, n)
    coherent_gain = float(np.mean(window))
    window_energy = float(np.sum(window * window))
    if coherent_gain <= 0.0:
        raise ValueError("window has non-positive coherent gain")
    enbw_bins = n * window_energy / (np.sum(window) ** 2)
    frequencies = np.fft.rfftfreq(n, d=1.0 / sample_rate)
    bins = np.arange(frequencies.size, dtype=np.float64)
    segments = len(starts)

    complex_sum = np.zeros(frequencies.size, dtype=np.complex128)
    frame_abs_spectra = np.zeros((segments, frequencies.size), dtype=np.float64)
    for frame_index, start in enumerate(starts):
        chunk = np.zeros(n, dtype=np.float64)
        available = min(n, aligned.size - start)
        chunk[:available] = aligned[start : start + available]
        spectrum = np.fft.rfft(chunk * window, n=n)
        shifted = spectrum * np.exp(2j * np.pi * bins * start / n)
        complex_sum += shifted
        frame_abs_spectra[frame_index] = (
            2.0 * np.abs(shifted) / (n * coherent_gain)
        )

    mean_complex = complex_sum / segments
    amplitude = np.abs(mean_complex)
    frame_power_spectra = frame_abs_spectra * frame_abs_spectra
    mean_frame_power = np.mean(frame_power_spectra, axis=0)

    q_bin_amplitude = (
        2.0
        * (quantization_step / math.sqrt(12.0))
        * math.sqrt(window_energy)
        / (n * coherent_gain * math.sqrt(segments))
    )

    power: list[float] = []
    power_db: list[float] = []
    phase_rad: list[float] = []
    phase_deg: list[float] = []
    quant_bound: list[float] = []
    segment_bound: list[float] = []
    segment_variance: list[float] = []
    frame_count: list[int] = []
    bin_weight_l2: list[float] = []
    for band in bands:
        indices, weights = band_weights_for_band(frequencies, band)
        band_power = float(np.dot(weights, mean_frame_power[indices]))
        band_power = max(band_power, np.finfo(np.float64).tiny)
        frame_band_powers = frame_power_spectra[:, indices] @ weights
        if segments > 1:
            variance = float(np.var(frame_band_powers, ddof=1))
            seg_bound = segmentation_safety * math.sqrt(variance / segments)
        else:
            variance = 0.0
            seg_bound = 0.0
        weight_l2 = float(math.sqrt(np.sum(weights * weights)))
        band_amp = math.sqrt(band_power)
        q_amp = q_bin_amplitude * weight_l2
        q_bound = 2.0 * band_amp * q_amp
        z = np.dot(weights, mean_complex[indices])
        phase = float(np.angle(z)) if abs(z) > 0.0 else 0.0

        power.append(band_power)
        power_db.append(10.0 * math.log10(band_power))
        phase_rad.append(phase)
        phase_deg.append(math.degrees(phase))
        quant_bound.append(float(q_bound))
        segment_bound.append(float(seg_bound))
        segment_variance.append(variance)
        frame_count.append(segments)
        bin_weight_l2.append(weight_l2)

    return {
        "power": np.asarray(power, dtype=np.float64),
        "power_db": np.asarray(power_db, dtype=np.float64),
        "phase_rad": np.asarray(phase_rad, dtype=np.float64),
        "phase_deg": np.asarray(phase_deg, dtype=np.float64),
        "quantization_power_bound": np.asarray(quant_bound, dtype=np.float64),
        "segmentation_power_bound": np.asarray(segment_bound, dtype=np.float64),
        "segment_variance": np.asarray(segment_variance, dtype=np.float64),
        "segment_count": segments,
        "frame_starts": np.asarray(starts, dtype=np.int64),
        "coherent_gain": coherent_gain,
        "window_energy": window_energy,
        "enbw_bins": float(enbw_bins),
        "quantization_step": float(quantization_step),
        "segmentation_safety": float(segmentation_safety),
        "bin_weight_l2": np.asarray(bin_weight_l2, dtype=np.float64),
    }


def band_weights_for_band(
    frequencies_hz: np.ndarray, band: Band
) -> tuple[np.ndarray, np.ndarray]:
    mask = (frequencies_hz >= band.low_hz) & (frequencies_hz < band.high_hz)
    indices = np.flatnonzero(mask)
    if indices.size == 0:
        nearest = int(np.argmin(np.abs(frequencies_hz - band.center_hz)))
        indices = np.asarray([nearest], dtype=np.int64)
    weights = np.full(indices.size, 1.0 / indices.size, dtype=np.float64)
    return indices, weights

