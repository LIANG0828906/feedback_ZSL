"""BS.1770 K-weighting, channel weighting, and true-peak estimation."""

from __future__ import annotations

import hashlib
import math
from dataclasses import dataclass

import numpy as np

from .params import AnalysisParameters
from .wavio import AudioBuffer

# ITU-R BS.1770-4 coefficients for 48 kHz.
K_PRE_B = (1.53512485958697, -2.69169618940638, 1.19839281085285)
K_PRE_A = (1.0, -1.69065929318241, 0.73248077421585)
K_RLB_B = (1.0, -2.0, 1.0)
K_RLB_A = (1.0, -1.99004745483398, 0.99007225036621)
_FILTER_TAIL_PADDING = 16_384


def lufs_from_energy(energy: float) -> float | None:
    if not math.isfinite(energy) or energy <= 0.0:
        return None
    return -0.691 + 10.0 * math.log10(energy)


def amplitude_to_dbfs(amplitude: float) -> float | None:
    if not math.isfinite(amplitude) or amplitude <= 0.0:
        return None
    return 20.0 * math.log10(amplitude)


def _biquad_response(
    omega: np.ndarray, b: tuple[float, ...], a: tuple[float, ...]
) -> np.ndarray:
    z1 = np.exp(-1j * omega)
    z2 = z1 * z1
    return (b[0] + b[1] * z1 + b[2] * z2) / (a[0] + a[1] * z1 + a[2] * z2)


def k_weight_fft(samples: np.ndarray) -> np.ndarray:
    """Apply the two BS.1770 biquads using exact frequency-domain evaluation.

    A generous zero pad makes circular convolution numerically identical to
    the causal IIR response over the requested finite interval. This preserves
    filter state across seeks because every seek is derived from the prefix
    starting at sample zero.
    """
    values = np.asarray(samples, dtype=np.float64)
    if values.ndim == 1:
        values = values[:, None]
    frame_count, channel_count = values.shape
    if frame_count == 0:
        return values.copy()
    target_length = frame_count + _FILTER_TAIL_PADDING
    fft_length = 1 << (target_length - 1).bit_length()
    frequency = 2.0 * np.pi * np.arange(fft_length // 2 + 1) / fft_length
    response = _biquad_response(frequency, K_PRE_B, K_PRE_A) * _biquad_response(
        frequency, K_RLB_B, K_RLB_A
    )
    result = np.empty_like(values)
    for channel in range(channel_count):
        spectrum = np.fft.rfft(values[:, channel], n=fft_length)
        result[:, channel] = np.fft.irfft(spectrum * response, n=fft_length)[
            :frame_count
        ]
    return result


def direct_biquad_reference(
    samples: np.ndarray,
    b: tuple[float, ...],
    a: tuple[float, ...],
) -> np.ndarray:
    """Small scalar reference implementation used by the DSP tests."""
    output = np.empty(len(samples), dtype=np.float64)
    z1 = 0.0
    z2 = 0.0
    for index, value in enumerate(np.asarray(samples, dtype=np.float64)):
        y = b[0] * value + z1
        z1 = b[1] * value - a[1] * y + z2
        z2 = b[2] * value - a[2] * y
        output[index] = y
    return output


def block_bounds(
    frame_count: int,
    block_samples: int,
    hop_samples: int,
) -> list[tuple[int, int]]:
    if frame_count < block_samples:
        return []
    count = (frame_count - block_samples) // hop_samples + 1
    return [
        (index * hop_samples, index * hop_samples + block_samples)
        for index in range(count)
    ]


@dataclass(frozen=True)
class TruePeak:
    amplitude: float
    dbtp: float | None
    position_sample: float
    channel: int


@dataclass(frozen=True)
class DspContext:
    source_hash: str
    matrix_digest: str
    downmixed: np.ndarray
    filtered: np.ndarray
    block_bounds: tuple[tuple[int, int], ...]
    true_peaks: tuple[TruePeak, ...]
    _power_cumsums: dict[tuple[float, ...], np.ndarray]

    @classmethod
    def build(cls, audio: AudioBuffer, params: AnalysisParameters) -> DspContext:
        matrix = np.asarray(params.downmix, dtype=np.float64)
        downmixed = np.asarray(audio.samples @ matrix.T, dtype=np.float64)
        filtered = k_weight_fft(downmixed)
        bounds = tuple(
            block_bounds(
                audio.frame_count,
                params.block_samples,
                params.hop_samples,
            )
        )
        peaks = tuple(
            _estimate_true_peaks(
                downmixed,
                bounds,
                params.true_peak_oversample,
            )
        )
        return cls(
            source_hash=audio.source_sha256,
            matrix_digest=hashlib.sha256(
                np.asarray(params.downmix, dtype=np.float64).tobytes()
            ).hexdigest(),
            downmixed=downmixed,
            filtered=filtered,
            block_bounds=bounds,
            true_peaks=peaks,
            _power_cumsums={},
        )

    @property
    def block_count(self) -> int:
        return len(self.block_bounds)

    def weighted_power_cumsum(self, weights: tuple[float, ...]) -> np.ndarray:
        cached = self._power_cumsums.get(weights)
        if cached is not None:
            return cached
        weighted = np.zeros(self.filtered.shape[0], dtype=np.float64)
        for channel, weight in enumerate(weights):
            weighted += weight * np.square(self.filtered[:, channel])
        cumulative = np.empty(weighted.size + 1, dtype=np.float64)
        cumulative[0] = 0.0
        np.cumsum(weighted, out=cumulative[1:])
        self._power_cumsums[weights] = cumulative
        return cumulative

    def mean_square(
        self,
        start_sample: int,
        end_sample: int,
        weights: tuple[float, ...],
    ) -> float:
        cumulative = self.weighted_power_cumsum(weights)
        total = cumulative[end_sample] - cumulative[start_sample]
        return float(total / max(1, end_sample - start_sample))


def _utc_resample_4x(samples: np.ndarray, oversample: int) -> np.ndarray:
    """Band-limited zero-padded FFT interpolation to the requested rate."""
    values = np.asarray(samples, dtype=np.float64)
    if values.size == 0:
        return values.copy()
    target_length = values.size * oversample
    spectrum = np.fft.rfft(values)
    target_bins = target_length // 2 + 1
    padded = np.zeros(target_bins, dtype=np.complex128)
    padded[: spectrum.size] = spectrum
    return np.fft.irfft(padded, n=target_length) * oversample


def _estimate_true_peaks(
    downmixed: np.ndarray,
    bounds: tuple[tuple[int, int], ...],
    oversample: int,
) -> list[TruePeak]:
    if not bounds:
        return []
    mutable_amplitude = [0.0] * len(bounds)
    mutable_position = [float(start) for start, _ in bounds]
    mutable_channel = [0] * len(bounds)
    for channel in range(downmixed.shape[1]):
        oversampled = _utc_resample_4x(downmixed[:, channel], oversample)
        for index, (start, end) in enumerate(bounds):
            segment = np.abs(oversampled[start * oversample : end * oversample])
            if segment.size == 0:
                continue
            local_index = int(np.argmax(segment))
            amplitude = float(segment[local_index])
            if amplitude > mutable_amplitude[index]:
                mutable_amplitude[index] = amplitude
                mutable_position[index] = start + local_index / float(oversample)
                mutable_channel[index] = channel
    return [
        TruePeak(
            amplitude=amplitude,
            dbtp=amplitude_to_dbfs(amplitude),
            position_sample=position,
            channel=channel,
        )
        for amplitude, position, channel in zip(
            mutable_amplitude, mutable_position, mutable_channel
        )
    ]
