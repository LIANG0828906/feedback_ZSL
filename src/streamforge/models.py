from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .errors import BusinessError, ErrorCode, fail

GENESIS_HASH = "0" * 64
SLICE_MAGIC = b"FSLICE01"
GENERATOR_VERSION = 1


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_json(value: Any) -> str:
    return sha256_hex(canonical_json(value))


def round_div(value: int, divisor: int) -> int:
    if divisor <= 0:
        raise ValueError("divisor must be positive")
    return (value + divisor // 2) // divisor


def ns_to_sample(time_ns: int, sample_rate: int) -> int:
    return round_div(time_ns * sample_rate, 1_000_000_000)


@dataclass(frozen=True)
class VideoFrame:
    display_index: int
    pts_ns: int
    duration_ns: int
    source_index: int

    @property
    def end_ns(self) -> int:
        return self.pts_ns + self.duration_ns

    def to_dict(self) -> dict[str, int]:
        return {
            "display_index": self.display_index,
            "duration_ns": self.duration_ns,
            "end_ns": self.end_ns,
            "pts_ns": self.pts_ns,
            "source_index": self.source_index,
        }


@dataclass(frozen=True)
class AudioSource:
    path: Path
    sample_rate: int
    channels: int
    samples: np.ndarray
    raw_digest: str

    @property
    def sample_count(self) -> int:
        return int(self.samples.shape[0])

    @property
    def duration_ns(self) -> int:
        return self.sample_count * 1_000_000_000 // self.sample_rate


@dataclass(frozen=True)
class SliceBoundary:
    index: int
    start_ns: int
    end_ns: int
    source_start_sample: int
    source_end_sample: int

    @property
    def duration_ns(self) -> int:
        return self.end_ns - self.start_ns

    def to_dict(self) -> dict[str, int]:
        return {
            "end_ns": self.end_ns,
            "index": self.index,
            "source_end_sample": self.source_end_sample,
            "source_start_sample": self.source_start_sample,
            "start_ns": self.start_ns,
        }


@dataclass
class SourceTimeline:
    audio: AudioSource
    video_frames: list[VideoFrame]
    video_frames_by_pts: dict[int, VideoFrame]
    boundaries: list[SliceBoundary]
    total_ns: int
    slice_duration_ns: int
    tolerance_ns: int
    video_total_ns: int
    video_digest: str
    digest: str = ""

    def __post_init__(self) -> None:
        if not self.digest:
            self.digest = sha256_json(
                {
                    "audio_digest": self.audio.raw_digest,
                    "boundaries": [b.to_dict() for b in self.boundaries],
                    "slice_duration_ns": self.slice_duration_ns,
                    "tolerance_ns": self.tolerance_ns,
                    "video_digest": self.video_digest,
                }
            )

    def boundary_dicts(self) -> list[dict[str, int]]:
        return [boundary.to_dict() for boundary in self.boundaries]

    def frames_for_boundary(self, boundary: SliceBoundary) -> list[VideoFrame]:
        frames: list[VideoFrame] = []
        for frame in self.video_frames:
            if boundary.start_ns <= frame.pts_ns < boundary.end_ns:
                frames.append(frame)
            elif frame.pts_ns >= boundary.end_ns:
                break
        return frames


@dataclass(frozen=True)
class TierSpec:
    tier_id: str
    target_bitrate_kbps: int
    audio_sample_rate: int
    gain_db: float
    video_qp: int

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "TierSpec":
        if not isinstance(value, dict):
            raise fail(ErrorCode.INVALID_CONFIG, "tier must be an object")
        try:
            tier_id = str(value["id"])
            bitrate = int(value["target_bitrate_kbps"])
            rate = int(value["audio_sample_rate"])
            gain = float(value.get("gain_db", 0.0))
            qp = int(value.get("video_qp", 28))
        except (KeyError, TypeError, ValueError) as exc:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "invalid tier fields",
                error=str(exc),
                tier=value,
            ) from exc
        spec = cls(tier_id, bitrate, rate, gain, qp)
        spec.validate()
        return spec

    def validate(self) -> None:
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", self.tier_id):
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "tier id contains unsupported characters",
                tier=self.tier_id,
            )
        if self.target_bitrate_kbps <= 0:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "target bitrate must be positive",
                tier=self.tier_id,
            )
        if not 8000 <= self.audio_sample_rate <= 192000:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "tier audio sample rate out of range",
                tier=self.tier_id,
                sample_rate=self.audio_sample_rate,
            )
        if not -60.0 <= self.gain_db <= 24.0:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "gain_db out of range",
                tier=self.tier_id,
                gain_db=self.gain_db,
            )
        if not 0 <= self.video_qp <= 63:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "video_qp out of range",
                tier=self.tier_id,
                video_qp=self.video_qp,
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "audio_sample_rate": self.audio_sample_rate,
            "gain_db": self.gain_db,
            "id": self.tier_id,
            "target_bitrate_kbps": self.target_bitrate_kbps,
            "video_qp": self.video_qp,
        }

    def sorted_for_starvation(self, all_tiers: Iterable["TierSpec"]) -> int:
        ordered = sorted(all_tiers, key=lambda item: item.target_bitrate_kbps)
        return next(
            index for index, item in enumerate(ordered) if item.tier_id == self.tier_id
        )


@dataclass(frozen=True)
class BudgetConfig:
    global_parallelism: int
    total_memory_bytes: int
    single_slice_memory_bytes: int
    initial_block_bytes: int
    min_block_bytes: int
    decode_buffer_bytes: int
    buffer_low_bytes: int
    buffer_high_bytes: int
    starvation_ms: int = 1500
    starvation_rounds: int = 3

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "BudgetConfig":
        if not isinstance(value, dict):
            raise fail(ErrorCode.INVALID_CONFIG, "budget document must be an object")
        names = (
            "global_parallelism",
            "total_memory_bytes",
            "single_slice_memory_bytes",
            "initial_block_bytes",
            "min_block_bytes",
            "decode_buffer_bytes",
            "buffer_low_bytes",
            "buffer_high_bytes",
        )
        try:
            parsed = {name: int(value[name]) for name in names}
        except (KeyError, TypeError, ValueError) as exc:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "invalid budget configuration",
                error=str(exc),
            ) from exc
        cfg = cls(
            **parsed,
            starvation_ms=int(value.get("starvation_ms", 1500)),
            starvation_rounds=int(value.get("starvation_rounds", 3)),
        )
        cfg.validate()
        return cfg

    def validate(self) -> None:
        if self.global_parallelism <= 0:
            raise fail(ErrorCode.INVALID_CONFIG, "global_parallelism must be positive")
        if self.total_memory_bytes <= 0 or self.single_slice_memory_bytes <= 0:
            raise fail(ErrorCode.INVALID_CONFIG, "memory budgets must be positive")
        if self.min_block_bytes <= 0:
            raise fail(ErrorCode.INVALID_CONFIG, "min_block_bytes must be positive")
        if self.initial_block_bytes < self.min_block_bytes:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "initial_block_bytes must be >= min_block_bytes",
            )
        if self.decode_buffer_bytes <= 0:
            raise fail(ErrorCode.INVALID_CONFIG, "decode_buffer_bytes must be positive")
        if not 0 <= self.buffer_low_bytes < self.buffer_high_bytes:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "buffer watermark ordering is invalid",
            )
        if self.starvation_ms < 0 or self.starvation_rounds < 1:
            raise fail(ErrorCode.INVALID_CONFIG, "starvation thresholds are invalid")

    def to_dict(self) -> dict[str, int]:
        return {
            "buffer_high_bytes": self.buffer_high_bytes,
            "buffer_low_bytes": self.buffer_low_bytes,
            "decode_buffer_bytes": self.decode_buffer_bytes,
            "global_parallelism": self.global_parallelism,
            "initial_block_bytes": self.initial_block_bytes,
            "min_block_bytes": self.min_block_bytes,
            "single_slice_memory_bytes": self.single_slice_memory_bytes,
            "starvation_ms": self.starvation_ms,
            "starvation_rounds": self.starvation_rounds,
            "total_memory_bytes": self.total_memory_bytes,
        }


@dataclass(frozen=True)
class ProjectConfig:
    config_version: str
    config_path: Path
    audio_path: Path
    video_path: Path
    slice_duration_ms: int
    tolerance_ms: int
    tiers: tuple[TierSpec, ...]
    budget: BudgetConfig

    def tier_map(self) -> dict[str, TierSpec]:
        return {tier.tier_id: tier for tier in self.tiers}


@dataclass
class MemoryPlan:
    audio_bytes: int
    video_bytes: int
    block_bytes: int
    working_bytes: int
    estimated_bytes: int
    degraded: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "audio_bytes": self.audio_bytes,
            "block_bytes": self.block_bytes,
            "degraded": self.degraded,
            "estimated_bytes": self.estimated_bytes,
            "video_bytes": self.video_bytes,
            "working_bytes": self.working_bytes,
        }


@dataclass
class SliceArtifact:
    tier_id: str
    index: int
    start_ns: int
    end_ns: int
    audio_start_ns: int
    audio_end_ns: int
    audio_sample_start: int
    audio_sample_end: int
    audio_sample_rate: int
    audio_channels: int
    audio_bytes: int
    video_start_ns: int
    video_end_ns: int
    video_bytes: int
    frames: list[dict[str, Any]]
    content_hash: str
    prev_hash: str
    file_size: int
    path: Path
    memory: MemoryPlan

    def to_dict(self) -> dict[str, Any]:
        return {
            "audio_bytes": self.audio_bytes,
            "audio_channels": self.audio_channels,
            "audio_end_ns": self.audio_end_ns,
            "audio_sample_end": self.audio_sample_end,
            "audio_sample_rate": self.audio_sample_rate,
            "audio_sample_start": self.audio_sample_start,
            "audio_start_ns": self.audio_start_ns,
            "content_hash": self.content_hash,
            "end_ns": self.end_ns,
            "file_size": self.file_size,
            "frames": self.frames,
            "index": self.index,
            "memory": self.memory.to_dict(),
            "prev_hash": self.prev_hash,
            "start_ns": self.start_ns,
            "tier": self.tier_id,
            "video_bytes": self.video_bytes,
            "video_end_ns": self.video_end_ns,
            "video_start_ns": self.video_start_ns,
        }

    @classmethod
    def from_header(
        cls,
        header: dict[str, Any],
        *,
        path: Path,
        file_size: int,
        memory: MemoryPlan | None = None,
    ) -> "SliceArtifact":
        return cls(
            tier_id=str(header["tier"]),
            index=int(header["index"]),
            start_ns=int(header["start_ns"]),
            end_ns=int(header["end_ns"]),
            audio_start_ns=int(header["audio_start_ns"]),
            audio_end_ns=int(header["audio_end_ns"]),
            audio_sample_start=int(header["audio_sample_start"]),
            audio_sample_end=int(header["audio_sample_end"]),
            audio_sample_rate=int(header["audio_sample_rate"]),
            audio_channels=int(header["audio_channels"]),
            audio_bytes=int(header["audio_bytes"]),
            video_start_ns=int(header["video_start_ns"]),
            video_end_ns=int(header["video_end_ns"]),
            video_bytes=int(header["video_bytes"]),
            frames=list(header["frames"]),
            content_hash=str(header["content_hash"]),
            prev_hash=str(header["prev_hash"]),
            file_size=file_size,
            path=path,
            memory=memory
            or MemoryPlan(
                audio_bytes=int(header["audio_bytes"]),
                video_bytes=int(header["video_bytes"]),
                block_bytes=0,
                working_bytes=0,
                estimated_bytes=int(header["audio_bytes"]) + int(header["video_bytes"]),
                degraded=False,
            ),
        )


def validate_unique_tiers(tiers: Iterable[TierSpec]) -> tuple[TierSpec, ...]:
    values = tuple(tiers)
    seen: set[str] = set()
    for tier in values:
        if tier.tier_id in seen:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "duplicate tier id",
                tier=tier.tier_id,
            )
        seen.add(tier.tier_id)
    if not values:
        raise fail(ErrorCode.INVALID_CONFIG, "at least one tier is required")
    return values
