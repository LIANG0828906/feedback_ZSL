from __future__ import annotations

import hashlib
import json
import math
import re
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any

from .errors import invalid_project


_NOTE_RE = re.compile(r"^([A-Ga-g])([#b]?)(-?\d+)$")
_SEMITONES = {"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}


@dataclass(frozen=True)
class ChannelSpec:
    name: str
    path: str
    string_number: int
    pitch: str
    pan: float
    start_offset_samples: int
    reference: bool

    @property
    def frequency_hz(self) -> float:
        return pitch_to_frequency(self.pitch)


@dataclass(frozen=True)
class CrosstalkSpec:
    source: str
    target: str
    start_seconds: float
    end_seconds: float
    gain: float


@dataclass(frozen=True)
class SegmentSpec:
    name: str
    start_seconds: float
    end_seconds: float
    gains: dict[str, float]


@dataclass(frozen=True)
class DecouplingSpec:
    residual_threshold_db: float
    minimum_attenuation_db: float
    detection_margin_db: float
    max_unrelated_peak_loss_db: float


@dataclass(frozen=True)
class ProjectSpec:
    manifest_path: Path
    manifest_sha256: str
    project_id: str
    seed: int
    sample_rate: int
    bit_depth: int
    duration_seconds: float
    reference_times: tuple[float, ...]
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    block_size: int
    crossfade_seconds: float
    channels: tuple[ChannelSpec, ...]
    crosstalk: tuple[CrosstalkSpec, ...]
    segments: tuple[SegmentSpec, ...]
    decoupling: DecouplingSpec

    @property
    def total_samples(self) -> int:
        return int(round(self.duration_seconds * self.sample_rate))

    @property
    def channel_map(self) -> dict[str, ChannelSpec]:
        return {channel.name: channel for channel in self.channels}

    @property
    def reference_channel(self) -> ChannelSpec:
        matches = [channel for channel in self.channels if channel.reference]
        if len(matches) != 1:
            raise invalid_project(
                "manifest", "exactly one crosstalk-free reference channel is required"
            )
        return matches[0]


def pitch_to_semitone(pitch: str) -> int:
    match = _NOTE_RE.match(pitch)
    if not match:
        raise ValueError(f"invalid pitch: {pitch}")
    note, accidental, octave_text = match.groups()
    value = _SEMITONES[note.upper()]
    if accidental == "#":
        value += 1
    elif accidental == "b":
        value -= 1
    return (int(octave_text) + 1) * 12 + value


def pitch_to_frequency(pitch: str) -> float:
    return 440.0 * (2.0 ** ((pitch_to_semitone(pitch) - 69) / 12.0))


def _require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise invalid_project("manifest", f"{label} must be an object")
    return value


def _require_list(value: Any, label: str) -> list[Any]:
    if not isinstance(value, list):
        raise invalid_project("manifest", f"{label} must be an array")
    return value


def _finite_number(value: Any, label: str, channel: str = "manifest") -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise invalid_project(channel, f"{label} must be a number")
    number = float(value)
    if not math.isfinite(number):
        raise invalid_project(channel, f"{label} must be finite")
    return number


def canonical_manifest_digest(document: dict[str, Any]) -> str:
    canonical = json.dumps(
        document, sort_keys=True, separators=(",", ":"), ensure_ascii=False
    ).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def load_manifest(path: Path) -> ProjectSpec:
    try:
        raw = path.read_text(encoding="utf-8")
    except FileNotFoundError as exc:
        raise invalid_project("manifest", f"manifest not found: {path.name}") from exc
    except OSError as exc:
        raise invalid_project("manifest", f"cannot read manifest: {exc}") from exc
    try:
        document = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise invalid_project("manifest", f"invalid JSON: {exc.msg}") from exc
    document = _require_object(document, "manifest")
    digest = canonical_manifest_digest(document)

    if document.get("schema_version") != 1:
        raise invalid_project("manifest", "unsupported schema_version")
    project_id = document.get("project_id")
    if not isinstance(project_id, str) or not project_id.strip():
        raise invalid_project("manifest", "project_id must be a non-empty string")

    def integer_field(key: str, default: int | None = None) -> int:
        value = document.get(key, default)
        if isinstance(value, bool) or not isinstance(value, int):
            raise invalid_project("manifest", f"{key} must be an integer")
        return value

    seed = integer_field("seed")
    sample_rate = integer_field("sample_rate")
    bit_depth = integer_field("bit_depth")
    block_size = integer_field("block_size", 4096)
    if seed < 0:
        raise invalid_project("manifest", "seed must be non-negative")
    if sample_rate < 8000:
        raise invalid_project("manifest", "sample_rate must be at least 8000")
    if bit_depth not in (16, 24, 32):
        raise invalid_project("manifest", "bit_depth must be 16, 24, or 32")
    if block_size < 1:
        raise invalid_project("manifest", "block_size must be positive")

    duration = _finite_number(document.get("duration_seconds"), "duration_seconds")
    if duration <= 0:
        raise invalid_project("manifest", "duration_seconds must be positive")
    target_loudness = _finite_number(
        document.get("target_loudness_lufs"), "target_loudness_lufs"
    )
    true_peak_limit = _finite_number(
        document.get("true_peak_limit_dbtp", -1.0), "true_peak_limit_dbtp"
    )
    if target_loudness > 0:
        raise invalid_project("manifest", "target_loudness_lufs must be at most 0")
    if true_peak_limit > 0:
        raise invalid_project("manifest", "true_peak_limit_dbtp must be at most 0")
    crossfade_seconds = _finite_number(
        document.get("crossfade_seconds", 0.05), "crossfade_seconds"
    )
    if crossfade_seconds < 0:
        raise invalid_project("manifest", "crossfade_seconds must be non-negative")

    reference_times_raw = _require_list(
        document.get("reference_times_seconds"), "reference_times_seconds"
    )
    if not reference_times_raw:
        raise invalid_project("manifest", "reference_times_seconds must not be empty")
    reference_times = tuple(
        _finite_number(value, "reference time") for value in reference_times_raw
    )
    for index, value in enumerate(reference_times):
        if value < 0 or value >= duration:
            raise invalid_project(
                "manifest", "reference time must lie inside the project duration"
            )
        if index and value <= reference_times[index - 1]:
            raise invalid_project(
                "manifest",
                "reference_times_seconds must be strictly ascending",
                reference_index=index,
            )

    channels_raw = _require_list(document.get("channels"), "channels")
    if len(channels_raw) < 2:
        raise invalid_project("manifest", "at least two channels are required")
    channels: list[ChannelSpec] = []
    names: set[str] = set()
    string_numbers: set[int] = set()
    pitches: set[int] = set()
    for index, item in enumerate(channels_raw):
        item = _require_object(item, f"channels[{index}]")
        name = item.get("name")
        if not isinstance(name, str) or not name:
            raise invalid_project("manifest", f"channels[{index}].name is invalid")
        channel = name
        if name in names:
            raise invalid_project(channel, "duplicate channel name")
        names.add(name)

        path_text = item.get("path")
        if not isinstance(path_text, str) or not path_text:
            raise invalid_project(channel, "path must be a non-empty string")
        string_number = item.get("string")
        if isinstance(string_number, bool) or not isinstance(string_number, int):
            raise invalid_project(channel, "string must be an integer")
        if string_number in string_numbers:
            raise invalid_project(channel, "duplicate string number")
        string_numbers.add(string_number)

        pitch = item.get("pitch")
        if not isinstance(pitch, str):
            raise invalid_project(channel, "pitch must be a string")
        try:
            semitone = pitch_to_semitone(pitch)
        except ValueError as exc:
            raise invalid_project(channel, str(exc)) from exc
        if semitone in pitches:
            raise invalid_project(channel, "duplicate pitch")
        pitches.add(semitone)

        pan = _finite_number(item.get("pan", 0.0), "pan", channel)
        if not -1.0 <= pan <= 1.0:
            raise invalid_project(channel, "pan must be in [-1, 1]")
        offset = item.get("start_offset_samples", 0)
        if isinstance(offset, bool) or not isinstance(offset, int):
            raise invalid_project(channel, "start_offset_samples must be an integer")
        if abs(offset) >= round(duration * sample_rate):
            raise invalid_project(channel, "start_offset_samples exceeds duration")
        reference = item.get("reference", False)
        if not isinstance(reference, bool):
            raise invalid_project(channel, "reference must be boolean")
        channels.append(
            ChannelSpec(
                name=name,
                path=path_text,
                string_number=string_number,
                pitch=pitch,
                pan=pan,
                start_offset_samples=offset,
                reference=reference,
            )
        )

    channel_map = {channel.name: channel for channel in channels}
    reference_channels = [channel.name for channel in channels if channel.reference]
    if len(reference_channels) != 1:
        raise invalid_project(
            "manifest", "exactly one crosstalk-free reference channel is required"
        )

    crosstalk_raw = _require_list(document.get("crosstalk", []), "crosstalk")
    crosstalk: list[CrosstalkSpec] = []
    for index, item in enumerate(crosstalk_raw):
        item = _require_object(item, f"crosstalk[{index}]")
        source = item.get("source")
        target = item.get("target")
        if source not in channel_map:
            raise invalid_project("manifest", f"crosstalk[{index}] source is unknown")
        if target not in channel_map:
            raise invalid_project("manifest", f"crosstalk[{index}] target is unknown")
        if source == target:
            raise invalid_project(
                str(target), "crosstalk source and target must differ"
            )
        if channel_map[str(target)].reference:
            raise invalid_project(
                str(target), "reference channel must not receive crosstalk"
            )
        window = _require_list(item.get("window_seconds"), "crosstalk window")
        if len(window) != 2:
            raise invalid_project(
                str(target), "crosstalk window must contain start and end"
            )
        start = _finite_number(window[0], "crosstalk start", str(target))
        end = _finite_number(window[1], "crosstalk end", str(target))
        if start < 0 or end > duration or end <= start:
            raise invalid_project(
                str(target), "crosstalk window must be ordered inside the duration"
            )
        gain = _finite_number(item.get("gain"), "crosstalk gain", str(target))
        if gain <= 0:
            raise invalid_project(str(target), "crosstalk gain must be positive")
        crosstalk.append(
            CrosstalkSpec(
                source=str(source),
                target=str(target),
                start_seconds=start,
                end_seconds=end,
                gain=gain,
            )
        )

    segments_raw = _require_list(document.get("segments"), "segments")
    if not segments_raw:
        raise invalid_project("manifest", "segments must not be empty")
    segments: list[SegmentSpec] = []
    segment_names: set[str] = set()
    previous_end: Fraction | None = None
    for index, item in enumerate(segments_raw):
        item = _require_object(item, f"segments[{index}]")
        name = item.get("name")
        if not isinstance(name, str) or not name:
            raise invalid_project("manifest", f"segments[{index}].name is invalid")
        if name in segment_names:
            raise invalid_project("manifest", f"duplicate segment name: {name}")
        segment_names.add(name)
        start = _finite_number(item.get("start_seconds"), "segment start")
        end = _finite_number(item.get("end_seconds"), "segment end")
        start_fraction = Fraction(str(start))
        end_fraction = Fraction(str(end))
        if start_fraction < 0 or end_fraction <= start_fraction:
            raise invalid_project("manifest", f"segment {name} has invalid bounds")
        if previous_end is None and start_fraction != 0:
            raise invalid_project("manifest", "first segment must start at zero")
        if previous_end is not None and start_fraction != previous_end:
            raise invalid_project("manifest", "segments must be contiguous and ordered")
        if end_fraction > Fraction(str(duration)):
            raise invalid_project("manifest", f"segment {name} exceeds duration")
        previous_end = end_fraction
        gains_raw = _require_object(item.get("gains"), f"segments[{index}].gains")
        gains: dict[str, float] = {}
        for channel in channels:
            if channel.name not in gains_raw:
                raise invalid_project(
                    channel.name, f"segment {name} is missing channel gain"
                )
            value = _finite_number(
                gains_raw[channel.name], f"segment {name} gain", channel.name
            )
            if value < 0:
                raise invalid_project(channel.name, "segment gain must be non-negative")
            gains[channel.name] = value
        segments.append(
            SegmentSpec(
                name=name,
                start_seconds=start,
                end_seconds=end,
                gains=gains,
            )
        )
    if previous_end != Fraction(str(duration)):
        raise invalid_project("manifest", "segments must cover the full duration")
    minimum_segment_seconds = min(
        segment.end_seconds - segment.start_seconds for segment in segments
    )
    if crossfade_seconds > minimum_segment_seconds:
        raise invalid_project(
            "manifest", "crossfade_seconds exceeds the shortest segment"
        )

    decoupling_raw = _require_object(
        document.get("decoupling", {}), "decoupling"
    )
    residual_threshold = _finite_number(
        decoupling_raw.get("residual_threshold_db", -42.0),
        "residual_threshold_db",
    )
    minimum_attenuation = _finite_number(
        decoupling_raw.get("minimum_attenuation_db", 18.0),
        "minimum_attenuation_db",
    )
    detection_margin = _finite_number(
        decoupling_raw.get("detection_margin_db", 12.0), "detection_margin_db"
    )
    max_peak_loss = _finite_number(
        decoupling_raw.get("max_unrelated_peak_loss_db", 1.0),
        "max_unrelated_peak_loss_db",
    )
    if minimum_attenuation <= 0 or max_peak_loss < 0:
        raise invalid_project("manifest", "decoupling thresholds are invalid")

    return ProjectSpec(
        manifest_path=path,
        manifest_sha256=digest,
        project_id=project_id,
        seed=seed,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        duration_seconds=duration,
        reference_times=reference_times,
        target_loudness_lufs=target_loudness,
        true_peak_limit_dbtp=true_peak_limit,
        block_size=block_size,
        crossfade_seconds=crossfade_seconds,
        channels=tuple(channels),
        crosstalk=tuple(crosstalk),
        segments=tuple(segments),
        decoupling=DecouplingSpec(
            residual_threshold_db=residual_threshold,
            minimum_attenuation_db=minimum_attenuation,
            detection_margin_db=detection_margin,
            max_unrelated_peak_loss_db=max_peak_loss,
        ),
    )


def load_and_validate_channels(
    project: ProjectSpec,
) -> tuple[dict[str, Any], dict[str, dict[str, Any]]]:
    from .wavio import read_wav_mono

    base = project.manifest_path.parent
    arrays: dict[str, Any] = {}
    metadata: dict[str, dict[str, Any]] = {}
    for channel in project.channels:
        path = base / channel.path
        samples, info = read_wav_mono(path, channel.name)
        if info.sample_rate != project.sample_rate:
            raise invalid_project(
                channel.name,
                "sample rate does not match manifest",
                expected_sample_rate=project.sample_rate,
                actual_sample_rate=info.sample_rate,
            )
        if info.bit_depth != project.bit_depth:
            raise invalid_project(
                channel.name,
                "bit depth does not match manifest",
                expected_bit_depth=project.bit_depth,
                actual_bit_depth=info.bit_depth,
            )
        if info.channels != 1:
            raise invalid_project(
                channel.name,
                "channel WAV must be mono",
                expected_channels=1,
                actual_channels=info.channels,
            )
        if info.frames != project.total_samples:
            raise invalid_project(
                channel.name,
                "channel frame count does not match duration",
                expected_frames=project.total_samples,
                actual_frames=info.frames,
            )
        arrays[channel.name] = samples
        metadata[channel.name] = {
            "file_sha256": info.sha256,
            "frames": info.frames,
            "sample_rate": info.sample_rate,
            "bit_depth": info.bit_depth,
            "channels": info.channels,
        }
    return arrays, metadata
