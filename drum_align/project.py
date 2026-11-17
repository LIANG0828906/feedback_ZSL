"""Project JSON parsing and validation."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import BusinessError
from .jsonio import display_name, read_json


SCHEMA_VERSION = 1
ALGORITHM_VERSION = "drum-align-v1"
DEFAULT_BLOCK_SAMPLES = 16_384
DEFAULT_PHASE_WINDOW = 4_096
DEFAULT_PHASE_PRE_ROLL = 480
DEFAULT_PHASE_THRESHOLD = 0.35
DEFAULT_PHASE_GAIN = 0.5
DEFAULT_CURVE_POINTS = 12
DEFAULT_MAX_OFFSET_SECONDS = 20.0


@dataclass(frozen=True)
class TrackSpec:
    camera: str
    path: Path
    display_path: str
    gain: float = 1.0


@dataclass(frozen=True)
class PhaseConfig:
    window_samples: int = DEFAULT_PHASE_WINDOW
    pre_roll_samples: int = DEFAULT_PHASE_PRE_ROLL
    threshold: float = DEFAULT_PHASE_THRESHOLD
    attenuation_gain: float = DEFAULT_PHASE_GAIN


@dataclass
class LoadedProject:
    project_path: Path
    root: Path
    sample_rate: int | None
    base_camera: str
    bar_starts: list[int]
    min_bar_starts: int
    tracks: dict[str, TrackSpec]
    phase: PhaseConfig
    curve_points: int
    max_offset_seconds: float
    block_samples: int
    master_path: Path
    report_path: Path
    snapshot_path: Path
    raw: dict[str, Any] = field(repr=False)

    def digest_payload(
        self,
        *,
        wav_hashes: dict[str, str],
        wav_metadata: dict[str, dict[str, int]],
    ) -> dict[str, Any]:
        ordered_tracks = []
        for camera in sorted(self.tracks):
            track = self.tracks[camera]
            ordered_tracks.append(
                {
                    "camera": camera,
                    "display_path": track.display_path,
                    "gain": track.gain,
                    "wav_sha256": wav_hashes[camera],
                    "wav": wav_metadata[camera],
                }
            )
        return {
            "algorithm_version": ALGORITHM_VERSION,
            "schema_version": SCHEMA_VERSION,
            "sample_rate": self.sample_rate,
            "base_camera": self.base_camera,
            "bar_starts_samples": self.bar_starts,
            "min_bar_starts": self.min_bar_starts,
            "tracks": ordered_tracks,
            "phase_check": {
                "window_samples": self.phase.window_samples,
                "pre_roll_samples": self.phase.pre_roll_samples,
                "threshold": self.phase.threshold,
                "attenuation_gain": self.phase.attenuation_gain,
            },
            "curve_points": self.curve_points,
            "max_offset_seconds": self.max_offset_seconds,
            "block_samples": self.block_samples,
        }


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _require_int(value: Any, field_name: str, *, minimum: int | None = None) -> int:
    if not _is_int(value):
        raise BusinessError("E_PROJECT_INVALID", f"{field_name} must be an integer")
    if minimum is not None and value < minimum:
        raise BusinessError(
            "E_PROJECT_INVALID", f"{field_name} must be at least {minimum}"
        )
    return value


def _require_number(
    value: Any, field_name: str, *, minimum: float | None = None
) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise BusinessError("E_PROJECT_INVALID", f"{field_name} must be a number")
    number = float(value)
    if not math.isfinite(number):
        raise BusinessError("E_PROJECT_INVALID", f"{field_name} must be finite")
    if minimum is not None and number < minimum:
        raise BusinessError(
            "E_PROJECT_INVALID", f"{field_name} must be at least {minimum}"
        )
    return number


def _parse_timecode(value: str, sample_rate: int, frame_rate: float) -> int:
    parts = value.strip().split(":")
    if len(parts) not in (3, 4):
        raise BusinessError(
            "E_PROJECT_INVALID", f"invalid timecode {value!r}; expected HH:MM:SS[:FF]"
        )
    try:
        hours = int(parts[0])
        minutes = int(parts[1])
        seconds = float(parts[2])
        frames = int(parts[3]) if len(parts) == 4 else 0
    except ValueError as exc:
        raise BusinessError("E_PROJECT_INVALID", f"invalid timecode {value!r}") from exc
    if min(hours, minutes, frames) < 0 or seconds < 0:
        raise BusinessError("E_PROJECT_INVALID", f"invalid timecode {value!r}")
    total_seconds = hours * 3600.0 + minutes * 60.0 + seconds + frames / frame_rate
    return int(round(total_seconds * sample_rate))


def _parse_bar_value(
    item: Any, *, sample_rate: int, frame_rate: float, default_unit: str
) -> int:
    if _is_int(item):
        if default_unit == "seconds":
            return int(round(item * sample_rate))
        return item
    if isinstance(item, float):
        return int(round(item * sample_rate))
    if isinstance(item, str):
        return _parse_timecode(item, sample_rate, frame_rate)
    if isinstance(item, dict):
        if "samples" in item:
            return _require_int(item["samples"], "bar_starts.samples", minimum=0)
        if "seconds" in item:
            return int(
                round(
                    _require_number(
                        item["seconds"], "bar_starts.seconds", minimum=0.0
                    )
                    * sample_rate
                )
            )
        if "timecode" in item:
            return _parse_timecode(str(item["timecode"]), sample_rate, frame_rate)
    raise BusinessError("E_PROJECT_INVALID", f"unsupported bar start value {item!r}")


def _parse_bar_starts(
    data: dict[str, Any], *, sample_rate: int | None
) -> tuple[list[Any], str]:
    if "bar_starts_samples" in data:
        values = data["bar_starts_samples"]
        return list(values) if isinstance(values, list) else values, "samples"
    if "bar_times_seconds" in data:
        values = data["bar_times_seconds"]
        return list(values) if isinstance(values, list) else values, "seconds"
    if "bar_starts" in data:
        values = data["bar_starts"]
        return list(values) if isinstance(values, list) else values, "samples"
    if "timecode_table" in data:
        table = data["timecode_table"]
        if isinstance(table, list):
            return table, "timecode"
        if isinstance(table, dict) and isinstance(table.get("bar_starts"), list):
            return table["bar_starts"], "timecode"
    raise BusinessError(
        "E_PROJECT_INVALID",
        "missing bar starts (use bar_starts_samples or bar_times_seconds)",
    )


def _parse_tracks(
    data: dict[str, Any], root: Path
) -> dict[str, TrackSpec]:
    raw_tracks = data.get("tracks")
    if raw_tracks is None:
        raise BusinessError("E_PROJECT_INVALID", "missing tracks")
    entries: list[tuple[str, Any]] = []
    if isinstance(raw_tracks, dict):
        entries = [(str(name), spec) for name, spec in raw_tracks.items()]
    elif isinstance(raw_tracks, list):
        for spec in raw_tracks:
            if not isinstance(spec, dict):
                raise BusinessError("E_PROJECT_INVALID", "track entries must be objects")
            name = spec.get("camera", spec.get("name", spec.get("id")))
            if not isinstance(name, str) or not name:
                raise BusinessError(
                    "E_PROJECT_INVALID", "each track needs a camera/name/id"
                )
            entries.append((name, spec))
    else:
        raise BusinessError("E_PROJECT_INVALID", "tracks must be an object or array")
    if not entries:
        raise BusinessError("E_PROJECT_INVALID", "tracks must not be empty")

    tracks: dict[str, TrackSpec] = {}
    for name, spec in entries:
        if name in tracks:
            raise BusinessError("E_PROJECT_INVALID", f"duplicate camera {name!r}")
        if isinstance(spec, str):
            raw_path = spec
            gain = 1.0
        elif isinstance(spec, dict):
            raw_path = spec.get("path", spec.get("file"))
            gain = _require_number(spec.get("gain", 1.0), f"tracks.{name}.gain", minimum=0)
        else:
            raise BusinessError(
                "E_PROJECT_INVALID", f"invalid track definition for {name!r}"
            )
        if not isinstance(raw_path, str) or not raw_path:
            raise BusinessError(
                "E_PROJECT_INVALID", f"track {name!r} is missing a WAV path"
            )
        candidate = Path(raw_path)
        resolved = candidate if candidate.is_absolute() else root / candidate
        tracks[name] = TrackSpec(
            camera=name,
            path=resolved,
            display_path=display_name(resolved, root),
            gain=gain,
        )
    return tracks


def load_project(project_path: Path) -> LoadedProject:
    project_path = project_path.resolve()
    try:
        data = read_json(project_path)
    except FileNotFoundError as exc:
        raise BusinessError("E_PROJECT_READ", "project JSON not found") from exc
    except (OSError, ValueError) as exc:
        raise BusinessError(
            "E_PROJECT_READ", f"cannot read project JSON: {type(exc).__name__}"
        ) from exc
    if not isinstance(data, dict):
        raise BusinessError("E_PROJECT_INVALID", "project root must be an object")

    version = data.get("schema_version", SCHEMA_VERSION)
    if version != SCHEMA_VERSION:
        raise BusinessError(
            "E_PROJECT_INVALID", f"unsupported schema_version {version!r}"
        )

    sample_rate_value = data.get("sample_rate")
    sample_rate = (
        None
        if sample_rate_value is None
        else _require_int(sample_rate_value, "sample_rate", minimum=1)
    )
    if sample_rate is not None and sample_rate != 48_000:
        raise BusinessError(
            "E_SAMPLE_RATE",
            f"project sample_rate must be 48000, got {sample_rate}",
        )

    base_camera = data.get("base_camera", data.get("reference_camera"))
    if not isinstance(base_camera, str) or not base_camera:
        raise BusinessError("E_PROJECT_INVALID", "missing base_camera")

    min_bar_starts = _require_int(
        data.get("min_bar_starts", data.get("min_anchor_count", 2)),
        "min_bar_starts",
        minimum=1,
    )
    raw_bars, default_unit = _parse_bar_starts(data, sample_rate=sample_rate)
    if not isinstance(raw_bars, list):
        raise BusinessError("E_PROJECT_INVALID", "bar starts must be an array")
    if len(raw_bars) < min_bar_starts:
        raise BusinessError(
            "E_BAR_STARTS_COUNT",
            f"only {len(raw_bars)} bar starts; project requires {min_bar_starts}",
        )
    # A time-based tick needs the sample rate. If it is omitted from the project,
    # it is validated later after the reference WAV has been opened.
    if sample_rate is not None:
        frame_rate = _require_number(data.get("timecode_frame_rate", 25.0), "timecode_frame_rate", minimum=0.001)
        bars = [
            _parse_bar_value(
                item,
                sample_rate=sample_rate,
                frame_rate=frame_rate,
                default_unit=default_unit,
            )
            for item in raw_bars
        ]
        _validate_bars(bars)
    else:
        bars = []

    tracks = _parse_tracks(data, project_path.parent)
    if base_camera not in tracks:
        raise BusinessError(
            "E_BASE_CAMERA",
            f"base camera {base_camera!r} is not present in tracks",
            camera=base_camera,
        )

    phase_data = data.get("phase_check", data.get("phase", {}))
    if not isinstance(phase_data, dict):
        raise BusinessError("E_PROJECT_INVALID", "phase_check must be an object")
    phase = PhaseConfig(
        window_samples=_require_int(
            phase_data.get("window_samples", DEFAULT_PHASE_WINDOW),
            "phase_check.window_samples",
            minimum=32,
        ),
        pre_roll_samples=_require_int(
            phase_data.get("pre_roll_samples", DEFAULT_PHASE_PRE_ROLL),
            "phase_check.pre_roll_samples",
            minimum=0,
        ),
        threshold=_require_number(
            phase_data.get("threshold", DEFAULT_PHASE_THRESHOLD),
            "phase_check.threshold",
            minimum=-1.0,
        ),
        attenuation_gain=_require_number(
            phase_data.get("attenuation_gain", phase_data.get("gain", DEFAULT_PHASE_GAIN)),
            "phase_check.attenuation_gain",
            minimum=0.0,
        ),
    )
    if phase.threshold > 1.0:
        raise BusinessError(
            "E_PROJECT_INVALID", "phase_check.threshold must not exceed 1"
        )
    if phase.attenuation_gain > 1.0:
        raise BusinessError(
            "E_PROJECT_INVALID", "phase_check.attenuation_gain must not exceed 1"
        )

    curve_points = _require_int(
        data.get("curve_points", DEFAULT_CURVE_POINTS),
        "curve_points",
        minimum=1,
    )
    max_offset_seconds = _require_number(
        data.get("max_offset_seconds", DEFAULT_MAX_OFFSET_SECONDS),
        "max_offset_seconds",
        minimum=0.0,
    )

    render_data = data.get("render", {})
    output_data = data.get("outputs", {})
    if not isinstance(render_data, dict):
        raise BusinessError("E_PROJECT_INVALID", "render must be an object")
    if not isinstance(output_data, dict):
        raise BusinessError("E_PROJECT_INVALID", "outputs must be an object")
    block_samples = _require_int(
        render_data.get("block_samples", data.get("block_samples", DEFAULT_BLOCK_SAMPLES)),
        "render.block_samples",
        minimum=1,
    )

    def output_path(name: str, default: str) -> Path:
        value = output_data.get(name, default)
        if not isinstance(value, str) or not value:
            raise BusinessError("E_PROJECT_INVALID", f"outputs.{name} must be a path")
        path = Path(value)
        return path if path.is_absolute() else project_path.parent / path

    return LoadedProject(
        project_path=project_path,
        root=project_path.parent,
        sample_rate=sample_rate,
        base_camera=base_camera,
        bar_starts=bars,
        min_bar_starts=min_bar_starts,
        tracks=tracks,
        phase=phase,
        curve_points=curve_points,
        max_offset_seconds=max_offset_seconds,
        block_samples=block_samples,
        master_path=output_path("master", "aligned_master.wav"),
        report_path=output_path("report", "alignment_report.json"),
        snapshot_path=output_path("snapshot", "alignment_state.json"),
        raw=data,
    )


def complete_time_based_bars(
    project: LoadedProject, sample_rate: int
) -> list[int]:
    """Resolve timecode/seconds bar starts when sample rate came from the WAV."""
    if project.bar_starts:
        return project.bar_starts
    raw_bars, default_unit = _parse_bar_starts(project.raw, sample_rate=sample_rate)
    frame_rate = _require_number(
        project.raw.get("timecode_frame_rate", 25.0),
        "timecode_frame_rate",
        minimum=0.001,
    )
    bars = [
        _parse_bar_value(
            item,
            sample_rate=sample_rate,
            frame_rate=frame_rate,
            default_unit=default_unit,
        )
        for item in raw_bars
    ]
    _validate_bars(bars)
    project.bar_starts = bars
    return bars


def _validate_bars(bars: list[int]) -> None:
    for index, value in enumerate(bars):
        if not _is_int(value) or value < 0:
            raise BusinessError(
                "E_BAR_STARTS_ORDER",
                f"bar start at index {index} is not a non-negative integer sample",
            )
        if index and value <= bars[index - 1]:
            raise BusinessError(
                "E_BAR_STARTS_ORDER",
                f"bar starts must be strictly ascending (index {index})",
            )
