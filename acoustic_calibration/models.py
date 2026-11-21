"""Immutable data contracts shared by computation, storage and the UI."""

from __future__ import annotations

import math
from dataclasses import dataclass, field, replace
from typing import Any, Iterable, Sequence

import numpy as np

from .hashing import sha256_obj

BAND_MODES = ("octave", "third_octave")


def _finite(value: float, name: str) -> float:
    value = float(value)
    if not math.isfinite(value):
        raise ValueError(f"{name} must be finite")
    return value


@dataclass(frozen=True)
class ThresholdConfig:
    """Adaptive direct-sound threshold configuration.

    The used linear threshold is the maximum of the absolute dBFS floor, a
    robust noise-floor multiplier, and a fraction of the channel peak.
    """

    floor_dbfs: float = -60.0
    noise_multiplier: float = 6.0
    noise_window_ms: float = 10.0
    peak_fraction: float = 0.005

    def __post_init__(self) -> None:
        if not -140.0 <= float(self.floor_dbfs) <= -1.0:
            raise ValueError("threshold floor_dbfs must be in [-140, -1]")
        if not 1.0 <= float(self.noise_multiplier) <= 20.0:
            raise ValueError("noise_multiplier must be in [1, 20]")
        if not 1.0 <= float(self.noise_window_ms) <= 100.0:
            raise ValueError("noise_window_ms must be in [1, 100]")
        if not 0.0 <= float(self.peak_fraction) <= 0.5:
            raise ValueError("peak_fraction must be in [0, 0.5]")

    def to_dict(self) -> dict[str, Any]:
        return {
            "floor_dbfs": float(self.floor_dbfs),
            "noise_multiplier": float(self.noise_multiplier),
            "noise_window_ms": float(self.noise_window_ms),
            "peak_fraction": float(self.peak_fraction),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ThresholdConfig":
        return cls(**data)


@dataclass(frozen=True)
class AlignmentParams:
    reference_channel: int = 0
    threshold: ThresholdConfig = field(default_factory=ThresholdConfig)

    def __post_init__(self) -> None:
        if int(self.reference_channel) < 0:
            raise ValueError("reference_channel must be non-negative")

    def to_dict(self) -> dict[str, Any]:
        return {
            "reference_channel": int(self.reference_channel),
            "threshold": self.threshold.to_dict(),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AlignmentParams":
        return cls(
            reference_channel=int(data["reference_channel"]),
            threshold=ThresholdConfig.from_dict(data["threshold"]),
        )


@dataclass(frozen=True)
class TargetCurve:
    """Piecewise log-frequency target curve in dB SPL-like relative units."""

    points: tuple[tuple[float, float], ...] = ((20.0, 0.0), (1000.0, 0.0), (20000.0, 0.0))

    def __post_init__(self) -> None:
        normalized: list[tuple[float, float]] = []
        for frequency, gain in self.points:
            frequency = _finite(float(frequency), "target frequency")
            gain = _finite(float(gain), "target gain")
            if frequency <= 0.0:
                raise ValueError("target curve frequencies must be positive")
            if not -60.0 <= gain <= 60.0:
                raise ValueError("target curve gains must be in [-60, 60] dB")
            normalized.append((frequency, gain))
        if len(normalized) < 2:
            raise ValueError("target curve requires at least two points")
        if any(normalized[index][0] >= normalized[index + 1][0] for index in range(len(normalized) - 1)):
            raise ValueError("target curve frequencies must be strictly increasing")
        object.__setattr__(self, "points", tuple(normalized))

    def evaluate(self, frequencies: np.ndarray | Iterable[float]) -> np.ndarray:
        values = np.asarray(list(frequencies) if not isinstance(frequencies, np.ndarray) else frequencies, dtype=np.float64)
        points = np.asarray(self.points, dtype=np.float64)
        if values.size == 0:
            return np.empty(0, dtype=np.float64)
        safe = np.maximum(values, 1e-9)
        return np.interp(np.log2(safe), np.log2(points[:, 0]), points[:, 1])

    def to_dict(self) -> dict[str, Any]:
        return {"points": [[float(f), float(g)] for f, g in self.points]}

    @classmethod
    def from_dict(cls, data: dict[str, Any] | Sequence[Sequence[float]]) -> "TargetCurve":
        raw_points = data["points"] if isinstance(data, dict) else data
        return cls(tuple((float(pair[0]), float(pair[1])) for pair in raw_points))

    @classmethod
    def flat(cls, gain_db: float = 0.0) -> "TargetCurve":
        return cls(((20.0, float(gain_db)), (20000.0, float(gain_db))))


@dataclass(frozen=True)
class SpectralConfig:
    band_mode: str = "third_octave"
    fmin_hz: float = 63.0
    fmax_hz: float = 16000.0
    frame_size: int = 4096
    frame_overlap: float = 0.5
    window: str = "hann"

    def __post_init__(self) -> None:
        if self.band_mode not in BAND_MODES:
            raise ValueError(f"band_mode must be one of {BAND_MODES}")
        if not 20.0 <= float(self.fmin_hz) < float(self.fmax_hz) <= 24000.0:
            raise ValueError("spectral range must satisfy 20 <= fmin < fmax <= 24000")
        if int(self.frame_size) < 256 or int(self.frame_size) > 262144:
            raise ValueError("frame_size must be in [256, 262144]")
        if not 0.0 <= float(self.frame_overlap) <= 0.95:
            raise ValueError("frame_overlap must be in [0, 0.95]")
        if self.window not in ("hann", "hamming", "blackman", "rectangular"):
            raise ValueError("unsupported window")

    def to_dict(self) -> dict[str, Any]:
        return {
            "band_mode": self.band_mode,
            "fmin_hz": float(self.fmin_hz),
            "fmax_hz": float(self.fmax_hz),
            "frame_size": int(self.frame_size),
            "frame_overlap": float(self.frame_overlap),
            "window": self.window,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SpectralConfig":
        return cls(**data)


@dataclass(frozen=True)
class CalibrationParams:
    alignment: AlignmentParams = field(default_factory=AlignmentParams)
    spectral: SpectralConfig = field(default_factory=SpectralConfig)
    smoothing_fraction: float = 1.0 / 3.0
    target_curve: TargetCurve = field(default_factory=TargetCurve.flat)
    filter_order: int = 4095
    max_boost_db: float = 18.0
    max_cut_db: float = 24.0
    error_threshold_db: float = 12.0
    consistency_tolerance_db: float = 3.0

    def __post_init__(self) -> None:
        if not 1.0 / 24.0 <= float(self.smoothing_fraction) <= 2.0:
            raise ValueError("smoothing_fraction must be in [1/24, 2] octave")
        if not isinstance(self.filter_order, int):
            raise ValueError("filter_order must be an integer")
        if not 0.0 < float(self.max_boost_db) <= 60.0:
            raise ValueError("max_boost_db must be in (0, 60]")
        if not 0.0 < float(self.max_cut_db) <= 80.0:
            raise ValueError("max_cut_db must be in (0, 80]")
        if not 0.0 < float(self.error_threshold_db) <= 60.0:
            raise ValueError("error_threshold_db must be in (0, 60]")
        if not 0.0 < float(self.consistency_tolerance_db) <= 30.0:
            raise ValueError("consistency_tolerance_db must be in (0, 30]")

    def to_dict(self) -> dict[str, Any]:
        return {
            "alignment": self.alignment.to_dict(),
            "spectral": self.spectral.to_dict(),
            "smoothing_fraction": float(self.smoothing_fraction),
            "target_curve": self.target_curve.to_dict(),
            "filter_order": int(self.filter_order),
            "max_boost_db": float(self.max_boost_db),
            "max_cut_db": float(self.max_cut_db),
            "error_threshold_db": float(self.error_threshold_db),
            "consistency_tolerance_db": float(self.consistency_tolerance_db),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "CalibrationParams":
        return cls(
            alignment=AlignmentParams.from_dict(data["alignment"]),
            spectral=SpectralConfig.from_dict(data["spectral"]),
            smoothing_fraction=float(data["smoothing_fraction"]),
            target_curve=TargetCurve.from_dict(data["target_curve"]),
            filter_order=int(data["filter_order"]),
            max_boost_db=float(data["max_boost_db"]),
            max_cut_db=float(data["max_cut_db"]),
            error_threshold_db=float(data["error_threshold_db"]),
            consistency_tolerance_db=float(data["consistency_tolerance_db"]),
        )


@dataclass(frozen=True)
class BandSpec:
    index: int
    center_hz: float
    low_hz: float
    high_hz: float
    label: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": int(self.index),
            "center_hz": float(self.center_hz),
            "low_hz": float(self.low_hz),
            "high_hz": float(self.high_hz),
            "label": self.label,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BandSpec":
        return cls(
            index=int(data["index"]),
            center_hz=float(data["center_hz"]),
            low_hz=float(data["low_hz"]),
            high_hz=float(data["high_hz"]),
            label=str(data["label"]),
        )


@dataclass(frozen=True)
class ChannelAlignment:
    channel_index: int
    channel_name: str
    arrival_sample: int
    threshold: float
    noise_floor: float
    direct_peak: float
    offset: int
    gain: float
    found: bool = True

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_index": int(self.channel_index),
            "channel_name": self.channel_name,
            "arrival_sample": int(self.arrival_sample),
            "threshold": float(self.threshold),
            "noise_floor": float(self.noise_floor),
            "direct_peak": float(self.direct_peak),
            "offset": int(self.offset),
            "gain": float(self.gain),
            "found": bool(self.found),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ChannelAlignment":
        return cls(
            channel_index=int(data["channel_index"]),
            channel_name=str(data["channel_name"]),
            arrival_sample=int(data["arrival_sample"]),
            threshold=float(data["threshold"]),
            noise_floor=float(data["noise_floor"]),
            direct_peak=float(data["direct_peak"]),
            offset=int(data["offset"]),
            gain=float(data["gain"]),
            found=bool(data.get("found", True)),
        )


@dataclass(frozen=True)
class AlignmentResult:
    reference_channel: int
    reference_arrival: int
    channels: tuple[ChannelAlignment, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "reference_channel": int(self.reference_channel),
            "reference_arrival": int(self.reference_arrival),
            "channels": [channel.to_dict() for channel in self.channels],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "AlignmentResult":
        return cls(
            reference_channel=int(data["reference_channel"]),
            reference_arrival=int(data["reference_arrival"]),
            channels=tuple(ChannelAlignment.from_dict(item) for item in data["channels"]),
        )


@dataclass(frozen=True)
class BandEnergy:
    index: int
    center_hz: float
    low_hz: float
    high_hz: float
    power: float
    power_db: float
    segments: int
    coherent_gain: float
    coherent_gain_rel: float
    segment_std_rel: float
    rounding_rel: float
    relative_upper: float

    @property
    def magnitude(self) -> float:
        return math.sqrt(max(0.0, float(self.power)))

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": int(self.index),
            "center_hz": float(self.center_hz),
            "low_hz": float(self.low_hz),
            "high_hz": float(self.high_hz),
            "power": float(self.power),
            "power_db": float(self.power_db),
            "segments": int(self.segments),
            "coherent_gain": float(self.coherent_gain),
            "coherent_gain_rel": float(self.coherent_gain_rel),
            "segment_std_rel": float(self.segment_std_rel),
            "rounding_rel": float(self.rounding_rel),
            "relative_upper": float(self.relative_upper),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BandEnergy":
        return cls(
            index=int(data["index"]),
            center_hz=float(data["center_hz"]),
            low_hz=float(data["low_hz"]),
            high_hz=float(data["high_hz"]),
            power=float(data["power"]),
            power_db=float(data["power_db"]),
            segments=int(data["segments"]),
            coherent_gain=float(data["coherent_gain"]),
            coherent_gain_rel=float(data["coherent_gain_rel"]),
            segment_std_rel=float(data["segment_std_rel"]),
            rounding_rel=float(data["rounding_rel"]),
            relative_upper=float(data["relative_upper"]),
        )


@dataclass(frozen=True)
class SpectralResult:
    channel_index: int
    bands: tuple[BandEnergy, ...]

    def to_dict(self) -> dict[str, Any]:
        return {"channel_index": int(self.channel_index), "bands": [band.to_dict() for band in self.bands]}

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SpectralResult":
        return cls(
            channel_index=int(data["channel_index"]),
            bands=tuple(BandEnergy.from_dict(item) for item in data["bands"]),
        )


@dataclass(frozen=True)
class SmoothingResult:
    channel_index: int
    power_db: tuple[float, ...]
    weights: tuple[tuple[float, ...], ...]
    recomputed_bands: tuple[int, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_index": int(self.channel_index),
            "power_db": [float(value) for value in self.power_db],
            "weights": [[float(value) for value in row] for row in self.weights],
            "recomputed_bands": [int(value) for value in self.recomputed_bands],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SmoothingResult":
        return cls(
            channel_index=int(data["channel_index"]),
            power_db=tuple(float(value) for value in data["power_db"]),
            weights=tuple(tuple(float(value) for value in row) for row in data["weights"]),
            recomputed_bands=tuple(int(value) for value in data.get("recomputed_bands", [])),
        )


@dataclass(frozen=True)
class FilterDesign:
    channel_index: int
    order: int
    coefficients: tuple[float, ...]
    group_delay_samples: float
    group_delay_ms: float
    desired_gain_db: tuple[float, ...]
    truncation_rel: float
    recomputed_bands: tuple[int, ...] = ()
    reused_frequency_bins: int = 0
    recomputed_frequency_bins: int = 0
    valid: bool = True

    def coefficient_array(self) -> np.ndarray:
        output = np.asarray(self.coefficients, dtype=np.float64)
        output.setflags(write=False)
        return output

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_index": int(self.channel_index),
            "order": int(self.order),
            "coefficients": [float(value) for value in self.coefficients],
            "group_delay_samples": float(self.group_delay_samples),
            "group_delay_ms": float(self.group_delay_ms),
            "desired_gain_db": [float(value) for value in self.desired_gain_db],
            "truncation_rel": float(self.truncation_rel),
            "recomputed_bands": [int(value) for value in self.recomputed_bands],
            "reused_frequency_bins": int(self.reused_frequency_bins),
            "recomputed_frequency_bins": int(self.recomputed_frequency_bins),
            "valid": bool(self.valid),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "FilterDesign":
        return cls(
            channel_index=int(data["channel_index"]),
            order=int(data["order"]),
            coefficients=tuple(float(value) for value in data["coefficients"]),
            group_delay_samples=float(data["group_delay_samples"]),
            group_delay_ms=float(data["group_delay_ms"]),
            desired_gain_db=tuple(float(value) for value in data["desired_gain_db"]),
            truncation_rel=float(data["truncation_rel"]),
            recomputed_bands=tuple(int(value) for value in data.get("recomputed_bands", [])),
            reused_frequency_bins=int(data.get("reused_frequency_bins", 0)),
            recomputed_frequency_bins=int(data.get("recomputed_frequency_bins", 0)),
            valid=bool(data.get("valid", True)),
        )


@dataclass(frozen=True)
class ErrorNode:
    name: str
    relative_error: float
    cumulative_upper_db: float
    amplification: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "relative_error": float(self.relative_error),
            "cumulative_upper_db": float(self.cumulative_upper_db),
            "amplification": float(self.amplification),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ErrorNode":
        return cls(
            name=str(data["name"]),
            relative_error=float(data["relative_error"]),
            cumulative_upper_db=float(data["cumulative_upper_db"]),
            amplification=float(data["amplification"]),
        )


@dataclass(frozen=True)
class BandError:
    channel_index: int
    band_index: int
    upper_db: float
    threshold_crossed: bool
    dominant_node: str
    attribution_node: str
    nodes: tuple[ErrorNode, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_index": int(self.channel_index),
            "band_index": int(self.band_index),
            "upper_db": float(self.upper_db),
            "threshold_crossed": bool(self.threshold_crossed),
            "dominant_node": self.dominant_node,
            "attribution_node": self.attribution_node,
            "nodes": [node.to_dict() for node in self.nodes],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BandError":
        return cls(
            channel_index=int(data["channel_index"]),
            band_index=int(data["band_index"]),
            upper_db=float(data["upper_db"]),
            threshold_crossed=bool(data["threshold_crossed"]),
            dominant_node=str(data["dominant_node"]),
            attribution_node=str(data["attribution_node"]),
            nodes=tuple(ErrorNode.from_dict(item) for item in data["nodes"]),
        )


@dataclass(frozen=True)
class ConsistencyAlert:
    channel_index: int
    channel_name: str
    band_index: int
    center_hz: float
    value_db: float
    peer_median_db: float
    deviation_db: float
    tolerance_db: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel_index": int(self.channel_index),
            "channel_name": self.channel_name,
            "band_index": int(self.band_index),
            "center_hz": float(self.center_hz),
            "value_db": float(self.value_db),
            "peer_median_db": float(self.peer_median_db),
            "deviation_db": float(self.deviation_db),
            "tolerance_db": float(self.tolerance_db),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ConsistencyAlert":
        return cls(
            channel_index=int(data["channel_index"]),
            channel_name=str(data["channel_name"]),
            band_index=int(data["band_index"]),
            center_hz=float(data["center_hz"]),
            value_db=float(data["value_db"]),
            peer_median_db=float(data["peer_median_db"]),
            deviation_db=float(data["deviation_db"]),
            tolerance_db=float(data["tolerance_db"]),
        )


@dataclass(frozen=True)
class ChannelComputation:
    alignment: ChannelAlignment
    spectral: SpectralResult
    smoothing: SmoothingResult
    filter: FilterDesign
    errors: tuple[BandError, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "alignment": self.alignment.to_dict(),
            "spectral": self.spectral.to_dict(),
            "smoothing": self.smoothing.to_dict(),
            "filter": self.filter.to_dict(),
            "errors": [error.to_dict() for error in self.errors],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ChannelComputation":
        return cls(
            alignment=ChannelAlignment.from_dict(data["alignment"]),
            spectral=SpectralResult.from_dict(data["spectral"]),
            smoothing=SmoothingResult.from_dict(data["smoothing"]),
            filter=FilterDesign.from_dict(data["filter"]),
            errors=tuple(BandError.from_dict(item) for item in data["errors"]),
        )


@dataclass(frozen=True)
class CalibrationState:
    version: int
    parent_hash: str | None
    params: CalibrationParams
    source_hash: str
    sample_rate: int
    channel_names: tuple[str, ...]
    bands: tuple[BandSpec, ...]
    channels: tuple[ChannelComputation, ...]
    consistency_alerts: tuple[ConsistencyAlert, ...]
    affected_channels: tuple[int, ...]
    affected_bands: tuple[int, ...]
    recomputed_nodes: tuple[str, ...]
    node_hashes: dict[str, str]
    content_hash: str
    rollback_of: int | None = None
    run_id: str | None = None
    alignment_diff: dict[str, Any] = field(default_factory=dict)

    def channel(self, index: int) -> ChannelComputation:
        return self.channels[index]

    def node_hash(self, key: str) -> str | None:
        return self.node_hashes.get(key)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": int(self.version),
            "parent_hash": self.parent_hash,
            "params": self.params.to_dict(),
            "source_hash": self.source_hash,
            "sample_rate": int(self.sample_rate),
            "channel_names": list(self.channel_names),
            "bands": [band.to_dict() for band in self.bands],
            "channels": [channel.to_dict() for channel in self.channels],
            "consistency_alerts": [alert.to_dict() for alert in self.consistency_alerts],
            "affected_channels": [int(value) for value in self.affected_channels],
            "affected_bands": [int(value) for value in self.affected_bands],
            "recomputed_nodes": list(self.recomputed_nodes),
            "node_hashes": dict(self.node_hashes),
            "content_hash": self.content_hash,
            "rollback_of": self.rollback_of,
            "run_id": self.run_id,
            "alignment_diff": self.alignment_diff,
        }

    def hash_payload(self) -> dict[str, Any]:
        payload = self.to_dict()
        payload.pop("content_hash", None)
        payload.pop("run_id", None)
        return payload

    def compute_content_hash(self) -> str:
        return sha256_obj(self.hash_payload())

    def with_computed_hash(self) -> "CalibrationState":
        provisional = replace(self, content_hash="")
        return replace(provisional, content_hash=provisional.compute_content_hash())

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "CalibrationState":
        return cls(
            version=int(data["version"]),
            parent_hash=data.get("parent_hash"),
            params=CalibrationParams.from_dict(data["params"]),
            source_hash=str(data["source_hash"]),
            sample_rate=int(data["sample_rate"]),
            channel_names=tuple(str(item) for item in data["channel_names"]),
            bands=tuple(BandSpec.from_dict(item) for item in data["bands"]),
            channels=tuple(ChannelComputation.from_dict(item) for item in data["channels"]),
            consistency_alerts=tuple(ConsistencyAlert.from_dict(item) for item in data.get("consistency_alerts", [])),
            affected_channels=tuple(int(item) for item in data.get("affected_channels", [])),
            affected_bands=tuple(int(item) for item in data.get("affected_bands", [])),
            recomputed_nodes=tuple(str(item) for item in data.get("recomputed_nodes", [])),
            node_hashes={str(key): str(value) for key, value in data.get("node_hashes", {}).items()},
            content_hash=str(data["content_hash"]),
            rollback_of=None if data.get("rollback_of") is None else int(data["rollback_of"]),
            run_id=data.get("run_id"),
            alignment_diff=dict(data.get("alignment_diff", {})),
        )


def all_node_keys(channel_names: Sequence[str], bands: Sequence[BandSpec]) -> list[str]:
    keys: list[str] = []
    for channel_index, channel_name in enumerate(channel_names):
        keys.append(f"align:{channel_name}")
        for band in bands:
            keys.append(f"spectral:{channel_name}:{band.index}")
            keys.append(f"smooth:{channel_name}:{band.index}")
            keys.append(f"error:{channel_name}:{band.index}")
        keys.append(f"filter:{channel_name}")
    for band in bands:
        keys.append(f"consistency:{band.index}")
    return keys
