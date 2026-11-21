from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np


@dataclass(frozen=True)
class DegradeStage:
    name: str
    block_size: int
    temp_factor: float


@dataclass(frozen=True)
class TrackConfig:
    track_id: str
    target_bitrate_kbps: int
    resample_rate: int
    gain_db: float
    video_quantization: int
    memory_limit_bytes: int
    degrade_stages: tuple[DegradeStage, ...]
    params_version: str
    raw: dict[str, Any]


@dataclass(frozen=True)
class AppConfig:
    schema_version: int
    slice_duration_ms: int
    global_parallelism: int
    memory_budget_bytes: int
    slice_memory_budget_bytes: int
    decode_buffer_capacity_ms: int
    target_buffer_ms: int
    low_buffer_ms: int
    max_decode_buffer_bytes: int
    tracks: tuple[TrackConfig, ...]
    failure_injection: dict[str, str]
    raw: dict[str, Any]
    config_version: str

    @property
    def tracks_by_id(self) -> dict[str, TrackConfig]:
        return {track.track_id: track for track in self.tracks}


@dataclass(frozen=True)
class VideoFrame:
    display_index: int
    pts_ticks: int
    duration_ticks: int

    @property
    def end_ticks(self) -> int:
        return self.pts_ticks + self.duration_ticks


@dataclass(frozen=True)
class SourceAudio:
    path: Path
    sha256: str
    sample_rate: int
    channels: int
    samples: np.ndarray

    @property
    def sample_count(self) -> int:
        return int(self.samples.shape[0])


@dataclass(frozen=True)
class VideoDescription:
    path: Path
    sha256: str
    timescale: int
    frames: tuple[VideoFrame, ...]

    @property
    def end_ticks(self) -> int:
        return self.frames[-1].end_ticks


@dataclass(frozen=True)
class SliceBoundary:
    index: int
    start_ticks: int
    end_ticks: int
    start_source_sample: int
    end_source_sample: int
    output_samples_by_track: dict[str, int]

    @property
    def ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.ticks,
            "start_source_sample": self.start_source_sample,
            "end_source_sample": self.end_source_sample,
            "source_sample_count": self.end_source_sample - self.start_source_sample,
            "output_samples_by_track": dict(sorted(self.output_samples_by_track.items())),
        }


@dataclass(frozen=True)
class Timeline:
    ticks_per_second: int
    source_sample_rate: int
    source_channels: int
    source_sample_count: int
    source_end_ticks: int
    video_timescale: int
    video_end_ticks: int
    slice_duration_ticks: int
    boundaries: tuple[SliceBoundary, ...]
    track_sample_rates: dict[str, int]
    source_audio_sha256: str
    source_video_sha256: str

    @property
    def boundaries_sha256(self) -> str:
        from .util import sha256_obj

        return sha256_obj(self.boundary_table())

    def boundary_table(self) -> dict[str, Any]:
        return {
            "schema_version": 1,
            "ticks_per_second": self.ticks_per_second,
            "source_sample_rate": self.source_sample_rate,
            "source_channels": self.source_channels,
            "source_sample_count": self.source_sample_count,
            "source_end_ticks": self.source_end_ticks,
            "video_timescale": self.video_timescale,
            "video_end_ticks": self.video_end_ticks,
            "slice_duration_ticks": self.slice_duration_ticks,
            "slice_count": len(self.boundaries),
            "source_audio_sha256": self.source_audio_sha256,
            "source_video_sha256": self.source_video_sha256,
            "slices": [boundary.to_dict() for boundary in self.boundaries],
            "tracks": {
                track_id: {
                    "resample_rate": sample_rate,
                    "slices": [
                        {
                            "index": boundary.index,
                            "start_ticks": boundary.start_ticks,
                            "end_ticks": boundary.end_ticks,
                            "start_source_sample": boundary.start_source_sample,
                            "end_source_sample": boundary.end_source_sample,
                            "start_output_sample": boundary.start_ticks
                            * sample_rate
                            // self.ticks_per_second,
                            "end_output_sample": boundary.end_ticks
                            * sample_rate
                            // self.ticks_per_second,
                            "output_sample_count": boundary.output_samples_by_track[track_id],
                        }
                        for boundary in self.boundaries
                    ],
                }
                for track_id, sample_rate in sorted(self.track_sample_rates.items())
            },
        }

