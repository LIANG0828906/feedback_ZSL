from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .audio_io import read_wav, sha256_file
from .errors import BusinessError, StructuredIssue, ValidationFailure


@dataclass(frozen=True)
class CameraSample:
    time: float
    position: np.ndarray
    forward: np.ndarray


@dataclass(frozen=True)
class SyncAnchor:
    source_sample: int
    target_time: float


@dataclass(frozen=True)
class EventSpec:
    id: str
    type: str
    start_time: float
    duration: float
    source_position: np.ndarray
    gain_db: float
    detection_threshold: float
    detection_search_ms: float

    @property
    def end_time(self) -> float:
        return self.start_time + self.duration


@dataclass
class TrackSpec:
    name: str
    path: str
    absolute_path: Path
    declared_sample_rate: int
    declared_channels: int
    gain_db: float
    sync_anchors: tuple[SyncAnchor, ...]
    events: list[EventSpec]
    samples: np.ndarray | None = field(default=None, repr=False)


@dataclass(frozen=True)
class ReverbRegion:
    start_time: float
    end_time: float
    rt60: float
    wet_dry: float


@dataclass(frozen=True)
class Occluder:
    id: str
    minimum: np.ndarray
    maximum: np.ndarray


@dataclass(frozen=True)
class OutputConfig:
    sample_rate: int
    channels: int
    duration: float
    total_samples: int
    block_frames: int
    report_frame_stride: int
    target_loudness_lufs: float
    true_peak_limit_dbtp: float


@dataclass(frozen=True)
class SpatialLimits:
    max_ild_db: float
    max_itd_us: float
    max_ild_rate_db_per_s: float
    max_itd_rate_us_per_s: float
    head_radius_m: float
    speed_of_sound_m_s: float
    ild_strength_db: float


@dataclass
class Scene:
    scene_path: Path
    scene_dir: Path
    version: int
    name: str
    duration: float
    output: OutputConfig
    camera_up: np.ndarray
    camera_path: list[CameraSample]
    tracks: list[TrackSpec]
    reverb_regions: list[ReverbRegion]
    reverb_blend_width: float
    occluders: list[Occluder]
    spatial_limits: SpatialLimits
    raw: dict[str, Any]
    scene_digest: str

    def all_events(self) -> list[tuple[TrackSpec, EventSpec]]:
        return [(track, event) for track in self.tracks for event in track.events]


def _as_vector(value: Any) -> np.ndarray | None:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        return None
    try:
        vector = np.asarray(value, dtype=np.float64)
    except (TypeError, ValueError):
        return None
    if not np.isfinite(vector).all():
        return None
    return vector


def _finite_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    return result if math.isfinite(result) else None


def _canonical_scene_for_digest(scene: dict[str, Any]) -> dict[str, Any]:
    canonical = json.loads(json.dumps(scene, sort_keys=True))
    for track in canonical.get("tracks", []):
        raw_path = str(track.get("path", ""))
        track["path"] = raw_path.replace("\\", "/")
    camera = canonical.get("camera")
    if isinstance(camera, dict) and isinstance(camera.get("path"), list):
        camera["path"] = sorted(
            camera["path"],
            key=lambda item: float(item.get("time", 0.0)),
        )
    return canonical


def _parse_scene(
    scene_path: Path, raw: dict[str, Any]
) -> tuple[
    OutputConfig,
    np.ndarray,
    list[CameraSample],
    list[TrackSpec],
    list[ReverbRegion],
    float,
    list[Occluder],
    SpatialLimits,
    list[StructuredIssue],
]:
    issues: list[StructuredIssue] = []
    scene_dir = scene_path.parent

    duration = _finite_number(raw.get("duration"))
    if duration is None or duration <= 0:
        issues.append(
            StructuredIssue(
                "E_SCENE_DURATION",
                "scene duration must be a positive finite number",
            )
        )
        duration = 0.0

    output_raw = raw.get("output", {})
    if not isinstance(output_raw, dict):
        issues.append(
            StructuredIssue("E_OUTPUT_CONFIG", "output must be an object")
        )
        output_raw = {}
    sample_rate = output_raw.get("sample_rate", 48000)
    channels = output_raw.get("channels", 2)
    block_frames = output_raw.get("block_frames", 4096)
    report_frame_stride = output_raw.get(
        "report_frame_stride",
        max(1, int(round(float(sample_rate) * 0.1)))
        if _finite_number(sample_rate) is not None
        else 4800,
    )
    target_loudness = output_raw.get("target_loudness_lufs", -23.0)
    true_peak_limit = output_raw.get("true_peak_limit_dbtp", -1.0)

    def positive_int(value: Any, label: str) -> int:
        if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
            issues.append(
                StructuredIssue(
                    "E_OUTPUT_CONFIG", f"{label} must be a positive integer"
                )
            )
            return 1
        return value

    sample_rate_i = positive_int(sample_rate, "output.sample_rate")
    channels_i = positive_int(channels, "output.channels")
    block_i = positive_int(block_frames, "output.block_frames")
    report_i = positive_int(report_frame_stride, "output.report_frame_stride")
    if channels_i != 2:
        issues.append(
            StructuredIssue(
                "E_OUTPUT_CONFIG", "this renderer requires two output channels"
            )
        )
    target_f = _finite_number(target_loudness)
    peak_f = _finite_number(true_peak_limit)
    if target_f is None or not (-70.0 <= target_f <= 0.0):
        issues.append(
            StructuredIssue(
                "E_OUTPUT_CONFIG",
                "target_loudness_lufs must be between -70 and 0",
            )
        )
        target_f = -23.0
    if peak_f is None or not (-20.0 <= peak_f <= 0.0):
        issues.append(
            StructuredIssue(
                "E_OUTPUT_CONFIG",
                "true_peak_limit_dbtp must be between -20 and 0",
            )
        )
        peak_f = -1.0

    exact_samples = duration * sample_rate_i
    total_samples = int(round(exact_samples)) if exact_samples > 0 else 0
    if total_samples <= 0 or abs(exact_samples - total_samples) > 1e-6:
        issues.append(
            StructuredIssue(
                "E_OUTPUT_DURATION",
                "duration multiplied by sample rate must be an integer",
                details={"exact_samples": exact_samples},
            )
        )
    output = OutputConfig(
        sample_rate=sample_rate_i,
        channels=channels_i,
        duration=duration,
        total_samples=total_samples,
        block_frames=block_i,
        report_frame_stride=report_i,
        target_loudness_lufs=target_f,
        true_peak_limit_dbtp=peak_f,
    )

    camera_raw = raw.get("camera", {})
    if not isinstance(camera_raw, dict):
        issues.append(
            StructuredIssue("E_CAMERA_SCHEMA", "camera must be an object")
        )
        camera_raw = {}
    camera_up = _as_vector(camera_raw.get("up", [0.0, 0.0, 1.0]))
    if camera_up is None or np.linalg.norm(camera_up) < 1e-9:
        issues.append(
            StructuredIssue(
                "E_CAMERA_SCHEMA", "camera.up must be a non-zero 3D vector"
            )
        )
        camera_up = np.array([0.0, 0.0, 1.0])
    else:
        camera_up = camera_up / np.linalg.norm(camera_up)

    camera_path: list[CameraSample] = []
    path_raw = camera_raw.get("path")
    if not isinstance(path_raw, list) or len(path_raw) < 2:
        issues.append(
            StructuredIssue(
                "E_CAMERA_SCHEMA", "camera.path must contain at least two samples"
            )
        )
        path_raw = []
    last_time = -math.inf
    for index, item in enumerate(path_raw):
        if not isinstance(item, dict):
            issues.append(
                StructuredIssue(
                    "E_CAMERA_SCHEMA",
                    f"camera path sample {index} must be an object",
                )
            )
            continue
        time = _finite_number(item.get("time"))
        position = _as_vector(item.get("position"))
        forward = _as_vector(item.get("forward"))
        if time is None or position is None or forward is None:
            issues.append(
                StructuredIssue(
                    "E_CAMERA_SCHEMA",
                    f"camera path sample {index} requires finite time, position, forward",
                )
            )
            continue
        if time < 0 or time > duration + 1e-9:
            issues.append(
                StructuredIssue(
                    "E_CAMERA_RANGE",
                    f"camera path time {time} is outside scene duration",
                )
            )
        if not time > last_time:
            issues.append(
                StructuredIssue(
                    "E_CAMERA_TIME",
                    "camera path times must be strictly increasing",
                    details={"index": index, "previous": last_time, "time": time},
                )
            )
        last_time = time
        norm = float(np.linalg.norm(forward))
        if norm < 1e-9:
            issues.append(
                StructuredIssue(
                    "E_CAMERA_SCHEMA",
                    f"camera forward at sample {index} must be non-zero",
                )
            )
        else:
            forward = forward / norm
        camera_path.append(CameraSample(time, position, forward))
    if camera_path and (camera_path[0].time > 1e-6 or camera_path[-1].time < duration - 1e-6):
        issues.append(
            StructuredIssue(
                "E_CAMERA_RANGE",
                "camera path must cover the entire scene duration",
            )
        )

    tracks_raw = raw.get("tracks")
    if not isinstance(tracks_raw, list) or not tracks_raw:
        issues.append(
            StructuredIssue("E_TRACKS_SCHEMA", "tracks must be a non-empty array")
        )
        tracks_raw = []

    tracks: list[TrackSpec] = []
    seen_track_names: set[str] = set()
    seen_event_ids: set[str] = set()
    for track_index, item in enumerate(tracks_raw):
        fallback_name = f"track[{track_index}]"
        if not isinstance(item, dict):
            issues.append(
                StructuredIssue(
                    "E_TRACKS_SCHEMA", "track entry must be an object", track=fallback_name
                )
            )
            continue
        name = str(item.get("name", fallback_name))
        if name in seen_track_names:
            issues.append(
                StructuredIssue(
                    "E_TRACK_DUPLICATE", "track names must be unique", track=name
                )
            )
        seen_track_names.add(name)
        path_value = item.get("path")
        if not isinstance(path_value, str) or not path_value:
            issues.append(
                StructuredIssue(
                    "E_TRACK_PATH", "track path must be a non-empty string", track=name
                )
            )
            path_value = ""
        absolute_path = (scene_dir / path_value).resolve() if path_value else scene_dir
        declared_rate = item.get("sample_rate")
        declared_channels = item.get("channels")
        if (
            isinstance(declared_rate, bool)
            or not isinstance(declared_rate, int)
            or declared_rate <= 0
        ):
            issues.append(
                StructuredIssue(
                    "E_TRACK_RATE", "sample_rate must be a positive integer", track=name
                )
            )
            declared_rate = 1
        if (
            isinstance(declared_channels, bool)
            or not isinstance(declared_channels, int)
            or declared_channels <= 0
        ):
            issues.append(
                StructuredIssue(
                    "E_TRACK_CHANNELS",
                    "channels must be a positive integer",
                    track=name,
                )
            )
            declared_channels = 1
        gain_db = _finite_number(item.get("gain_db", 0.0))
        if gain_db is None:
            issues.append(
                StructuredIssue(
                    "E_TRACK_GAIN", "gain_db must be finite", track=name
                )
            )
            gain_db = 0.0

        anchors: list[SyncAnchor] = []
        anchors_raw = item.get("sync_anchors", [])
        if not isinstance(anchors_raw, list):
            issues.append(
                StructuredIssue(
                    "E_SYNC_ANCHOR", "sync_anchors must be an array", track=name
                )
            )
            anchors_raw = []
        last_source = -1
        last_target = -math.inf
        for anchor_index, anchor in enumerate(anchors_raw):
            if not isinstance(anchor, dict):
                issues.append(
                    StructuredIssue(
                        "E_SYNC_ANCHOR",
                        f"sync anchor {anchor_index} must be an object",
                        track=name,
                    )
                )
                continue
            source_sample = anchor.get("source_sample")
            target_time = _finite_number(anchor.get("target_time"))
            if (
                isinstance(source_sample, bool)
                or not isinstance(source_sample, int)
                or source_sample < 0
                or target_time is None
                or target_time < 0
                or target_time > duration + 1e-9
            ):
                issues.append(
                    StructuredIssue(
                        "E_SYNC_ANCHOR",
                        "sync anchor requires non-negative integer source_sample and target_time",
                        track=name,
                    )
                )
                continue
            if source_sample <= last_source or target_time <= last_target:
                issues.append(
                    StructuredIssue(
                        "E_SYNC_ANCHOR",
                        "sync anchors must be strictly increasing",
                        track=name,
                    )
                )
            last_source = source_sample
            last_target = target_time
            anchors.append(SyncAnchor(source_sample, target_time))
        if len(anchors) == 1:
            issues.append(
                StructuredIssue(
                    "E_SYNC_ANCHOR",
                    "at least two sync anchors are required when anchors are supplied",
                    track=name,
                )
            )

        events: list[EventSpec] = []
        events_raw = item.get("events")
        if not isinstance(events_raw, list) or not events_raw:
            issues.append(
                StructuredIssue(
                    "E_EVENTS_SCHEMA", "events must be a non-empty array", track=name
                )
            )
            events_raw = []
        for event_index, event_raw in enumerate(events_raw):
            fallback_event = f"{name}:event[{event_index}]"
            if not isinstance(event_raw, dict):
                issues.append(
                    StructuredIssue(
                        "E_EVENTS_SCHEMA",
                        "event entry must be an object",
                        track=name,
                        event_id=fallback_event,
                    )
                )
                continue
            event_id = str(event_raw.get("id", fallback_event))
            if event_id in seen_event_ids:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_DUPLICATE",
                        "event IDs must be globally unique",
                        track=name,
                        event_id=event_id,
                    )
                )
            seen_event_ids.add(event_id)
            event_type = event_raw.get("type")
            if not isinstance(event_type, str) or not event_type:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_SCHEMA",
                        "event type must be a non-empty string",
                        track=name,
                        event_id=event_id,
                    )
                )
                event_type = "unknown"
            start_time = _finite_number(event_raw.get("start_time"))
            event_duration = _finite_number(event_raw.get("duration"))
            source_position = _as_vector(event_raw.get("source_position"))
            event_gain = _finite_number(event_raw.get("gain_db", 0.0))
            if start_time is None or start_time < 0:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_TIME",
                        "event start_time must be non-negative and finite",
                        track=name,
                        event_id=event_id,
                    )
                )
            if event_duration is None or event_duration <= 0:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_DURATION",
                        "event duration must be positive and finite",
                        track=name,
                        event_id=event_id,
                    )
                )
            if source_position is None or event_gain is None:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_SCHEMA",
                        "event requires source_position and finite gain_db",
                        track=name,
                        event_id=event_id,
                    )
                )
            if (
                start_time is None
                or start_time < 0
                or event_duration is None
                or event_duration <= 0
                or source_position is None
                or event_gain is None
            ):
                start_time = max(0.0, start_time or 0.0)
                event_duration = max(1e-6, event_duration or 1e-6)
                source_position = (
                    source_position
                    if source_position is not None
                    else np.zeros(3, dtype=np.float64)
                )
                event_gain = event_gain if event_gain is not None else 0.0
            if start_time + event_duration > duration + 1e-9:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_TIME",
                        "event end exceeds scene duration",
                        track=name,
                        event_id=event_id,
                    )
                )
            detection_raw = event_raw.get("detection", {})
            if not isinstance(detection_raw, dict):
                detection_raw = {}
            threshold = _finite_number(detection_raw.get("threshold", 0.08))
            search_ms = _finite_number(detection_raw.get("search_ms", 30.0))
            if threshold is None or not (0.0 < threshold < 1.0):
                issues.append(
                    StructuredIssue(
                        "E_EVENT_DETECTION",
                        "detection.threshold must be between 0 and 1",
                        track=name,
                        event_id=event_id,
                    )
                )
                threshold = 0.08
            if search_ms is None or search_ms < 0:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_DETECTION",
                        "detection.search_ms must be non-negative",
                        track=name,
                        event_id=event_id,
                    )
                )
                search_ms = 30.0
            events.append(
                EventSpec(
                    id=event_id,
                    type=event_type,
                    start_time=start_time,
                    duration=event_duration,
                    source_position=source_position,
                    gain_db=event_gain,
                    detection_threshold=threshold,
                    detection_search_ms=search_ms,
                )
            )
        ordered = sorted(events, key=lambda event: (event.start_time, event.id))
        for previous, current in zip(ordered, ordered[1:]):
            if current.start_time < previous.end_time - 1e-12:
                issues.append(
                    StructuredIssue(
                        "E_EVENT_OVERLAP",
                        "events within a track must not overlap",
                        track=name,
                        event_id=current.id,
                        details={
                            "previous_event_id": previous.id,
                            "previous_end": previous.end_time,
                            "start_time": current.start_time,
                        },
                    )
                )
        tracks.append(
            TrackSpec(
                name=name,
                path=path_value.replace("\\", "/"),
                absolute_path=absolute_path,
                declared_sample_rate=declared_rate,
                declared_channels=declared_channels,
                gain_db=gain_db,
                sync_anchors=tuple(anchors),
                events=events,
            )
        )

    reverb_raw = raw.get("reverb", {})
    if not isinstance(reverb_raw, dict):
        issues.append(
            StructuredIssue("E_REVERB_SCHEMA", "reverb must be an object")
        )
        reverb_raw = {}
    blend_width = _finite_number(reverb_raw.get("blend_width", 0.25))
    if blend_width is None or blend_width <= 0:
        issues.append(
            StructuredIssue(
                "E_REVERB_SCHEMA", "reverb.blend_width must be positive"
            )
        )
        blend_width = 0.25
    regions: list[ReverbRegion] = []
    regions_raw = reverb_raw.get("regions")
    if not isinstance(regions_raw, list) or not regions_raw:
        issues.append(
            StructuredIssue(
                "E_REVERB_SCHEMA", "reverb.regions must be a non-empty array"
            )
        )
        regions_raw = []
    for index, region_raw in enumerate(regions_raw):
        if not isinstance(region_raw, dict):
            issues.append(
                StructuredIssue(
                    "E_REVERB_SCHEMA", f"reverb region {index} must be an object"
                )
            )
            continue
        start = _finite_number(region_raw.get("start_time"))
        end = _finite_number(region_raw.get("end_time"))
        rt60 = _finite_number(region_raw.get("rt60"))
        wet_dry = _finite_number(region_raw.get("wet_dry"))
        if (
            start is None
            or end is None
            or rt60 is None
            or wet_dry is None
            or start < 0
            or end <= start
            or end > duration + 1e-9
            or rt60 <= 0
            or not (0.0 <= wet_dry <= 1.0)
        ):
            issues.append(
                StructuredIssue(
                    "E_REVERB_REGION",
                    f"reverb region {index} has invalid bounds, rt60 or wet_dry",
                )
            )
            continue
        regions.append(ReverbRegion(start, end, rt60, wet_dry))
    regions.sort(key=lambda region: region.start_time)
    if regions:
        if abs(regions[0].start_time) > 1e-9 or abs(regions[-1].end_time - duration) > 1e-9:
            issues.append(
                StructuredIssue(
                    "E_REVERB_REGION",
                    "reverb regions must cover the whole scene duration",
                )
            )
        for previous, current in zip(regions, regions[1:]):
            if abs(current.start_time - previous.end_time) > 1e-9:
                issues.append(
                    StructuredIssue(
                        "E_REVERB_REGION",
                        "reverb regions must be contiguous and non-overlapping",
                    )
                )
        for previous, current in zip(regions, regions[1:]):
            if 2.0 * blend_width > current.start_time - previous.start_time + 1e-12:
                issues.append(
                    StructuredIssue(
                        "E_REVERB_BLEND",
                        "blend_width is too wide for the adjacent regions",
                    )
                )

    occluders: list[Occluder] = []
    occluders_raw = raw.get("occluders", [])
    if not isinstance(occluders_raw, list):
        issues.append(
            StructuredIssue("E_OCCLUDER_SCHEMA", "occluders must be an array")
        )
        occluders_raw = []
    for index, occluder_raw in enumerate(occluders_raw):
        if not isinstance(occluder_raw, dict):
            issues.append(
                StructuredIssue(
                    "E_OCCLUDER_SCHEMA", f"occluder {index} must be an object"
                )
            )
            continue
        minimum = _as_vector(occluder_raw.get("min"))
        maximum = _as_vector(occluder_raw.get("max"))
        if (
            minimum is None
            or maximum is None
            or np.any(maximum < minimum)
        ):
            issues.append(
                StructuredIssue(
                    "E_OCCLUDER_SCHEMA",
                    f"occluder {index} requires valid min/max bounds",
                )
            )
            continue
        occluders.append(
            Occluder(str(occluder_raw.get("id", f"occ{index}")), minimum, maximum)
        )

    spatial_raw = raw.get("spatial_limits", {})
    if not isinstance(spatial_raw, dict):
        issues.append(
            StructuredIssue("E_SPATIAL_SCHEMA", "spatial_limits must be an object")
        )
        spatial_raw = {}

    def spatial_value(
        key: str, default: float, minimum: float, *, inclusive: bool = False
    ) -> float:
        value = _finite_number(spatial_raw.get(key, default))
        valid = value is not None and (
            value >= minimum if inclusive else value > minimum
        )
        if not valid:
            issues.append(
                StructuredIssue(
                    "E_SPATIAL_SCHEMA", f"spatial_limits.{key} is invalid"
                )
            )
            return default
        return value

    spatial_limits = SpatialLimits(
        max_ild_db=spatial_value("max_ild_db", 10.0, 0.0, inclusive=True),
        max_itd_us=spatial_value("max_itd_us", 620.0, 0.0, inclusive=True),
        max_ild_rate_db_per_s=spatial_value(
            "max_ild_rate_db_per_s", 80.0, 0.0, inclusive=True
        ),
        max_itd_rate_us_per_s=spatial_value(
            "max_itd_rate_us_per_s", 5000.0, 0.0, inclusive=True
        ),
        head_radius_m=spatial_value("head_radius_m", 0.0875, 0.01),
        speed_of_sound_m_s=spatial_value("speed_of_sound_m_s", 343.0, 100.0),
        ild_strength_db=spatial_value(
            "ild_strength_db", 8.0, 0.0, inclusive=True
        ),
    )
    produced = duration * sample_rate_i
    if abs(produced - total_samples) > 1e-6:
        issues.append(
            StructuredIssue(
                "E_OUTPUT_DURATION",
                "output duration does not map to an integer sample count",
            )
        )

    return (
        output,
        camera_up,
        camera_path,
        tracks,
        regions,
        blend_width,
        occluders,
        spatial_limits,
        issues,
    )


def load_scene(scene_path: str | Path, *, load_audio: bool = True) -> Scene:
    path = Path(scene_path).resolve()
    try:
        raw_text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise BusinessError(
            "E_SCENE_IO", f"cannot read scene JSON: {exc}", details={"path": path.name}
        ) from exc
    try:
        raw = json.loads(raw_text)
    except json.JSONDecodeError as exc:
        raise BusinessError(
            "E_SCENE_JSON",
            f"invalid scene JSON: {exc.msg}",
            details={"line": exc.lineno, "column": exc.colno},
        ) from exc
    if not isinstance(raw, dict):
        raise BusinessError("E_SCENE_JSON", "scene JSON root must be an object")

    (
        output,
        camera_up,
        camera_path,
        tracks,
        regions,
        blend_width,
        occluders,
        spatial_limits,
        issues,
    ) = _parse_scene(path, raw)

    file_hashes: dict[str, str] = {}
    for track in tracks:
        if not track.absolute_path.is_file():
            issues.append(
                StructuredIssue(
                    "E_TRACK_MISSING",
                    "track audio file does not exist",
                    track=track.name,
                    details={"path": track.path},
                )
            )
            continue
        try:
            samples, actual_rate, actual_channels = read_wav(track.absolute_path)
        except Exception as exc:
            issues.append(
                StructuredIssue(
                    "E_TRACK_DECODE",
                    f"cannot decode track WAV: {exc}",
                    track=track.name,
                )
            )
            continue
        if actual_rate != track.declared_sample_rate:
            issues.append(
                StructuredIssue(
                    "E_TRACK_RATE_MISMATCH",
                    "declared and actual sample rates differ",
                    track=track.name,
                    details={
                        "declared": track.declared_sample_rate,
                        "actual": actual_rate,
                    },
                )
            )
        if actual_channels != track.declared_channels:
            issues.append(
                StructuredIssue(
                    "E_TRACK_CHANNEL_MISMATCH",
                    "declared and actual channel counts differ",
                    track=track.name,
                    details={
                        "declared": track.declared_channels,
                        "actual": actual_channels,
                    },
                )
            )
        if track.sync_anchors:
            for anchor in track.sync_anchors:
                if anchor.source_sample > samples.shape[0]:
                    issues.append(
                        StructuredIssue(
                            "E_SYNC_ANCHOR",
                            "sync anchor lies beyond track sample count",
                            track=track.name,
                            details={
                                "source_sample": anchor.source_sample,
                                "track_samples": int(samples.shape[0]),
                            },
                        )
                    )
        if actual_rate == track.declared_sample_rate and actual_channels == track.declared_channels:
            track.samples = samples
            file_hashes[track.name] = sha256_file(track.absolute_path)

    if issues:
        raise ValidationFailure(issues)

    canonical = _canonical_scene_for_digest(raw)
    digest_payload = {
        "scene": canonical,
        "track_sha256": {name: file_hashes[name] for name in sorted(file_hashes)},
    }
    digest = hashlib.sha256(
        json.dumps(
            digest_payload,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=True,
        ).encode("utf-8")
    ).hexdigest()

    return Scene(
        scene_path=path,
        scene_dir=path.parent,
        version=int(raw.get("version", 1)),
        name=str(raw.get("name", path.stem)),
        duration=output.duration,
        output=output,
        camera_up=camera_up,
        camera_path=camera_path,
        tracks=tracks,
        reverb_regions=regions,
        reverb_blend_width=blend_width,
        occluders=occluders,
        spatial_limits=spatial_limits,
        raw=raw,
        scene_digest=digest,
    )

