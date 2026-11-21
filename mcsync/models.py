"""Configuration and report data contracts shared by all processing stages."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import Issue
from .utils import optional_float, sha256_json


def _tuple_of_dicts(value: Any) -> tuple[dict[str, Any], ...]:
    if not value:
        return ()
    return tuple(dict(item) for item in value)


@dataclass
class MarkerConfig:
    period_seconds: float = 0.5
    pulse_duration_samples: int = 96
    first_marker_sample: float | None = None
    search_fraction: float = 0.35
    min_confidence: float = 0.35
    min_snr_db: float = 8.0
    envelope_smoothing_samples: int | None = None
    expected_marker_count: int | None = None

    def validate(self) -> None:
        from .errors import E_CONFIG_INVALID, SyncError

        if self.period_seconds <= 0:
            raise SyncError(E_CONFIG_INVALID, "marker period must be positive")
        if self.pulse_duration_samples <= 0:
            raise SyncError(
                E_CONFIG_INVALID, "marker pulse duration must be positive"
            )
        if not 0 < self.search_fraction < 1:
            raise SyncError(
                E_CONFIG_INVALID, "marker search fraction must be between 0 and 1"
            )
        if not 0 <= self.min_confidence <= 1:
            raise SyncError(
                E_CONFIG_INVALID, "marker confidence threshold must be in [0, 1]"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "period_seconds": self.period_seconds,
            "pulse_duration_samples": self.pulse_duration_samples,
            "first_marker_sample": self.first_marker_sample,
            "search_fraction": self.search_fraction,
            "min_confidence": self.min_confidence,
            "min_snr_db": self.min_snr_db,
            "envelope_smoothing_samples": self.envelope_smoothing_samples,
            "expected_marker_count": self.expected_marker_count,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "MarkerConfig":
        data = data or {}
        return cls(
            period_seconds=float(data.get("period_seconds", 0.5)),
            pulse_duration_samples=int(data.get("pulse_duration_samples", 96)),
            first_marker_sample=optional_float(data.get("first_marker_sample")),
            search_fraction=float(data.get("search_fraction", 0.35)),
            min_confidence=float(data.get("min_confidence", 0.35)),
            min_snr_db=float(data.get("min_snr_db", 8.0)),
            envelope_smoothing_samples=(
                int(data["envelope_smoothing_samples"])
                if data.get("envelope_smoothing_samples") is not None
                else None
            ),
            expected_marker_count=(
                int(data["expected_marker_count"])
                if data.get("expected_marker_count") is not None
                else None
            ),
        )


@dataclass
class SegmentationConfig:
    segment_length_samples: int = 48000
    delay_search_samples: int = 1024
    min_markers_per_segment: int = 1
    correlation_window_samples: int = 8192
    refine_search_samples: int = 96

    def validate(self) -> None:
        from .errors import E_CONFIG_INVALID, SyncError

        if self.segment_length_samples <= 0:
            raise SyncError(
                E_CONFIG_INVALID, "segment length must be positive"
            )
        if self.delay_search_samples <= 0:
            raise SyncError(
                E_CONFIG_INVALID, "delay search range must be positive"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_length_samples": self.segment_length_samples,
            "delay_search_samples": self.delay_search_samples,
            "min_markers_per_segment": self.min_markers_per_segment,
            "correlation_window_samples": self.correlation_window_samples,
            "refine_search_samples": self.refine_search_samples,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "SegmentationConfig":
        data = data or {}
        return cls(
            segment_length_samples=int(data.get("segment_length_samples", 48000)),
            delay_search_samples=int(data.get("delay_search_samples", 1024)),
            min_markers_per_segment=int(data.get("min_markers_per_segment", 1)),
            correlation_window_samples=int(
                data.get("correlation_window_samples", 8192)
            ),
            refine_search_samples=int(data.get("refine_search_samples", 96)),
        )


@dataclass
class FitConfig:
    residual_threshold_samples: float = 2.5
    min_segments: int = 2
    robust_iterations: int = 2
    min_drift_ppm: float = 0.25
    jump_threshold_samples: float = 8.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "residual_threshold_samples": self.residual_threshold_samples,
            "min_segments": self.min_segments,
            "robust_iterations": self.robust_iterations,
            "min_drift_ppm": self.min_drift_ppm,
            "jump_threshold_samples": self.jump_threshold_samples,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "FitConfig":
        data = data or {}
        return cls(
            residual_threshold_samples=float(
                data.get("residual_threshold_samples", 2.5)
            ),
            min_segments=int(data.get("min_segments", 2)),
            robust_iterations=int(data.get("robust_iterations", 2)),
            min_drift_ppm=float(data.get("min_drift_ppm", 0.25)),
            jump_threshold_samples=float(
                data.get("jump_threshold_samples", 8.0)
            ),
        )


@dataclass
class CompensationConfig:
    mode: str = "windowed_sinc"
    tolerance_samples: float = 2.0
    sinc_half_taps: int = 16
    parameter_version: str = ""

    def validate(self) -> None:
        from .errors import E_MODE_INVALID, SyncError

        if self.mode not in {"linear", "windowed_sinc"}:
            raise SyncError(
                E_MODE_INVALID,
                f"unsupported interpolation mode: {self.mode}",
                fields=["compensation.mode"],
                details={"allowed": ["linear", "windowed_sinc"]},
            )
        if self.tolerance_samples <= 0:
            raise SyncError(
                "E_CONFIG_INVALID", "tolerance samples must be positive"
            )
        if self.sinc_half_taps < 2:
            raise SyncError(
                "E_CONFIG_INVALID", "sinc half taps must be at least 2"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "tolerance_samples": self.tolerance_samples,
            "sinc_half_taps": self.sinc_half_taps,
            "parameter_version": self.parameter_version,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "CompensationConfig":
        data = data or {}
        return cls(
            mode=str(data.get("mode", "windowed_sinc")),
            tolerance_samples=float(data.get("tolerance_samples", 2.0)),
            sinc_half_taps=int(data.get("sinc_half_taps", 16)),
            parameter_version=str(data.get("parameter_version", "")),
        )


@dataclass
class DiagnosisConfig:
    min_drift_ppm: float = 0.5
    jump_threshold_samples: float = 8.0
    residual_threshold_samples: float = 2.5
    confidence_threshold: float = 0.55

    def to_dict(self) -> dict[str, Any]:
        return {
            "min_drift_ppm": self.min_drift_ppm,
            "jump_threshold_samples": self.jump_threshold_samples,
            "residual_threshold_samples": self.residual_threshold_samples,
            "confidence_threshold": self.confidence_threshold,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "DiagnosisConfig":
        data = data or {}
        return cls(
            min_drift_ppm=float(data.get("min_drift_ppm", 0.5)),
            jump_threshold_samples=float(
                data.get("jump_threshold_samples", 8.0)
            ),
            residual_threshold_samples=float(
                data.get("residual_threshold_samples", 2.5)
            ),
            confidence_threshold=float(data.get("confidence_threshold", 0.55)),
        )


@dataclass
class PipelineConfig:
    reference_channel: int = 0
    active_channels: tuple[int, ...] | None = None
    marker: MarkerConfig = field(default_factory=MarkerConfig)
    segmentation: SegmentationConfig = field(default_factory=SegmentationConfig)
    fit: FitConfig = field(default_factory=FitConfig)
    compensation: CompensationConfig = field(default_factory=CompensationConfig)
    diagnosis: DiagnosisConfig = field(default_factory=DiagnosisConfig)

    def validate(self) -> None:
        self.marker.validate()
        self.segmentation.validate()
        self.compensation.validate()
        if self.reference_channel < 0:
            from .errors import E_CHANNEL_INVALID, SyncError

            raise SyncError(
                E_CHANNEL_INVALID, "reference channel must be non-negative"
            )

    def fingerprint(self) -> str:
        return sha256_json(
            {
                "reference_channel": self.reference_channel,
                "active_channels": self.active_channels,
                "marker": self.marker.to_dict(),
                "segmentation": self.segmentation.to_dict(),
                "fit": self.fit.to_dict(),
                "compensation": self.compensation.to_dict(),
                "diagnosis": self.diagnosis.to_dict(),
            }
        )

    def copy_with(self, **changes: Any) -> "PipelineConfig":
        values = {
            "reference_channel": self.reference_channel,
            "active_channels": self.active_channels,
            "marker": self.marker,
            "segmentation": self.segmentation,
            "fit": self.fit,
            "compensation": self.compensation,
            "diagnosis": self.diagnosis,
        }
        values.update(changes)
        return PipelineConfig(**values)

    def clone(self) -> "PipelineConfig":
        return PipelineConfig(
            reference_channel=self.reference_channel,
            active_channels=(
                tuple(self.active_channels)
                if self.active_channels is not None
                else None
            ),
            marker=MarkerConfig.from_dict(self.marker.to_dict()),
            segmentation=SegmentationConfig.from_dict(
                self.segmentation.to_dict()
            ),
            fit=FitConfig.from_dict(self.fit.to_dict()),
            compensation=CompensationConfig.from_dict(
                self.compensation.to_dict()
            ),
            diagnosis=DiagnosisConfig.from_dict(self.diagnosis.to_dict()),
        )


@dataclass
class ChannelSpec:
    index: int
    name: str
    sample_rate: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "name": self.name,
            "sample_rate": self.sample_rate,
        }


@dataclass
class ChannelLayout:
    channels: tuple[ChannelSpec, ...]
    reference_channel: int
    sample_rate: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "reference_channel": self.reference_channel,
            "channel_count": len(self.channels),
            "sample_rate": self.sample_rate,
            "channels": [channel.to_dict() for channel in self.channels],
        }


@dataclass
class Marker:
    channel: int
    marker_index: int
    arrival_sample: float
    confidence: float
    snr_db: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "marker_index": self.marker_index,
            "arrival_sample": self.arrival_sample,
            "confidence": self.confidence,
            "snr_db": self.snr_db,
        }


@dataclass
class SegmentDelay:
    channel: int
    segment_index: int
    start_sample: int
    end_sample: int
    delay_samples: float
    peak_correlation: float | None
    ci_low_samples: float | None
    ci_high_samples: float | None
    marker_count: int
    method: str = "gcc-phat"

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "segment_index": self.segment_index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "delay_samples": self.delay_samples,
            "peak_correlation": self.peak_correlation,
            "ci_low_samples": self.ci_low_samples,
            "ci_high_samples": self.ci_high_samples,
            "marker_count": self.marker_count,
            "method": self.method,
        }


@dataclass
class DriftFit:
    channel: int
    initial_offset_samples: float
    drift_ppm: float
    slope: float
    residual_rms: float
    residual_max: float
    residual_threshold_samples: float
    max_residual_segment: int | None
    residual_by_segment: tuple[float, ...]
    predicted_by_segment: tuple[dict[str, Any], ...]
    exceeded_threshold: bool
    slope_std: float = 0.0
    drift_ppm_ci_low: float | None = None
    drift_ppm_ci_high: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "initial_offset_samples": self.initial_offset_samples,
            "drift_ppm": self.drift_ppm,
            "slope": self.slope,
            "slope_std": self.slope_std,
            "drift_ppm_ci_low": self.drift_ppm_ci_low,
            "drift_ppm_ci_high": self.drift_ppm_ci_high,
            "residual_rms": self.residual_rms,
            "residual_max": self.residual_max,
            "residual_threshold_samples": self.residual_threshold_samples,
            "max_residual_segment": self.max_residual_segment,
            "residual_by_segment": list(self.residual_by_segment),
            "predicted_by_segment": list(self.predicted_by_segment),
            "exceeded_threshold": self.exceeded_threshold,
        }


@dataclass
class DriftDiagnosis:
    channel: int
    conclusion: str
    confidence: float
    evidence_segments: tuple[int, ...]
    ci_low_ppm: float | None
    ci_high_ppm: float | None
    jump_segments: tuple[int, ...]
    candidates: tuple[str, ...]
    message: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "conclusion": self.conclusion,
            "confidence": self.confidence,
            "evidence_segments": list(self.evidence_segments),
            "ci_low_ppm": self.ci_low_ppm,
            "ci_high_ppm": self.ci_high_ppm,
            "jump_segments": list(self.jump_segments),
            "candidates": list(self.candidates),
            "message": self.message,
        }


@dataclass
class CompensationParams:
    mode: str
    parameter_version: str
    tolerance_samples: float
    sinc_half_taps: int
    output_length: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "parameter_version": self.parameter_version,
            "tolerance_samples": self.tolerance_samples,
            "sinc_half_taps": self.sinc_half_taps,
            "output_length": self.output_length,
        }


@dataclass
class ChannelResidual:
    channel: int
    marker_count: int
    residual_rms_samples: float
    residual_max_samples: float
    max_abs_delay_samples: float
    within_tolerance: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel,
            "marker_count": self.marker_count,
            "residual_rms_samples": self.residual_rms_samples,
            "residual_max_samples": self.residual_max_samples,
            "max_abs_delay_samples": self.max_abs_delay_samples,
            "within_tolerance": self.within_tolerance,
        }


@dataclass
class ResidualSummary:
    tolerance_samples: float
    worst_channel: int | None
    max_abs_delay_samples: float
    rms_samples: float
    channels: tuple[ChannelResidual, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "tolerance_samples": self.tolerance_samples,
            "worst_channel": self.worst_channel,
            "max_abs_delay_samples": self.max_abs_delay_samples,
            "rms_samples": self.rms_samples,
            "channels": [channel.to_dict() for channel in self.channels],
        }


@dataclass
class SyncReport:
    sample_rate: int
    reference_channel: int
    channel_layout: ChannelLayout
    markers: tuple[Marker, ...]
    segment_delays: tuple[SegmentDelay, ...]
    drift_fits: tuple[DriftFit, ...]
    diagnoses: tuple[DriftDiagnosis, ...]
    compensation: CompensationParams | None
    residual_summary: ResidualSummary | None
    segmentation: SegmentationConfig
    fitting: FitConfig
    parameter_version: str = ""
    schema_version: str = "v3"
    source_version: str = "v3"
    kind: str = "sync_diagnostic_report"
    issues: tuple[Issue, ...] = ()
    provenance: dict[str, str] = field(default_factory=dict)
    integrity: dict[str, Any] = field(default_factory=dict)

    def markers_for(self, channel: int) -> tuple[Marker, ...]:
        return tuple(
            marker for marker in self.markers if marker.channel == channel
        )

    def segments_for(self, channel: int) -> tuple[SegmentDelay, ...]:
        return tuple(
            segment
            for segment in self.segment_delays
            if segment.channel == channel
        )

    def fit_for(self, channel: int) -> DriftFit | None:
        for fit in self.drift_fits:
            if fit.channel == channel:
                return fit
        return None

    def core_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "kind": self.kind,
            "source_version": self.source_version,
            "sample_rate": self.sample_rate,
            "reference_channel": self.reference_channel,
            "parameter_version": self.parameter_version,
            "channel_layout": self.channel_layout.to_dict(),
            "segmentation": self.segmentation.to_dict(),
            "fitting": self.fitting.to_dict(),
            "markers": [marker.to_dict() for marker in self.markers],
            "segment_delays": [
                segment.to_dict() for segment in self.segment_delays
            ],
            "drift_fits": [fit.to_dict() for fit in self.drift_fits],
            "diagnoses": [
                diagnosis.to_dict() for diagnosis in self.diagnoses
            ],
            "compensation": (
                self.compensation.to_dict()
                if self.compensation is not None
                else None
            ),
            "residual_summary": (
                self.residual_summary.to_dict()
                if self.residual_summary is not None
                else None
            ),
            "issues": [issue.to_dict() for issue in self.issues],
            "provenance": dict(self.provenance),
            "integrity": dict(self.integrity),
        }

    def content_dict(self) -> dict[str, Any]:
        payload = self.core_dict()
        payload.pop("integrity", None)
        return payload

    def content_hash(self) -> str:
        return sha256_json(self.content_dict())

    def with_integrity(self) -> "SyncReport":
        content_hash = self.content_hash()
        self.integrity = {
            "algorithm": "sha256",
            "content_sha256": content_hash,
            "scope": "canonical_report_without_integrity",
        }
        return self

    def to_dict(self) -> dict[str, Any]:
        if not self.integrity:
            self.with_integrity()
        return self.core_dict()


def report_from_core(data: dict[str, Any]) -> SyncReport:
    from .versioning import parse_report

    return parse_report(data)

