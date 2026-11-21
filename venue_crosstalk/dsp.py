"""Deterministic filtering, transfer estimation, and sample alignment helpers."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .config import AnalysisConfig, BandSpec


def raised_cosine_mask(
    frequencies: np.ndarray,
    band: BandSpec,
    transition_hz: float,
) -> np.ndarray:
    """Build a flat passband with raised-cosine transitions."""

    bandwidth = band.bandwidth_hz
    transition = min(float(transition_hz), bandwidth * 0.8)
    half = transition / 2.0
    low_outer = max(0.0, band.low_hz - half)
    low_inner = min(band.high_hz, band.low_hz + half)
    high_inner = max(band.low_hz, band.high_hz - half)
    high_outer = band.high_hz + half
    mask = np.zeros_like(frequencies, dtype=np.float64)
    plateau = (frequencies >= low_inner) & (frequencies <= high_inner)
    mask[plateau] = 1.0
    rise = (frequencies > low_outer) & (frequencies < low_inner)
    if np.any(rise):
        phase = (frequencies[rise] - low_outer) / max(low_inner - low_outer, 1e-12)
        mask[rise] = 0.5 - 0.5 * np.cos(np.pi * phase)
    fall = (frequencies > high_inner) & (frequencies < high_outer)
    if np.any(fall):
        phase = (frequencies[fall] - high_inner) / max(high_outer - high_inner, 1e-12)
        mask[fall] = 0.5 + 0.5 * np.cos(np.pi * phase)
    return mask


def apply_frequency_mask(signal: np.ndarray, mask: np.ndarray) -> np.ndarray:
    values = np.asarray(signal, dtype=np.float64)
    spectrum = np.fft.rfft(values)
    return np.fft.irfft(spectrum * mask[: spectrum.size], n=values.size)


def _hann(n: int) -> np.ndarray:
    if n <= 1:
        return np.ones(max(n, 1), dtype=np.float64)
    return np.hanning(n)


def estimate_arrival_sample(reference: np.ndarray, pickup: np.ndarray) -> int:
    """Estimate the pickup arrival lag via FFT cross-correlation.

    Absolute correlation is used so a polarity inversion does not hide the peak.
    """

    x = np.asarray(reference, dtype=np.float64)
    y = np.asarray(pickup, dtype=np.float64)
    if x.size == 0 or y.size == 0:
        return 0
    length = x.size + y.size - 1
    nfft = 1 << (length - 1).bit_length()
    correlation = np.fft.irfft(
        np.fft.rfft(y, nfft) * np.conj(np.fft.rfft(x, nfft)),
        nfft,
    )
    lags = np.arange(nfft)
    lags[lags >= x.size] -= nfft
    valid = (lags >= -(x.size - 1)) & (lags <= y.size - 1)
    candidate_lags = lags[valid]
    candidate_values = np.abs(correlation[valid])
    return int(candidate_lags[int(np.argmax(candidate_values))])


@dataclass(frozen=True)
class BandEstimate:
    pair_id: str
    band: BandSpec
    magnitude: float
    magnitude_db: float
    phase_rad: float
    phase_deg: float
    magnitude_uncertainty_db: float
    phase_uncertainty_deg: float
    magnitude_low: float
    magnitude_high: float
    phase_low_deg: float
    phase_high_deg: float
    coherence: float
    sample_count: int
    effective_samples: float
    quantization_step: float
    transition_hz: float
    content_hash: str = ""

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BandEstimate":
        from .config import BandSpec

        return cls(
            pair_id=str(data["pair"]),
            band=BandSpec(
                index=int(data["band_index"]),
                name=str(data["band"]),
                low_hz=float(data["band_low_hz"]),
                high_hz=float(data["band_high_hz"]),
            ),
            magnitude=float(data["coupling_magnitude"]),
            magnitude_db=float(data["coupling_db"]),
            phase_rad=float(data["phase_rad"]),
            phase_deg=float(data["phase_deg"]),
            magnitude_uncertainty_db=float(data["magnitude_uncertainty_db"]),
            phase_uncertainty_deg=float(data["phase_uncertainty_deg"]),
            magnitude_low=float(data["magnitude_interval"][0]),
            magnitude_high=float(data["magnitude_interval"][1]),
            phase_low_deg=float(data["phase_interval_deg"][0]),
            phase_high_deg=float(data["phase_interval_deg"][1]),
            coherence=float(data["coherence"]),
            sample_count=int(data["sample_count"]),
            effective_samples=float(data["effective_samples"]),
            quantization_step=float(data["quantization_step"]),
            transition_hz=float(data["transition_hz"]),
            content_hash=str(data.get("content_hash", "")),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "pair": self.pair_id,
            "band": self.band.name,
            "band_index": self.band.index,
            "band_low_hz": self.band.low_hz,
            "band_high_hz": self.band.high_hz,
            "coupling_magnitude": self.magnitude,
            "coupling_db": self.magnitude_db,
            "phase_rad": self.phase_rad,
            "phase_deg": self.phase_deg,
            "magnitude_uncertainty_db": self.magnitude_uncertainty_db,
            "phase_uncertainty_deg": self.phase_uncertainty_deg,
            "magnitude_interval": [self.magnitude_low, self.magnitude_high],
            "phase_interval_deg": [self.phase_low_deg, self.phase_high_deg],
            "coherence": self.coherence,
            "sample_count": self.sample_count,
            "effective_samples": self.effective_samples,
            "quantization_step": self.quantization_step,
            "transition_hz": self.transition_hz,
            "content_hash": self.content_hash,
        }


def _wrap_degrees(value: float) -> float:
    return (value + 180.0) % 360.0 - 180.0


def estimate_band_transfer(
    pair_id: str,
    reference: np.ndarray,
    pickup: np.ndarray,
    band: BandSpec,
    config: AnalysisConfig,
) -> BandEstimate:
    """Estimate complex source-to-pickup coupling for one band."""

    x = np.asarray(reference, dtype=np.float64)
    y = np.asarray(pickup, dtype=np.float64)
    if x.size != y.size or x.size < 8:
        raise ValueError("aligned reference and pickup must have equal non-trivial length")
    window = _hann(x.size)
    x_spec = np.fft.rfft(x * window)
    y_spec = np.fft.rfft(y * window)
    frequencies = np.fft.rfftfreq(x.size, d=1.0 / config.sample_rate)
    mask = raised_cosine_mask(frequencies, band, config.transition_hz)
    core = mask >= 0.5
    if not np.any(core):
        core = mask > 0
    weights = np.square(mask[core])
    auto = np.abs(x_spec[core]) ** 2
    auto_sum = float(np.sum(auto * weights))
    if auto_sum <= 1e-30:
        transfer = 0.0 + 0.0j
        coherence = 0.0
    else:
        cross = y_spec[core] * np.conj(x_spec[core])
        transfer = complex(np.sum(cross * weights) / auto_sum)
        cross_sum = abs(np.sum(cross * weights)) ** 2
        coherence = float(
            np.clip(
                cross_sum
                / max(
                    np.sum(auto * weights) * np.sum(np.abs(y_spec[core]) ** 2 * weights),
                    1e-30,
                ),
                0.0,
                1.0,
            )
        )
    magnitude = float(abs(transfer))
    phase = float(np.angle(transfer)) if magnitude > 0 else 0.0
    rms_y = float(np.sqrt(np.mean(np.square(y)))) if y.size else 0.0
    quantization_step = 1.0 / 32768.0
    quantization_rms = quantization_step / math.sqrt(12.0)
    quantization_relative = quantization_rms / max(rms_y, quantization_rms)
    actual_transition = min(config.transition_hz, band.bandwidth_hz * 0.8)
    transition_relative = actual_transition / band.bandwidth_hz
    effective_samples = max(
        1.0,
        x.size * min(1.0, band.bandwidth_hz / (config.sample_rate / 2.0)),
    )
    sample_relative = 1.0 / math.sqrt(effective_samples)
    relative_uncertainty = math.sqrt(
        quantization_relative**2 + sample_relative**2 + transition_relative**2
    )
    magnitude_uncertainty_db = 20.0 * math.log10(max(1.0 + relative_uncertainty, 1e-15))
    phase_instability = math.atan(relative_uncertainty)
    coherence_penalty = 0.35 / math.sqrt(max(coherence, 1e-6))
    phase_uncertainty_rad = min(math.pi, phase_instability + coherence_penalty)
    phase_uncertainty_deg = math.degrees(phase_uncertainty_rad)
    magnitude_low = magnitude / (10.0 ** (magnitude_uncertainty_db / 20.0))
    magnitude_high = magnitude * (10.0 ** (magnitude_uncertainty_db / 20.0))
    phase_center_deg = math.degrees(phase)
    return BandEstimate(
        pair_id=pair_id,
        band=band,
        magnitude=magnitude,
        magnitude_db=20.0 * math.log10(max(magnitude, 1e-15)),
        phase_rad=phase,
        phase_deg=phase_center_deg,
        magnitude_uncertainty_db=magnitude_uncertainty_db,
        phase_uncertainty_deg=phase_uncertainty_deg,
        magnitude_low=magnitude_low,
        magnitude_high=magnitude_high,
        phase_low_deg=_wrap_degrees(phase_center_deg - phase_uncertainty_deg),
        phase_high_deg=_wrap_degrees(phase_center_deg + phase_uncertainty_deg),
        coherence=coherence,
        sample_count=int(x.size),
        effective_samples=effective_samples,
        quantization_step=quantization_step,
        transition_hz=actual_transition,
    )


def synthesize_band_response(
    reference: np.ndarray,
    sample_rate: int,
    bands: list[BandSpec],
    magnitudes: list[float],
    phases_rad: list[float],
    transition_hz: float,
) -> np.ndarray:
    """Construct a deterministic multi-band capture from a reference signal."""

    spectrum = np.fft.rfft(np.asarray(reference, dtype=np.float64))
    frequencies = np.fft.rfftfreq(reference.size, d=1.0 / sample_rate)
    response = np.zeros_like(spectrum)
    for band, magnitude, phase in zip(bands, magnitudes, phases_rad, strict=True):
        mask = raised_cosine_mask(frequencies, band, transition_hz)
        response += mask * magnitude * np.exp(1j * phase)
    return np.fft.irfft(spectrum * response, n=reference.size)


def shift_by_fractional_samples(signal: np.ndarray, delay_samples: int) -> np.ndarray:
    """Integer delay used by the sample generator's propagation model."""

    values = np.asarray(signal, dtype=np.float64)
    if delay_samples <= 0:
        return values.copy()
    result = np.zeros_like(values)
    if delay_samples < values.size:
        result[delay_samples:] = values[:-delay_samples]
    return result
