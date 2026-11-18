"""Project schema, strict validation, and deterministic input fingerprints."""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import ValidationFailure, error_item
from .wavio import inspect_wav


_MISSING = object()


def _pick(mapping: dict[str, Any], *names: str, default: Any = _MISSING) -> Any:
    for name in names:
        if name in mapping:
            return mapping[name]
    if default is _MISSING:
        raise KeyError(names[0])
    return default


def _as_float(value: Any, default: float | None = None) -> float | None:
    if value is None:
        return default
    if isinstance(value, bool):
        raise ValueError("boolean is not a number")
    return float(value)


def _as_int(value: Any, default: int | None = None) -> int | None:
    if value is None:
        return default
    if isinstance(value, bool):
        raise ValueError("boolean is not an integer")
    number = float(value)
    if not number.is_integer():
        raise ValueError("value is not an integer")
    return int(number)


def _finite(value: float) -> bool:
    return math.isfinite(value)


def _relative_display_path(value: str) -> str:
    path = Path(value)
    return path.name if path.is_absolute() else value.replace("\\", "/")


@dataclass(frozen=True)
class TimelinePoint:
    time_seconds: float
    azimuth_deg: float
    elevation_deg: float

    def to_dict(self) -> dict[str, float]:
        return {
            "time_seconds": self.time_seconds,
            "azimuth_deg": self.azimuth_deg,
            "elevation_deg": self.elevation_deg,
        }


@dataclass(frozen=True)
class ActorSpec:
    id: str
    channel: str
    channels: int
    azimuth_deg: float
    elevation_deg: float
    timeline: tuple[TimelinePoint, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "channel": _relative_display_path(self.channel),
            "channels": self.channels,
            "azimuth_deg": self.azimuth_deg,
            "elevation_deg": self.elevation_deg,
            "timeline": [point.to_dict() for point in self.timeline],
        }


@dataclass(frozen=True)
class SpeakerSpec:
    id: str
    channel: str
    channels: int
    azimuth_deg: float
    elevation_deg: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "channel": _relative_display_path(self.channel),
            "channels": self.channels,
            "azimuth_deg": self.azimuth_deg,
            "elevation_deg": self.elevation_deg,
        }


@dataclass(frozen=True)
class FeedbackSpec:
    actor_id: str
    speaker_id: str
    gain_db: float
    delay_ms: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "actor_id": self.actor_id,
            "speaker_id": self.speaker_id,
            "gain_db": self.gain_db,
            "delay_ms": self.delay_ms,
        }


@dataclass(frozen=True)
class ReferenceWindow:
    start_seconds: float
    end_seconds: float
    actor_id: str | None = None
    speaker_id: str | None = None
    purpose: str = "crosstalk_reference"

    def to_dict(self) -> dict[str, Any]:
        return {
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "actor_id": self.actor_id,
            "speaker_id": self.speaker_id,
            "purpose": self.purpose,
        }


@dataclass(frozen=True)
class CrosstalkConfig:
    reference_windows: tuple[ReferenceWindow, ...]
    direct_probe_windows: tuple[ReferenceWindow, ...]
    max_delay_ms: float
    residual_energy_ratio_max: float
    max_direct_energy_loss_ratio: float
    feedback: tuple[FeedbackSpec, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "reference_windows": [item.to_dict() for item in self.reference_windows],
            "direct_probe_windows": [item.to_dict() for item in self.direct_probe_windows],
            "max_delay_ms": self.max_delay_ms,
            "residual_energy_ratio_max": self.residual_energy_ratio_max,
            "max_direct_energy_loss_ratio": self.max_direct_energy_loss_ratio,
            "feedback": [item.to_dict() for item in self.feedback],
        }


@dataclass(frozen=True)
class SpatialConfig:
    head_radius_m: float
    speed_of_sound_mps: float
    parameter_block_seconds: float
    smoothing_seconds: float
    segment_report_seconds: float
    max_ild_rate_db_per_s: float
    max_itd_rate_us_per_s: float
    ild_max_db: float
    model: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "head_radius_m": self.head_radius_m,
            "speed_of_sound_mps": self.speed_of_sound_mps,
            "parameter_block_seconds": self.parameter_block_seconds,
            "smoothing_seconds": self.smoothing_seconds,
            "segment_report_seconds": self.segment_report_seconds,
            "max_ild_rate_db_per_s": self.max_ild_rate_db_per_s,
            "max_itd_rate_us_per_s": self.max_itd_rate_us_per_s,
            "ild_max_db": self.ild_max_db,
            "model": self.model,
        }


@dataclass(frozen=True)
class ExportConfig:
    format: str
    sample_rate: int
    bit_depth: int
    channels: int
    target_lufs: float
    true_peak_limit_dbtp: float
    block_frames: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "format": self.format,
            "sample_rate": self.sample_rate,
            "bit_depth": self.bit_depth,
            "channels": self.channels,
            "target_lufs": self.target_lufs,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
            "block_frames": self.block_frames,
        }


@dataclass(frozen=True)
class Project:
    schema_version: int
    project_id: str
    seed: int
    sample_rate: int
    bit_depth: int
    duration_seconds: float
    frames: int
    actors: tuple[ActorSpec, ...]
    speakers: tuple[SpeakerSpec, ...]
    crosstalk: CrosstalkConfig
    spatial: SpatialConfig
    export: ExportConfig
    source_path: str = field(default="", compare=False, repr=False)

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "project_id": self.project_id,
            "seed": self.seed,
            "sample_rate": self.sample_rate,
            "bit_depth": self.bit_depth,
            "duration_seconds": self.duration_seconds,
            "frames": self.frames,
            "actors": [actor.to_dict() for actor in self.actors],
            "speakers": [speaker.to_dict() for speaker in self.speakers],
            "crosstalk": self.crosstalk.to_dict(),
            "spatial": self.spatial.to_dict(),
            "export": self.export.to_dict(),
        }

    def canonical_json(self) -> str:
        return json.dumps(self.to_dict(), sort_keys=True, separators=(",", ":"), ensure_ascii=True)

    def content_digest(self) -> str:
        return hashlib.sha256(self.canonical_json().encode("utf-8")).hexdigest()

    def input_digest(self, audio_dir: str | Path) -> str:
        digest = hashlib.sha256()
        digest.update(self.canonical_json().encode("utf-8"))
        for channel in self.channel_order():
            path = resolve_channel_path(audio_dir, channel)
            digest.update(channel.encode("utf-8"))
            with path.open("rb") as fh:
                for block in iter(lambda: fh.read(1024 * 1024), b""):
                    digest.update(block)
        return digest.hexdigest()

    def channel_order(self) -> list[str]:
        return [actor.channel for actor in self.actors] + [
            speaker.channel for speaker in self.speakers
        ]

    def actor_by_id(self, actor_id: str) -> ActorSpec:
        for actor in self.actors:
            if actor.id == actor_id:
                return actor
        raise KeyError(actor_id)

    def speaker_by_id(self, speaker_id: str) -> SpeakerSpec:
        for speaker in self.speakers:
            if speaker.id == speaker_id:
                return speaker
        raise KeyError(speaker_id)


def resolve_channel_path(audio_dir: str | Path, channel: str) -> Path:
    path = Path(channel)
    if path.is_absolute():
        return path
    return Path(audio_dir) / path


def load_project(
    project_path: str | Path,
    audio_dir: str | Path | None = None,
    *,
    validate_files: bool = True,
) -> Project:
    """Parse and validate a project, collecting all actionable errors."""

    path = Path(project_path)
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ValidationFailure(
            [error_item("PROJECT_FILE_MISSING", channel="project", message=str(exc))]
        ) from exc
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ValidationFailure(
            [error_item("PROJECT_JSON_INVALID", channel="project", message=str(exc))]
        ) from exc
    if not isinstance(raw, dict):
        raise ValidationFailure(
            [error_item("PROJECT_SCHEMA_ERROR", channel="project", message="Root must be an object.")]
        )

    errors: list[dict[str, Any]] = []
    project = _parse_project(raw, str(path), errors)
    if project is not None and validate_files:
        base = Path(audio_dir) if audio_dir is not None else path.parent
        _validate_audio_files(project, base, errors)
    if errors or project is None:
        raise ValidationFailure(errors or [error_item("PROJECT_SCHEMA_ERROR", channel="project")])
    return project


def _parse_project(
    raw: dict[str, Any], source_path: str, errors: list[dict[str, Any]]
) -> Project | None:
    def fail(code: str, channel: str = "project", message: str | None = None, **extra: Any) -> None:
        errors.append(error_item(code, channel=channel, message=message, **extra))

    try:
        schema_version = _as_int(_pick(raw, "schema_version", default=1))
        project_id = str(_pick(raw, "project_id", "id", default="binaural_project"))
        seed = _as_int(_pick(raw, "seed", "random_seed", default=0))
        sample_rate = _as_int(_pick(raw, "sample_rate", "sampleRate", default=48000))
        bit_depth = _as_int(_pick(raw, "bit_depth", "bitDepth", default=24))
        duration = _as_float(_pick(raw, "duration_seconds", "duration", "duration_sec", default=0.0))
    except (TypeError, ValueError, KeyError) as exc:
        fail("PROJECT_SCHEMA_ERROR", message=f"Invalid scalar project field: {exc}")
        return None

    if schema_version != 1:
        fail("UNSUPPORTED_SCHEMA_VERSION", message=f"Expected schema_version 1, got {schema_version}.")
    if sample_rate is None or sample_rate < 8000:
        fail("SAMPLE_RATE_INVALID", message="Project sample_rate must be at least 8000 Hz.")
    if bit_depth not in (16, 24, 32):
        fail("BIT_DEPTH_INVALID", message="Project bit_depth must be 16, 24, or 32.")
    if duration is None or not _finite(duration) or duration <= 0:
        fail("DURATION_INVALID", message="duration_seconds must be finite and positive.")
        duration = 0.0
    if sample_rate is None or sample_rate <= 0:
        sample_rate = 48000
    if bit_depth not in (16, 24, 32):
        bit_depth = 24
    frames = int(round(duration * sample_rate))
    if frames <= 0:
        fail("DURATION_INVALID", message="Project contains no samples.")

    actors_raw = _pick(raw, "actors", "actor_channels", default=[])
    speakers_raw = _pick(raw, "speakers", "speaker_channels", default=[])
    if not isinstance(actors_raw, list) or not actors_raw:
        fail("ACTORS_MISSING", message="At least one actor channel is required.")
        actors_raw = []
    if not isinstance(speakers_raw, list):
        fail("SPEAKERS_INVALID", message="speakers must be an array.")
        speakers_raw = []

    actors: list[ActorSpec] = []
    seen_actor_ids: set[str] = set()
    for index, item in enumerate(actors_raw):
        actor = _parse_actor(item, index, duration, errors, seen_actor_ids)
        if actor is not None:
            actors.append(actor)

    speakers: list[SpeakerSpec] = []
    seen_speaker_ids: set[str] = set()
    for index, item in enumerate(speakers_raw):
        speaker = _parse_speaker(item, index, errors)
        if speaker is not None:
            if speaker.id in seen_speaker_ids:
                fail(
                    "DUPLICATE_SPEAKER_ID",
                    channel=speaker.channel,
                    message="Speaker ids must be unique.",
                    speaker_id=speaker.id,
                )
                continue
            seen_speaker_ids.add(speaker.id)
            speakers.append(speaker)

    actor_ids = {actor.id for actor in actors}
    speaker_ids = {speaker.id for speaker in speakers}
    crosstalk_raw = raw.get("crosstalk", {})
    if not isinstance(crosstalk_raw, dict):
        fail("CROSSTALK_CONFIG_INVALID", message="crosstalk must be an object.")
        crosstalk_raw = {}

    feedback = _parse_feedback(raw, crosstalk_raw, actor_ids, speaker_ids, errors)
    reference_windows = _parse_windows(
        raw,
        crosstalk_raw,
        "reference_windows",
        "reference_segments",
        "crosstalk_reference_windows",
        purpose="crosstalk_reference",
        local_names=("reference_windows", "direct_free_windows"),
        duration=duration,
        actor_ids=actor_ids,
        speaker_ids=speaker_ids,
        errors=errors,
    )
    direct_probes = _parse_windows(
        raw,
        crosstalk_raw,
        "direct_probe_windows",
        "direct_windows",
        "actor_only_windows",
        purpose="direct_probe",
        local_names=("direct_probe_windows", "actor_only_windows"),
        duration=duration,
        actor_ids=actor_ids,
        speaker_ids=speaker_ids,
        errors=errors,
    )

    try:
        max_delay_ms = _as_float(
            _pick(crosstalk_raw, "max_delay_ms", "max_delay", default=30.0)
        )
        residual_max = _as_float(
            _pick(
                crosstalk_raw,
                "residual_energy_ratio_max",
                "residual_threshold",
                default=0.02,
            )
        )
        direct_loss_max = _as_float(
            _pick(
                crosstalk_raw,
                "max_direct_energy_loss_ratio",
                "direct_loss_max",
                default=0.05,
            )
        )
    except (TypeError, ValueError) as exc:
        fail("CROSSTALK_THRESHOLD_INVALID", message=str(exc))
        max_delay_ms, residual_max, direct_loss_max = 30.0, 0.02, 0.05
    assert max_delay_ms is not None and residual_max is not None and direct_loss_max is not None
    if not _finite(max_delay_ms) or max_delay_ms < 0 or max_delay_ms > 1000:
        fail("CROSSTALK_DELAY_BOUND_INVALID", message="max_delay_ms must be in [0, 1000].")
    if not _finite(residual_max) or residual_max <= 0:
        fail("CROSSTALK_THRESHOLD_INVALID", message="Residual ratio threshold must be positive.")
    if not _finite(direct_loss_max) or not 0 <= direct_loss_max < 1:
        fail("CROSSTALK_THRESHOLD_INVALID", message="Direct loss ratio must be in [0, 1).")

    spatial = _parse_spatial(raw.get("spatial", {}), errors)
    export = _parse_export(raw, sample_rate, bit_depth, errors)

    if feedback and not reference_windows:
        fail(
            "REFERENCE_WINDOWS_MISSING",
            channel="project",
            message="Crosstalk feedback requires declared actor-direct-free reference windows.",
        )

    if errors:
        return None
    assert export is not None
    return Project(
        schema_version=schema_version or 1,
        project_id=project_id,
        seed=seed or 0,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        duration_seconds=duration,
        frames=frames,
        actors=tuple(actors),
        speakers=tuple(speakers),
        crosstalk=CrosstalkConfig(
            reference_windows=tuple(reference_windows),
            direct_probe_windows=tuple(direct_probes),
            max_delay_ms=max_delay_ms,
            residual_energy_ratio_max=residual_max,
            max_direct_energy_loss_ratio=direct_loss_max,
            feedback=tuple(feedback),
        ),
        spatial=spatial,
        export=export,
        source_path=source_path,
    )


def _parse_actor(
    item: Any,
    index: int,
    duration: float,
    errors: list[dict[str, Any]],
    seen_ids: set[str],
) -> ActorSpec | None:
    channel_hint = f"actors[{index}]"
    if not isinstance(item, dict):
        errors.append(
            error_item("ACTOR_SCHEMA_ERROR", channel=channel_hint, message="Actor must be an object.")
        )
        return None
    try:
        actor_id = str(_pick(item, "id", "actor_id", "name", default=f"actor_{index + 1}"))
        channel = str(_pick(item, "channel", "file", "path"))
        channels = _as_int(_pick(item, "channels", "channel_count", default=1))
        azimuth = _as_float(_pick(item, "azimuth_deg", "azimuth", default=0.0))
        elevation = _as_float(_pick(item, "elevation_deg", "elevation", default=0.0))
    except (TypeError, ValueError, KeyError) as exc:
        errors.append(
            error_item("ACTOR_SCHEMA_ERROR", channel=channel_hint, message=str(exc))
        )
        return None

    if actor_id in seen_ids:
        errors.append(
            error_item(
                "DUPLICATE_ACTOR_ID",
                channel=channel,
                message="Actor ids must be unique.",
                actor_id=actor_id,
            )
        )
    seen_ids.add(actor_id)
    if channels <= 0:
        errors.append(
            error_item(
                "CHANNEL_COUNT_MISMATCH",
                channel=channel,
                message="Actor channel count must be positive.",
                expected=1,
                actual=channels,
            )
        )
    if azimuth is None or not _finite(azimuth) or not -180.0 <= azimuth <= 180.0:
        errors.append(
            error_item(
                "ACTOR_AZIMUTH_OUT_OF_RANGE",
                channel=channel,
                message="Actor azimuth must be in [-180, 180].",
                actual=azimuth,
            )
        )
        azimuth = 0.0
    if elevation is None or not _finite(elevation) or not -90.0 <= elevation <= 90.0:
        errors.append(
            error_item(
                "ACTOR_ELEVATION_OUT_OF_RANGE",
                channel=channel,
                message="Actor elevation must be in [-90, 90].",
                actual=elevation,
            )
        )
        elevation = 0.0

    timeline_raw = _pick(item, "timeline", "trajectory", "spatial_timeline", default=None)
    timeline: list[TimelinePoint] = []
    if timeline_raw is not None:
        if not isinstance(timeline_raw, list):
            errors.append(
                error_item(
                    "ACTOR_TIMELINE_INVALID",
                    channel=channel,
                    message="Actor timeline must be an array.",
                )
            )
        else:
            last_time = -1.0
            for point_index, point in enumerate(timeline_raw):
                try:
                    if isinstance(point, (list, tuple)):
                        time_s = _as_float(point[0])
                        az = _as_float(point[1])
                        elev = _as_float(point[2] if len(point) > 2 else 0.0)
                    elif isinstance(point, dict):
                        time_s = _as_float(
                            _pick(point, "time_seconds", "time", "at_seconds")
                        )
                        az = _as_float(
                            _pick(point, "azimuth_deg", "azimuth", default=azimuth)
                        )
                        elev = _as_float(
                            _pick(point, "elevation_deg", "elevation", default=elevation)
                        )
                    else:
                        raise ValueError("timeline point must be an object or [time, azimuth, elevation]")
                except (TypeError, ValueError, KeyError, IndexError) as exc:
                    errors.append(
                        error_item(
                            "ACTOR_TIMELINE_INVALID",
                            channel=channel,
                            message=f"timeline[{point_index}]: {exc}",
                        )
                    )
                    continue
                assert time_s is not None and az is not None and elev is not None
                if not _finite(time_s) or time_s < 0 or time_s > duration:
                    errors.append(
                        error_item(
                            "ACTOR_TIMELINE_OUT_OF_RANGE",
                            channel=channel,
                            message="Timeline time must be inside the program duration.",
                            index=point_index,
                            actual=time_s,
                        )
                    )
                if not _finite(az) or not -180.0 <= az <= 180.0:
                    errors.append(
                        error_item(
                            "ACTOR_AZIMUTH_OUT_OF_RANGE",
                            channel=channel,
                            message="Timeline azimuth must be in [-180, 180].",
                            index=point_index,
                            actual=az,
                        )
                    )
                if not _finite(elev) or not -90.0 <= elev <= 90.0:
                    errors.append(
                        error_item(
                            "ACTOR_ELEVATION_OUT_OF_RANGE",
                            channel=channel,
                            message="Timeline elevation must be in [-90, 90].",
                            index=point_index,
                            actual=elev,
                        )
                    )
                if time_s < last_time:
                    errors.append(
                        error_item(
                            "ACTOR_TIMELINE_NOT_SORTED",
                            channel=channel,
                            message="Timeline points must be ordered by time.",
                            index=point_index,
                        )
                    )
                last_time = time_s
                timeline.append(TimelinePoint(time_s, az, elev))

    if not timeline:
        timeline = [TimelinePoint(0.0, azimuth, elevation)]
    elif timeline[0].time_seconds > 0:
        timeline.insert(0, TimelinePoint(0.0, azimuth, elevation))

    return ActorSpec(
        id=actor_id,
        channel=channel,
        channels=channels or 1,
        azimuth_deg=azimuth,
        elevation_deg=elevation,
        timeline=tuple(timeline),
    )


def _parse_speaker(item: Any, index: int, errors: list[dict[str, Any]]) -> SpeakerSpec | None:
    channel_hint = f"speakers[{index}]"
    if not isinstance(item, dict):
        errors.append(
            error_item(
                "SPEAKER_SCHEMA_ERROR", channel=channel_hint, message="Speaker must be an object."
            )
        )
        return None
    try:
        speaker_id = str(_pick(item, "id", "speaker_id", "name", default=f"speaker_{index + 1}"))
        channel = str(_pick(item, "channel", "file", "path"))
        channels = _as_int(_pick(item, "channels", "channel_count", default=1))
        azimuth = _as_float(_pick(item, "azimuth_deg", "azimuth", default=0.0))
        elevation = _as_float(_pick(item, "elevation_deg", "elevation", default=0.0))
    except (TypeError, ValueError, KeyError) as exc:
        errors.append(
            error_item("SPEAKER_SCHEMA_ERROR", channel=channel_hint, message=str(exc))
        )
        return None
    if channels <= 0:
        errors.append(
            error_item(
                "CHANNEL_COUNT_MISMATCH",
                channel=channel,
                message="Speaker channel count must be positive.",
                expected=1,
                actual=channels,
            )
        )
    if azimuth is None or not _finite(azimuth) or not -180.0 <= azimuth <= 180.0:
        errors.append(
            error_item(
                "SPEAKER_AZIMUTH_OUT_OF_RANGE",
                channel=channel,
                message="Speaker azimuth must be in [-180, 180].",
                actual=azimuth,
            )
        )
        azimuth = 0.0
    if elevation is None or not _finite(elevation) or not -90.0 <= elevation <= 90.0:
        errors.append(
            error_item(
                "SPEAKER_ELEVATION_OUT_OF_RANGE",
                channel=channel,
                message="Speaker elevation must be in [-90, 90].",
                actual=elevation,
            )
        )
        elevation = 0.0
    return SpeakerSpec(
        id=speaker_id,
        channel=channel,
        channels=channels or 1,
        azimuth_deg=azimuth,
        elevation_deg=elevation,
    )


def _parse_feedback(
    raw: dict[str, Any],
    crosstalk_raw: dict[str, Any],
    actor_ids: set[str],
    speaker_ids: set[str],
    errors: list[dict[str, Any]],
) -> list[FeedbackSpec]:
    entries: list[tuple[Any, str | None]] = []
    top_feedback = _pick(
        crosstalk_raw,
        "feedback",
        "injections",
        "feedback_paths",
        default=None,
    )
    if top_feedback is not None:
        if isinstance(top_feedback, list):
            entries.extend((entry, None) for entry in top_feedback)
        else:
            errors.append(
                error_item(
                    "CROSSTALK_FEEDBACK_INVALID",
                    channel="project",
                    message="crosstalk feedback must be an array.",
                )
            )
    for speaker_raw in raw.get("speakers", raw.get("speaker_channels", [])):
        if not isinstance(speaker_raw, dict):
            continue
        speaker_id = str(
            _pick(speaker_raw, "id", "speaker_id", "name", default="")
        )
        local = _pick(
            speaker_raw,
            "feedback",
            "injections",
            "crosstalk",
            "leakage",
            default=None,
        )
        if local is None:
            continue
        if isinstance(local, list):
            entries.extend((entry, speaker_id) for entry in local)
        else:
            errors.append(
                error_item(
                    "CROSSTALK_FEEDBACK_INVALID",
                    channel=speaker_id or "project",
                    message="Speaker feedback must be an array.",
                )
            )

    result: list[FeedbackSpec] = []
    seen: set[tuple[str, str]] = set()
    for entry, implied_speaker in entries:
        if not isinstance(entry, dict):
            errors.append(
                error_item(
                    "CROSSTALK_FEEDBACK_INVALID",
                    channel=implied_speaker or "project",
                    message="Feedback entries must be objects.",
                )
            )
            continue
        try:
            actor_id = str(_pick(entry, "actor_id", "actor", "source_actor"))
            speaker_id = str(
                _pick(entry, "speaker_id", "speaker", default=implied_speaker or "")
            )
            if "gain_linear" in entry:
                gain_linear = _as_float(entry["gain_linear"])
                assert gain_linear is not None
                if gain_linear <= 0 or not _finite(gain_linear):
                    raise ValueError("gain_linear must be finite and positive")
                gain_db = 20.0 * math.log10(gain_linear)
            else:
                gain_db = _as_float(_pick(entry, "gain_db", "gain", default=-18.0))
            delay_ms = _as_float(
                _pick(entry, "delay_ms", "delay", default=0.0)
            )
        except (TypeError, ValueError, KeyError) as exc:
            errors.append(
                error_item(
                    "CROSSTALK_FEEDBACK_INVALID",
                    channel=implied_speaker or "project",
                    message=str(exc),
                )
            )
            continue
        assert gain_db is not None and delay_ms is not None
        if actor_id not in actor_ids:
            errors.append(
                error_item(
                    "UNKNOWN_ACTOR_ID",
                    channel=actor_id,
                    message="Feedback references an unknown actor.",
                )
            )
            continue
        if speaker_id not in speaker_ids:
            errors.append(
                error_item(
                    "UNKNOWN_SPEAKER_ID",
                    channel=speaker_id,
                    message="Feedback references an unknown speaker.",
                )
            )
            continue
        if not _finite(gain_db) or gain_db > 12.0 or gain_db < -120.0:
            errors.append(
                error_item(
                    "CROSSTALK_GAIN_OUT_OF_RANGE",
                    channel=speaker_id,
                    message="Feedback gain_db must be in [-120, 12].",
                    actual=gain_db,
                )
            )
            continue
        if not _finite(delay_ms) or delay_ms < 0 or delay_ms > 1000:
            errors.append(
                error_item(
                    "CROSSTALK_DELAY_OUT_OF_RANGE",
                    channel=speaker_id,
                    message="Feedback delay_ms must be in [0, 1000].",
                    actual=delay_ms,
                )
            )
            continue
        key = (actor_id, speaker_id)
        if key in seen:
            errors.append(
                error_item(
                    "DUPLICATE_CROSSTALK_PATH",
                    channel=speaker_id,
                    message="Each actor/speaker feedback pair may only be declared once.",
                )
            )
            continue
        seen.add(key)
        result.append(FeedbackSpec(actor_id, speaker_id, gain_db, delay_ms))
    return sorted(result, key=lambda item: (item.speaker_id, item.actor_id))


def _parse_windows(
    raw: dict[str, Any],
    crosstalk_raw: dict[str, Any],
    *primary_names: str,
    purpose: str,
    local_names: tuple[str, ...],
    duration: float,
    actor_ids: set[str],
    speaker_ids: set[str],
    errors: list[dict[str, Any]],
) -> list[ReferenceWindow]:
    value: Any = None
    for name in primary_names:
        if name in crosstalk_raw:
            value = crosstalk_raw[name]
            break
        if name in raw:
            value = raw[name]
            break
    windows: list[ReferenceWindow] = []
    if value is None:
        for actor_raw in raw.get("actors", raw.get("actor_channels", [])):
            if not isinstance(actor_raw, dict):
                continue
            actor_id = str(
                _pick(actor_raw, "id", "actor_id", "name", default="")
            )
            local = _pick(
                actor_raw,
                *local_names,
                default=None,
            )
            if local is not None:
                if isinstance(local, list):
                    for entry in local:
                        windows.extend(
                            _parse_window_entry(
                                entry,
                                purpose,
                                duration,
                                actor_id,
                                None,
                                actor_ids,
                                speaker_ids,
                                errors,
                            )
                        )
                else:
                    errors.append(
                        error_item(
                            "REFERENCE_WINDOW_INVALID",
                            channel=actor_id,
                            message="Actor reference_windows must be an array.",
                        )
                    )
        return windows
    if not isinstance(value, list):
        errors.append(
            error_item(
                "REFERENCE_WINDOW_INVALID",
                channel="project",
                message=f"{primary_names[0]} must be an array.",
            )
        )
        return windows
    for entry in value:
        windows.extend(
            _parse_window_entry(
                entry,
                purpose,
                duration,
                None,
                None,
                actor_ids,
                speaker_ids,
                errors,
            )
        )
    return windows


def _parse_window_entry(
    entry: Any,
    purpose: str,
    duration: float,
    implied_actor: str | None,
    implied_speaker: str | None,
    actor_ids: set[str],
    speaker_ids: set[str],
    errors: list[dict[str, Any]],
) -> list[ReferenceWindow]:
    try:
        if isinstance(entry, (list, tuple)):
            start = _as_float(entry[0])
            end = _as_float(entry[1])
            actor_id = str(entry[2]) if len(entry) > 2 and entry[2] is not None else implied_actor
            speaker_id = (
                str(entry[3]) if len(entry) > 3 and entry[3] is not None else implied_speaker
            )
        elif isinstance(entry, dict):
            start = _as_float(
                _pick(entry, "start_seconds", "start", "from_seconds")
            )
            end = _as_float(
                _pick(entry, "end_seconds", "end", "to_seconds", "stop_seconds")
            )
            actor_id = _pick(
                entry, "actor_id", "actor", default=implied_actor
            )
            speaker_id = _pick(
                entry, "speaker_id", "speaker", default=implied_speaker
            )
            actor_id = str(actor_id) if actor_id is not None else None
            speaker_id = str(speaker_id) if speaker_id is not None else None
        else:
            raise ValueError("window must be an object or [start, end, actor, speaker]")
    except (TypeError, ValueError, KeyError, IndexError) as exc:
        errors.append(
            error_item(
                "REFERENCE_WINDOW_INVALID",
                channel=implied_actor or implied_speaker or "project",
                message=str(exc),
            )
        )
        return []
    assert start is not None and end is not None
    channel = actor_id or speaker_id or "project"
    if not _finite(start) or not _finite(end):
        errors.append(
            error_item(
                "REFERENCE_WINDOW_INVALID",
                channel=channel,
                message="Window bounds must be finite.",
            )
        )
        return []
    if start < 0 or end > duration or end <= start:
        errors.append(
            error_item(
                "REFERENCE_WINDOW_OUT_OF_RANGE",
                channel=channel,
                message="Window must satisfy 0 <= start < end <= duration.",
                start_seconds=start,
                end_seconds=end,
            )
        )
        return []
    if actor_id is not None and actor_id not in actor_ids:
        errors.append(
            error_item(
                "UNKNOWN_ACTOR_ID",
                channel=actor_id,
                message="Reference window references an unknown actor.",
            )
        )
        return []
    if speaker_id is not None and speaker_id not in speaker_ids:
        errors.append(
            error_item(
                "UNKNOWN_SPEAKER_ID",
                channel=speaker_id,
                message="Reference window references an unknown speaker.",
            )
        )
        return []
    return [ReferenceWindow(start, end, actor_id, speaker_id, purpose)]


def _parse_spatial(raw: Any, errors: list[dict[str, Any]]) -> SpatialConfig:
    if not isinstance(raw, dict):
        errors.append(
            error_item("SPATIAL_CONFIG_INVALID", message="spatial must be an object.")
        )
        raw = {}
    try:
        head_radius = _as_float(_pick(raw, "head_radius_m", "head_radius", default=0.0875))
        speed = _as_float(_pick(raw, "speed_of_sound_mps", "speed_of_sound", default=343.0))
        block_seconds = _as_float(
            _pick(raw, "parameter_block_seconds", "parameter_block_sec", default=0.01)
        )
        smoothing = _as_float(
            _pick(raw, "smoothing_seconds", "smoothing_sec", default=0.05)
        )
        report_seconds = _as_float(
            _pick(raw, "segment_report_seconds", "report_block_seconds", default=1.0)
        )
        max_ild_rate = _as_float(
            _pick(raw, "max_ild_rate_db_per_s", "max_ild_rate", default=8.0)
        )
        max_itd_rate = _as_float(
            _pick(raw, "max_itd_rate_us_per_s", "max_itd_rate", default=120.0)
        )
        ild_max = _as_float(_pick(raw, "ild_max_db", "max_ild_db", default=18.0))
        model = str(
            _pick(
                raw,
                "model",
                "hrtf_model",
                default="woodworth_itd_head_shadow_fir_v1",
            )
        )
    except (TypeError, ValueError) as exc:
        errors.append(error_item("SPATIAL_CONFIG_INVALID", message=str(exc)))
        head_radius, speed = 0.0875, 343.0
        block_seconds, smoothing, report_seconds = 0.01, 0.05, 1.0
        max_ild_rate, max_itd_rate, ild_max = 8.0, 120.0, 18.0
        model = "woodworth_itd_head_shadow_fir_v1"
    assert head_radius is not None and speed is not None
    assert block_seconds is not None and smoothing is not None and report_seconds is not None
    assert max_ild_rate is not None and max_itd_rate is not None and ild_max is not None
    if not _finite(head_radius) or head_radius <= 0:
        errors.append(error_item("SPATIAL_CONFIG_INVALID", message="head_radius_m must be positive."))
    if not _finite(speed) or speed <= 0:
        errors.append(
            error_item("SPATIAL_CONFIG_INVALID", message="speed_of_sound_mps must be positive.")
        )
    if not _finite(block_seconds) or block_seconds <= 0:
        errors.append(
            error_item("SPATIAL_CONFIG_INVALID", message="parameter_block_seconds must be positive.")
        )
    if not _finite(smoothing) or smoothing < 0:
        errors.append(
            error_item("SPATIAL_CONFIG_INVALID", message="smoothing_seconds must be non-negative.")
        )
    if not _finite(report_seconds) or report_seconds <= 0:
        errors.append(
            error_item("SPATIAL_CONFIG_INVALID", message="segment_report_seconds must be positive.")
        )
    if not _finite(max_ild_rate) or max_ild_rate <= 0:
        errors.append(
            error_item("SPATIAL_RATE_LIMIT_INVALID", message="max_ild_rate_db_per_s must be positive.")
        )
    if not _finite(max_itd_rate) or max_itd_rate <= 0:
        errors.append(
            error_item(
                "SPATIAL_RATE_LIMIT_INVALID",
                message="max_itd_rate_us_per_s must be positive.",
            )
        )
    if not _finite(ild_max) or ild_max <= 0 or ild_max > 30:
        errors.append(error_item("SPATIAL_CONFIG_INVALID", message="ild_max_db must be in (0, 30]."))
    return SpatialConfig(
        head_radius_m=head_radius,
        speed_of_sound_mps=speed,
        parameter_block_seconds=block_seconds,
        smoothing_seconds=smoothing,
        segment_report_seconds=report_seconds,
        max_ild_rate_db_per_s=max_ild_rate,
        max_itd_rate_us_per_s=max_itd_rate,
        ild_max_db=ild_max,
        model=model,
    )


def _parse_export(
    raw: dict[str, Any],
    sample_rate: int,
    bit_depth: int,
    errors: list[dict[str, Any]],
) -> ExportConfig | None:
    raw_export = raw.get("export", raw.get("output", {}))
    if not isinstance(raw_export, dict):
        errors.append(error_item("EXPORT_CONFIG_INVALID", message="export must be an object."))
        return None
    try:
        fmt = str(_pick(raw_export, "format", "container", default="wav")).lower()
        export_sr = _as_int(
            _pick(raw_export, "sample_rate", "sampleRate", default=sample_rate)
        )
        export_depth = _as_int(
            _pick(raw_export, "bit_depth", "bitDepth", default=bit_depth)
        )
        channels = _as_int(_pick(raw_export, "channels", "channel_count", default=2))
        target = _as_float(
            _pick(
                raw_export,
                "target_lufs",
                "target_loudness_lufs",
                "integrated_lufs",
                default=-16.0,
            )
        )
        true_peak = _as_float(
            _pick(
                raw_export,
                "true_peak_limit_dbtp",
                "true_peak_limit_db",
                "max_true_peak_dbtp",
                default=-1.0,
            )
        )
        block_frames = _as_int(
            _pick(raw_export, "block_frames", "chunk_frames", default=4096)
        )
    except (TypeError, ValueError) as exc:
        errors.append(error_item("EXPORT_CONFIG_INVALID", message=str(exc)))
        return None
    assert export_sr is not None and export_depth is not None and channels is not None
    assert target is not None and true_peak is not None and block_frames is not None
    if fmt not in {"wav", "wave"}:
        errors.append(
            error_item(
                "UNSUPPORTED_EXPORT_FORMAT",
                message="Only PCM WAV export is supported.",
                actual=fmt,
            )
        )
    if export_sr != sample_rate:
        errors.append(
            error_item(
                "EXPORT_SAMPLE_RATE_MISMATCH",
                message="Export sample rate must equal the project sample rate.",
                expected=sample_rate,
                actual=export_sr,
            )
        )
    if export_depth not in (16, 24, 32):
        errors.append(
            error_item(
                "BIT_DEPTH_INVALID",
                message="Export bit depth must be 16, 24, or 32.",
                actual=export_depth,
            )
        )
    if channels != 2:
        errors.append(
            error_item(
                "EXPORT_CHANNEL_COUNT_INVALID",
                message="Binaural export must contain exactly two channels.",
                actual=channels,
            )
        )
    if not _finite(target) or target < -60 or target > 0:
        errors.append(
            error_item(
                "TARGET_LOUDNESS_INVALID",
                message="target_lufs must be in [-60, 0].",
                actual=target,
            )
        )
    if not _finite(true_peak) or true_peak > 0:
        errors.append(
            error_item(
                "TRUE_PEAK_LIMIT_INVALID",
                message="true_peak_limit_dbtp must be finite and <= 0 dBTP.",
                actual=true_peak,
            )
        )
    if block_frames < 64:
        errors.append(
            error_item(
                "BLOCK_FRAMES_INVALID",
                message="block_frames must be at least 64.",
                actual=block_frames,
            )
        )
    return ExportConfig("wav", export_sr, export_depth, channels, target, true_peak, block_frames)


def _validate_audio_files(
    project: Project, audio_dir: str | Path, errors: list[dict[str, Any]]
) -> None:
    for channel_spec in list(project.actors) + list(project.speakers):
        path = resolve_channel_path(audio_dir, channel_spec.channel)
        channel = channel_spec.channel
        try:
            info = inspect_wav(path, channel)
        except ValidationFailure as exc:
            errors.extend(exc.errors)
            continue
        if info.sample_rate != project.sample_rate:
            errors.append(
                error_item(
                    "SAMPLE_RATE_MISMATCH",
                    channel=channel,
                    message="WAV sample rate does not match the project.",
                    expected=project.sample_rate,
                    actual=info.sample_rate,
                )
            )
        if info.bit_depth != project.bit_depth:
            errors.append(
                error_item(
                    "BIT_DEPTH_MISMATCH",
                    channel=channel,
                    message="WAV bit depth does not match the project.",
                    expected=project.bit_depth,
                    actual=info.bit_depth,
                )
            )
        if info.channels != channel_spec.channels:
            errors.append(
                error_item(
                    "CHANNEL_COUNT_MISMATCH",
                    channel=channel,
                    message="WAV channel count does not match the declaration.",
                    expected=channel_spec.channels,
                    actual=info.channels,
                )
            )
        elif channel_spec.channels != 1:
            code = (
                "UNSUPPORTED_ACTOR_CHANNEL_LAYOUT"
                if isinstance(channel_spec, ActorSpec)
                else "UNSUPPORTED_SPEAKER_CHANNEL_LAYOUT"
            )
            errors.append(
                error_item(
                    code,
                    channel=channel,
                    message="Actor and speaker inputs must be mono.",
                    actual=info.channels,
                )
            )
        if info.frames < project.frames:
            errors.append(
                error_item(
                    "FRAME_COUNT_TOO_SHORT",
                    channel=channel,
                    message="Declared WAV is shorter than the project duration.",
                    expected_frames=project.frames,
                    actual_frames=info.frames,
                )
            )

