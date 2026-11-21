"""Deterministic local FFT spectral and band-energy estimation."""

from __future__ import annotations

import math
from typing import Callable, Sequence

import numpy as np

from .models import BandEnergy, BandSpec, SpectralConfig, SpectralResult
from .wavio import WavData

_EPS = np.finfo(np.float64).eps


def make_band_label(center_hz: float) -> str:
    if center_hz >= 1000.0:
        return f"{center_hz / 1000.0:g}k"
    return f"{center_hz:g}"


def make_bands(config: SpectralConfig, sample_rate: int = 48000) -> tuple[BandSpec, ...]:
    exponent_step = 1.0 if config.band_mode == "octave" else 1.0 / 3.0
    fmax = min(float(config.fmax_hz), sample_rate / 2.0)
    first = int(math.ceil(math.log2(config.fmin_hz) / exponent_step - 1e-12))
    last = int(math.floor(math.log2(fmax) / exponent_step + 1e-12))
    bands: list[BandSpec] = []
    for index, exponent in enumerate(range(first, last + 1)):
        center = 2.0 ** (exponent * exponent_step)
        low = center * 2.0 ** (-exponent_step / 2.0)
        high = min(center * 2.0 ** (exponent_step / 2.0), sample_rate / 2.0)
        bands.append(
            BandSpec(
                index=index,
                center_hz=float(center),
                low_hz=float(low),
                high_hz=float(high),
                label=make_band_label(center),
            )
        )
    return tuple(bands)


def _window(name: str, size: int) -> np.ndarray:
    if name == "hann":
        return np.hanning(size)
    if name == "hamming":
        return np.hamming(size)
    if name == "blackman":
        return np.blackman(size)
    return np.ones(size, dtype=np.float64)


def _frames(samples: np.ndarray, frame_size: int, overlap: float) -> np.ndarray:
    if samples.size <= frame_size:
        padded = np.zeros(frame_size, dtype=np.float64)
        padded[: samples.size] = samples
        return padded.reshape(1, -1)
    step = max(1, int(round(frame_size * (1.0 - overlap))))
    starts = list(range(0, samples.size - frame_size + 1, step))
    if starts[-1] != samples.size - frame_size:
        starts.append(samples.size - frame_size)
    return np.vstack([samples[start : start + frame_size] for start in starts])


def estimate_channel_spectrum(
    aligned_samples: np.ndarray,
    sample_rate: int,
    bands: Sequence[BandSpec],
    config: SpectralConfig,
    channel_index: int = 0,
    checkpoint: Callable[[int], None] | None = None,
) -> SpectralResult:
    """Estimate band power and a direct, reusable relative upper bound.

    The bound combines window coherent-gain correction, the standard error of
    the per-segment band powers, and floating-point FFT rounding.
    """

    samples = np.asarray(aligned_samples, dtype=np.float64)
    frame_size = int(config.frame_size)
    frames = _frames(samples, frame_size, config.frame_overlap)
    window = _window(config.window, frame_size)
    coherent_gain = float(np.mean(window))
    if coherent_gain <= 0.0:
        coherent_gain = 1.0
    correction = 1.0 / (coherent_gain * coherent_gain)
    windowed = frames * window[np.newaxis, :]
    spectra = np.fft.rfft(windowed, axis=1)
    powers = np.abs(spectra) ** 2 * correction
    frequencies = np.fft.rfftfreq(frame_size, d=1.0 / sample_rate)

    # The window is corrected by its exact coherent gain. The residual bound is
    # the rounding of that normalisation, not the raw 1/cg correction itself.
    coherent_gain_rel = float(_EPS * math.sqrt(frame_size) / max(coherent_gain, _EPS))
    rounding_rel = float(_EPS * math.sqrt(frame_size) * 8.0)
    segment_energies = np.empty((powers.shape[0], len(bands)), dtype=np.float64)
    for band_index, band in enumerate(bands):
        if checkpoint is not None:
            checkpoint(band_index)
        selected = np.flatnonzero((frequencies >= band.low_hz) & (frequencies < band.high_hz))
        if selected.size == 0:
            nearest = int(np.argmin(np.abs(frequencies - band.center_hz)))
            selected = np.asarray([nearest], dtype=np.int64)
        segment_energies[:, band_index] = np.sum(powers[:, selected], axis=1)

    count = int(segment_energies.shape[0])
    mean_energy = np.mean(segment_energies, axis=0)
    if count > 1:
        standard_error = np.std(segment_energies, axis=0, ddof=1) / np.sqrt(count)
    else:
        standard_error = np.zeros(len(bands), dtype=np.float64)
    relative_segment_error = np.divide(
        standard_error,
        np.abs(mean_energy),
        out=np.zeros_like(standard_error),
        where=np.abs(mean_energy) > 0.0,
    )
    relative_upper = np.clip(coherent_gain_rel + 2.0 * relative_segment_error + rounding_rel, 0.0, 2.0)

    band_results: list[BandEnergy] = []
    for band, energy, seg_rel, total_rel in zip(
        bands,
        mean_energy,
        relative_segment_error,
        relative_upper,
        strict=True,
    ):
        power = max(float(energy), 1e-24)
        band_results.append(
            BandEnergy(
                index=band.index,
                center_hz=band.center_hz,
                low_hz=band.low_hz,
                high_hz=band.high_hz,
                power=power,
                power_db=float(10.0 * math.log10(power)),
                segments=count,
                coherent_gain=coherent_gain,
                coherent_gain_rel=float(coherent_gain_rel),
                segment_std_rel=float(seg_rel),
                rounding_rel=rounding_rel,
                relative_upper=float(total_rel),
            )
        )
    return SpectralResult(channel_index=channel_index, bands=tuple(band_results))


def estimate_spectrum(
    aligned: np.ndarray,
    sample_rate: int,
    bands: Sequence[BandSpec],
    config: SpectralConfig,
) -> tuple[SpectralResult, ...]:
    return tuple(
        estimate_channel_spectrum(aligned[index], sample_rate, bands, config, index)
        for index in range(aligned.shape[0])
    )


def direct_spectrum(
    wav: WavData,
    bands: Sequence[BandSpec],
    config: SpectralConfig,
) -> tuple[SpectralResult, ...]:
    return estimate_spectrum(wav.channels, wav.sample_rate, bands, config)
