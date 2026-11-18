"""BS.1770 integrated loudness, 4x true peak, and deterministic peak limiting."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .errors import ProcessingFailure
from .project import ExportConfig


@dataclass(frozen=True)
class MasterBusResult:
    left: np.ndarray
    right: np.ndarray
    integrated_lufs: float
    target_lufs: float
    loudness_deviation_lu: float
    true_peak_dbtp: float
    true_peak_limit_dbtp: float
    sample_peak_dbfs: float
    suppressed_samples: int
    limiter_engaged: bool
    pre_limiter_lufs: float
    pre_limiter_true_peak_dbtp: float

    def summary_dict(self) -> dict[str, Any]:
        return {
            "integrated_lufs": self.integrated_lufs,
            "target_lufs": self.target_lufs,
            "loudness_deviation_lu": self.loudness_deviation_lu,
            "true_peak_dbtp": self.true_peak_dbtp,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
            "sample_peak_dbfs": self.sample_peak_dbfs,
            "suppressed_samples": self.suppressed_samples,
            "limiter_engaged": self.limiter_engaged,
            "pre_limiter_lufs": self.pre_limiter_lufs,
            "pre_limiter_true_peak_dbtp": self.pre_limiter_true_peak_dbtp,
        }


def master_binaural_bus(
    left: np.ndarray,
    right: np.ndarray,
    export: ExportConfig,
) -> MasterBusResult:
    """Mix, loudness-normalize, and true-peak constrain a stereo bus."""

    if left.size != right.size:
        raise ValueError("left and right lengths differ")
    if not np.any(np.isfinite(left)) or not np.any(np.isfinite(right)):
        raise ProcessingFailure("BUS_NONFINITE", "Binaural bus contains non-finite samples.")

    pre_lufs = integrated_loudness(left, right, export.sample_rate)
    pre_tp = true_peak_dbtp(left, right, export.sample_rate)
    if pre_lufs is None:
        raise ProcessingFailure("SILENT_BUS", "Binaural bus has no measurable program loudness.")

    gain = 10.0 ** ((export.target_lufs - pre_lufs) / 20.0)
    limit_linear = 10.0 ** (export.true_peak_limit_dbtp / 20.0)
    ceiling = limit_linear * 0.88
    suppressed_left = np.zeros(left.shape, dtype=bool)
    suppressed_right = np.zeros(right.shape, dtype=bool)
    limiter_engaged = False
    final_left = left.copy()
    final_right = right.copy()
    final_lufs = pre_lufs

    for _iteration in range(24):
        driven_left = left * gain
        driven_right = right * gain
        peak = np.maximum(np.abs(driven_left), np.abs(driven_right))
        mask = peak > ceiling
        limiter_engaged = limiter_engaged or bool(np.any(mask))
        if np.any(mask):
            suppressed_left |= np.abs(driven_left) > ceiling
            suppressed_right |= np.abs(driven_right) > ceiling
            final_left = _soft_limit(driven_left, ceiling, limit_linear)
            final_right = _soft_limit(driven_right, ceiling, limit_linear)
        else:
            final_left = driven_left
            final_right = driven_right

        candidate_tp = true_peak_dbtp(
            final_left, final_right, export.sample_rate
        )
        if candidate_tp > export.true_peak_limit_dbtp:
            safety = (
                limit_linear * 0.995 / max(10.0 ** (candidate_tp / 20.0), 1e-30)
            )
            final_left = final_left * safety
            final_right = final_right * safety

        final_lufs_value = integrated_loudness(
            final_left, final_right, export.sample_rate
        )
        if final_lufs_value is None:
            raise ProcessingFailure("SILENT_BUS", "Peak limiting removed all measurable audio.")
        final_lufs = final_lufs_value
        final_tp = true_peak_dbtp(final_left, final_right, export.sample_rate)
        if (
            abs(final_lufs - export.target_lufs) <= 0.02
            and final_tp <= export.true_peak_limit_dbtp + 1e-9
        ):
            break
        correction = 10.0 ** ((export.target_lufs - final_lufs) / 20.0)
        gain *= correction
        if not math.isfinite(gain) or gain > 1e6:
            break

    # Final global safety trim applies only if the 4x true-peak measurement still
    # exceeds the limit. It preserves inter-channel timing and stereo balance.
    final_tp = true_peak_dbtp(final_left, final_right, export.sample_rate)
    if final_tp > export.true_peak_limit_dbtp:
        safety = (
            limit_linear * 0.995 / max(10.0 ** (final_tp / 20.0), 1e-30)
        )
        final_left *= safety
        final_right *= safety

    absolute_ceiling = limit_linear * 0.999
    over_left = np.abs(final_left) > absolute_ceiling
    over_right = np.abs(final_right) > absolute_ceiling
    if np.any(over_left) or np.any(over_right):
        suppressed_left |= over_left
        suppressed_right |= over_right
        final_left = np.clip(final_left, -absolute_ceiling, absolute_ceiling)
        final_right = np.clip(final_right, -absolute_ceiling, absolute_ceiling)
    suppressed = int(np.count_nonzero(suppressed_left) + np.count_nonzero(suppressed_right))

    final_lufs_value = integrated_loudness(final_left, final_right, export.sample_rate)
    if final_lufs_value is None:
        raise ProcessingFailure("SILENT_BUS", "Binaural bus has no measurable program loudness.")
    final_tp = true_peak_dbtp(final_left, final_right, export.sample_rate)
    if final_tp > export.true_peak_limit_dbtp + 1e-9:
        # Scaling after the final true-peak measurement is proportional and can
        # never increase the measured peak.
        safety = limit_linear * 0.995 / max(10.0 ** (final_tp / 20.0), 1e-30)
        final_left *= safety
        final_right *= safety
        final_lufs_value = integrated_loudness(
            final_left, final_right, export.sample_rate
        )
        assert final_lufs_value is not None
        final_tp = true_peak_dbtp(final_left, final_right, export.sample_rate)

    deviation = final_lufs_value - export.target_lufs
    if abs(deviation) > 0.5 + 1e-9:
        raise ProcessingFailure(
            "LOUDNESS_TARGET_UNREACHABLE",
            "Could not meet the integrated-loudness target under the true-peak limit.",
            details={
                "integrated_lufs": final_lufs_value,
                "target_lufs": export.target_lufs,
                "deviation_lu": deviation,
                "true_peak_dbtp": final_tp,
                "true_peak_limit_dbtp": export.true_peak_limit_dbtp,
            },
        )
    if final_tp > export.true_peak_limit_dbtp + 1e-9:
        raise ProcessingFailure(
            "TRUE_PEAK_LIMIT_EXCEEDED",
            "Rendered bus exceeds the declared true-peak limit.",
            details={"actual": final_tp, "limit": export.true_peak_limit_dbtp},
        )

    sample_peak = float(np.max(np.abs(np.concatenate([final_left, final_right]))))
    return MasterBusResult(
        left=final_left,
        right=final_right,
        integrated_lufs=final_lufs_value,
        target_lufs=export.target_lufs,
        loudness_deviation_lu=deviation,
        true_peak_dbtp=final_tp,
        true_peak_limit_dbtp=export.true_peak_limit_dbtp,
        sample_peak_dbfs=20.0 * math.log10(max(sample_peak, 1e-15)),
        suppressed_samples=suppressed,
        limiter_engaged=limiter_engaged,
        pre_limiter_lufs=pre_lufs,
        pre_limiter_true_peak_dbtp=pre_tp,
    )


def integrated_loudness(
    left: np.ndarray, right: np.ndarray, sample_rate: int
) -> float | None:
    """Calculate gated integrated loudness in LUFS (BS.1770-4)."""

    if left.size < int(0.4 * sample_rate):
        return None
    weighted = k_weight(np.stack([left, right], axis=0), sample_rate)
    block = max(1, int(round(0.4 * sample_rate)))
    step = max(1, int(round(0.1 * sample_rate)))
    squares = np.sum(weighted * weighted, axis=0)
    block_energies = []
    for start in range(0, squares.size - block + 1, step):
        block_energies.append(float(np.mean(squares[start : start + block])))
    energies = np.asarray(block_energies, dtype=np.float64)
    if energies.size == 0:
        return None
    with np.errstate(divide="ignore"):
        loudness = -0.691 + 10.0 * np.log10(np.maximum(energies, 1e-30))
    absolute = energies[loudness > -70.0]
    if absolute.size == 0:
        return None
    relative_gate = -0.691 + 10.0 * math.log10(float(np.mean(absolute))) - 10.0
    gated = energies[(loudness > -70.0) & (loudness > relative_gate)]
    if gated.size == 0:
        return None
    return -0.691 + 10.0 * math.log10(float(np.mean(gated)))


def k_weight(signals: np.ndarray, sample_rate: int) -> np.ndarray:
    """Apply the two-stage BS.1770 K-weighting filter in the frequency domain."""

    if signals.ndim == 1:
        signals = signals.reshape(1, -1)
    b1, a1 = _high_shelf(sample_rate)
    b2, a2 = _high_pass(sample_rate)
    n = signals.shape[1]
    tail = max(4096, int(0.5 * sample_rate))
    nfft = 1 << (n + tail - 1).bit_length()
    freqs = np.fft.rfftfreq(nfft, d=1.0 / sample_rate)
    z = np.exp(-2j * np.pi * freqs / sample_rate)
    response = _biquad_response(b1, a1, z) * _biquad_response(b2, a2, z)
    result = np.empty_like(signals, dtype=np.float64)
    for channel_index in range(signals.shape[0]):
        spectrum = np.fft.rfft(signals[channel_index], nfft)
        result[channel_index] = np.fft.irfft(spectrum * response, nfft)[:n]
    return result


def true_peak_dbtp(
    left: np.ndarray, right: np.ndarray, sample_rate: int
) -> float:
    peak = max(
        _true_peak_channel(left, sample_rate),
        _true_peak_channel(right, sample_rate),
    )
    return 20.0 * math.log10(max(peak, 1e-15))


def _true_peak_channel(signal: np.ndarray, sample_rate: int) -> float:
    n = signal.size
    if n == 0:
        return 0.0
    oversample = 4
    tail = 64
    m = 1 << (n + tail - 1).bit_length()
    spectrum = np.fft.rfft(signal, m)
    output_length = oversample * m
    upsampled_spectrum = np.zeros(output_length // 2 + 1, dtype=np.complex128)
    # A real, band-limited interpolation places original positive-frequency
    # bins at the same physical frequency in the longer spectrum.
    positive_bins = m // 2
    upsampled_spectrum[:positive_bins] = spectrum[:positive_bins] * oversample
    # The old Nyquist boundary is split between the adjacent quarter-rate bands.
    upsampled_spectrum[output_length // 2] = spectrum[m // 2] * 0.5 * oversample
    upsampled = np.fft.irfft(upsampled_spectrum, output_length)
    return float(np.max(np.abs(upsampled[: oversample * n])))


def _soft_limit(signal: np.ndarray, ceiling: float, absolute_limit: float) -> np.ndarray:
    result = signal.copy()
    magnitude = np.abs(signal)
    mask = magnitude > ceiling
    if not np.any(mask):
        return result
    width = max(absolute_limit - ceiling, 1e-12)
    result[mask] = np.sign(signal[mask]) * (
        ceiling + width * np.tanh((magnitude[mask] - ceiling) / width)
    )
    return result


def _high_shelf(sample_rate: int) -> tuple[np.ndarray, np.ndarray]:
    gain_db = 3.999843853973347
    q = 0.7071752369554196
    fc = 1681.974450955533
    k = math.tan(math.pi * fc / sample_rate)
    vh = 10.0 ** (gain_db / 20.0)
    vb = vh**0.4996667741545416
    denominator = 1.0 + k / q + k * k
    b = np.array(
        [
            (vh + vb * k / q + k * k) / denominator,
            2.0 * (k * k - vh) / denominator,
            (vh - vb * k / q + k * k) / denominator,
        ]
    )
    a = np.array(
        [
            1.0,
            2.0 * (k * k - 1.0) / denominator,
            (1.0 - k / q + k * k) / denominator,
        ]
    )
    return b, a


def _high_pass(sample_rate: int) -> tuple[np.ndarray, np.ndarray]:
    q = 0.5003270373238773
    fc = 38.13547087602444
    k = math.tan(math.pi * fc / sample_rate)
    denominator = 1.0 + k / q + k * k
    b = np.array([1.0 / denominator, -2.0 / denominator, 1.0 / denominator])
    a = np.array(
        [
            1.0,
            2.0 * (k * k - 1.0) / denominator,
            (1.0 - k / q + k * k) / denominator,
        ]
    )
    return b, a


def _biquad_response(
    b: np.ndarray, a: np.ndarray, z: np.ndarray
) -> np.ndarray:
    return (b[0] + b[1] * z + b[2] * z * z) / (
        a[0] + a[1] * z + a[2] * z * z
    )

