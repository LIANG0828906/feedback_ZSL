"""Immutable configuration and result records."""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field, fields
from typing import Any, Iterable

from .errors import E_INVALID_CONFIG, E_INVALID_MARKERS, SyncError


def canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def content_hash(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def dataclass_to_dict(value: Any) -> Any:
    if hasattr(value, "__dataclass_fields__"):
        return {k: dataclass_to_dict(v) for k, v in asdict(value).items()}
    if isinstance(value, dict):
        return {str(k): dataclass_to_dict(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [dataclass_to_dict(v) for v in value]
    return value


@dataclass(frozen=True)
class SyncConfig:
    reference_channel: int = 0
    enabled_channels: tuple[int, ...] | None = None
    segment_seconds: float = 2.0
    interpolation: str = "linear"
    strategy: str = "auto"
    duration_strategy_threshold_s: float = 600.0
    channel_strategy_threshold: int = 6
    max_lag_seconds: float = 0.20
    min_segment_correlation: float = 0.08
    marker_min_confidence: float = 0.25
    marker_min_snr: float = 5.0
    tolerance_samples: float = 1.5
    sinc_half_taps: int = 8
    linear_min_abs_rate_ppm: float = 0.05
    step_min_samples: float = 1.0
    step_bic_gain: float = 6.0
    fit_min_points: int = 4

    def validate(self) -> None:
        if self.interpolation not in {"linear", "windowed_sinc"}:
            raise SyncError(E_INVALID_CONFIG, f"unsupported interpolation mode: {self.interpolation}")
        if self.strategy not in {"auto", "full", "segmented"}:
            raise SyncError(E_INVALID_CONFIG, f"unsupported strategy: {self.strategy}")
        if self.segment_seconds <= 0:
            raise SyncError(E_INVALID_CONFIG, "segment_seconds must be positive")
        if self.max_lag_seconds <= 0:
            raise SyncError(E_INVALID_CONFIG, "max_lag_seconds must be positive")
        if self.tolerance_samples <= 0:
            raise SyncError(E_INVALID_CONFIG, "tolerance_samples must be positive")
        if self.sinc_half_taps < 2:
            raise SyncError(E_INVALID_CONFIG, "sinc_half_taps must be at least 2")
        if self.reference_channel < 0:
            raise SyncError(E_INVALID_CONFIG, "reference_channel must be non-negative")
        if self.enabled_channels is not None:
            if len(set(self.enabled_channels)) != len(self.enabled_channels):
                raise SyncError(E_INVALID_CONFIG, "enabled_channels contains duplicates")
            if any(channel < 0 for channel in self.enabled_channels):
                raise SyncError(E_INVALID_CONFIG, "enabled_channels must be non-negative")
            if self.reference_channel not in self.enabled_channels:
                raise SyncError(E_INVALID_CONFIG, "reference_channel must be enabled")

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)

    def signature(self) -> str:
        return content_hash(self.to_dict())


@dataclass(frozen=True)
class MarkerSpec:
    sample_rate: int | None
    expected_samples: tuple[int, ...]
    period_samples: int | None
    pulse_width_samples: int
    search_radius_samples: int
    min_confidence: float
    channel_count: int | None = None
    template: tuple[float, ...] | None = None
    raw: dict = field(default_factory=dict, compare=False)

    @property
    def count(self) -> int:
        return len(self.expected_samples)

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)

    def signature(self) -> str:
        return content_hash(self.to_dict())


def _coerce_int(value: Any, name: str) -> int:
    try:
        return int(value)
    except (TypeError, ValueError) as exc:
        raise SyncError(E_INVALID_MARKERS, f"{name} must be an integer") from exc


def parse_marker_spec(payload: dict) -> MarkerSpec:
    if not isinstance(payload, dict):
        raise SyncError(E_INVALID_MARKERS, "marker description must be a JSON object")

    marker = payload.get("marker", payload)
    if not isinstance(marker, dict):
        raise SyncError(E_INVALID_MARKERS, "marker section must be an object")

    expected_raw: Iterable[Any] | None = payload.get("marker_samples")
    if expected_raw is None:
        expected_raw = marker.get("expected_samples")
    if expected_raw is None:
        expected_raw = marker.get("samples")

    sample_rate = payload.get("sample_rate", marker.get("sample_rate"))
    sample_rate = _coerce_int(sample_rate, "sample_rate") if sample_rate is not None else None

    period = marker.get("period_samples", payload.get("period_samples"))
    if period is not None:
        period = _coerce_int(period, "period_samples")
        if period <= 0:
            raise SyncError(E_INVALID_MARKERS, "period_samples must be positive")

    first = marker.get("first_marker_sample", payload.get("first_marker_sample"))
    count = marker.get("marker_count", payload.get("marker_count", marker.get("expected_count")))
    if expected_raw is None and first is not None and period is not None and count is not None:
        first_i = _coerce_int(first, "first_marker_sample")
        count_i = _coerce_int(count, "marker_count")
        if count_i < 0:
            raise SyncError(E_INVALID_MARKERS, "marker_count must be non-negative")
        expected_raw = [first_i + period * index for index in range(count_i)]

    if expected_raw is None:
        raise SyncError(
            E_INVALID_MARKERS,
            "provide marker_samples/expected_samples, or first_marker_sample + period_samples + marker_count",
        )

    expected = tuple(_coerce_int(value, "marker sample") for value in expected_raw)
    if not expected:
        raise SyncError(E_INVALID_MARKERS, "marker sequence is empty")
    if any(value < 0 for value in expected):
        raise SyncError(E_INVALID_MARKERS, "marker samples must be non-negative")
    if any(right <= left for left, right in zip(expected, expected[1:])):
        raise SyncError(E_INVALID_MARKERS, "marker samples must be strictly increasing")

    pulse_width = _coerce_int(
        marker.get("pulse_width_samples", payload.get("pulse_width_samples", 1)),
        "pulse_width_samples",
    )
    if pulse_width <= 0:
        raise SyncError(E_INVALID_MARKERS, "pulse_width_samples must be positive")

    if period is None and len(expected) > 1:
        period = int(round((expected[-1] - expected[0]) / (len(expected) - 1)))
    radius_default = max(pulse_width, int((period or pulse_width * 4) * 0.45))
    radius = _coerce_int(
        marker.get("search_radius_samples", payload.get("search_radius_samples", radius_default)),
        "search_radius_samples",
    )
    if radius <= 0:
        raise SyncError(E_INVALID_MARKERS, "search_radius_samples must be positive")

    min_conf_raw = marker.get(
        "min_confidence",
        payload.get("min_confidence", 0.25),
    )
    try:
        min_conf = float(min_conf_raw)
    except (TypeError, ValueError) as exc:
        raise SyncError(E_INVALID_MARKERS, "min_confidence must be numeric") from exc
    if not 0.0 <= min_conf <= 1.0:
        raise SyncError(E_INVALID_MARKERS, "min_confidence must be in [0, 1]")

    channel_count_raw = payload.get("channel_count", marker.get("channel_count"))
    channel_count = (
        _coerce_int(channel_count_raw, "channel_count") if channel_count_raw is not None else None
    )

    template_raw = marker.get("template")
    template = None
    if template_raw is not None:
        try:
            template = tuple(float(value) for value in template_raw)
        except (TypeError, ValueError) as exc:
            raise SyncError(E_INVALID_MARKERS, "template must be a numeric array") from exc
        if not template:
            raise SyncError(E_INVALID_MARKERS, "template must not be empty")

    return MarkerSpec(
        sample_rate=sample_rate,
        expected_samples=expected,
        period_samples=period,
        pulse_width_samples=pulse_width,
        search_radius_samples=radius,
        min_confidence=min_conf,
        channel_count=channel_count,
        template=template,
        raw=payload,
    )


@dataclass(frozen=True)
class MarkerHit:
    marker_index: int
    arrival_sample: float | None
    confidence: float
    expected_sample: int
    accepted: bool

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


@dataclass(frozen=True)
class MarkerChannelResult:
    channel: int
    markers: tuple[MarkerHit, ...]
    missing_indices: tuple[int, ...]
    peak_to_noise: float

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


@dataclass(frozen=True)
class SegmentDelay:
    segment_index: int
    start_sample: int
    end_sample: int
    center_sample: float
    delay_samples: float | None
    peak_correlation: float
    ci_low_samples: float | None
    ci_high_samples: float | None
    valid: bool
    method: str

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


@dataclass(frozen=True)
class DriftModel:
    channel: int
    model_kind: str
    offset_samples: float
    drift_ppm: float
    step_sample: float | None = None
    step_samples: float = 0.0
    offset_ci: tuple[float, float] | None = None
    drift_ppm_ci: tuple[float, float] | None = None
    step_ci: tuple[float, float] | None = None
    confidence: float = 0.0
    evidence_segments: tuple[int, ...] = ()
    fit_residual_rms: float | None = None
    error_code: str | None = None
    candidates: tuple[str, ...] = ()
    diagnosis: str = ""

    def delay_at(self, sample: float | Any) -> Any:
        import numpy as np

        values = np.asarray(sample, dtype=np.float64)
        delay = self.offset_samples + self.drift_ppm * 1e-6 * values
        if self.step_samples and self.step_sample is not None:
            delay = delay + np.where(values >= self.step_sample, self.step_samples, 0.0)
        return delay.item() if values.ndim == 0 else delay

    def signature(self) -> str:
        return content_hash(self.to_dict())

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


@dataclass(frozen=True)
class ChannelResult:
    channel: int
    marker_result: MarkerChannelResult
    segment_delays: tuple[SegmentDelay, ...]
    drift: DriftModel
    segment_hashes: tuple[str, ...]
    before_rms: tuple[float | None, ...]
    after_rms: tuple[float | None, ...]
    before_max: tuple[float | None, ...]
    after_max: tuple[float | None, ...]
    marker_counts_before: tuple[int, ...]
    marker_counts_after: tuple[int, ...]
    tolerance_ok: bool
    cache_hits: int = 0
    cache_misses: int = 0

    def to_dict(self) -> dict:
        return dataclass_to_dict(self)


def config_field_names() -> set[str]:
    return {item.name for item in fields(SyncConfig)}
