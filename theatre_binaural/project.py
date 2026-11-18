from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np
import soundfile as sf

from .errors import input_error


SOURCE_ALIASES = {
    "actor": "actor",
    "performer": "actor",
    "lav": "actor",
    "lavalier": "actor",
    "actor_lav": "actor",
    "lavalier_actor": "actor",
    "environment": "environment",
    "ambience": "environment",
    "ambient": "environment",
    "env": "environment",
    "amb": "environment",
    "stage_environment": "environment",
    "stage_ambience": "environment",
    "stage_boundary": "environment",
    "audience": "audience",
    "crowd": "audience",
    "crowd_ambience": "audience",
    "audience_ambience": "audience",
    "speaker_reference": "speaker_reference",
    "speaker_return_reference": "speaker_reference",
    "speaker": "speaker_reference",
    "reference": "speaker_reference",
    "return_reference": "speaker_reference",
}


@dataclass(frozen=True)
class TrackSpec:
    index: int
    name: str
    path: Path
    path_display: str
    sample_rate: int
    channels: int
    bit_depth: int | None
    source_type: str
    anchor: np.ndarray | None


@dataclass(frozen=True)
class HeadSample:
    time_s: float
    position: np.ndarray
    quaternion_wxyz: np.ndarray


@dataclass(frozen=True)
class InterpolationCheck:
    time_s: float
    channel: str | None
    expected_azimuth_deg: float | None


@dataclass(frozen=True)
class Project:
    path: Path
    root: Path
    raw: dict[str, Any]
    project_id: str
    seed: int
    duration_s: float
    sample_rate: int
    tracks: tuple[TrackSpec, ...]
    reference_channel: str
    head_samples: tuple[HeadSample, ...]
    orientation_convention: str
    head_radius_m: float
    speed_of_sound_m_s: float
    max_ild_db: float
    max_ild_rate_db_per_s: float
    max_itd_rate_us_per_s: float
    downmix: dict[str, tuple[float, float]]
    target_loudness_lufs: float
    true_peak_dbtp: float
    block_frames: int
    leakage_lowpass_hz: float
    leakage_max_delay_ms: float
    leakage_injection_channels: tuple[str, ...]
    leakage_gain_db: float
    leakage_per_channel_gain_db: dict[str, float]
    interpolation_checks: tuple[InterpolationCheck, ...]
    azimuth_tolerance_deg: float
    limiter_lookahead_ms: float
    limiter_release_ms: float

    @property
    def track_map(self) -> dict[str, TrackSpec]:
        return {track.name: track for track in self.tracks}

    @property
    def mixed_tracks(self) -> tuple[TrackSpec, ...]:
        return tuple(
            track
            for track in self.tracks
            if track.source_type in {"actor", "environment", "audience"}
        )

    def display_path(self, path: Path) -> str:
        try:
            return path.resolve().relative_to(self.root.resolve()).as_posix()
        except ValueError:
            return path.name


def _require_mapping(value: Any, channel: str, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must be an object")
    return value


def _require_list(value: Any, channel: str, label: str) -> list[Any]:
    if not isinstance(value, list):
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must be an array")
    return value


def _number(value: Any, channel: str, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must be numeric")
    result = float(value)
    if not math.isfinite(result):
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must be finite")
    return result


def _integer(value: Any, channel: str, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must be an integer")
    return int(value)


def _first(mapping: dict[str, Any], names: Iterable[str], default: Any = None) -> Any:
    for name in names:
        if name in mapping:
            return mapping[name]
    return default


def _vector3(value: Any, channel: str, label: str) -> np.ndarray:
    if isinstance(value, dict):
        if not all(axis in value for axis in ("x", "y", "z")):
            raise input_error("E_PROJECT_SCHEMA", channel, f"{label} needs x, y and z")
        value = [value["x"], value["y"], value["z"]]
    values = _require_list(value, channel, label)
    if len(values) != 3:
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must have 3 values")
    return np.asarray(
        [_number(item, channel, f"{label}[{index}]") for index, item in enumerate(values)],
        dtype=np.float64,
    )


def _quaternion(
    value: Any,
    channel: str,
    label: str,
    order: str,
    tolerance: float = 1.0e-4,
) -> np.ndarray:
    if isinstance(value, dict):
        if all(key in value for key in ("w", "x", "y", "z")):
            components = [value[key] for key in ("w", "x", "y", "z")]
        elif all(key in value for key in ("qw", "qx", "qy", "qz")):
            components = [value[key] for key in ("qw", "qx", "qy", "qz")]
        else:
            raise input_error(
                "E_PROJECT_SCHEMA", channel, f"{label} needs w, x, y and z"
            )
    else:
        components = _require_list(value, channel, label)
    if len(components) != 4:
        raise input_error("E_PROJECT_SCHEMA", channel, f"{label} must have 4 values")
    numbers = [
        _number(item, channel, f"{label}[{index}]")
        for index, item in enumerate(components)
    ]
    if order == "xyzw":
        quat = np.asarray([numbers[3], numbers[0], numbers[1], numbers[2]], dtype=np.float64)
    else:
        quat = np.asarray(numbers, dtype=np.float64)
    norm = float(np.linalg.norm(quat))
    if norm <= 0.0 or abs(norm - 1.0) > tolerance:
        raise input_error(
            "E_QUATERNION_NOT_NORMALIZED",
            channel,
            f"{label} norm is {norm:.9f}, expected 1.0",
        )
    return quat / norm


def _detect_bit_depth(subtype: str) -> int | None:
    if subtype.startswith("PCM_"):
        try:
            return int(subtype.split("_", 1)[1])
        except ValueError:
            return None
    if subtype == "FLOAT":
        return 32
    if subtype == "DOUBLE":
        return 64
    return None


def _normalize_bit_depth(value: Any, channel: str) -> int | None:
    if value is None:
        return None
    if isinstance(value, str):
        text = value.strip().lower()
        if text in {"float32", "f32"}:
            return 32
        if text in {"float64", "f64", "double"}:
            return 64
        text = text.replace("-bit", "").replace("_bit", "").strip()
        if text.startswith("pcm_"):
            text = text.split("_", 1)[1]
        try:
            return int(text)
        except ValueError as exc:
            raise input_error("E_PROJECT_SCHEMA", channel, f"invalid bit depth {value!r}") from exc
    if isinstance(value, bool) or not isinstance(value, int):
        raise input_error("E_PROJECT_SCHEMA", channel, "bit_depth must be an integer")
    return value


def _normalize_source_type(value: Any, channel: str) -> str:
    if not isinstance(value, str):
        raise input_error("E_SOURCE_TYPE", channel, "source_type must be a string")
    key = value.strip().lower().replace("-", "_").replace(" ", "_")
    source_type = SOURCE_ALIASES.get(key)
    if source_type is None:
        raise input_error("E_SOURCE_TYPE", channel, f"unsupported source type {value!r}")
    return source_type


def _parse_tracks(data: dict[str, Any], root: Path) -> tuple[TrackSpec, ...]:
    raw_tracks = data.get("tracks", data.get("channels", data.get("sources")))
    if raw_tracks is None:
        raise input_error("E_PROJECT_SCHEMA", "project", "tracks/channels is required")
    raw_tracks = _require_list(raw_tracks, "project", "tracks")
    if len(raw_tracks) != 8:
        raise input_error(
            "E_TRACK_COUNT",
            "project",
            f"expected exactly 8 tracks, found {len(raw_tracks)}",
        )
    global_audio = data.get("audio", {})
    if not isinstance(global_audio, dict):
        global_audio = {}
    global_sr = _first(data, ("sample_rate",), global_audio.get("sample_rate", data.get("sample_rate")))
    global_bit_depth = _first(
        data,
        ("bit_depth",),
        global_audio.get("bit_depth", data.get("bit_depth")),
    )

    tracks: list[TrackSpec] = []
    seen: set[str] = set()
    for index, item in enumerate(raw_tracks):
        if not isinstance(item, dict):
            raise input_error("E_PROJECT_SCHEMA", f"track_{index}", "track must be an object")
        name = item.get("name", item.get("id"))
        if not isinstance(name, str) or not name.strip():
            raise input_error("E_PROJECT_SCHEMA", f"track_{index}", "track name is required")
        name = name.strip()
        if name in seen:
            raise input_error("E_DUPLICATE_CHANNEL", name, "duplicate channel name")
        seen.add(name)
        raw_path = _first(item, ("path", "file", "file_path", "audio_path"))
        if not isinstance(raw_path, str) or not raw_path:
            raise input_error("E_PROJECT_SCHEMA", name, "track path is required")
        path = Path(raw_path)
        if not path.is_absolute():
            path = root / path
        track_audio = item.get("audio", {})
        if not isinstance(track_audio, dict):
            track_audio = {}
        sample_rate_value = _first(
            item,
            ("sample_rate", "sample_rate_hz"),
            _first(track_audio, ("sample_rate", "sample_rate_hz"), global_sr),
        )
        channels_value = _first(
            item,
            ("channels", "channel_count"),
            _first(track_audio, ("channels", "channel_count"), 1),
        )
        bit_depth_value = _first(
            item,
            ("bit_depth", "bit_depth_bits"),
            _first(track_audio, ("bit_depth", "bit_depth_bits"), global_bit_depth),
        )
        source_object = item.get("source", {})
        if not isinstance(source_object, dict):
            source_object = {}
        source_type = _normalize_source_type(
            _first(
                item,
                ("source_type", "type", "role", "kind"),
                _first(
                    source_object,
                    ("type", "source_type", "role", "kind"),
                    None,
                ),
            ),
            name,
        )
        anchor_value = _first(
            item,
            ("anchor", "anchor_position", "stage_position", "location", "position"),
            _first(
                source_object,
                (
                    "anchor",
                    "anchor_position",
                    "stage_position",
                    "location",
                    "position",
                ),
                None,
            ),
        )
        anchor = None
        if anchor_value is not None:
            anchor = _vector3(anchor_value, name, "anchor")
        elif source_type in {"actor", "environment", "audience"}:
            raise input_error("E_ANCHOR_MISSING", name, "spatial source needs an anchor")

        if sample_rate_value is None:
            raise input_error("E_PROJECT_SCHEMA", name, "sample_rate is required")
        if channels_value is None:
            raise input_error("E_PROJECT_SCHEMA", name, "channels is required")
        parsed_sample_rate = _integer(sample_rate_value, name, "sample_rate")
        parsed_channels = _integer(channels_value, name, "channels")
        if parsed_sample_rate <= 0 or parsed_channels <= 0:
            raise input_error(
                "E_PROJECT_SCHEMA",
                name,
                "sample_rate and channels must be positive",
            )
        tracks.append(
            TrackSpec(
                index=index,
                name=name,
                path=path,
                path_display=_display_path(path, root),
                sample_rate=parsed_sample_rate,
                channels=parsed_channels,
                bit_depth=_normalize_bit_depth(bit_depth_value, name),
                source_type=source_type,
                anchor=anchor,
            )
        )
    return tuple(tracks)


def _display_path(path: Path, root: Path) -> str:
    try:
        return path.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return path.name


def _parse_head_tracking(data: dict[str, Any], tolerance: float) -> tuple[HeadSample, ...]:
    block = data.get(
        "head_tracking",
        data.get(
            "tracking",
            data.get("head_pose_samples", data.get("head_tracking_samples")),
        ),
    )
    if block is None:
        raise input_error("E_PROJECT_SCHEMA", "head_tracking", "head_tracking is required")
    if isinstance(block, list):
        tracking = block
        order = "wxyz"
        convention = "local_to_world"
    elif isinstance(block, dict):
        tracking = _first(
            block,
            ("samples", "frames", "tracking_samples", "poses"),
            None,
        )
        order = str(block.get("quaternion_order", data.get("quaternion_order", "wxyz"))).lower()
        order = order.replace(",", "").replace("_", "").strip()
        convention = str(
            block.get("orientation_convention", data.get("orientation_convention", "local_to_world"))
        ).lower()
    else:
        raise input_error("E_PROJECT_SCHEMA", "head_tracking", "head_tracking must be object or array")
    if order not in {"wxyz", "xyzw"}:
        raise input_error("E_PROJECT_SCHEMA", "head_tracking", "quaternion_order must be wxyz or xyzw")
    if convention not in {"local_to_world", "world_to_head"}:
        raise input_error(
            "E_PROJECT_SCHEMA",
            "head_tracking",
            "orientation_convention must be local_to_world or world_to_head",
        )
    if tracking is None and isinstance(block, dict):
        timestamps = _first(block, ("timestamps", "times"), None)
        positions = _first(block, ("positions", "head_positions"), None)
        orientations = _first(
            block,
            ("orientations", "quaternions", "orientation_quaternions"),
            None,
        )
        if timestamps is not None and positions is not None and orientations is not None:
            timestamps = _require_list(timestamps, "head_tracking", "timestamps")
            positions = _require_list(positions, "head_tracking", "positions")
            orientations = _require_list(
                orientations, "head_tracking", "orientations"
            )
            if not (len(timestamps) == len(positions) == len(orientations)):
                raise input_error(
                    "E_PROJECT_SCHEMA",
                    "head_tracking",
                    "timestamps, positions and orientations must have equal length",
                )
            tracking = [
                {
                    "t": timestamps[index],
                    "position": positions[index],
                    "orientation_quaternion": orientations[index],
                }
                for index in range(len(timestamps))
            ]
    tracking = _require_list(tracking, "head_tracking", "tracking samples")
    if not tracking:
        raise input_error("E_TRACKING_EMPTY", "head_tracking", "tracking sequence is empty")
    samples: list[HeadSample] = []
    previous = -math.inf
    for index, item in enumerate(tracking):
        item = _require_mapping(item, "head_tracking", f"samples[{index}]")
        time_value = _first(
            item,
            ("t", "time", "time_s", "timestamp", "timestamp_s"),
        )
        if time_value is None:
            raise input_error("E_PROJECT_SCHEMA", "head_tracking", f"samples[{index}] has no time")
        time_s = _number(time_value, "head_tracking", f"samples[{index}].time")
        if index and not time_s > previous:
            raise input_error(
                "E_TRACKING_TIME_NOT_STRICT",
                "head_tracking",
                f"timestamps are not strictly increasing at sample {index}",
            )
        previous = time_s
        position_value = _first(
            item,
            ("position", "head_position", "listener_position"),
            None,
        )
        if position_value is None:
            raise input_error(
                "E_PROJECT_SCHEMA",
                "head_tracking",
                f"samples[{index}] has no position",
            )
        quaternion_value = _first(
            item,
            (
                "orientation",
                "quaternion",
                "orientation_quaternion",
                "quaternion_wxyz",
                "quat",
                "rotation",
            ),
            None,
        )
        if quaternion_value is None:
            raise input_error(
                "E_PROJECT_SCHEMA",
                "head_tracking",
                f"samples[{index}] has no orientation",
            )
        samples.append(
            HeadSample(
                time_s=time_s,
                position=_vector3(position_value, "head_tracking", "position"),
                quaternion_wxyz=_quaternion(
                    quaternion_value,
                    "head_tracking",
                    f"samples[{index}].quaternion",
                    order,
                    tolerance,
                ),
            )
        )
    return tuple(samples)


def _parse_downmix(data: dict[str, Any], tracks: tuple[TrackSpec, ...]) -> dict[str, tuple[float, float]]:
    binaural = data.get("binaural", {})
    if not isinstance(binaural, dict):
        binaural = {}
    raw = _first(
        binaural,
        ("downmix", "downmix_rules", "weights", "binaural_downmix"),
        _first(
            data,
            ("downmix", "downmix_rules", "weights", "binaural_downmix"),
            {},
        ),
    )
    default_weights = {
        "actor": (1.0, 1.0),
        "environment": (0.75, 0.75),
        "audience": (0.55, 0.55),
    }
    downmix: dict[str, tuple[float, float]] = {}
    if isinstance(raw, dict):
        for track in tracks:
            if track.source_type == "speaker_reference":
                continue
            value = raw.get(track.name, raw.get("*", default_weights[track.source_type]))
            if isinstance(value, dict):
                left = _number(
                    _first(value, ("left", "l", "left_gain", "gain"), 0.0),
                    track.name,
                    "downmix.left",
                )
                right = _number(
                    _first(value, ("right", "r", "right_gain", "gain"), 0.0),
                    track.name,
                    "downmix.right",
                )
            elif isinstance(value, (list, tuple)):
                if len(value) != 2:
                    raise input_error("E_DOWNMIX", track.name, "downmix pair must have 2 values")
                left = _number(value[0], track.name, "downmix.left")
                right = _number(value[1], track.name, "downmix.right")
            else:
                gain = _number(value, track.name, "downmix weight")
                left = right = gain
            if left < 0 or right < 0:
                raise input_error("E_DOWNMIX", track.name, "downmix weights must be non-negative")
            downmix[track.name] = (left, right)
    elif isinstance(raw, list):
        by_name: dict[str, Any] = {}
        if all(
            isinstance(entry, (list, tuple)) and len(entry) == 2
            for entry in raw
        ):
            mixable_tracks = [
                track
                for track in tracks
                if track.source_type != "speaker_reference"
            ]
            if len(raw) != len(mixable_tracks):
                raise input_error(
                    "E_DOWNMIX",
                    "project",
                    "positional downmix array must match the number of mixed channels",
                )
            for track, pair in zip(mixable_tracks, raw):
                by_name[track.name] = {"left": pair[0], "right": pair[1]}
        for entry in raw:
            if isinstance(entry, (list, tuple)):
                continue
            entry = _require_mapping(entry, "downmix", "downmix rule")
            name = entry.get("channel", entry.get("name"))
            if not isinstance(name, str):
                raise input_error("E_DOWNMIX", "downmix", "rule needs channel")
            by_name[name] = entry
        for track in tracks:
            if track.source_type == "speaker_reference":
                continue
            value = by_name.get(
                track.name,
                {
                    "left": default_weights[track.source_type][0],
                    "right": default_weights[track.source_type][1],
                },
            )
            left = _number(
                _first(value, ("left", "l", "left_gain", "gain"), 0.0),
                track.name,
                "downmix.left",
            )
            right = _number(
                _first(value, ("right", "r", "right_gain", "gain"), 0.0),
                track.name,
                "downmix.right",
            )
            downmix[track.name] = (left, right)
    else:
        raise input_error("E_DOWNMIX", "project", "downmix must be object or array")
    return downmix


def _parse_interpolation_checks(
    data: dict[str, Any], tracks: tuple[TrackSpec, ...]
) -> tuple[InterpolationCheck, ...]:
    interpolation = data.get("interpolation", {})
    if not isinstance(interpolation, dict):
        interpolation = {}
    raw = _first(
        interpolation,
        ("checks", "check_points", "check_times", "checkpoints"),
        data.get("interpolation_checks", []),
    )
    if raw is None:
        raw = []
    if isinstance(raw, dict):
        raw = raw.get("checks", raw.get("points", []))
    raw = _require_list(raw, "interpolation", "checks")
    checks: list[InterpolationCheck] = []
    for index, item in enumerate(raw):
        if isinstance(item, (int, float)) and not isinstance(item, bool):
            checks.append(InterpolationCheck(float(item), None, None))
            continue
        item = _require_mapping(item, "interpolation", f"checks[{index}]")
        time_value = _first(item, ("time", "time_s", "t"))
        if time_value is None:
            raise input_error("E_PROJECT_SCHEMA", "interpolation", "check has no time")
        channel = item.get("channel", item.get("track"))
        if channel is not None and channel not in {track.name for track in tracks}:
            raise input_error("E_INTERPOLATION_CHECK", str(channel), "unknown channel")
        expected = _first(
            item,
            ("expected_azimuth_deg", "azimuth_deg", "expected_azimuth", "azimuth"),
        )
        checks.append(
            InterpolationCheck(
                time_s=_number(time_value, "interpolation", "check time"),
                channel=str(channel) if channel is not None else None,
                expected_azimuth_deg=(
                    _number(expected, "interpolation", "expected azimuth")
                    if expected is not None
                    else None
                ),
            )
        )
    return tuple(checks)


def load_project(path: str | Path, validate_audio: bool = True) -> Project:
    project_path = Path(path)
    try:
        raw_bytes = project_path.read_bytes()
    except FileNotFoundError as exc:
        raise input_error("E_PROJECT_MISSING", "project", f"project file not found: {project_path.name}") from exc
    try:
        data = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise input_error("E_PROJECT_JSON", "project", f"invalid project JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise input_error("E_PROJECT_SCHEMA", "project", "project JSON must be an object")
    root = project_path.resolve().parent
    tracks = _parse_tracks(data, root)
    head_data = data.get(
        "head_tracking",
        data.get("tracking", data.get("head_pose_samples")),
    )
    if isinstance(head_data, dict):
        try:
            quat_tol = float(head_data.get("quaternion_norm_tolerance", 1.0e-4))
        except (TypeError, ValueError) as exc:
            raise input_error(
                "E_PROJECT_SCHEMA",
                "head_tracking",
                "quaternion_norm_tolerance must be numeric",
            ) from exc
    else:
        try:
            quat_tol = float(data.get("quaternion_norm_tolerance", 1.0e-4))
        except (TypeError, ValueError) as exc:
            raise input_error(
                "E_PROJECT_SCHEMA",
                "head_tracking",
                "quaternion_norm_tolerance must be numeric",
            ) from exc
    if not math.isfinite(quat_tol) or quat_tol <= 0:
        raise input_error("E_PROJECT_SCHEMA", "head_tracking", "invalid quaternion tolerance")
    head_samples = _parse_head_tracking(data, quat_tol)
    sample_rate = tracks[0].sample_rate
    if any(track.sample_rate != sample_rate for track in tracks):
        raise input_error(
            "E_SAMPLE_RATE_INCONSISTENT",
            "project",
            "all tracks must use one sample rate for the binaural bus",
        )
    if validate_audio:
        _validate_audio_tracks(tracks)

    reference_channel = _first(
        data,
        ("reference_channel", "speaker_reference_channel"),
        None,
    )
    leakage = data.get("leakage", {})
    if not isinstance(leakage, dict):
        leakage = {}
    if reference_channel is None:
        reference_channel = _first(
            leakage,
            ("reference_channel", "reference", "speaker_reference"),
            None,
        )
    if reference_channel is None:
        refs = [track.name for track in tracks if track.source_type == "speaker_reference"]
        if len(refs) == 1:
            reference_channel = refs[0]
    if not isinstance(reference_channel, str) or reference_channel not in {track.name for track in tracks}:
        raise input_error("E_REFERENCE_CHANNEL", "speaker_reference", "reference channel is missing or unknown")
    if next(track for track in tracks if track.name == reference_channel).source_type != "speaker_reference":
        pass

    interpolation_checks = _parse_interpolation_checks(data, tracks)
    interpolation = data.get("interpolation", {})
    if not isinstance(interpolation, dict):
        interpolation = {}
    tolerance = _number(
        _first(interpolation, ("azimuth_tolerance_deg", "angle_tolerance_deg"), 2.0),
        "interpolation",
        "azimuth tolerance",
    )
    if tolerance < 0:
        raise input_error("E_PROJECT_SCHEMA", "interpolation", "azimuth tolerance must be >= 0")

    head_model = data.get("head_model", {})
    if not isinstance(head_model, dict):
        head_model = {}
    head_radius = _number(
        _first(head_model, ("head_radius_m", "head_radius"), 0.0875),
        "head_model",
        "head radius",
    )
    c = _number(
        _first(head_model, ("speed_of_sound_m_s", "speed_of_sound"), 343.0),
        "head_model",
        "speed of sound",
    )
    max_ild = _number(
        _first(head_model, ("max_ild_db", "ild_strength_db"), 12.0),
        "head_model",
        "max ILD",
    )
    if head_radius <= 0 or c <= 0 or max_ild < 0:
        raise input_error("E_PROJECT_SCHEMA", "head_model", "head model parameters must be positive")

    head_tracking = data.get(
        "head_tracking",
        data.get("tracking", data.get("head_pose_samples")),
    )
    if not isinstance(head_tracking, dict):
        head_tracking = {}
    max_ild_rate = _number(
        _first(
            head_tracking,
            (
                "max_ild_rate_db_per_s",
                "max_ild_slew_db_per_s",
                "max_ild_rate_db_s",
            ),
            _first(interpolation, ("max_ild_rate_db_per_s",), 600.0),
        ),
        "head_tracking",
        "max ILD rate",
    )
    max_itd_rate = _number(
        _first(
            head_tracking,
            (
                "max_itd_rate_us_per_s",
                "max_itd_slew_us_per_s",
                "max_itd_rate_us_s",
            ),
            _first(interpolation, ("max_itd_rate_us_per_s",), 30000.0),
        ),
        "head_tracking",
        "max ITD rate",
    )
    if max_ild_rate <= 0 or max_itd_rate <= 0:
        raise input_error("E_PROJECT_SCHEMA", "head_tracking", "rate limits must be positive")

    render = data.get("render", {})
    if not isinstance(render, dict):
        render = {}
    block_frames = _integer(
        _first(render, ("block_frames", "block_size"), 4800),
        "render",
        "block frames",
    )
    if block_frames <= 0:
        raise input_error("E_PROJECT_SCHEMA", "render", "block_frames must be positive")

    binaural = data.get("binaural", {})
    if not isinstance(binaural, dict):
        binaural = {}
    targets = data.get(
        "targets",
        data.get("master", data.get("loudness", {})),
    )
    if not isinstance(targets, dict):
        targets = {}
    target_loudness = _number(
        _first(
            targets,
            (
                "loudness_lufs",
                "target_loudness_lufs",
                "target_lufs",
                "integrated_loudness_lufs",
            ),
            _first(data, ("target_loudness_lufs",), -23.0),
        ),
        "targets",
        "target loudness",
    )
    true_peak = _number(
        _first(
            targets,
            (
                "true_peak_dbtp",
                "max_true_peak_dbtp",
                "true_peak_max_dbtp",
                "max_true_peak_db",
            ),
            _first(data, ("true_peak_dbtp", "max_true_peak_dbtp"), -1.0),
        ),
        "targets",
        "true peak",
    )
    if true_peak >= 0:
        raise input_error("E_PROJECT_SCHEMA", "targets", "true peak limit must be below 0 dBTP")

    leakage_lowpass = _number(
        _first(leakage, ("lowpass_hz", "leak_band_high_hz", "band_high_hz"), 120.0),
        "leakage",
        "leakage lowpass",
    )
    if not 0 < leakage_lowpass < sample_rate / 2:
        raise input_error("E_PROJECT_SCHEMA", "leakage", "leakage lowpass is outside Nyquist")
    max_delay_ms = _number(
        _first(leakage, ("max_delay_ms",), 25.0),
        "leakage",
        "max delay",
    )
    if max_delay_ms < 0:
        raise input_error("E_PROJECT_SCHEMA", "leakage", "max delay must be non-negative")
    injection_raw = _first(
        leakage,
        ("injection_channels", "injected_channels", "spill_channels"),
        [],
    )
    injection = _require_list(injection_raw, "leakage", "injection_channels")
    injection_channels: list[str] = []
    track_names = {track.name for track in tracks}
    for name in injection:
        if not isinstance(name, str) or name not in track_names:
            raise input_error("E_LEAKAGE_CHANNEL", str(name), "unknown leakage injection channel")
        injection_channels.append(name)
    leakage_gain = _number(_first(leakage, ("gain_db", "leakage_gain_db"), -9.0), "leakage", "gain")
    per_channel_raw = _first(leakage, ("channel_gain_db", "gains_db", "per_channel_gain_db"), {})
    if not isinstance(per_channel_raw, dict):
        raise input_error("E_PROJECT_SCHEMA", "leakage", "channel gains must be an object")
    per_channel_gain = {
        str(name): _number(value, str(name), "leakage gain")
        for name, value in per_channel_raw.items()
    }
    for name in per_channel_gain:
        if name not in track_names:
            raise input_error("E_LEAKAGE_CHANNEL", name, "unknown leakage gain channel")
    for name in injection_channels:
        if name not in per_channel_gain:
            per_channel_gain[name] = leakage_gain

    seed_value = data.get("seed", 0)
    seed = _integer(seed_value, "project", "seed")
    project_id = str(data.get("project_id", data.get("id", project_path.stem)))
    duration = _number(data.get("duration_seconds", data.get("duration_s", 0.0)), "project", "duration")
    if duration <= 0:
        duration = 1.0

    lookahead = _number(
        _first(render, ("limiter_lookahead_ms", "lookahead_ms"), 5.0),
        "render",
        "limiter lookahead",
    )
    release = _number(
        _first(render, ("limiter_release_ms", "release_ms"), 80.0),
        "render",
        "limiter release",
    )
    if lookahead < 0 or release <= 0:
        raise input_error("E_PROJECT_SCHEMA", "render", "invalid limiter timing")

    return Project(
        path=project_path.resolve(),
        root=root,
        raw=data,
        project_id=project_id,
        seed=seed,
        duration_s=duration,
        sample_rate=sample_rate,
        tracks=tracks,
        reference_channel=reference_channel,
        head_samples=head_samples,
        orientation_convention=str(
            (head_tracking.get("orientation_convention", data.get("orientation_convention", "local_to_world")))
        ).lower(),
        head_radius_m=head_radius,
        speed_of_sound_m_s=c,
        max_ild_db=max_ild,
        max_ild_rate_db_per_s=max_ild_rate,
        max_itd_rate_us_per_s=max_itd_rate,
        downmix=_parse_downmix(data, tracks),
        target_loudness_lufs=target_loudness,
        true_peak_dbtp=true_peak,
        block_frames=block_frames,
        leakage_lowpass_hz=leakage_lowpass,
        leakage_max_delay_ms=max_delay_ms,
        leakage_injection_channels=tuple(injection_channels),
        leakage_gain_db=leakage_gain,
        leakage_per_channel_gain_db=per_channel_gain,
        interpolation_checks=interpolation_checks,
        azimuth_tolerance_deg=tolerance,
        limiter_lookahead_ms=lookahead,
        limiter_release_ms=release,
    )


def _validate_audio_tracks(tracks: tuple[TrackSpec, ...]) -> None:
    for track in tracks:
        if not track.path.is_file():
            raise input_error("E_FILE_MISSING", track.name, f"audio file not found: {track.path_display}")
        try:
            info = sf.info(str(track.path))
        except Exception as exc:
            raise input_error("E_AUDIO_READ", track.name, f"cannot read audio file: {exc}") from exc
        if int(info.samplerate) != track.sample_rate:
            raise input_error(
                "E_SAMPLE_RATE_MISMATCH",
                track.name,
                f"declared {track.sample_rate} Hz, actual {info.samplerate} Hz",
            )
        if int(info.channels) != track.channels:
            raise input_error(
                "E_CHANNEL_COUNT_MISMATCH",
                track.name,
                f"declared {track.channels} channels, actual {info.channels}",
            )
        if track.bit_depth is not None:
            actual_bits = _detect_bit_depth(info.subtype)
            if actual_bits != track.bit_depth:
                raise input_error(
                    "E_BIT_DEPTH_MISMATCH",
                    track.name,
                    f"declared {track.bit_depth} bits, actual {actual_bits} bits ({info.subtype})",
                )


def read_track(track: TrackSpec) -> np.ndarray:
    try:
        data, sample_rate = sf.read(
            str(track.path),
            dtype="float64",
            always_2d=True,
        )
    except Exception as exc:
        raise input_error("E_AUDIO_READ", track.name, f"cannot read audio: {exc}") from exc
    if sample_rate != track.sample_rate:
        raise input_error(
            "E_SAMPLE_RATE_MISMATCH",
            track.name,
            f"declared {track.sample_rate} Hz, actual {sample_rate} Hz",
        )
    if data.shape[1] != track.channels:
        raise input_error(
            "E_CHANNEL_COUNT_MISMATCH",
            track.name,
            f"declared {track.channels} channels, actual {data.shape[1]}",
        )
    mono = np.mean(data, axis=1, dtype=np.float64)
    if not np.all(np.isfinite(mono)):
        raise input_error("E_AUDIO_NONFINITE", track.name, "audio contains NaN or Inf")
    return mono.astype(np.float64, copy=False)


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def project_digest(project: Project) -> str:
    payload = {
        "project": project.raw,
        "tracks": [
            {
                "name": track.name,
                "path": track.path_display,
                "sha256": file_sha256(track.path),
                "sample_rate": track.sample_rate,
                "channels": track.channels,
                "bit_depth": track.bit_depth,
                "source_type": track.source_type,
            }
            for track in project.tracks
        ],
    }
    encoded = json.dumps(payload, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()

