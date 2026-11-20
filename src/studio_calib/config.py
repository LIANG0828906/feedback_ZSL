from __future__ import annotations

from dataclasses import asdict, dataclass, field, replace
from typing import Any, Iterable

import numpy as np

from .errors import CalibrationError


@dataclass(frozen=True)
class TargetPoint:
    frequency_hz: float
    gain_db: float


def _flat_target() -> tuple[TargetPoint, ...]:
    return (TargetPoint(20.0, 0.0), TargetPoint(20000.0, 0.0))


@dataclass(frozen=True)
class CalibrationConfig:
    reference_channel: str = "FL"
    threshold_sigma: float = 8.0
    threshold_peak_ratio: float = 0.05
    frame_size: int = 4096
    overlap: float = 0.5
    window: str = "hann"
    band_fraction: int = 3
    f_min_hz: float = 20.0
    f_max_hz: float = 20000.0
    smoothing_octaves: float = 0.25
    target_curve: tuple[TargetPoint, ...] = field(default_factory=_flat_target)
    filter_taps: int = 2049
    max_boost_db: float = 12.0
    max_cut_db: float = -18.0
    error_threshold_db: float = 1.0
    segmentation_safety: float = 3.0
    consistency_gain_threshold_db: float = 2.5
    consistency_phase_threshold_deg: float = 60.0
    gain_jump_threshold_db: float = 4.0
    low_frequency_limit_hz: float = 250.0

    def validate(self, sample_rate: int = 48000, channel_names: Iterable[str] = ()) -> None:
        names = tuple(channel_names)
        if names and self.reference_channel not in names:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                f"reference channel {self.reference_channel!r} is not in {list(names)!r}",
                "reference_channel",
            )
        if not np.isfinite(self.threshold_sigma) or self.threshold_sigma <= 0:
            raise CalibrationError(
                "E_CONFIG_INVALID", "threshold_sigma must be positive", "threshold_sigma"
            )
        if not 0.0 <= self.threshold_peak_ratio < 1.0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "threshold_peak_ratio must be in [0, 1)",
                "threshold_peak_ratio",
            )
        if not isinstance(self.frame_size, int) or self.frame_size < 256:
            raise CalibrationError(
                "E_CONFIG_INVALID", "frame_size must be an integer >= 256", "frame_size"
            )
        if not 0.0 <= self.overlap < 1.0:
            raise CalibrationError(
                "E_CONFIG_INVALID", "overlap must be in [0, 1)", "overlap"
            )
        if self.window not in {"hann", "hamming", "blackman", "rectangular"}:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                f"unsupported window {self.window!r}",
                "window",
            )
        if self.band_fraction not in {1, 3}:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "band_fraction must be 1 (octave) or 3 (one-third octave)",
                "band_fraction",
            )
        nyquist = sample_rate / 2.0
        if not 0.0 < self.f_min_hz < self.f_max_hz <= nyquist:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                f"frequency range must satisfy 0 < f_min < f_max <= {nyquist}",
                "f_min_hz/f_max_hz",
            )
        if not np.isfinite(self.smoothing_octaves) or not 0.0 < self.smoothing_octaves <= 3.0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "smoothing_octaves must be in (0, 3]",
                "smoothing_octaves",
            )
        if not isinstance(self.filter_taps, int) or self.filter_taps % 2 != 1:
            raise CalibrationError(
                "E_FILTER_INVALID",
                "filter_taps must be an odd integer",
                "filter_taps",
            )
        if not 63 <= self.filter_taps <= 8193:
            raise CalibrationError(
                "E_FILTER_INVALID",
                "filter_taps must be in [63, 8193]",
                "filter_taps",
            )
        if not np.isfinite(self.max_boost_db) or self.max_boost_db < 0:
            raise CalibrationError(
                "E_FILTER_INVALID", "max_boost_db must be >= 0", "max_boost_db"
            )
        if not np.isfinite(self.max_cut_db) or self.max_cut_db > 0:
            raise CalibrationError(
                "E_FILTER_INVALID", "max_cut_db must be <= 0", "max_cut_db"
            )
        if not np.isfinite(self.error_threshold_db) or not 0.0 < self.error_threshold_db <= 60:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "error_threshold_db must be in (0, 60]",
                "error_threshold_db",
            )
        if not np.isfinite(self.segmentation_safety) or self.segmentation_safety <= 0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "segmentation_safety must be positive",
                "segmentation_safety",
            )
        if self.consistency_gain_threshold_db <= 0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "consistency_gain_threshold_db must be positive",
                "consistency_gain_threshold_db",
            )
        if not 0.0 < self.consistency_phase_threshold_deg < 180.0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "consistency_phase_threshold_deg must be in (0, 180)",
                "consistency_phase_threshold_deg",
            )
        if self.gain_jump_threshold_db <= 0:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "gain_jump_threshold_db must be positive",
                "gain_jump_threshold_db",
            )
        if not 0.0 < self.low_frequency_limit_hz < self.f_max_hz:
            raise CalibrationError(
                "E_CONFIG_INVALID",
                "low_frequency_limit_hz must be inside the analysis range",
                "low_frequency_limit_hz",
            )
        self._validate_target()

    def _validate_target(self) -> None:
        if len(self.target_curve) < 2:
            raise CalibrationError(
                "E_FILTER_INVALID",
                "target_curve must contain at least two points",
                "target_curve",
            )
        frequencies = [point.frequency_hz for point in self.target_curve]
        gains = [point.gain_db for point in self.target_curve]
        if not all(np.isfinite(v) for v in frequencies + gains):
            raise CalibrationError(
                "E_FILTER_INVALID", "target_curve contains non-finite values", "target_curve"
            )
        if any(f <= 0 for f in frequencies):
            raise CalibrationError(
                "E_FILTER_INVALID",
                "target_curve frequencies must be positive",
                "target_curve",
            )
        if any(b <= a for a, b in zip(frequencies, frequencies[1:])):
            raise CalibrationError(
                "E_FILTER_INVALID",
                "target_curve frequencies must be strictly increasing",
                "target_curve",
            )
        if any(g < -60.0 or g > 60.0 for g in gains):
            raise CalibrationError(
                "E_FILTER_INVALID",
                "target_curve gain must be in [-60, 60] dB",
                "target_curve",
            )

    def target_gain_db(self, frequencies_hz: np.ndarray | float) -> np.ndarray:
        frequencies = np.asarray(frequencies_hz, dtype=np.float64)
        control_f = np.asarray(
            [point.frequency_hz for point in self.target_curve], dtype=np.float64
        )
        control_g = np.asarray(
            [point.gain_db for point in self.target_curve], dtype=np.float64
        )
        safe_f = np.maximum(frequencies, 1e-9)
        return np.interp(np.log2(safe_f), np.log2(control_f), control_g)

    def update(self, **changes: Any) -> "CalibrationConfig":
        return replace(self, **changes)

    def to_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["target_curve"] = [
            {"frequency_hz": point.frequency_hz, "gain_db": point.gain_db}
            for point in self.target_curve
        ]
        return data

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "CalibrationConfig":
        values = dict(data)
        values["target_curve"] = tuple(
            TargetPoint(
                frequency_hz=float(point["frequency_hz"]),
                gain_db=float(point["gain_db"]),
            )
            for point in values.get("target_curve", [])
        )
        return cls(**values)


def target_curve_from_points(points: Iterable[tuple[float, float]]) -> tuple[TargetPoint, ...]:
    return tuple(TargetPoint(float(f), float(g)) for f, g in points)


TARGET_PRESETS: dict[str, tuple[TargetPoint, ...]] = {
    "flat": target_curve_from_points([(20, 0), (20000, 0)]),
    "house_curve": target_curve_from_points(
        [(20, 3.0), (80, 2.0), (250, 0.5), (1000, 0.0), (4000, -0.5), (20000, -1.0)]
    ),
    "cinema_x": target_curve_from_points(
        [(20, 4.0), (63, 3.0), (200, 1.5), (1000, 0.0), (4000, -1.0), (20000, -3.0)]
    ),
}

