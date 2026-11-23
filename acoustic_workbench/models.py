"""Shared dataclasses and deterministic serialization helpers."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

import numpy as np


@dataclass(frozen=True)
class ChannelInfo:
    id: str
    label: str
    azimuth_deg: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "label": self.label,
            "azimuth_deg": self.azimuth_deg,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ChannelInfo":
        return cls(
            id=str(data["id"]),
            label=str(data.get("label", data["id"])),
            azimuth_deg=float(data.get("azimuth_deg", 0.0)),
        )


@dataclass(frozen=True)
class ChannelLayout:
    name: str
    channels: tuple[ChannelInfo, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "channels": [channel.to_dict() for channel in self.channels],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ChannelLayout":
        return cls(
            name=str(data["name"]),
            channels=tuple(ChannelInfo.from_dict(item) for item in data["channels"]),
        )

    @property
    def channel_ids(self) -> tuple[str, ...]:
        return tuple(channel.id for channel in self.channels)


@dataclass
class InjectionConfig:
    missing_channel: str | None = None
    invert_timing_order: bool = False
    gain_scale: float = 1.0
    bad_frames: list[int] = field(default_factory=list)

    def enabled(self) -> bool:
        return bool(
            self.missing_channel
            or self.invert_timing_order
            or self.gain_scale != 1.0
            or self.bad_frames
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "missing_channel": self.missing_channel,
            "invert_timing_order": self.invert_timing_order,
            "gain_scale": self.gain_scale,
            "bad_frames": list(self.bad_frames),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None) -> "InjectionConfig":
        data = data or {}
        return cls(
            missing_channel=data.get("missing_channel"),
            invert_timing_order=bool(data.get("invert_timing_order", False)),
            gain_scale=float(data.get("gain_scale", 1.0)),
            bad_frames=sorted({int(item) for item in data.get("bad_frames", [])}),
        )


@dataclass
class Sample:
    id: str
    suite: str
    sample_rate: int | None
    layout: ChannelLayout | None
    ir: np.ndarray
    dry: np.ndarray
    ir_path: Path
    dry_path: Path
    truth: dict[str, Any]
    injection: InjectionConfig = field(default_factory=InjectionConfig)
    source_hash: str = ""

    @property
    def duration_s(self) -> float:
        if not self.sample_rate:
            return 0.0
        return self.ir.shape[-1] / float(self.sample_rate)

    @property
    def channel_count(self) -> int:
        return int(self.ir.shape[0]) if self.ir.ndim == 2 else 0

    @property
    def dry_length(self) -> int:
        return int(self.dry.shape[0])


@dataclass(frozen=True)
class TolerancePolicy:
    delay_samples: float = 1.0
    energy_relative: float = 0.08
    drr_db: float = 0.75

    def to_dict(self) -> dict[str, float]:
        return {
            "delay_samples": self.delay_samples,
            "energy_relative": self.energy_relative,
            "drr_db": self.drr_db,
        }


@dataclass
class ResolutionDecision:
    requested_mode: str
    effective_mode: str
    frame_length: int
    hop_length: int
    estimation_window: int
    overlap_ratio: float
    rationale: list[str]
    ir_duration_s: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "requested_mode": self.requested_mode,
            "effective_mode": self.effective_mode,
            "frame_length": self.frame_length,
            "hop_length": self.hop_length,
            "estimation_window": self.estimation_window,
            "overlap_ratio": self.overlap_ratio,
            "rationale": list(self.rationale),
            "ir_duration_s": self.ir_duration_s,
        }


@dataclass
class FrameEstimate:
    frame_index: int
    frame_start: int
    frame_end: int
    channel_ids: tuple[str, ...]
    delay_samples: tuple[float, ...]
    delay_in_frame: tuple[float, ...]
    itd_ref_samples: tuple[float, ...]
    direct_energy: tuple[float, ...]
    early_energy: tuple[float, ...]
    reverberant_energy: tuple[float, ...]
    drr_db: tuple[float, ...]
    direction_deg: float
    direction_width_deg: float
    confidence: float
    algorithm_version: str
    resolution: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        channels = []
        for idx, channel_id in enumerate(self.channel_ids):
            channels.append(
                {
                    "id": channel_id,
                    "delay_samples": float(self.delay_samples[idx]),
                    "delay_in_frame": float(self.delay_in_frame[idx]),
                    "itd_ref_samples": float(self.itd_ref_samples[idx]),
                    "direct_energy": float(self.direct_energy[idx]),
                    "early_energy": float(self.early_energy[idx]),
                    "reverberant_energy": float(self.reverberant_energy[idx]),
                    "drr_db": float(self.drr_db[idx]),
                }
            )
        return {
            "frame_index": self.frame_index,
            "frame_start": self.frame_start,
            "frame_end": self.frame_end,
            "channels": channels,
            "direction_deg": float(self.direction_deg),
            "direction_width_deg": float(self.direction_width_deg),
            "confidence": float(self.confidence),
            "algorithm_version": self.algorithm_version,
            "resolution": dict(self.resolution),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "FrameEstimate":
        channels = data["channels"]
        return cls(
            frame_index=int(data["frame_index"]),
            frame_start=int(data["frame_start"]),
            frame_end=int(data["frame_end"]),
            channel_ids=tuple(str(item["id"]) for item in channels),
            delay_samples=tuple(float(item["delay_samples"]) for item in channels),
            delay_in_frame=tuple(float(item["delay_in_frame"]) for item in channels),
            itd_ref_samples=tuple(float(item["itd_ref_samples"]) for item in channels),
            direct_energy=tuple(float(item["direct_energy"]) for item in channels),
            early_energy=tuple(float(item["early_energy"]) for item in channels),
            reverberant_energy=tuple(float(item["reverberant_energy"]) for item in channels),
            drr_db=tuple(float(item["drr_db"]) for item in channels),
            direction_deg=float(data["direction_deg"]),
            direction_width_deg=float(data["direction_width_deg"]),
            confidence=float(data["confidence"]),
            algorithm_version=str(data["algorithm_version"]),
            resolution=dict(data.get("resolution", {})),
        )


@dataclass(frozen=True)
class RenderConfig:
    convolution_window: int = 2048
    render_block_size: int = 512
    channel_map: tuple[tuple[str, str], ...] | None = None

    def mapping(self) -> tuple[tuple[str, str], ...]:
        return self.channel_map or ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "convolution_window": self.convolution_window,
            "render_block_size": self.render_block_size,
            "channel_map": [{"output": out_id, "input": in_id} for out_id, in_id in self.mapping()],
        }


@dataclass
class RenderStats:
    sample_count: int
    output_channels: tuple[str, ...]
    peak: float
    rms: float
    per_channel_rms: tuple[float, ...]
    per_frame_energy: dict[int, dict[str, float]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_count": self.sample_count,
            "output_channels": list(self.output_channels),
            "peak": self.peak,
            "rms": self.rms,
            "per_channel_rms": list(self.per_channel_rms),
            "per_frame_energy": {
                str(frame): dict(values) for frame, values in sorted(self.per_frame_energy.items())
            },
        }


@dataclass
class RenderResult:
    output: np.ndarray
    stats: RenderStats
    frame_blocks: dict[int, tuple[int, ...]]


def canonical_json(data: Any) -> str:
    return json.dumps(data, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def stable_seed(*parts: Any) -> int:
    digest = hashlib.sha256("|".join(str(part) for part in parts).encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "little", signed=False)


def array_digest(arrays: Iterable[np.ndarray]) -> str:
    digest = hashlib.sha256()
    for array in arrays:
        contiguous = np.ascontiguousarray(array)
        digest.update(str(contiguous.dtype).encode("ascii"))
        digest.update(str(contiguous.shape).encode("ascii"))
        digest.update(contiguous.tobytes(order="C"))
    return digest.hexdigest()
