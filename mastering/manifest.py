from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import HardError, Issue


@dataclass(frozen=True)
class AutomationNode:
    time: float
    gain_db: float


@dataclass(frozen=True)
class DriftAnchor:
    output_sample: int
    source_sample: int


@dataclass(frozen=True)
class AutomationConfig:
    gain_min_db: float
    gain_max_db: float
    jump_threshold_db: float
    smoothing_seconds: float


@dataclass(frozen=True)
class OutputConfig:
    sample_rate: int
    channels: int
    bits_per_sample: int
    frames: int
    true_peak_limit_dbtp: float
    target_loudness_lufs: float | None
    loudness_tolerance_lu: float


@dataclass(frozen=True)
class TrackSpec:
    name: str
    file: str
    path: Path
    sample_rate: int
    bits_per_sample: int
    channels: int
    automation: tuple[AutomationNode, ...]
    drift_anchors: tuple[DriftAnchor, ...]


@dataclass(frozen=True)
class ProjectSpec:
    name: str
    seed: int
    output_seconds: float
    schema_version: int
    reference_pulse_output_sample: int
    automation_config: AutomationConfig
    output: OutputConfig
    tracks: tuple[TrackSpec, ...]
    manifest_path: Path

    @property
    def output_frames(self) -> int:
        return int(round(self.output_seconds * self.output.sample_rate))


def _finite_number(value: Any, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"{label} must be numeric")
    number = float(value)
    if not math.isfinite(number):
        raise ValueError(f"{label} must be finite")
    return number


def _positive_integer(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ValueError(f"{label} must be a positive integer")
    return int(value)


def _nonnegative_integer(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError(f"{label} must be a non-negative integer")
    return int(value)


def _parse_automation(
    raw: Any,
    track_name: str,
    config: AutomationConfig,
) -> tuple[tuple[AutomationNode, ...], list[Issue]]:
    issues: list[Issue] = []
    if raw is None:
        return (), issues
    if not isinstance(raw, list):
        return (), [
            Issue(
                "INVALID_AUTOMATION_CURVE",
                "automation must be an array of time/gain objects",
                track_name,
            )
        ]
    nodes: list[AutomationNode] = []
    for index, item in enumerate(raw):
        try:
            if not isinstance(item, dict):
                raise ValueError("node is not an object")
            node_time = _finite_number(item.get("time"), "time")
            gain_db = _finite_number(item.get("gain_db"), "gain_db")
            if node_time < 0:
                raise ValueError("time must be non-negative")
            if gain_db < config.gain_min_db or gain_db > config.gain_max_db:
                raise ValueError(
                    f"gain_db {gain_db} outside [{config.gain_min_db}, {config.gain_max_db}]"
                )
            if nodes and node_time < nodes[-1].time:
                raise ValueError("node times must be non-decreasing")
            nodes.append(AutomationNode(node_time, gain_db))
        except ValueError as exc:
            issues.append(
                Issue(
                    "INVALID_AUTOMATION_CURVE",
                    f"automation node {index}: {exc}",
                    track_name,
                    {"node_index": index},
                )
            )
    return tuple(nodes), issues


def _parse_drift_anchors(
    raw: Any,
    track_name: str,
    output_frames: int,
) -> tuple[tuple[DriftAnchor, ...], list[Issue]]:
    issues: list[Issue] = []
    if raw in (None, []):
        return (), issues
    if not isinstance(raw, list):
        return (), [
            Issue("INVALID_DRIFT_ANCHOR", "drift_anchors must be null or an array", track_name)
        ]
    anchors: list[DriftAnchor] = []
    for index, item in enumerate(raw):
        try:
            if not isinstance(item, dict):
                raise ValueError("anchor is not an object")
            output_raw = item.get("output_sample")
            source_raw = item.get("source_sample")
            if (
                isinstance(output_raw, bool)
                or not isinstance(output_raw, int)
                or isinstance(source_raw, bool)
                or not isinstance(source_raw, int)
            ):
                raise ValueError("anchor sample positions must be integers")
            output_sample = output_raw
            source_sample = source_raw
            if output_sample < 0 or source_sample < 0:
                raise ValueError("anchor sample positions must be non-negative")
            if output_sample >= output_frames:
                raise ValueError("anchor output_sample lies outside the master timeline")
            if anchors:
                if output_sample <= anchors[-1].output_sample:
                    raise ValueError("output_sample values must be strictly increasing")
                if source_sample <= anchors[-1].source_sample:
                    raise ValueError("source_sample values must be strictly increasing")
            anchors.append(DriftAnchor(output_sample, source_sample))
        except (TypeError, ValueError) as exc:
            issues.append(
                Issue(
                    "INVALID_DRIFT_ANCHOR",
                    f"drift anchor {index}: {exc}",
                    track_name,
                    {"anchor_index": index},
                )
            )
    return tuple(anchors), issues


def load_manifest(path: str | Path) -> ProjectSpec:
    manifest_path = Path(path)
    try:
        with manifest_path.open("r", encoding="utf-8") as handle:
            data = json.load(handle)
    except OSError as exc:
        raise HardError(
            [Issue("MANIFEST_NOT_FOUND", f"cannot read manifest: {exc}", details={"path": str(path)})]
        ) from exc
    except json.JSONDecodeError as exc:
        raise HardError(
            [Issue("INVALID_MANIFEST", f"manifest is not valid JSON: {exc}", details={"path": str(path)})]
        ) from exc

    issues: list[Issue] = []
    if not isinstance(data, dict):
        raise HardError([Issue("INVALID_MANIFEST", "manifest root must be an object")])

    schema_version = data.get("schema_version")
    if (
        isinstance(schema_version, bool)
        or not isinstance(schema_version, int)
        or schema_version != 1
    ):
        issues.append(
            Issue(
                "INVALID_MANIFEST",
                "schema_version must be 1",
                details={"actual": schema_version},
            )
        )

    project = data.get("project")
    output = data.get("output")
    sync = data.get("sync")
    automation_raw = data.get("automation_config", {})
    tracks_raw = data.get("tracks")
    if not isinstance(project, dict):
        issues.append(Issue("INVALID_MANIFEST", "project must be an object"))
        project = {}
    if not isinstance(output, dict):
        issues.append(Issue("INVALID_MANIFEST", "output must be an object"))
        output = {}
    if not isinstance(sync, dict):
        issues.append(Issue("INVALID_MANIFEST", "sync must be an object"))
        sync = {}
    if not isinstance(automation_raw, dict):
        issues.append(Issue("INVALID_MANIFEST", "automation_config must be an object"))
        automation_raw = {}
    if not isinstance(tracks_raw, list):
        issues.append(Issue("INVALID_MANIFEST", "tracks must be an array"))
        tracks_raw = []

    try:
        output_seconds = _finite_number(project.get("output_seconds"), "project.output_seconds")
        if output_seconds <= 0:
            raise ValueError("project.output_seconds must be positive")
    except ValueError as exc:
        issues.append(Issue("INVALID_MANIFEST", str(exc)))
        output_seconds = 0.0

    try:
        gain_min_db = _finite_number(
            automation_raw.get("gain_min_db", -60.0), "automation_config.gain_min_db"
        )
        gain_max_db = _finite_number(
            automation_raw.get("gain_max_db", 12.0), "automation_config.gain_max_db"
        )
        jump_threshold_db = _finite_number(
            automation_raw.get("jump_threshold_db", 3.0),
            "automation_config.jump_threshold_db",
        )
        smoothing_seconds = _finite_number(
            automation_raw.get("smoothing_seconds", 0.05),
            "automation_config.smoothing_seconds",
        )
        if gain_min_db > gain_max_db:
            raise ValueError("automation gain_min_db exceeds gain_max_db")
        if jump_threshold_db < 0 or smoothing_seconds < 0:
            raise ValueError("automation threshold and smoothing duration must be non-negative")
        automation_config = AutomationConfig(
            gain_min_db, gain_max_db, jump_threshold_db, smoothing_seconds
        )
    except ValueError as exc:
        issues.append(Issue("INVALID_MANIFEST", str(exc)))
        automation_config = AutomationConfig(-60.0, 12.0, 3.0, 0.05)

    try:
        output_sr = _positive_integer(output.get("sample_rate"), "output.sample_rate")
        output_channels = _positive_integer(output.get("channels"), "output.channels")
        output_bits = _positive_integer(output.get("bits_per_sample"), "output.bits_per_sample")
        true_peak = _finite_number(
            output.get("true_peak_limit_dbtp", -1.0), "output.true_peak_limit_dbtp"
        )
        tolerance = _finite_number(
            output.get("loudness_tolerance_lu", 0.5), "output.loudness_tolerance_lu"
        )
        target_raw = output.get("target_loudness_lufs")
        target = None if target_raw is None else _finite_number(
            target_raw, "output.target_loudness_lufs"
        )
        if output_channels > 2:
            raise ValueError("output.channels supports mono or stereo")
        if output_bits not in (16, 24, 32):
            raise ValueError("output.bits_per_sample must be 16, 24, or 32")
        if true_peak > 0:
            raise ValueError("true_peak_limit_dbtp must not be positive")
        if tolerance <= 0:
            raise ValueError("loudness_tolerance_lu must be positive")
    except ValueError as exc:
        issues.append(Issue("INVALID_MANIFEST", str(exc)))
        output_sr = 48000
        output_channels = 2
        output_bits = 24
        true_peak = -1.0
        tolerance = 0.5
        target = None

    try:
        reference_pulse = _nonnegative_integer(
            sync.get("reference_pulse_output_sample"),
            "sync.reference_pulse_output_sample",
        )
    except ValueError as exc:
        issues.append(Issue("INVALID_MANIFEST", str(exc)))
        reference_pulse = 0

    output_frames = int(round(output_seconds * output_sr)) if output_seconds > 0 else 0
    if output_frames <= 0 and output_seconds > 0:
        issues.append(Issue("INVALID_MANIFEST", "calculated master frame count is zero"))
    if output_frames >= 250_000_000:
        issues.append(Issue("INVALID_MANIFEST", "requested master timeline is unreasonably long"))
    if reference_pulse >= output_frames and output_frames > 0:
        issues.append(Issue("INVALID_MANIFEST", "sync reference pulse lies outside the master timeline"))

    parsed_tracks: list[TrackSpec] = []
    seen_names: set[str] = set()
    for track_index, raw_track in enumerate(tracks_raw):
        if not isinstance(raw_track, dict):
            issues.append(
                Issue(
                    "INVALID_MANIFEST",
                    "track entry must be an object",
                    details={"track_index": track_index},
                )
            )
            continue
        track_name_value = raw_track.get("name")
        track_name = str(track_name_value) if track_name_value is not None else None
        try:
            if not isinstance(track_name_value, str) or not track_name_value:
                raise ValueError("track.name must be a non-empty string")
            if track_name in seen_names:
                raise ValueError("track names must be unique")
            seen_names.add(track_name)

            file_name = raw_track.get("file")
            if not isinstance(file_name, str) or not file_name:
                raise ValueError("track.file must be a non-empty string")
            sample_rate = _positive_integer(raw_track.get("sample_rate"), "sample_rate")
            bits = _positive_integer(raw_track.get("bits_per_sample"), "bits_per_sample")
            channels = _positive_integer(raw_track.get("channels"), "channels")
            if bits not in (16, 24, 32):
                raise ValueError("bits_per_sample must be 16, 24, or 32")
            if channels > 2:
                raise ValueError("track channels must be mono or stereo")
        except ValueError as exc:
            issues.append(
                Issue(
                    "INVALID_MANIFEST",
                    f"track {track_index}: {exc}",
                    track_name,
                    {"track_index": track_index},
                )
            )
            continue

        automation, automation_issues = _parse_automation(
            raw_track.get("automation"), track_name, automation_config
        )
        issues.extend(automation_issues)
        anchors, anchor_issues = _parse_drift_anchors(
            raw_track.get("drift_anchors"), track_name, output_frames
        )
        issues.extend(anchor_issues)
        relative_path = Path(file_name)
        resolved_path = relative_path if relative_path.is_absolute() else manifest_path.parent / relative_path
        parsed_tracks.append(
            TrackSpec(
                name=track_name,
                file=file_name,
                path=resolved_path,
                sample_rate=sample_rate,
                bits_per_sample=bits,
                channels=channels,
                automation=automation,
                drift_anchors=anchors,
            )
        )

    if issues:
        raise HardError(issues, "manifest rejected")

    try:
        project_name = project.get("name")
        if not isinstance(project_name, str) or not project_name:
            raise ValueError("project.name must be a non-empty string")
        seed_value = project.get("seed", 0)
        if isinstance(seed_value, bool) or not isinstance(seed_value, int):
            raise ValueError("project.seed must be an integer")
    except ValueError as exc:
        raise HardError([Issue("INVALID_MANIFEST", str(exc))]) from exc

    output_config = OutputConfig(
        sample_rate=output_sr,
        channels=output_channels,
        bits_per_sample=output_bits,
        frames=output_frames,
        true_peak_limit_dbtp=true_peak,
        target_loudness_lufs=target,
        loudness_tolerance_lu=tolerance,
    )
    return ProjectSpec(
        name=project_name,
        seed=int(seed_value),
        output_seconds=output_seconds,
        schema_version=int(schema_version),
        reference_pulse_output_sample=reference_pulse,
        automation_config=automation_config,
        output=output_config,
        tracks=tuple(parsed_tracks),
        manifest_path=manifest_path,
    )
