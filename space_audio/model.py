from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from typing import Any

import numpy as np

from .config import AnalysisConfig
from .errors import ErrorCode, WorkbenchError, meta_error
from .layout import ChannelLayout, get_layout


@dataclass(frozen=True)
class DirectEvent:
    position_samples: float
    amplitude: float
    sigma_samples: float
    source_frame: int

    def value_at(self, sample_index: np.ndarray | float) -> np.ndarray | float:
        z = (np.asarray(sample_index, dtype=np.float64) - self.position_samples)
        return self.amplitude * np.exp(-0.5 * (z / self.sigma_samples) ** 2)


@dataclass(frozen=True)
class ReflectionEvent:
    position_samples: int
    amplitude: float


@dataclass(frozen=True)
class TailEvent:
    start_samples: int
    end_samples: int
    amplitude: float
    tau_s: float
    frequency_1_hz: float
    frequency_2_hz: float
    phase_rad: float

    def value_at(self, absolute_indices: np.ndarray, sample_rate: int) -> np.ndarray:
        indices = np.asarray(absolute_indices, dtype=np.int64)
        t = (indices - self.start_samples) / float(sample_rate)
        envelope = self.amplitude * np.exp(-np.maximum(t, 0.0) / self.tau_s)
        return envelope * (
            0.72 * np.sin(2.0 * np.pi * self.frequency_1_hz * t + self.phase_rad)
            + 0.28
            * np.sin(
                2.0 * np.pi * self.frequency_2_hz * t + 0.7 * self.phase_rad
            )
        )


@dataclass(frozen=True)
class ConstructionChannel:
    channel_id: str
    direct_events: tuple[DirectEvent, ...]
    reflections: tuple[ReflectionEvent, ...]
    tails: tuple[TailEvent, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "direct_events": [
                {
                    "position_samples": event.position_samples,
                    "amplitude": event.amplitude,
                    "sigma_samples": event.sigma_samples,
                    "source_frame": event.source_frame,
                }
                for event in self.direct_events
            ],
            "reflections": [
                {
                    "position_samples": event.position_samples,
                    "amplitude": event.amplitude,
                }
                for event in self.reflections
            ],
            "tails": [
                {
                    "start_samples": event.start_samples,
                    "end_samples": event.end_samples,
                    "amplitude": event.amplitude,
                    "tau_s": event.tau_s,
                    "frequency_1_hz": event.frequency_1_hz,
                    "frequency_2_hz": event.frequency_2_hz,
                    "phase_rad": event.phase_rad,
                }
                for event in self.tails
            ],
        }


@dataclass(frozen=True)
class FrameTruth:
    frame_index: int
    channel_id: str
    direct_delay_samples: float
    direct_delay_s: float
    direct_energy: float
    early_reflection_energy: float
    reverb_energy: float
    drr: float
    tdoa_samples: float = 0.0
    tdoa_s: float = 0.0
    reflection_delays_samples: tuple[int, ...] = ()

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_index": self.frame_index,
            "channel": self.channel_id,
            "direct_delay_samples": self.direct_delay_samples,
            "direct_delay_s": self.direct_delay_s,
            "direct_energy": self.direct_energy,
            "early_reflection_energy": self.early_reflection_energy,
            "reverb_energy": self.reverb_energy,
            "drr": self.drr,
            "tdoa_samples": self.tdoa_samples,
            "tdoa_s": self.tdoa_s,
            "reflection_delays_samples": list(self.reflection_delays_samples),
        }


@dataclass(frozen=True)
class ChannelTruthSummary:
    channel_id: str
    direct_delay_samples: float
    direct_delay_s: float
    early_reflection_energy: float
    reverb_energy: float
    drr: float
    tdoa_samples: float
    tdoa_s: float
    frames: tuple[FrameTruth, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "channel": self.channel_id,
            "direct_delay_samples": self.direct_delay_samples,
            "direct_delay_s": self.direct_delay_s,
            "early_reflection_energy": self.early_reflection_energy,
            "reverb_energy": self.reverb_energy,
            "drr": self.drr,
            "tdoa_samples": self.tdoa_samples,
            "tdoa_s": self.tdoa_s,
            "frames": [frame.to_dict() for frame in self.frames],
        }


@dataclass(frozen=True)
class AcousticSample:
    sample_id: str
    sample_rate: int
    layout: ChannelLayout
    ir: np.ndarray
    dry: np.ndarray
    construction: tuple[ConstructionChannel, ...]
    declared_truth: tuple[ChannelTruthSummary, ...]
    manifest_sha256: str
    manifest_path: str
    ir_file: str
    dry_file: str

    @property
    def channel_count(self) -> int:
        return int(self.ir.shape[0])

    @property
    def ir_length(self) -> int:
        return int(self.ir.shape[1])

    @property
    def dry_length(self) -> int:
        return int(self.dry.shape[1])

    def frame_count(self, config: AnalysisConfig) -> int:
        if self.ir_length < config.frame_len:
            return 1
        return 1 + (self.ir_length - config.frame_len) // config.hop

    def construction_channel(self, channel_id: str) -> ConstructionChannel:
        for channel in self.construction:
            if channel.channel_id == channel_id:
                return channel
        raise WorkbenchError(
            code=ErrorCode.CHAN,
            message=f"construction metadata missing for channel {channel_id}",
            channel=channel_id,
        )

    def input_hash(self) -> str:
        digest = hashlib.sha256()
        digest.update(b"spatial-audio-input-v1\0")
        digest.update(self.manifest_sha256.encode("ascii"))
        digest.update(b"\0layout\0")
        digest.update(self.layout.name.encode("utf-8"))
        for channel in self.layout.channels:
            digest.update(channel.id.encode("utf-8"))
            digest.update(np.float64(channel.azimuth_deg).tobytes())
            digest.update(np.float64(channel.elevation_deg).tobytes())
        digest.update(b"\0ir\0")
        digest.update(np.ascontiguousarray(self.ir, dtype="<f8").tobytes())
        digest.update(b"\0dry\0")
        digest.update(np.ascontiguousarray(self.dry, dtype="<f8").tobytes())
        digest.update(b"\0construction\0")
        digest.update(
            json.dumps(
                [channel.to_dict() for channel in self.construction],
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            ).encode("utf-8")
        )
        return digest.hexdigest()


def _constructed_values(
    construction: ConstructionChannel,
    indices: np.ndarray,
    sample_rate: int,
) -> np.ndarray:
    """Evaluate the analytic construction, independent of the stored IR array."""

    indices = np.asarray(indices, dtype=np.float64)
    values = np.zeros(indices.shape, dtype=np.float64)
    for event in construction.direct_events:
        support = 8.0 * event.sigma_samples + 1.0
        mask = np.abs(indices - event.position_samples) <= support
        if np.any(mask):
            values[mask] += np.asarray(event.value_at(indices[mask]), dtype=np.float64)
    if construction.reflections:
        positions = np.fromiter(
            (event.position_samples for event in construction.reflections),
            dtype=np.int64,
            count=len(construction.reflections),
        )
        amplitudes = np.fromiter(
            (event.amplitude for event in construction.reflections),
            dtype=np.float64,
            count=len(construction.reflections),
        )
        for position, amplitude in zip(positions, amplitudes):
            hit = indices == float(position)
            if np.any(hit):
                values[hit] += amplitude
    for tail in construction.tails:
        low = int(tail.start_samples)
        high = int(tail.end_samples)
        mask = (indices >= low) & (indices < high)
        if np.any(mask):
            values[mask] += tail.value_at(
                indices[mask].astype(np.int64),
                sample_rate,
            )
    return values


def _analytic_peak_delay(
    construction: ConstructionChannel,
    start: int,
    end: int,
    sample_rate: int,
) -> float:
    candidates = [
        event
        for event in construction.direct_events
        if start - 3.0 * event.sigma_samples
        <= event.position_samples
        < end
    ]
    if candidates:
        direct = max(
            candidates,
            key=lambda event: (event.amplitude, -event.position_samples),
        )
        return float(direct.position_samples)

    integer_indices = np.arange(start, end, dtype=np.float64)
    integer_values = np.abs(
        _constructed_values(construction, integer_indices, sample_rate)
    )
    peak_integer = int(np.argmax(integer_values))
    grid = np.linspace(
        max(float(start), start + peak_integer - 1.0),
        min(float(end - 1), start + peak_integer + 1.0),
        401,
    )
    grid_values = np.abs(_constructed_values(construction, grid, sample_rate))
    return float(grid[int(np.argmax(grid_values))])


def frame_truth_for_channel(
    sample: AcousticSample,
    construction: ConstructionChannel,
    frame_index: int,
    config: AnalysisConfig,
) -> FrameTruth:
    current_events = [
        event
        for event in construction.direct_events
        if event.source_frame == frame_index
    ]
    if current_events:
        previous_events = [
            event
            for event in construction.direct_events
            if event.source_frame < frame_index
        ]
        if previous_events:
            previous_position = max(
                event.position_samples for event in previous_events
            )
            if min(
                event.position_samples for event in current_events
            ) <= previous_position:
                raise WorkbenchError(
                    ErrorCode.ORDER,
                    "direct-path event order is inverted in analytic construction",
                    field="arrival_order",
                    channel=construction.channel_id,
                    frame_index=frame_index,
                    details={
                        "current_position": min(
                            event.position_samples for event in current_events
                        ),
                        "previous_position": previous_position,
                    },
                )
    start = frame_index * config.hop
    search_end = start + config.window_len
    direct_delay = _analytic_peak_delay(
        construction,
        start,
        search_end,
        sample.sample_rate,
    )
    center = int(round(direct_delay))
    direct_indices = np.arange(
        center - config.direct_half_width,
        center + config.direct_half_width + 1,
        dtype=np.float64,
    )
    direct_values = _constructed_values(
        construction,
        direct_indices,
        sample.sample_rate,
    )
    direct_energy = float(np.dot(direct_values, direct_values))
    early_low = center + config.direct_half_width + 1
    early_high = start + config.window_len
    early_indices = np.arange(early_low, early_high, dtype=np.float64)
    early_values = _constructed_values(
        construction,
        early_indices,
        sample.sample_rate,
    )
    early_energy = float(np.dot(early_values, early_values))
    reflection_positions = tuple(
        event.position_samples
        for event in construction.reflections
        if early_low <= event.position_samples < early_high
    )

    late_low = start + config.window_len
    late_high = start + config.hop
    late_indices = np.arange(late_low, late_high, dtype=np.float64)
    late_values = _constructed_values(
        construction,
        late_indices,
        sample.sample_rate,
    )
    reverb_energy = float(np.dot(late_values, late_values))
    drr = direct_energy / max(reverb_energy, 1.0e-30)
    return FrameTruth(
        frame_index=frame_index,
        channel_id=construction.channel_id,
        direct_delay_samples=direct_delay,
        direct_delay_s=direct_delay / sample.sample_rate,
        direct_energy=float(direct_energy),
        early_reflection_energy=float(early_energy),
        reverb_energy=float(reverb_energy),
        drr=float(drr),
        reflection_delays_samples=reflection_positions,
    )


def frame_truth_for_sample(
    sample: AcousticSample,
    frame_index: int,
    config: AnalysisConfig,
) -> tuple[FrameTruth, ...]:
    rows: list[FrameTruth] = []
    reference_delay: float | None = None
    for construction in sample.construction:
        truth = frame_truth_for_channel(sample, construction, frame_index, config)
        if reference_delay is None:
            reference_delay = truth.direct_delay_samples
        rows.append(
            FrameTruth(
                frame_index=truth.frame_index,
                channel_id=truth.channel_id,
                direct_delay_samples=truth.direct_delay_samples,
                direct_delay_s=truth.direct_delay_s,
                direct_energy=truth.direct_energy,
                early_reflection_energy=truth.early_reflection_energy,
                reverb_energy=truth.reverb_energy,
                drr=truth.drr,
                tdoa_samples=truth.direct_delay_samples - reference_delay,
                tdoa_s=(truth.direct_delay_samples - reference_delay)
                / sample.sample_rate,
                reflection_delays_samples=truth.reflection_delays_samples,
            )
        )
    return tuple(rows)


def validate_construction_order(sample: AcousticSample) -> None:
    """Reject source-frame ordering inversions before any frame is estimated."""

    for construction in sample.construction:
        ordered = sorted(
            construction.direct_events,
            key=lambda event: event.source_frame,
        )
        previous_position: float | None = None
        for event in ordered:
            if (
                previous_position is not None
                and event.position_samples <= previous_position
            ):
                raise WorkbenchError(
                    ErrorCode.ORDER,
                    "direct-path event order is inverted in analytic construction",
                    field="arrival_order",
                    channel=construction.channel_id,
                    frame_index=event.source_frame,
                    details={
                        "current_position": event.position_samples,
                        "previous_position": previous_position,
                    },
                )
            previous_position = event.position_samples


def parse_construction_channel(
    payload: dict[str, Any],
    channel_id: str,
    sample_rate: int,
) -> ConstructionChannel:
    direct_payload = payload.get("direct_events")
    reflection_payload = payload.get("reflections")
    tail_payload = payload.get("tails")
    if not isinstance(direct_payload, list) or not direct_payload:
        raise meta_error("direct event construction is missing", "construction.direct_events")
    if not isinstance(reflection_payload, list):
        raise meta_error("reflection construction is missing", "construction.reflections")
    if not isinstance(tail_payload, list):
        raise meta_error("tail construction is missing", "construction.tails")

    direct_events: list[DirectEvent] = []
    for index, item in enumerate(direct_payload):
        try:
            direct_events.append(
                DirectEvent(
                    position_samples=float(item["position_samples"]),
                    amplitude=float(item["amplitude"]),
                    sigma_samples=float(item.get("sigma_samples", 1.3)),
                    source_frame=int(item.get("source_frame", index)),
                )
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise meta_error(
                "invalid direct event construction",
                f"construction.{channel_id}.direct_events[{index}]",
            ) from exc

    reflections: list[ReflectionEvent] = []
    for index, item in enumerate(reflection_payload):
        try:
            reflections.append(
                ReflectionEvent(
                    position_samples=int(item["position_samples"]),
                    amplitude=float(item["amplitude"]),
                )
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise meta_error(
                "invalid reflection construction",
                f"construction.{channel_id}.reflections[{index}]",
            ) from exc

    tails: list[TailEvent] = []
    for index, item in enumerate(tail_payload):
        try:
            start = int(item["start_samples"])
            end = int(item["end_samples"])
            tails.append(
                TailEvent(
                    start_samples=start,
                    end_samples=end,
                    amplitude=float(item["amplitude"]),
                    tau_s=float(item["tau_s"]),
                    frequency_1_hz=float(item["frequency_1_hz"]),
                    frequency_2_hz=float(item["frequency_2_hz"]),
                    phase_rad=float(item["phase_rad"]),
                )
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise meta_error(
                "invalid tail construction",
                f"construction.{channel_id}.tails[{index}]",
            ) from exc
        if end <= start or sample_rate <= 0:
            raise meta_error(
                "tail construction has invalid sample range",
                f"construction.{channel_id}.tails[{index}]",
            )

    for event in direct_events:
        if not math.isfinite(event.position_samples) or not math.isfinite(event.amplitude):
            raise meta_error(
                "direct construction values must be finite",
                f"construction.{channel_id}.direct_events",
            )
    for event in reflections:
        if not math.isfinite(event.amplitude):
            raise meta_error(
                "reflection construction values must be finite",
                f"construction.{channel_id}.reflections",
            )
    return ConstructionChannel(
        channel_id=channel_id,
        direct_events=tuple(direct_events),
        reflections=tuple(reflections),
        tails=tuple(tails),
    )


def layout_from_name(name: str) -> ChannelLayout:
    return get_layout(name)
