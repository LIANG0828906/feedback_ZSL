from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .automation import AutomationPlan, SmoothingConfig, load_and_validate_automation
from .errors import Issue, ProjectRejected
from .resample import DriftMap, build_drift_map
from .wav import WavError, WavHeader, parse_output_format, read_wav_header


@dataclass(frozen=True)
class OutputConfig:
    sample_rate: int
    channels: int
    wave_format: str
    bits_per_sample: int
    true_peak_dbtp: float
    target_lufs: float | None
    duration_seconds: float | None
    smoothing: SmoothingConfig
    limiter_lookahead_ms: float
    limiter_release_ms: float
    limiter_max_iterations: int
    limiter_safety_margin_db: float
    true_peak_oversample: int

    @property
    def output_format_name(self) -> str:
        return "float32" if self.wave_format == "float" else f"pcm_s{self.bits_per_sample}"


@dataclass(frozen=True)
class TrackSpec:
    track_id: str
    name: str
    relative_file: str
    resolved_path: Path
    declared_sample_rate: int
    declared_bit_depth: int
    declared_channels: int
    header: WavHeader
    pan: float
    sync_spec: dict[str, Any]
    target_sync_seconds: float
    drift_map: DriftMap


@dataclass(frozen=True)
class LoadedProject:
    project_name: str
    manifest_path: Path
    config_path: Path
    automation_path: Path
    config: OutputConfig
    tracks: tuple[TrackSpec, ...]
    automation: dict[str, AutomationPlan]
    output_frames: int
    duration_seconds: float
    tick_rate: float
    sync_target_seconds: float


def _load_json(path: Path, kind: str) -> Any:
    try:
        with path.open("r", encoding="utf-8") as handle:
            return json.load(handle)
    except FileNotFoundError:
        raise ProjectRejected([Issue("FILE_NOT_FOUND", f"{kind} file not found: {path}")])
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ProjectRejected(
            [Issue("INVALID_JSON", f"cannot read {kind} JSON: {exc}")]
        )


def _number(
    mapping: dict[str, Any],
    key: str,
    default: float | int | None = None,
) -> float | int | None:
    value = mapping.get(key, default)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    return result if math.isfinite(result) else None


def load_output_config(path: Path) -> OutputConfig:
    raw = _load_json(path, "output config")
    if not isinstance(raw, dict):
        raise ProjectRejected(
            [Issue("INVALID_OUTPUT_CONFIG", "output config root must be an object")]
        )
    issues: list[Issue] = []
    output = raw.get("output", raw)
    if not isinstance(output, dict):
        raise ProjectRejected(
            [Issue("INVALID_OUTPUT_CONFIG", "output section must be an object")]
        )

    sample_rate_raw = _number(output, "sample_rate")
    channels_raw = _number(output, "channels")
    sample_rate = int(sample_rate_raw) if sample_rate_raw is not None else 0
    channels = int(channels_raw) if channels_raw is not None else 0
    if sample_rate <= 0 or sample_rate_raw is None:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "sample_rate must be positive"))
    if channels <= 0 or channels_raw is None:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "channels must be positive"))

    format_value = output.get("format")
    if format_value is None and "bit_depth" in output:
        bit_depth_raw = _number(output, "bit_depth")
        if bit_depth_raw == 32 and output.get("sample_format") == "float":
            format_value = "float32"
        else:
            format_value = int(bit_depth_raw) if bit_depth_raw is not None else None
    try:
        wave_format, bits = parse_output_format(format_value)
    except ValueError as exc:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", str(exc)))
        wave_format, bits = "pcm", 24

    true_peak_raw = _number(output, "true_peak_dbtp", -1.0)
    if true_peak_raw is None:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "true_peak_dbtp must be finite"))
        true_peak_raw = -1.0
    if not -60.0 <= float(true_peak_raw) <= 0.0:
        issues.append(
            Issue(
                "INVALID_OUTPUT_CONFIG",
                "true_peak_dbtp must be in [-60, 0]",
            )
        )

    target_lufs_raw = None
    for key in ("target_lufs", "target_integrated_lufs"):
        if key in output:
            target_lufs_raw = _number(output, key)
            break
    if target_lufs_raw is not None and not -70.0 <= float(target_lufs_raw) <= 0.0:
        issues.append(
            Issue(
                "INVALID_OUTPUT_CONFIG",
                "target loudness must be in [-70, 0] LUFS",
            )
        )

    duration_raw = _number(output, "duration_seconds")
    if duration_raw is not None and float(duration_raw) <= 0.0:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "duration_seconds must be positive")
        )

    automation = raw.get("automation", {})
    if not isinstance(automation, dict):
        automation = {}
    gain_min = _number(automation, "gain_min_db", -96.0)
    gain_max = _number(automation, "gain_max_db", 24.0)
    max_interval = _number(automation, "smooth_max_interval_seconds", 0.03)
    min_delta = _number(automation, "smooth_min_gain_delta_db", 6.0)
    smooth_duration = _number(automation, "smooth_duration_seconds", 0.20)
    if (
        gain_min is None
        or gain_max is None
        or max_interval is None
        or min_delta is None
        or smooth_duration is None
    ):
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "automation settings must be finite"))
        gain_min = -96.0
        gain_max = 24.0
        max_interval = 0.03
        min_delta = 6.0
        smooth_duration = 0.20
    if float(gain_min) > float(gain_max):
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "automation gain_min_db exceeds gain_max_db")
        )
    if float(max_interval) < 0.0:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "smooth_max_interval_seconds must be non-negative")
        )
    if float(min_delta) < 0.0:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "smooth_min_gain_delta_db must be non-negative")
        )
    if float(smooth_duration) <= 0.0:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "smooth_duration_seconds must be positive")
        )

    limiter = raw.get("limiter", {})
    if not isinstance(limiter, dict):
        limiter = {}
    lookahead_ms = _number(limiter, "lookahead_ms", 2.0)
    release_ms = _number(limiter, "release_ms", 50.0)
    iterations = _number(limiter, "max_iterations", 8)
    safety_margin = _number(limiter, "safety_margin_db", 0.10)
    oversample = _number(raw.get("true_peak", {}), "oversample", 4) if isinstance(
        raw.get("true_peak", {}), dict
    ) else _number(raw, "true_peak_oversample", 4)
    if lookahead_ms is None or float(lookahead_ms) < 0.0:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "limiter.lookahead_ms must be non-negative"))
        lookahead_ms = 2.0
    if release_ms is None or float(release_ms) < 0.0:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "limiter.release_ms must be non-negative"))
        release_ms = 50.0
    if iterations is None or int(iterations) < 1:
        issues.append(Issue("INVALID_OUTPUT_CONFIG", "limiter.max_iterations must be >= 1"))
        iterations = 8
    if safety_margin is None or not 0.0 <= float(safety_margin) <= 6.0:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "limiter.safety_margin_db must be in [0, 6]")
        )
        safety_margin = 0.10
    if oversample is None or int(oversample) < 1:
        issues.append(
            Issue("INVALID_OUTPUT_CONFIG", "true_peak.oversample must be >= 1")
        )
        oversample = 4

    if issues:
        raise ProjectRejected(issues)
    return OutputConfig(
        sample_rate=sample_rate,
        channels=channels,
        wave_format=wave_format,
        bits_per_sample=bits,
        true_peak_dbtp=float(true_peak_raw),
        target_lufs=None if target_lufs_raw is None else float(target_lufs_raw),
        duration_seconds=None if duration_raw is None else float(duration_raw),
        smoothing=SmoothingConfig(
            gain_min_db=float(gain_min),
            gain_max_db=float(gain_max),
            max_interval_seconds=float(max_interval),
            min_gain_delta_db=float(min_delta),
            duration_seconds=float(smooth_duration),
        ),
        limiter_lookahead_ms=float(lookahead_ms),
        limiter_release_ms=float(release_ms),
        limiter_max_iterations=int(iterations),
        limiter_safety_margin_db=float(safety_margin),
        true_peak_oversample=int(oversample),
    )


def _track_name(raw: dict[str, Any], index: int) -> str:
    value = raw.get("id", raw.get("name", raw.get("track")))
    if value is None or str(value).strip() == "":
        return f"track_{index:02d}"
    return str(value)


def load_project(
    manifest_path: str | Path,
    automation_path: str | Path,
    config_path: str | Path,
) -> LoadedProject:
    """Validate the complete project, including every header and automation curve."""

    manifest_path = Path(manifest_path).resolve()
    automation_path = Path(automation_path).resolve()
    config_path = Path(config_path).resolve()
    config = load_output_config(config_path)
    manifest = _load_json(manifest_path, "manifest")
    if not isinstance(manifest, dict):
        raise ProjectRejected(
            [Issue("INVALID_MANIFEST", "manifest root must be an object")]
        )
    project_section = manifest.get("project", {})
    if not isinstance(project_section, dict):
        project_section = {}
    project_name = str(
        project_section.get("name", manifest.get("name", manifest_path.stem))
    )
    tick_rate_raw = _number(
        project_section,
        "tick_rate",
        _number(manifest, "tick_rate", config.sample_rate),
    )
    tick_rate = float(tick_rate_raw) if tick_rate_raw is not None else float(config.sample_rate)
    if tick_rate <= 0.0:
        raise ProjectRejected(
            [Issue("INVALID_MANIFEST", "tick_rate must be positive")]
        )
    sync_target_raw = _number(
        project_section,
        "sync_target_seconds",
        _number(manifest, "sync_target_seconds", 0.02),
    )
    sync_target_seconds = float(sync_target_raw) if sync_target_raw is not None else 0.02

    tracks_raw = manifest.get("tracks")
    if not isinstance(tracks_raw, list) or not tracks_raw:
        raise ProjectRejected(
            [Issue("INVALID_MANIFEST", "tracks must be a non-empty array")]
        )

    issues: list[Issue] = []
    track_specs: list[TrackSpec] = []
    seen_ids: set[str] = set()
    for index, raw_track in enumerate(tracks_raw):
        if not isinstance(raw_track, dict):
            issues.append(
                Issue("INVALID_MANIFEST", "track entry must be an object", f"track_{index:02d}")
            )
            continue
        track_id = _track_name(raw_track, index)
        display_name = str(raw_track.get("name", track_id))
        if track_id in seen_ids:
            issues.append(
                Issue("DUPLICATE_TRACK_ID", "track id/name is duplicated", track_id)
            )
            continue
        seen_ids.add(track_id)

        relative_file = raw_track.get("file")
        if not isinstance(relative_file, str) or not relative_file:
            issues.append(
                Issue("INVALID_MANIFEST", "track file must be a non-empty string", track_id)
            )
            continue
        track_path = Path(relative_file)
        if not track_path.is_absolute():
            track_path = manifest_path.parent / track_path
        track_path = track_path.resolve()

        declared_sr_raw = _number(raw_track, "sample_rate")
        declared_bits_raw = _number(raw_track, "bit_depth")
        declared_channels_raw = _number(raw_track, "channels")
        if declared_sr_raw is None:
            issues.append(
                Issue("INVALID_MANIFEST", "sample_rate declaration is missing", track_id)
            )
            continue
        if declared_bits_raw is None:
            issues.append(
                Issue("INVALID_MANIFEST", "bit_depth declaration is missing", track_id)
            )
            continue
        if declared_channels_raw is None:
            issues.append(
                Issue("INVALID_MANIFEST", "channels declaration is missing", track_id)
            )
            continue
        declared_sr = int(declared_sr_raw)
        declared_bits = int(declared_bits_raw)
        declared_channels = int(declared_channels_raw)

        if not track_path.is_file():
            issues.append(
                Issue(
                    "FILE_NOT_FOUND",
                    "track file does not exist",
                    track_id,
                    {"file": str(track_path)},
                )
            )
            continue
        try:
            header = read_wav_header(track_path)
        except (OSError, WavError, ValueError) as exc:
            issues.append(
                Issue(
                    "INVALID_WAV_HEADER",
                    f"cannot parse WAV header: {exc}",
                    track_id,
                    {"file": str(track_path)},
                )
            )
            continue
        if header.sample_rate != declared_sr:
            issues.append(
                Issue(
                    "SAMPLE_RATE_MISMATCH",
                    "declared sample rate does not match WAV header",
                    track_id,
                    {
                        "declared": declared_sr,
                        "actual": header.sample_rate,
                    },
                )
            )
        if header.bits_per_sample != declared_bits or not header.is_pcm:
            issues.append(
                Issue(
                    "BIT_DEPTH_MISMATCH",
                    "declared PCM bit depth does not match WAV header",
                    track_id,
                    {
                        "declared": declared_bits,
                        "actual": header.bits_per_sample,
                        "format_code": header.format_code,
                    },
                )
            )
        if header.channels != declared_channels:
            issues.append(
                Issue(
                    "CHANNEL_COUNT_MISMATCH",
                    "declared channel count does not match WAV header",
                    track_id,
                    {
                        "declared": declared_channels,
                        "actual": header.channels,
                    },
                )
            )

        drift_map, drift_issues = build_drift_map(
            track_id,
            raw_track.get("drift"),
            header.frame_count,
            header.sample_rate,
            config.sample_rate,
            tick_rate,
        )
        issues.extend(drift_issues)
        pan_raw = _number(raw_track, "pan", 0.0)
        pan = float(pan_raw) if pan_raw is not None else 0.0
        if not -1.0 <= pan <= 1.0:
            issues.append(Issue("INVALID_MANIFEST", "pan must be in [-1, 1]", track_id))
            pan = 0.0
        sync_spec = raw_track.get("sync")
        if not isinstance(sync_spec, dict):
            sync_spec = {}
        track_sync_target = _number(
            sync_spec,
            "target_time_seconds",
            _number(sync_spec, "target_seconds", sync_target_seconds),
        )
        track_specs.append(
            TrackSpec(
                track_id=track_id,
                name=display_name,
                relative_file=relative_file,
                resolved_path=track_path,
                declared_sample_rate=declared_sr,
                declared_bit_depth=declared_bits,
                declared_channels=declared_channels,
                header=header,
                pan=pan,
                sync_spec=sync_spec,
                target_sync_seconds=(
                    float(track_sync_target)
                    if track_sync_target is not None
                    else sync_target_seconds
                ),
                drift_map=drift_map,
            )
        )

    if issues:
        raise ProjectRejected(issues)

    # All file/existence/header/drift checks have passed before curves are examined.
    automation = load_and_validate_automation(
        automation_path,
        [track.track_id for track in track_specs],
        config.smoothing,
    )

    duration_raw = config.duration_seconds
    if duration_raw is None:
        duration_raw = _number(project_section, "duration_seconds")
    if duration_raw is None:
        duration_raw = _number(manifest, "duration_seconds")
    if duration_raw is not None:
        duration_seconds = float(duration_raw)
    else:
        duration_seconds = max(
            track.header.frame_count / track.header.sample_rate for track in track_specs
        )
        for track in track_specs:
            if track.drift_map.enabled:
                duration_seconds = max(
                    duration_seconds,
                    track.drift_map.anchors[-1].target_seconds,
                    track.drift_map.anchors[0].target_seconds,
                )
    if duration_seconds <= 0.0:
        raise ProjectRejected(
            [Issue("INVALID_MANIFEST", "project duration must be positive")]
        )
    output_frames = int(round(duration_seconds * config.sample_rate))
    if output_frames <= 0:
        raise ProjectRejected(
            [Issue("INVALID_MANIFEST", "output frame count must be positive")]
        )
    return LoadedProject(
        project_name=project_name,
        manifest_path=manifest_path,
        config_path=config_path,
        automation_path=automation_path,
        config=config,
        tracks=tuple(track_specs),
        automation=automation,
        output_frames=output_frames,
        duration_seconds=duration_seconds,
        tick_rate=tick_rate,
        sync_target_seconds=sync_target_seconds,
    )

