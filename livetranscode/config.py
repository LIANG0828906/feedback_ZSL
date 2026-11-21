from __future__ import annotations

from typing import Any

from .errors import ErrorCode, build_error
from .models import AppConfig, DegradeStage, TrackConfig
from .util import ensure_safe_id, sha256_obj

DEFAULT_DEGRADE_STAGES = (
    {"name": "full", "block_size": 65536, "temp_factor": 1.0},
    {"name": "shrink_block", "block_size": 16384, "temp_factor": 0.75},
    {"name": "minimal_buffers", "block_size": 4096, "temp_factor": 0.5},
)


def _positive_int(value: Any, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise build_error(ErrorCode.CONFIG_INVALID, f"{field} must be a positive integer", field=field, value=value)
    return value


def _nonnegative_int(value: Any, field: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise build_error(ErrorCode.CONFIG_INVALID, f"{field} must be a non-negative integer", field=field, value=value)
    return value


def _number(value: Any, field: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise build_error(ErrorCode.CONFIG_INVALID, f"{field} must be a number", field=field, value=value)
    return float(value)


def _parse_stages(raw: Any, track_field: str) -> tuple[DegradeStage, ...]:
    if raw is None:
        raw = list(DEFAULT_DEGRADE_STAGES)
    if not isinstance(raw, list) or not raw:
        raise build_error(
            ErrorCode.CONFIG_INVALID,
            "degrade_stages must be a non-empty list",
            field=f"{track_field}.degrade_stages",
        )
    stages: list[DegradeStage] = []
    previous_block: int | None = None
    previous_temp = 2.0
    for index, item in enumerate(raw):
        field = f"{track_field}.degrade_stages[{index}]"
        if not isinstance(item, dict):
            raise build_error(ErrorCode.CONFIG_INVALID, "degrade stage must be an object", field=field)
        name = item.get("name")
        if not isinstance(name, str) or not name:
            raise build_error(ErrorCode.CONFIG_INVALID, "degrade stage name is required", field=f"{field}.name")
        block = _positive_int(item.get("block_size"), f"{field}.block_size")
        factor = _number(item.get("temp_factor"), f"{field}.temp_factor")
        if factor <= 0 or factor > 1:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "temp_factor must be in (0, 1]",
                field=f"{field}.temp_factor",
                value=factor,
            )
        if previous_block is not None and block > previous_block:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "degrade sequence must not increase block_size",
                field=f"{field}.block_size",
            )
        if factor > previous_temp:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "degrade sequence must not increase temp_factor",
                field=f"{field}.temp_factor",
            )
        previous_block = block
        previous_temp = factor
        stages.append(DegradeStage(name=name, block_size=block, temp_factor=factor))
    return tuple(stages)


def parse_config(raw: Any) -> AppConfig:
    if not isinstance(raw, dict):
        raise build_error(ErrorCode.CONFIG_INVALID, "config root must be an object")
    schema_version = raw.get("schema_version", 1)
    if schema_version != 1:
        raise build_error(
            ErrorCode.CONFIG_INVALID,
            "unsupported config schema_version",
            schema_version=schema_version,
        )
    slice_duration_ms = _positive_int(raw.get("slice_duration_ms"), "slice_duration_ms")
    parallelism = _positive_int(raw.get("global_parallelism"), "global_parallelism")
    memory_budget = _positive_int(raw.get("memory_budget_bytes"), "memory_budget_bytes")
    slice_memory_budget = _positive_int(
        raw.get("slice_memory_budget_bytes", memory_budget),
        "slice_memory_budget_bytes",
    )
    if slice_memory_budget > memory_budget:
        raise build_error(
            ErrorCode.CONFIG_INVALID,
            "slice_memory_budget_bytes must not exceed memory_budget_bytes",
            slice_memory_budget_bytes=slice_memory_budget,
            memory_budget_bytes=memory_budget,
        )
    raw_tracks = raw.get("tracks")
    if not isinstance(raw_tracks, list) or not raw_tracks:
        raise build_error(ErrorCode.CONFIG_INVALID, "tracks must be a non-empty list", field="tracks")
    tracks: list[TrackConfig] = []
    seen_ids: set[str] = set()
    for index, item in enumerate(raw_tracks):
        field = f"tracks[{index}]"
        if not isinstance(item, dict):
            raise build_error(ErrorCode.CONFIG_INVALID, "track must be an object", field=field)
        track_id = ensure_safe_id(item.get("id"), f"{field}.id")
        if track_id in seen_ids:
            raise build_error(ErrorCode.CONFIG_INVALID, "duplicate track id", track_id=track_id)
        seen_ids.add(track_id)
        bitrate = _positive_int(item.get("target_bitrate_kbps"), f"{field}.target_bitrate_kbps")
        if bitrate > 100_000:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "target_bitrate_kbps is unrealistically large",
                field=f"{field}.target_bitrate_kbps",
            )
        audio_raw = item.get("audio", {})
        video_raw = item.get("video", {})
        if not isinstance(audio_raw, dict) or not isinstance(video_raw, dict):
            raise build_error(ErrorCode.CONFIG_INVALID, "audio and video must be objects", field=field)
        resample_rate = _positive_int(audio_raw.get("resample_rate", 48000), f"{field}.audio.resample_rate")
        if not 8000 <= resample_rate <= 192000:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "resample_rate must be between 8000 and 192000",
                field=f"{field}.audio.resample_rate",
                value=resample_rate,
            )
        gain_db = _number(audio_raw.get("gain_db", 0.0), f"{field}.audio.gain_db")
        if gain_db < -96 or gain_db > 24:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "gain_db must be between -96 and 24",
                field=f"{field}.audio.gain_db",
            )
        quantization = _positive_int(video_raw.get("quantization", 8), f"{field}.video.quantization")
        if quantization > 255:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "video quantization must be <= 255",
                field=f"{field}.video.quantization",
            )
        track_memory = _positive_int(item.get("memory_limit_bytes", slice_memory_budget), f"{field}.memory_limit_bytes")
        if track_memory > memory_budget:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "track memory_limit_bytes must not exceed memory_budget_bytes",
                field=f"{field}.memory_limit_bytes",
            )
        stages = _parse_stages(item.get("degrade_stages"), field)
        normalized_track = {
            "id": track_id,
            "target_bitrate_kbps": bitrate,
            "audio": {"resample_rate": resample_rate, "gain_db": gain_db},
            "video": {"quantization": quantization},
            "memory_limit_bytes": track_memory,
            "degrade_stages": [
                {"name": stage.name, "block_size": stage.block_size, "temp_factor": stage.temp_factor}
                for stage in stages
            ],
        }
        tracks.append(
            TrackConfig(
                track_id=track_id,
                target_bitrate_kbps=bitrate,
                resample_rate=resample_rate,
                gain_db=gain_db,
                video_quantization=quantization,
                memory_limit_bytes=track_memory,
                degrade_stages=stages,
                params_version=sha256_obj(normalized_track),
                raw=normalized_track,
            )
        )
    capacity = _positive_int(raw.get("decode_buffer_capacity_ms", 6000), "decode_buffer_capacity_ms")
    target = _positive_int(raw.get("target_buffer_ms", 1800), "target_buffer_ms")
    low = _nonnegative_int(raw.get("low_buffer_ms", 800), "low_buffer_ms")
    if not low < target <= capacity:
        raise build_error(
            ErrorCode.CONFIG_INVALID,
            "buffer thresholds must satisfy low_buffer_ms < target_buffer_ms <= decode_buffer_capacity_ms",
            low_buffer_ms=low,
            target_buffer_ms=target,
            decode_buffer_capacity_ms=capacity,
        )
    max_decode_bytes = _positive_int(raw.get("max_decode_buffer_bytes", memory_budget), "max_decode_buffer_bytes")
    injection_raw = raw.get("failure_injection", {})
    if not isinstance(injection_raw, dict):
        raise build_error(ErrorCode.CONFIG_INVALID, "failure_injection must be an object")
    injection: dict[str, str] = {}
    for key, value in injection_raw.items():
        if not isinstance(key, str) or not isinstance(value, str) or ":" not in key:
            raise build_error(
                ErrorCode.CONFIG_INVALID,
                "failure_injection keys must be '<track>:<slice>' and values must be strings",
            )
        injection[key] = value
    normalized = {
        "schema_version": 1,
        "slice_duration_ms": slice_duration_ms,
        "global_parallelism": parallelism,
        "memory_budget_bytes": memory_budget,
        "slice_memory_budget_bytes": slice_memory_budget,
        "decode_buffer_capacity_ms": capacity,
        "target_buffer_ms": target,
        "low_buffer_ms": low,
        "max_decode_buffer_bytes": max_decode_bytes,
        "tracks": [track.raw for track in tracks],
        "failure_injection": dict(sorted(injection.items())),
    }
    return AppConfig(
        schema_version=1,
        slice_duration_ms=slice_duration_ms,
        global_parallelism=parallelism,
        memory_budget_bytes=memory_budget,
        slice_memory_budget_bytes=slice_memory_budget,
        decode_buffer_capacity_ms=capacity,
        target_buffer_ms=target,
        low_buffer_ms=low,
        max_decode_buffer_bytes=max_decode_bytes,
        tracks=tuple(tracks),
        failure_injection=injection,
        raw=normalized,
        config_version=sha256_obj(normalized),
    )


def config_from_dict(raw: Any) -> AppConfig:
    return parse_config(raw)
