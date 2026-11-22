"""Core configuration, sample and frame data models."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping, Sequence

import numpy as np

from .canonical import canonical_json, array_bytes, sha256_bytes, sha256_json
from .errors import ErrorCode, WorkbenchError


ALGORITHM_VERSION = "spatial-recon-v1"
CHECKPOINT_FORMAT = "spatial-checkpoint-v1"

CHANNEL_AZIMUTHS: dict[str, float] = {
    "FL": -30.0,
    "FR": 30.0,
    "C": 0.0,
    "LFE": 0.0,
    "RL": -110.0,
    "RR": 110.0,
    "SL": -90.0,
    "SR": 90.0,
    "L": -30.0,
    "R": 30.0,
}

STANDARD_LAYOUTS: dict[str, tuple[str, ...]] = {
    "mono": ("C",),
    "stereo": ("FL", "FR"),
    "quad": ("FL", "FR", "RL", "RR"),
    "5.1": ("FL", "FR", "C", "LFE", "RL", "RR"),
    "binaural": ("L", "R"),
}


def _finite_float(value: Any, name: str) -> float:
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"{name} must be a finite number",
            stage="config",
        ) from exc
    if not math.isfinite(result):
        raise WorkbenchError(
            ErrorCode.E_PARA,
            f"{name} must be finite",
            stage="config",
        )
    return result


@dataclass(frozen=True)
class ChannelLayout:
    name: str
    channels: tuple[str, ...]
    azimuths_deg: tuple[float, ...]

    @classmethod
    def standard(cls, name: str) -> "ChannelLayout":
        if name not in STANDARD_LAYOUTS:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                f"unknown standard channel layout: {name}",
                stage="config",
                details={"layout": name, "supported": sorted(STANDARD_LAYOUTS)},
            )
        channels = STANDARD_LAYOUTS[name]
        return cls(
            name=name,
            channels=channels,
            azimuths_deg=tuple(CHANNEL_AZIMUTHS[channel] for channel in channels),
        )

    @classmethod
    def from_metadata(cls, value: Mapping[str, Any]) -> "ChannelLayout":
        try:
            name = str(value["name"])
            raw_channels = value["channels"]
            raw_azimuths = value.get("azimuths_deg")
        except (KeyError, TypeError) as exc:
            raise WorkbenchError(
                ErrorCode.E_META,
                "channel_layout metadata is incomplete",
                stage="load",
            ) from exc
        if not isinstance(raw_channels, Sequence) or isinstance(raw_channels, (str, bytes)):
            raise WorkbenchError(
                ErrorCode.E_META,
                "channel_layout.channels must be an array",
                stage="load",
            )
        channels = tuple(str(item) for item in raw_channels)
        if not name or not channels:
            raise WorkbenchError(
                ErrorCode.E_META,
                "channel_layout name and channels must be non-empty",
                stage="load",
            )
        if len(set(channels)) != len(channels):
            raise WorkbenchError(
                ErrorCode.E_META,
                "channel_layout contains duplicate channel names",
                stage="load",
                details={"channels": list(channels)},
            )
        if raw_azimuths is None:
            missing = [channel for channel in channels if channel not in CHANNEL_AZIMUTHS]
            if missing:
                raise WorkbenchError(
                    ErrorCode.E_META,
                    "channel_layout azimuths are required for unknown channel names",
                    stage="load",
                    details={"missing": missing},
                )
            azimuths = tuple(CHANNEL_AZIMUTHS[channel] for channel in channels)
        else:
            if len(raw_azimuths) != len(channels):
                raise WorkbenchError(
                    ErrorCode.E_META,
                    "channel_layout azimuth count does not match channels",
                    stage="load",
                )
            azimuths = tuple(
                _finite_float(value, "channel_layout azimuth") for value in raw_azimuths
            )
        return cls(name=name, channels=channels, azimuths_deg=azimuths)

    def channel_index(self, name: str) -> int:
        try:
            return self.channels.index(name)
        except ValueError as exc:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                f"channel {name!r} is not present in layout {self.name!r}",
                stage="channel-map",
                details={"channels": list(self.channels)},
            ) from exc

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "channels": list(self.channels),
            "azimuths_deg": list(self.azimuths_deg),
        }


@dataclass(frozen=True)
class ChannelMap:
    source_channels: tuple[str, ...]
    output_channels: tuple[str, ...]
    weights: Mapping[str, Mapping[str, float]]
    name: str = "identity"

    @classmethod
    def identity(cls, layout: ChannelLayout) -> "ChannelMap":
        weights = {
            channel: {channel: 1.0}
            for channel in layout.channels
        }
        return cls(
            source_channels=tuple(layout.channels),
            output_channels=tuple(layout.channels),
            weights=weights,
            name=f"identity:{layout.name}",
        )

    @classmethod
    def from_standard_output(
        cls,
        source_layout: ChannelLayout,
        output_layout: str,
    ) -> "ChannelMap":
        target = ChannelLayout.standard(output_layout)
        if source_layout.channels == target.channels:
            return cls(
                source_channels=tuple(source_layout.channels),
                output_channels=target.channels,
                weights={channel: {channel: 1.0} for channel in target.channels},
                name=f"{source_layout.name}->{output_layout}",
            )
        source_set = set(source_layout.channels)
        weights: dict[str, dict[str, float]] = {}
        for channel in target.channels:
            candidates = [channel]
            if channel == "FL":
                candidates.extend(["L", "RL", "SL"])
            elif channel == "FR":
                candidates.extend(["R", "RR", "SR"])
            elif channel == "L":
                candidates.extend(["FL", "RL", "SL"])
            elif channel == "R":
                candidates.extend(["FR", "RR", "SR"])
            elif channel in ("RL", "SL"):
                candidates.extend(["RL", "SL", "L", "FL"])
            elif channel in ("RR", "SR"):
                candidates.extend(["RR", "SR", "R", "FR"])
            elif channel == "C":
                candidates.extend(["FL", "FR", "L", "R"])
            elif channel == "LFE":
                candidates.extend(["LFE", "C", "FL", "RL"])
            chosen = next((item for item in candidates if item in source_set), None)
            if chosen is None:
                raise WorkbenchError(
                    ErrorCode.E_PARA,
                    f"cannot map source layout {source_layout.name!r} to {output_layout!r}",
                    stage="channel-map",
                    details={"output_channel": channel},
                )
            weights[channel] = {chosen: 1.0}
        return cls(
            source_channels=tuple(source_layout.channels),
            output_channels=target.channels,
            weights=weights,
            name=f"{source_layout.name}->{output_layout}",
        )

    @classmethod
    def from_mapping(
        cls,
        source_layout: ChannelLayout,
        output_channels: Sequence[str],
        weights: Mapping[str, Mapping[str, Any]],
    ) -> "ChannelMap":
        outputs = tuple(str(channel) for channel in output_channels)
        if not outputs or len(set(outputs)) != len(outputs):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "output_channels must be non-empty and unique",
                stage="channel-map",
            )
        allowed_sources = set(source_layout.channels)
        normalized: dict[str, dict[str, float]] = {}
        for output in outputs:
            raw = weights.get(output)
            if not isinstance(raw, Mapping) or not raw:
                raise WorkbenchError(
                    ErrorCode.E_PARA,
                    f"output channel {output!r} has no source mapping",
                    stage="channel-map",
                )
            normalized[output] = {}
            for source, weight in raw.items():
                source = str(source)
                if source not in allowed_sources:
                    raise WorkbenchError(
                        ErrorCode.E_CHAN,
                        f"mapping references missing source channel {source!r}",
                        stage="channel-map",
                        details={"source_channel": source},
                    )
                weight_float = _finite_float(weight, "channel weight")
                if not -4.0 <= weight_float <= 4.0:
                    raise WorkbenchError(
                        ErrorCode.E_PARA,
                        "channel mapping weight is outside [-4, 4]",
                        stage="channel-map",
                        details={"output": output, "source": source, "weight": weight_float},
                    )
                normalized[output][source] = weight_float
            if not any(abs(weight) > 0 for weight in normalized[output].values()):
                raise WorkbenchError(
                    ErrorCode.E_PARA,
                    f"output channel {output!r} has zero total weight",
                    stage="channel-map",
                )
        return cls(
            source_channels=tuple(source_layout.channels),
            output_channels=outputs,
            weights=normalized,
            name="custom",
        )

    @classmethod
    def from_config(
        cls,
        source_layout: ChannelLayout,
        output_layout: str | None,
        explicit: Mapping[str, Mapping[str, Any]] | None,
    ) -> "ChannelMap":
        if explicit is not None:
            return cls.from_mapping(
                source_layout,
                tuple(explicit.keys()),
                explicit,
            )
        if output_layout is None or output_layout == source_layout.name:
            return cls.identity(source_layout)
        return cls.from_standard_output(source_layout, output_layout)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "source_channels": list(self.source_channels),
            "output_channels": list(self.output_channels),
            "weights": {
                output: dict(sorted(sources.items()))
                for output, sources in sorted(self.weights.items())
            },
        }


@dataclass(frozen=True)
class AnalysisConfig:
    frame_length: int = 1024
    hop_length: int = 512
    energy_threshold: float = 0.005
    early_window_seconds: float = 0.03
    max_direct_delay_seconds: float = 0.25
    max_reflections: int = 8
    clip_limit: float = 1.0
    input_peak_limit: float = 4.0
    max_workers: int = 4
    max_buffer_bytes: int = 16 * 1024 * 1024
    output_layout: str | None = None
    channel_weights: Mapping[str, Mapping[str, float]] | None = None
    algorithm_version: str = ALGORITHM_VERSION

    def __post_init__(self) -> None:
        if not isinstance(self.frame_length, int) or self.frame_length < 16:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "frame_length must be an integer >= 16",
                stage="config",
            )
        if not isinstance(self.hop_length, int) or self.hop_length < 1:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "hop_length must be a positive integer",
                stage="config",
            )
        if self.hop_length > self.frame_length:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "hop_length must not exceed frame_length",
                stage="config",
            )
        if not 0.0 <= self.energy_threshold <= 1.0:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "energy_threshold must be in [0, 1]",
                stage="config",
            )
        if self.early_window_seconds <= 0.0 or not math.isfinite(self.early_window_seconds):
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "early_window_seconds must be positive and finite",
                stage="config",
            )
        if self.max_direct_delay_seconds <= 0.0:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_direct_delay_seconds must be positive",
                stage="config",
            )
        if not 1 <= self.max_reflections <= 64:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_reflections must be in [1, 64]",
                stage="config",
            )
        if self.clip_limit <= 0.0 or self.input_peak_limit <= 0.0:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "clip limits must be positive",
                stage="config",
            )
        if not 1 <= self.max_workers <= 64:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_workers must be in [1, 64]",
                stage="config",
            )
        if self.max_buffer_bytes < self.frame_length * 8:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "max_buffer_bytes is too small for one worker frame",
                stage="config",
            )
        if not self.algorithm_version:
            raise WorkbenchError(
                ErrorCode.E_PARA,
                "algorithm_version must be non-empty",
                stage="config",
            )

    def replace(self, **changes: Any) -> "AnalysisConfig":
        values = self.to_dict()
        values.update(changes)
        return AnalysisConfig(**values)

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_length": self.frame_length,
            "hop_length": self.hop_length,
            "energy_threshold": self.energy_threshold,
            "early_window_seconds": self.early_window_seconds,
            "max_direct_delay_seconds": self.max_direct_delay_seconds,
            "max_reflections": self.max_reflections,
            "clip_limit": self.clip_limit,
            "input_peak_limit": self.input_peak_limit,
            "max_workers": self.max_workers,
            "max_buffer_bytes": self.max_buffer_bytes,
            "output_layout": self.output_layout,
            "channel_weights": self.channel_weights,
            "algorithm_version": self.algorithm_version,
        }


@dataclass
class AcousticSample:
    ir: np.ndarray
    dry: np.ndarray
    sample_rate: int
    layout: ChannelLayout
    truth: Mapping[str, Any]
    sample_id: str = "local"
    bad_frames: tuple[int, ...] = ()
    missing_channels: tuple[str, ...] = ()
    source_path: str | None = None
    _content_hash: str | None = field(default=None, init=False, repr=False)

    def __post_init__(self) -> None:
        self.ir = np.ascontiguousarray(self.ir, dtype=np.float64)
        self.dry = np.ascontiguousarray(self.dry, dtype=np.float64)
        self.ir.flags.writeable = False
        self.dry.flags.writeable = False

    @property
    def content_hash(self) -> str:
        if self._content_hash is None:
            meta = {
                "format": "spatial-sample-v1",
                "sample_rate": self.sample_rate,
                "layout": self.layout.to_dict(),
                "truth": self.truth,
                "missing_channels": list(self.missing_channels),
            }
            self._content_hash = sha256_bytes(
                canonical_json(meta).encode("utf-8"),
                array_bytes(self.ir),
                array_bytes(self.dry),
            )
        return self._content_hash

    def copy_with(
        self,
        *,
        ir: np.ndarray | None = None,
        dry: np.ndarray | None = None,
        bad_frames: Iterable[int] | None = None,
        missing_channels: Iterable[str] | None = None,
    ) -> "AcousticSample":
        return AcousticSample(
            ir=np.array(self.ir if ir is None else ir, copy=True),
            dry=np.array(self.dry if dry is None else dry, copy=True),
            sample_rate=self.sample_rate,
            layout=self.layout,
            truth=dict(self.truth),
            sample_id=self.sample_id,
            bad_frames=tuple(self.bad_frames if bad_frames is None else bad_frames),
            missing_channels=tuple(
                self.missing_channels if missing_channels is None else missing_channels
            ),
            source_path=self.source_path,
        )


@dataclass(frozen=True)
class FrameRenderStats:
    output_energy: float
    output_peak: float
    output_rms: float

    @classmethod
    def from_signal(cls, signal: np.ndarray) -> "FrameRenderStats":
        if signal.size == 0:
            return cls(0.0, 0.0, 0.0)
        energy = float(np.sum(np.square(signal, dtype=np.float64)))
        peak = float(np.max(np.abs(signal)))
        rms = float(math.sqrt(energy / signal.size))
        return cls(output_energy=energy, output_peak=peak, output_rms=rms)

    def to_dict(self) -> dict[str, float]:
        return {
            "output_energy": self.output_energy,
            "output_peak": self.output_peak,
            "output_rms": self.output_rms,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "FrameRenderStats":
        return cls(
            output_energy=float(value["output_energy"]),
            output_peak=float(value["output_peak"]),
            output_rms=float(value["output_rms"]),
        )


@dataclass
class FrameParameters:
    frame_index: int
    source_analysis_index: int
    dry_start: int
    ir_start: int
    frame_length: int
    hop_length: int
    sample_rate: int
    direct_delays_samples: list[int]
    direct_delays_seconds: list[float]
    local_arrival_delays_samples: list[int]
    local_arrival_energy: list[float]
    delay_differences_samples: list[int]
    delay_differences_seconds: list[float]
    direct_energy: list[float]
    early_reflection_energy: list[float]
    early_reflection_energy_ratio: list[float]
    total_energy: list[float]
    energy_ratio: list[float]
    energy_decay_coefficients: list[float]
    ild_db: list[float]
    reflection_delays_samples: list[list[int]]
    reflection_gains: list[list[float]] = field(default_factory=list)
    reflection_amplitudes: list[list[float]] = field(default_factory=list)
    direction_azimuth_deg: float = 0.0
    direction_x: float = 0.0
    direction_y: float = 0.0
    direction_confidence: float = 0.0
    output_channels: tuple[str, ...] = ()
    channel_map: dict[str, Any] = field(default_factory=dict)

    @property
    def frame_energy(self) -> float:
        return float(sum(self.total_energy))

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "source_analysis_index": self.source_analysis_index,
            "dry_start": self.dry_start,
            "ir_start": self.ir_start,
            "frame_length": self.frame_length,
            "hop_length": self.hop_length,
            "sample_rate": self.sample_rate,
            "direct_delays_samples": list(self.direct_delays_samples),
            "direct_delays_seconds": list(self.direct_delays_seconds),
            "local_arrival_delays_samples": list(self.local_arrival_delays_samples),
            "local_arrival_energy": list(self.local_arrival_energy),
            "delay_differences_samples": list(self.delay_differences_samples),
            "delay_differences_seconds": list(self.delay_differences_seconds),
            "direct_energy": list(self.direct_energy),
            "early_reflection_energy": list(self.early_reflection_energy),
            "early_reflection_energy_ratio": list(self.early_reflection_energy_ratio),
            "total_energy": list(self.total_energy),
            "energy_ratio": list(self.energy_ratio),
            "energy_decay_coefficients": list(self.energy_decay_coefficients),
            "ild_db": list(self.ild_db),
            "reflection_delays_samples": [
                list(values) for values in self.reflection_delays_samples
            ],
            "reflection_gains": [
                list(values) for values in self.reflection_gains
            ],
            "reflection_amplitudes": [
                list(values) for values in self.reflection_amplitudes
            ],
            "direction_azimuth_deg": self.direction_azimuth_deg,
            "direction_x": self.direction_x,
            "direction_y": self.direction_y,
            "direction_confidence": self.direction_confidence,
            "output_channels": list(self.output_channels),
            "channel_map": self.channel_map,
        }

    @classmethod
    def from_dict(cls, value: Mapping[str, Any]) -> "FrameParameters":
        return cls(
            frame_index=int(value["frame_index"]),
            source_analysis_index=int(value["source_analysis_index"]),
            dry_start=int(value["dry_start"]),
            ir_start=int(value["ir_start"]),
            frame_length=int(value["frame_length"]),
            hop_length=int(value["hop_length"]),
            sample_rate=int(value["sample_rate"]),
            direct_delays_samples=[int(item) for item in value["direct_delays_samples"]],
            direct_delays_seconds=[float(item) for item in value["direct_delays_seconds"]],
            local_arrival_delays_samples=[
                int(item) for item in value["local_arrival_delays_samples"]
            ],
            local_arrival_energy=[
                float(item) for item in value["local_arrival_energy"]
            ],
            delay_differences_samples=[
                int(item) for item in value["delay_differences_samples"]
            ],
            delay_differences_seconds=[
                float(item) for item in value["delay_differences_seconds"]
            ],
            direct_energy=[float(item) for item in value["direct_energy"]],
            early_reflection_energy=[
                float(item) for item in value["early_reflection_energy"]
            ],
            early_reflection_energy_ratio=[
                float(item) for item in value["early_reflection_energy_ratio"]
            ],
            total_energy=[float(item) for item in value["total_energy"]],
            energy_ratio=[float(item) for item in value["energy_ratio"]],
            energy_decay_coefficients=[
                float(item) for item in value["energy_decay_coefficients"]
            ],
            ild_db=[float(item) for item in value["ild_db"]],
            reflection_delays_samples=[
                [int(item) for item in values]
                for values in value["reflection_delays_samples"]
            ],
            reflection_gains=[
                [float(item) for item in values]
                for values in value["reflection_gains"]
            ],
            reflection_amplitudes=[
                [float(item) for item in values]
                for values in value["reflection_amplitudes"]
            ],
            direction_azimuth_deg=float(value["direction_azimuth_deg"]),
            direction_x=float(value["direction_x"]),
            direction_y=float(value["direction_y"]),
            direction_confidence=float(value["direction_confidence"]),
            output_channels=tuple(str(item) for item in value["output_channels"]),
            channel_map=dict(value["channel_map"]),
        )

    def energy_statistics(self, render_stats: FrameRenderStats | None = None) -> dict[str, Any]:
        result: dict[str, Any] = {
            "direct_energy": list(self.direct_energy),
            "early_reflection_energy": list(self.early_reflection_energy),
            "early_reflection_energy_ratio": list(self.early_reflection_energy_ratio),
            "total_energy": list(self.total_energy),
            "energy_ratio": list(self.energy_ratio),
            "energy_decay_coefficients": list(self.energy_decay_coefficients),
            "ild_db": list(self.ild_db),
            "direction_azimuth_deg": self.direction_azimuth_deg,
            "direction_confidence": self.direction_confidence,
        }
        if render_stats is not None:
            result["render"] = render_stats.to_dict()
        return result

    def fingerprint(self) -> str:
        return sha256_json(self.to_dict())

    def clone(self) -> "FrameParameters":
        """Return an independent value copy for frozen state boundaries."""

        return FrameParameters.from_dict(self.to_dict())


@dataclass(frozen=True)
class RenderResult:
    output: np.ndarray
    channels: tuple[str, ...]
    sample_rate: int
    frames: tuple[FrameParameters, ...]
    render_stats: Mapping[int, FrameRenderStats]
    path: str
    checkpoint_generation: int = 0

    @property
    def output_hash(self) -> str:
        return sha256_bytes(array_bytes(self.output))

    def stats_dict(self) -> dict[str, Any]:
        return {
            frame_index: stats.to_dict()
            for frame_index, stats in sorted(self.render_stats.items())
        }


def arrays_equal(left: np.ndarray, right: np.ndarray) -> bool:
    return (
        left.shape == right.shape
        and left.dtype == right.dtype
        and np.array_equal(left, right)
    )

