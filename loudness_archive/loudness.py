from __future__ import annotations

import math
from collections.abc import Iterable
from dataclasses import dataclass

import numpy as np

from .config import Config
from .errors import E_META_INVALID, AppError
from .wav import WavInfo

LUFS_FLOOR = -120.0
TRUE_PEAK_FLOOR = -144.0

_K_STAGE_1_B = (1.53512485958697, -2.69169618940638, 1.19839281085285)
_K_STAGE_1_A = (1.0, -1.69065929318241, 0.73248077421585)
_K_STAGE_2_B = (1.0, -2.0, 1.0)
_K_STAGE_2_A = (1.0, -1.99004745483398, 0.99007225036621)


def default_downmix_weights(layout: str, channel_names: Iterable[str]) -> tuple[tuple[str, float], ...]:
    names = tuple(channel_names)
    if layout == "MONO":
        return tuple((name, 1.0) for name in names)
    side = 1.0 / math.sqrt(2.0)
    mapping = {
        "FL": 1.0,
        "FR": 1.0,
        "FC": 1.0,
        "LFE": 0.0,
        "BL": side,
        "BR": side,
        "SL": side,
        "SR": side,
    }
    return tuple((name, mapping[name]) for name in names)


def validate_downmix_weights(
    weights: Iterable[tuple[str, float]],
    channel_names: Iterable[str],
    field: str = "downmix_weights",
) -> tuple[tuple[str, float], ...]:
    names = tuple(channel_names)
    result = tuple(weights)
    if len(result) != len(names):
        raise AppError(
            E_META_INVALID,
            f"weight count {len(result)} does not match channel count {len(names)}",
            field=field,
        )
    for index, ((name, weight), expected_name) in enumerate(zip(result, names)):
        if name != expected_name:
            raise AppError(
                E_META_INVALID,
                f"channel order mismatch: {name!r} != {expected_name!r}",
                field=f"{field}[{index}].channel",
            )
        if isinstance(weight, bool) or not isinstance(weight, (int, float)):
            raise AppError(
                E_META_INVALID,
                "weight must be numeric",
                field=f"{field}[{index}].weight",
            )
        if not math.isfinite(float(weight)) or float(weight) < 0.0:
            raise AppError(
                E_META_INVALID,
                "weight must be a finite non-negative number",
                field=f"{field}[{index}].weight",
            )
    if sum(weight for _, weight in result) <= 0.0:
        raise AppError(
            E_META_INVALID,
            "at least one downmix weight must be positive",
            field=field,
        )
    return result


def _freq_response(coefficients: tuple[float, float, float], nfft: int) -> np.ndarray:
    b0, b1, b2 = coefficients
    z = np.exp(-2j * np.pi * np.arange(nfft // 2 + 1) / nfft)
    return b0 + b1 * z + b2 * z * z


def _k_weight_channel(samples: np.ndarray) -> np.ndarray:
    length = samples.size
    padding = min(16384, max(4096, length))
    nfft = 1 << math.ceil(math.log2(max(2, length + padding)))
    spectrum = np.fft.rfft(samples.astype(np.float64), n=nfft)
    response = _freq_response(_K_STAGE_1_B, nfft) / _freq_response(_K_STAGE_1_A, nfft)
    response *= _freq_response(_K_STAGE_2_B, nfft) / _freq_response(_K_STAGE_2_A, nfft)
    filtered = np.fft.irfft(spectrum * response, n=nfft)
    return filtered[:length]


def _energy_to_lufs(energy: float) -> float:
    if not math.isfinite(energy) or energy <= 1e-12:
        return LUFS_FLOOR
    return max(LUFS_FLOOR, -0.691 + 10.0 * math.log10(energy))


def _amplitude_to_db(value: float) -> float:
    if not math.isfinite(value) or value <= 1e-15:
        return TRUE_PEAK_FLOOR
    return max(TRUE_PEAK_FLOOR, 20.0 * math.log10(value))


def _cubic_peak(samples: np.ndarray, oversample: int) -> float:
    if samples.size == 0:
        return 0.0
    peak = float(np.max(np.abs(samples)))
    if samples.size < 4 or oversample <= 1:
        return peak
    y0 = samples[:-3]
    y1 = samples[1:-2]
    y2 = samples[2:-1]
    y3 = samples[3:]
    for step in range(1, oversample):
        t = step / oversample
        t2 = t * t
        t3 = t2 * t
        interpolated = 0.5 * (
            2.0 * y1
            + (-y0 + y2) * t
            + (2.0 * y0 - 5.0 * y1 + 4.0 * y2 - y3) * t2
            + (-y0 + 3.0 * y1 - 3.0 * y2 + y3) * t3
        )
        peak = max(peak, float(np.max(np.abs(interpolated))))
    return peak


def true_peak_dbfs(samples: np.ndarray, oversample: int = 4) -> float:
    if samples.ndim == 1:
        return _amplitude_to_db(_cubic_peak(samples, oversample))
    peak = max(_cubic_peak(samples[:, channel], oversample) for channel in range(samples.shape[1]))
    return _amplitude_to_db(peak)


def round_db(value: float, digits: int = 6) -> float:
    return round(value, digits)


@dataclass(frozen=True)
class SegmentAnalysis:
    index: int
    start_sample: int
    end_sample: int
    short_term_lufs: float
    true_peak_dbtp: float
    true_peak_limit_dbtp: float
    over_limit: bool
    excess_db: float
    delta_to_limit_db: float
    short_term_window_samples: int
    boundary_source: str
    basis_fields: tuple[str, str]

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    def to_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "sample_count": self.sample_count,
            "short_term_lufs": round_db(self.short_term_lufs),
            "true_peak_dbtp": round_db(self.true_peak_dbtp),
            "true_peak_limit_dbtp": round_db(self.true_peak_limit_dbtp),
            "over_limit": self.over_limit,
            "excess_db": round_db(self.excess_db),
            "delta_to_limit_db": round_db(self.delta_to_limit_db),
            "short_term_window_samples": self.short_term_window_samples,
            "boundary_source": self.boundary_source,
            "basis_fields": list(self.basis_fields),
        }


class LoudnessAnalyzer:
    """K-weighted loudness and true-peak analyzer with a cached filter pass."""

    def __init__(
        self,
        wav: WavInfo,
        config: Config,
        downmix_weights: Iterable[tuple[str, float]] | None = None,
    ):
        self.wav = wav
        self.config = config
        weights = (
            default_downmix_weights(wav.channel_layout, wav.channel_names)
            if downmix_weights is None
            else validate_downmix_weights(downmix_weights, wav.channel_names)
        )
        self.downmix_weights = weights
        self._filtered: np.ndarray | None = None
        self._energy: np.ndarray | None = None
        self._prefix_energy: np.ndarray | None = None

    def _prepare(self) -> None:
        if self._energy is not None:
            return
        filtered = np.empty_like(self.wav.audio, dtype=np.float64)
        for channel in range(self.wav.channels):
            filtered[:, channel] = _k_weight_channel(self.wav.audio[:, channel])
        weight_by_name = dict(self.downmix_weights)
        weights = np.array([weight_by_name[name] for name in self.wav.channel_names])
        energy = np.sum(filtered * filtered * weights[np.newaxis, :], axis=1)
        self._filtered = filtered
        self._energy = energy
        self._prefix_energy = np.concatenate(([0.0], np.cumsum(energy)))

    @property
    def filtered_audio(self) -> np.ndarray:
        self._prepare()
        assert self._filtered is not None
        return self._filtered

    def _window_lufs(self, start: int, end: int) -> float:
        self._prepare()
        assert self._prefix_energy is not None
        if end <= start:
            return LUFS_FLOOR
        energy = self._prefix_energy[end] - self._prefix_energy[start]
        return _energy_to_lufs(float(energy / (end - start)))

    def integrated_lufs(self) -> float:
        window = 19200
        hop = 4800
        if self.wav.frames <= window:
            return self._window_lufs(0, self.wav.frames)
        energies: list[float] = []
        self._prepare()
        assert self._prefix_energy is not None
        last_start = self.wav.frames - window
        for start in range(0, last_start + 1, hop):
            value = self._prefix_energy[start + window] - self._prefix_energy[start]
            energies.append(float(value / window))
        if not energies:
            return LUFS_FLOOR
        absolute_gate = 10.0 ** ((-70.0 + 0.691) / 10.0)
        above_absolute = [value for value in energies if value > absolute_gate]
        if not above_absolute:
            return LUFS_FLOOR
        preliminary = float(np.mean(above_absolute))
        relative_gate = preliminary * (10.0 ** (-10.0 / 10.0))
        above_relative = [value for value in above_absolute if value > relative_gate]
        if not above_relative:
            return LUFS_FLOOR
        return _energy_to_lufs(float(np.mean(above_relative)))

    def short_term_peak_lufs(self, start: int, end: int) -> tuple[float, int]:
        length = end - start
        if length <= 0:
            return LUFS_FLOOR, 0
        window = min(self.config.short_term_window_samples, length)
        hop = min(self.config.short_term_hop_samples, window)
        last_start = end - window
        starts = list(range(start, last_start + 1, hop))
        if not starts or starts[-1] != last_start:
            starts.append(last_start)
        peak = max(self._window_lufs(window_start, window_start + window) for window_start in starts)
        return peak, window

    def analyze_segment(
        self,
        index: int,
        start: int,
        end: int,
        limit_dbtp: float,
        boundary_source: str,
    ) -> SegmentAnalysis:
        short_term, window = self.short_term_peak_lufs(start, end)
        true_peak = true_peak_dbfs(
            self.wav.audio[start:end],
            oversample=self.config.true_peak_oversample,
        )
        delta = true_peak - limit_dbtp
        return SegmentAnalysis(
            index=index,
            start_sample=start,
            end_sample=end,
            short_term_lufs=round_db(short_term),
            true_peak_dbtp=round_db(true_peak),
            true_peak_limit_dbtp=round_db(limit_dbtp),
            over_limit=delta > 0.0,
            excess_db=round_db(max(0.0, delta)),
            delta_to_limit_db=round_db(delta),
            short_term_window_samples=window,
            boundary_source=boundary_source,
            basis_fields=(
                f"segments[{index}].true_peak_dbtp",
                f"segments[{index}].true_peak_limit_dbtp",
            ),
        )

    def analyze_boundaries(
        self,
        boundaries: Iterable[tuple[int, int]],
        limits: Iterable[float] | float,
        boundary_source: str,
    ) -> tuple[SegmentAnalysis, ...]:
        intervals = tuple(boundaries)
        if isinstance(limits, (int, float)):
            limit_values = tuple(float(limits) for _ in intervals)
        else:
            limit_values = tuple(float(item) for item in limits)
        if len(limit_values) != len(intervals):
            raise AppError(
                E_META_INVALID,
                "segment limit count does not match boundary count",
                field="true_peak_limit_dbtp",
            )
        results = []
        for index, ((start, end), limit) in enumerate(zip(intervals, limit_values)):
            if start < 0 or end > self.wav.frames or end <= start:
                raise AppError(
                    E_META_INVALID,
                    f"invalid segment boundary [{start}, {end}) for {self.wav.frames} frames",
                    field=f"segments[{index}].start_sample",
                )
            results.append(self.analyze_segment(index, start, end, limit, boundary_source))
        return tuple(results)

