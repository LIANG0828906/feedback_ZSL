"""Configuration and track data models."""

from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import E_CONFIG, MixError

SUPPORTED_INPUT_RATES = (16000, 48000)


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    )


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _as_bool(value: Any, default: bool = False) -> bool:
    if value is None:
        return default
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        return value.strip().lower() in {"1", "true", "yes", "on"}
    return bool(value)


def _resolve_path(base: Path, value: str | os.PathLike[str]) -> Path:
    path = Path(value)
    return path if path.is_absolute() else (base / path)


@dataclass
class TrackSpec:
    id: str
    path: str
    gain_db: float = 0.0
    mute_ranges: list[list[float]] = field(default_factory=list)
    delay_samples: int = 0
    version: int = 1
    sample_rate: int | None = None
    channels: int | None = None
    max_delay_samples: int | None = None

    @classmethod
    def from_dict(
        cls,
        value: dict[str, Any],
        index: int,
        *,
        output_rate: int | None = None,
    ) -> "TrackSpec":
        if not isinstance(value, dict):
            raise MixError(E_CONFIG, "track entries must be objects", details={"track_index": index})
        track_id = str(value.get("id") or f"track_{index}")
        path = value.get("path")
        if not path:
            raise MixError(E_CONFIG, "track path is required", track_id=track_id)
        try:
            gain = float(value.get("gain_db", 0.0))
            delay = int(value.get("delay_samples", 0))
            version = int(value.get("version", 1))
        except (TypeError, ValueError) as exc:
            raise MixError(E_CONFIG, f"invalid numeric track field: {exc}", track_id=track_id) from exc
        if not math.isfinite(gain):
            raise MixError(E_CONFIG, "gain_db must be finite", track_id=track_id)
        if version < 1:
            raise MixError(E_CONFIG, "track version must be >= 1", track_id=track_id)

        raw_mutes = value.get("mute_ranges", [])
        if raw_mutes is None:
            raw_mutes = []
        if not isinstance(raw_mutes, list):
            raise MixError(E_CONFIG, "mute_ranges must be a list", track_id=track_id)
        mutes: list[list[float]] = []
        for raw in raw_mutes:
            if isinstance(raw, dict):
                unit = str(raw.get("unit", "seconds"))
                if "start_sample" in raw or "end_sample" in raw or unit in {"samples", "sample"}:
                    if output_rate is None:
                        raise MixError(
                            E_CONFIG,
                            "sample-based mute ranges require an output sample rate",
                            track_id=track_id,
                        )
                    start = float(raw.get("start_sample", raw.get("start", 0.0))) / output_rate
                    end = float(raw.get("end_sample", raw.get("end", 0.0))) / output_rate
                else:
                    start = float(raw.get("start", 0.0))
                    end = float(raw.get("end", 0.0))
            elif isinstance(raw, (list, tuple)) and len(raw) == 2:
                start = float(raw[0])
                end = float(raw[1])
            else:
                raise MixError(
                    E_CONFIG,
                    "each mute range must be [start_seconds, end_seconds]",
                    track_id=track_id,
                )
            if not math.isfinite(start) or not math.isfinite(end) or end < start:
                raise MixError(E_CONFIG, "invalid mute range", track_id=track_id)
            mutes.append([start, end])

        sample_rate = value.get("sample_rate")
        channels = value.get("channels")
        max_delay = value.get("max_delay_samples")
        return cls(
            id=track_id,
            path=str(path),
            gain_db=gain,
            mute_ranges=mutes,
            delay_samples=delay,
            version=version,
            sample_rate=int(sample_rate) if sample_rate is not None else None,
            channels=int(channels) if channels is not None else None,
            max_delay_samples=int(max_delay) if max_delay is not None else None,
        )

    def to_index_dict(self) -> dict[str, Any]:
        # Keep the original reference-schema fields first.  The source metadata
        # is additive and lets a recovery read the right WAV after a restart.
        value: dict[str, Any] = {
            "id": self.id,
            "path": self.path,
            "gain_db": self.gain_db,
            "mute_ranges": self.mute_ranges,
            "delay_samples": self.delay_samples,
            "version": self.version,
        }
        if self.sample_rate is not None:
            value["sample_rate"] = self.sample_rate
        if self.channels is not None:
            value["channels"] = self.channels
        if self.max_delay_samples is not None:
            value["max_delay_samples"] = self.max_delay_samples
        return value

    def parameter_payload(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "path": self.path,
            "gain_db": self.gain_db,
            "mute_ranges": self.mute_ranges,
            "delay_samples": self.delay_samples,
            "version": self.version,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "max_delay_samples": self.max_delay_samples,
        }

    def effective_mute_intervals(self, output_rate: int) -> list[tuple[int, int]]:
        intervals = []
        for start, end in self.mute_ranges:
            # The reference timing model shifts both mute edges by the track's
            # delay.  floor() is used so a range beginning at a fractional
            # output sample consistently starts at that sample.
            a = math.floor(start * output_rate) + self.delay_samples
            b = math.floor(end * output_rate) + self.delay_samples
            if b > a:
                intervals.append((a, b))
        return merge_intervals(intervals)


@dataclass
class MixConfig:
    job_id: str
    output_rate: int
    output_channels: int
    slice_samples: int
    tracks: list[TrackSpec]
    total_samples: int | None = None
    bit_depth: int = 16
    max_workers: int = 1
    per_slice_budget_bytes: int = 128 * 1024 * 1024
    memory_budget_bytes: int | None = None
    block_samples: int | None = None
    task_priorities: dict[int, int] = field(default_factory=dict)
    output_dir: str = "output"
    trace_path: str = "trace.jsonl"
    max_delay_samples: int = 480000
    schema_version: int = 1

    @classmethod
    def from_dict(
        cls,
        value: dict[str, Any],
        *,
        config_dir: str | os.PathLike[str] = ".",
        output_dir_override: str | os.PathLike[str] | None = None,
        trace_override: str | os.PathLike[str] | None = None,
    ) -> "MixConfig":
        if not isinstance(value, dict):
            raise MixError(E_CONFIG, "configuration root must be an object")
        output = value.get("output", value)
        if not isinstance(output, dict):
            raise MixError(E_CONFIG, "output configuration must be an object")
        try:
            rate = int(output.get("sample_rate", output.get("rate", value.get("sample_rate", 48000))))
            channels = int(output.get("channels", value.get("channels", 1)))
            slice_samples = int(
                output.get(
                    "slice_samples",
                    value.get("slice_samples", output.get("slice_size", value.get("slice_size", 0))),
                )
            )
            bit_depth = int(output.get("bit_depth", value.get("bit_depth", 16)))
        except (TypeError, ValueError) as exc:
            raise MixError(E_CONFIG, f"invalid output field: {exc}") from exc
        if rate not in SUPPORTED_INPUT_RATES:
            raise MixError(E_CONFIG, "output sample_rate must be 16000 or 48000", details={"sample_rate": rate})
        if channels not in (1, 2):
            raise MixError(E_CONFIG, "output channels must be 1 or 2", details={"channels": channels})
        if slice_samples <= 0:
            raise MixError(E_CONFIG, "slice_samples must be positive")
        if bit_depth != 16:
            raise MixError(E_CONFIG, "only 16-bit PCM output is supported", details={"bit_depth": bit_depth})

        raw_tracks = value.get("tracks")
        if not isinstance(raw_tracks, list) or not raw_tracks:
            raise MixError(E_CONFIG, "at least one track is required")
        tracks = [TrackSpec.from_dict(item, i, output_rate=rate) for i, item in enumerate(raw_tracks)]
        if len({t.id for t in tracks}) != len(tracks):
            raise MixError(E_CONFIG, "track ids must be unique")

        scheduler = value.get("scheduler", {})
        if not isinstance(scheduler, dict):
            raise MixError(E_CONFIG, "scheduler configuration must be an object")

        def choose(*keys: str, default: Any = None) -> Any:
            for key in keys:
                if key in scheduler:
                    return scheduler[key]
                if key in value:
                    return value[key]
            return default

        try:
            max_workers = int(choose("max_workers", "parallelism", default=1))
            per_slice = int(
                choose(
                    "per_slice_budget_bytes",
                    "slice_memory_budget_bytes",
                    "per_slice_memory_bytes",
                    default=128 * 1024 * 1024,
                )
            )
            global_budget_raw = choose(
                "memory_budget_bytes",
                "total_memory_budget_bytes",
                "global_memory_budget_bytes",
                default=None,
            )
            block_samples_raw = choose("block_samples", "chunk_samples", default=None)
            priority_raw = choose("task_priorities", "priorities", default={})
            total_raw = output.get("total_samples", value.get("total_samples", choose("total_samples", default=None)))
            max_delay = int(choose("max_delay_samples", "delay_limit_samples", default=480000))
        except (TypeError, ValueError) as exc:
            raise MixError(E_CONFIG, f"invalid scheduler field: {exc}") from exc
        if max_workers < 1:
            raise MixError(E_CONFIG, "max_workers must be >= 1")
        if per_slice <= 0:
            raise MixError(E_CONFIG, "per_slice_budget_bytes must be positive")
        if global_budget_raw is not None and int(global_budget_raw) <= 0:
            raise MixError(E_CONFIG, "memory_budget_bytes must be positive")
        if block_samples_raw is not None and int(block_samples_raw) <= 0:
            raise MixError(E_CONFIG, "block_samples must be positive")
        if total_raw is not None and int(total_raw) < 0:
            raise MixError(E_CONFIG, "total_samples must be non-negative")
        if max_delay < 0:
            raise MixError(E_CONFIG, "max_delay_samples must be non-negative")
        if priority_raw is None:
            priority_raw = {}
        if isinstance(priority_raw, list):
            priority_raw = {str(index): value for index, value in enumerate(priority_raw)}
        if not isinstance(priority_raw, dict):
            raise MixError(E_CONFIG, "task_priorities must be an object or list")
        task_priorities: dict[int, int] = {}
        for key, priority in priority_raw.items():
            try:
                task_priorities[int(key)] = int(priority)
            except (TypeError, ValueError) as exc:
                raise MixError(E_CONFIG, "task priority keys and values must be integers") from exc

        job_id = str(value.get("job_id") or value.get("id") or "mix_job")
        output_value = (
            str(Path(output_dir_override).resolve())
            if output_dir_override is not None
            else str(value.get("output_dir", "output"))
        )
        trace_value = (
            str(Path(trace_override).resolve())
            if trace_override is not None
            else str(value.get("trace_path", "trace.jsonl"))
        )
        cfg = cls(
            job_id=job_id,
            output_rate=rate,
            output_channels=channels,
            slice_samples=slice_samples,
            tracks=tracks,
            total_samples=int(total_raw) if total_raw is not None else None,
            bit_depth=bit_depth,
            max_workers=max_workers,
            per_slice_budget_bytes=per_slice,
            memory_budget_bytes=int(global_budget_raw) if global_budget_raw is not None else None,
            block_samples=int(block_samples_raw) if block_samples_raw is not None else None,
            task_priorities=task_priorities,
            output_dir=output_value,
            trace_path=trace_value,
            max_delay_samples=max_delay,
        )
        cfg._config_dir = Path(config_dir)
        return cfg

    @classmethod
    def from_file(
        cls,
        path: str | os.PathLike[str],
        *,
        output_dir_override: str | os.PathLike[str] | None = None,
        trace_override: str | os.PathLike[str] | None = None,
    ) -> "MixConfig":
        config_path = Path(path)
        try:
            with config_path.open("r", encoding="utf-8") as handle:
                value = json.load(handle)
        except (OSError, json.JSONDecodeError) as exc:
            raise MixError(E_CONFIG, f"cannot read configuration: {exc}", details={"path": str(path)}) from exc
        cfg = cls.from_dict(
            value,
            config_dir=config_path.parent,
            output_dir_override=output_dir_override,
            trace_override=trace_override,
        )
        cfg._config_path = config_path
        return cfg

    @property
    def config_dir(self) -> Path:
        return getattr(self, "_config_dir", Path("."))

    @property
    def config_path(self) -> Path | None:
        return getattr(self, "_config_path", None)

    @property
    def output_path(self) -> Path:
        return _resolve_path(self.config_dir, self.output_dir)

    @property
    def trace_file(self) -> Path:
        return _resolve_path(self.config_dir, self.trace_path)

    def track_path(self, track: TrackSpec) -> Path:
        return _resolve_path(self.config_dir, track.path)

    @property
    def total_slices(self) -> int:
        total = self.resolved_total_samples()
        return math.ceil(total / self.slice_samples)

    def resolved_total_samples(self) -> int:
        if self.total_samples is not None:
            return self.total_samples
        # This fallback is only used for configurations that omit total
        # duration.  The actual WAV lengths are available after loading, so the
        # engine calls ``set_total_from_sources`` before rendering.
        return 0

    def set_total_from_sources(self, source_frames: dict[str, tuple[int, int]]) -> None:
        if self.total_samples is not None:
            return
        max_end = 0
        for track in self.tracks:
            frames, rate = source_frames[track.id]
            output_frames = math.ceil(frames * self.output_rate / rate) + max(0, track.delay_samples)
            max_end = max(max_end, output_frames)
        self.total_samples = max_end

    def with_output_dir(self, path: str | os.PathLike[str]) -> "MixConfig":
        clone = MixConfig.from_dict(
            self.to_dict(),
            config_dir=self.config_dir,
            output_dir_override=path,
            trace_override=self.trace_file,
        )
        clone.total_samples = self.total_samples
        return clone

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "job_id": self.job_id,
            "output": {
                "sample_rate": self.output_rate,
                "channels": self.output_channels,
                "bit_depth": self.bit_depth,
                "slice_samples": self.slice_samples,
                "total_samples": self.total_samples,
            },
            "tracks": [track.to_index_dict() for track in self.tracks],
            "scheduler": {
                "max_workers": self.max_workers,
                "per_slice_budget_bytes": self.per_slice_budget_bytes,
                "memory_budget_bytes": self.memory_budget_bytes,
                "block_samples": self.block_samples,
                "task_priorities": {str(key): value for key, value in sorted(self.task_priorities.items())},
            },
            "output_dir": self.output_dir,
            "trace_path": self.trace_path,
            "max_delay_samples": self.max_delay_samples,
            "task_priorities": {str(key): value for key, value in sorted(self.task_priorities.items())},
        }

    def output_contract(self) -> dict[str, Any]:
        return {
            "sample_rate": self.output_rate,
            "channels": self.output_channels,
            "bit_depth": self.bit_depth,
            "slice_samples": self.slice_samples,
            "total_samples": self.resolved_total_samples(),
        }

    def track_versions(self) -> dict[str, int]:
        return {track.id: track.version for track in self.tracks}

    def params_fingerprint(self) -> str:
        payload = {
            "output": self.output_contract(),
            "tracks": [track.parameter_payload() for track in self.tracks],
        }
        return sha256_text(canonical_json(payload))

    def artifact_version(self) -> int:
        return max(track.version for track in self.tracks)


def merge_intervals(intervals: Iterable[tuple[int, int]]) -> list[tuple[int, int]]:
    values = sorted((int(a), int(b)) for a, b in intervals if int(b) > int(a))
    if not values:
        return []
    merged = [values[0]]
    for start, end in values[1:]:
        last_start, last_end = merged[-1]
        if start <= last_end:
            merged[-1] = (last_start, max(last_end, end))
        else:
            merged.append((start, end))
    return merged


def interval_difference(
    left: Iterable[tuple[int, int]], right: Iterable[tuple[int, int]]
) -> list[tuple[int, int]]:
    """Return the parts of ``left`` not covered by ``right``."""

    result: list[tuple[int, int]] = []
    right_merged = merge_intervals(right)
    for start, end in merge_intervals(left):
        cursor = start
        for r_start, r_end in right_merged:
            if r_end <= cursor:
                continue
            if r_start >= end:
                break
            if r_start > cursor:
                result.append((cursor, min(r_start, end)))
            cursor = max(cursor, r_end)
            if cursor >= end:
                break
        if cursor < end:
            result.append((cursor, end))
    return merge_intervals(result)


def changed_mute_intervals(
    old: TrackSpec, new: TrackSpec, output_rate: int
) -> list[tuple[int, int]]:
    old_ranges = old.effective_mute_intervals(output_rate)
    new_ranges = new.effective_mute_intervals(output_rate)
    return merge_intervals(
        interval_difference(old_ranges, new_ranges) + interval_difference(new_ranges, old_ranges)
    )
