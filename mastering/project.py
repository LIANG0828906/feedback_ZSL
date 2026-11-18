from __future__ import annotations

import json
import math
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .errors import MasteringIssue, issue


_LANGUAGE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]*$")


@dataclass(frozen=True)
class TrackSpec:
    name: str
    role: str
    path: Path
    path_text: str
    language: str | None = None
    sample_rate: int | None = None
    channels: int | None = None
    bit_depth: int | None = None
    target_lufs: float | None = None
    true_peak_dbtp: float | None = None


@dataclass(frozen=True)
class LanguageRule:
    language: str
    target_lufs: float
    true_peak_dbtp: float


@dataclass(frozen=True)
class DuckingRule:
    depth_db: float = 12.0
    threshold_dbfs: float = -32.0
    attack_ms: float = 15.0
    release_ms: float = 250.0
    frame_ms: float = 10.0


@dataclass(frozen=True)
class LimiterRule:
    true_peak_dbtp: float = -1.0
    lookahead_ms: float = 5.0
    release_ms: float = 80.0
    oversample: int = 4
    safety_margin_db: float = 0.05


@dataclass(frozen=True)
class SilenceRule:
    threshold_dbfs: float = -60.0
    min_active_ratio: float = 0.005
    frame_ms: float = 20.0


@dataclass(frozen=True)
class DurationRule:
    trim_strategy: str = "trailing_silence"


@dataclass(frozen=True)
class Project:
    project_path: Path
    project_dir: Path
    sample_rate: int
    channels: int
    bit_depth: int
    block_frames: int
    output_dir: Path
    tracks: tuple[TrackSpec, ...]
    required_languages: tuple[str, ...]
    languages: dict[str, LanguageRule]
    ducking: DuckingRule
    limiter: LimiterRule
    silence: SilenceRule
    duration: DurationRule
    music_target_lufs: float | None
    sfx_target_lufs: float | None
    music_gain_db: float
    sfx_gain_db: float
    canonical: dict[str, Any] = field(repr=False)

    def tracks_by_role(self, role: str) -> list[TrackSpec]:
        return [track for track in self.tracks if track.role == role]

    def track(self, name: str) -> TrackSpec:
        for track in self.tracks:
            if track.name == name:
                return track
        raise KeyError(name)


def _number(value: Any, field_name: str, errors: list[MasteringIssue], track: str) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
        errors.append(issue("PROJECT_INVALID", track, f"{field_name} must be a finite number"))
        return None
    return float(value)


def _positive_int(value: Any, field_name: str, errors: list[MasteringIssue], track: str) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        errors.append(issue("PROJECT_INVALID", track, f"{field_name} must be a positive integer"))
        return None
    return value


def _load_json(path: Path, track: str, errors: list[MasteringIssue]) -> dict[str, Any] | None:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except FileNotFoundError:
        errors.append(issue("PROJECT_MISSING", track, f"JSON file does not exist: {path.name}"))
        return None
    except (OSError, UnicodeError) as exc:
        errors.append(issue("PROJECT_UNREADABLE", track, f"cannot read JSON: {exc}"))
        return None
    except json.JSONDecodeError as exc:
        errors.append(
            issue("PROJECT_INVALID_JSON", track, f"invalid JSON at line {exc.lineno}, column {exc.colno}")
        )
        return None
    if not isinstance(payload, dict):
        errors.append(issue("PROJECT_INVALID", track, "top-level JSON value must be an object"))
        return None
    return payload


def _parse_languages(
    payload: Any,
    errors: list[MasteringIssue],
    default_true_peak_dbtp: float,
) -> dict[str, LanguageRule]:
    if not isinstance(payload, dict):
        errors.append(issue("PROJECT_INVALID", "rules.languages", "languages must be an object"))
        return {}
    parsed: dict[str, LanguageRule] = {}
    for language, raw in payload.items():
        track = f"rules.languages.{language}"
        if (
            not isinstance(language, str)
            or not language.strip()
            or _LANGUAGE_RE.fullmatch(language) is None
        ):
            errors.append(issue("LANGUAGE_MISSING", track, "language id must be a non-empty string"))
            continue
        if not isinstance(raw, dict):
            errors.append(issue("PROJECT_INVALID", track, "language rule must be an object"))
            continue
        target = _number(raw.get("target_lufs"), "target_lufs", errors, track)
        true_peak = _number(
            raw.get(
                "true_peak_dbtp",
                raw.get("max_true_peak_dbtp", default_true_peak_dbtp),
            ),
            "true_peak_dbtp",
            errors,
            track,
        )
        if target is not None and not -70.0 <= target <= 0.0:
            errors.append(issue("PROJECT_INVALID", track, "target_lufs must be between -70 and 0"))
        if true_peak is not None and not -20.0 <= true_peak <= 0.0:
            errors.append(issue("PROJECT_INVALID", track, "true_peak_dbtp must be between -20 and 0"))
        if target is not None and true_peak is not None:
            parsed[language] = LanguageRule(language, target, true_peak)
    return parsed


def _parse_tracks(
    payload: Any,
    project_dir: Path,
    global_values: dict[str, int],
    errors: list[MasteringIssue],
) -> list[TrackSpec]:
    entries: Iterable[tuple[str | None, Any]]
    if isinstance(payload, dict):
        entries = payload.items()
    elif isinstance(payload, list):
        entries = ((None, item) for item in payload)
    else:
        errors.append(issue("PROJECT_INVALID", "tracks", "tracks must be an object or array"))
        return []

    tracks: list[TrackSpec] = []
    names: set[str] = set()
    for key, raw in entries:
        track_name = key if key is not None else (raw.get("name") if isinstance(raw, dict) else None)
        label = str(track_name or "track")
        if not isinstance(raw, dict):
            errors.append(issue("PROJECT_INVALID", label, "track must be an object"))
            continue
        name = raw.get("name", key)
        if not isinstance(name, str) or not name.strip():
            errors.append(issue("TRACK_NAME_MISSING", label, "track name is required"))
            continue
        if name in names:
            errors.append(issue("DUPLICATE_TRACK_NAME", name, f"duplicate track name {name}"))
            continue
        names.add(name)
        role = raw.get("role", raw.get("kind"))
        if not isinstance(role, str):
            errors.append(issue("TRACK_ROLE_MISSING", name, "track role is required"))
            continue
        role = role.lower()
        if role in ("voiceover", "dialogue", "dialog"):
            role = "voice"
        if role not in ("voice", "music", "sfx"):
            errors.append(issue("TRACK_ROLE_INVALID", name, f"unsupported role {role!r}"))
            continue
        path_value = raw.get("path")
        if not isinstance(path_value, str) or not path_value.strip():
            errors.append(issue("TRACK_PATH_MISSING", name, "track path is required"))
            continue
        path = Path(path_value)
        if not path.is_absolute():
            path = project_dir / path
        language = raw.get("language")
        if language is not None and (
            not isinstance(language, str)
            or not language.strip()
            or _LANGUAGE_RE.fullmatch(language) is None
        ):
            errors.append(issue("LANGUAGE_MISSING", name, "language must be a non-empty string"))
            language = None
        if role == "voice" and language is None:
            errors.append(issue("LANGUAGE_MISSING", name, "voice track requires a language"))

        sample_rate = _positive_int(
            raw.get("sample_rate", global_values.get("sample_rate")), "sample_rate", errors, name
        )
        channels = _positive_int(
            raw.get("channels", global_values.get("channels")), "channels", errors, name
        )
        bit_depth = _positive_int(
            raw.get("bit_depth", global_values.get("bit_depth")), "bit_depth", errors, name
        )
        target = raw.get("target_lufs")
        if target is not None:
            target = _number(target, "target_lufs", errors, name)
        peak = raw.get("true_peak_dbtp", raw.get("max_true_peak_dbtp"))
        if peak is not None:
            peak = _number(peak, "true_peak_dbtp", errors, name)
        tracks.append(
            TrackSpec(
                name=name,
                role=role,
                path=path,
                path_text=path_value,
                language=language,
                sample_rate=sample_rate,
                channels=channels,
                bit_depth=bit_depth,
                target_lufs=target,
                true_peak_dbtp=peak,
            )
        )
    return tracks


def _parse_ducking(payload: Any, errors: list[MasteringIssue]) -> DuckingRule:
    payload = payload if isinstance(payload, dict) else {}
    defaults = DuckingRule()
    depth = _number(payload.get("depth_db", defaults.depth_db), "ducking.depth_db", errors, "rules")
    threshold = _number(
        payload.get("threshold_dbfs", defaults.threshold_dbfs),
        "ducking.threshold_dbfs",
        errors,
        "rules",
    )
    attack = _number(
        payload.get("attack_ms", defaults.attack_ms), "ducking.attack_ms", errors, "rules"
    )
    release = _number(
        payload.get("release_ms", defaults.release_ms), "ducking.release_ms", errors, "rules"
    )
    frame = _number(payload.get("frame_ms", defaults.frame_ms), "ducking.frame_ms", errors, "rules")
    if depth is not None and not 0.0 <= depth <= 60.0:
        errors.append(issue("PROJECT_INVALID", "rules", "ducking.depth_db must be between 0 and 60"))
    if threshold is not None and not -120.0 <= threshold <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "ducking.threshold_dbfs is out of range"))
    if attack is not None and attack <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "ducking.attack_ms must be positive"))
    if release is not None and release <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "ducking.release_ms must be positive"))
    if frame is not None and frame <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "ducking.frame_ms must be positive"))
    return DuckingRule(
        depth_db=depth if depth is not None else defaults.depth_db,
        threshold_dbfs=threshold if threshold is not None else defaults.threshold_dbfs,
        attack_ms=attack if attack is not None else defaults.attack_ms,
        release_ms=release if release is not None else defaults.release_ms,
        frame_ms=frame if frame is not None else defaults.frame_ms,
    )


def _parse_limiter(payload: Any, errors: list[MasteringIssue]) -> LimiterRule:
    payload = payload if isinstance(payload, dict) else {}
    defaults = LimiterRule()
    peak = _number(
        payload.get("true_peak_dbtp", defaults.true_peak_dbtp),
        "limiter.true_peak_dbtp",
        errors,
        "rules",
    )
    lookahead = _number(
        payload.get("lookahead_ms", defaults.lookahead_ms),
        "limiter.lookahead_ms",
        errors,
        "rules",
    )
    release = _number(
        payload.get("release_ms", defaults.release_ms), "limiter.release_ms", errors, "rules"
    )
    oversample = _positive_int(
        payload.get("oversample", defaults.oversample), "limiter.oversample", errors, "rules"
    )
    margin = _number(
        payload.get("safety_margin_db", defaults.safety_margin_db),
        "limiter.safety_margin_db",
        errors,
        "rules",
    )
    if peak is not None and not -20.0 <= peak <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "limiter.true_peak_dbtp is out of range"))
    if lookahead is not None and lookahead < 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "limiter.lookahead_ms must be non-negative"))
    if release is not None and release <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "limiter.release_ms must be positive"))
    if oversample is not None and oversample not in (1, 2, 4, 8):
        errors.append(issue("PROJECT_INVALID", "rules", "limiter.oversample must be 1, 2, 4, or 8"))
    if margin is not None and not 0.0 <= margin <= 3.0:
        errors.append(issue("PROJECT_INVALID", "rules", "limiter.safety_margin_db is out of range"))
    return LimiterRule(
        true_peak_dbtp=peak if peak is not None else defaults.true_peak_dbtp,
        lookahead_ms=lookahead if lookahead is not None else defaults.lookahead_ms,
        release_ms=release if release is not None else defaults.release_ms,
        oversample=oversample if oversample is not None else defaults.oversample,
        safety_margin_db=margin if margin is not None else defaults.safety_margin_db,
    )


def _parse_silence(payload: Any, errors: list[MasteringIssue]) -> SilenceRule:
    payload = payload if isinstance(payload, dict) else {}
    defaults = SilenceRule()
    threshold = _number(
        payload.get("threshold_dbfs", defaults.threshold_dbfs),
        "silence.threshold_dbfs",
        errors,
        "rules",
    )
    ratio = _number(
        payload.get("min_active_ratio", defaults.min_active_ratio),
        "silence.min_active_ratio",
        errors,
        "rules",
    )
    frame = _number(payload.get("frame_ms", defaults.frame_ms), "silence.frame_ms", errors, "rules")
    if threshold is not None and not -120.0 <= threshold <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "silence.threshold_dbfs is out of range"))
    if ratio is not None and not 0.0 <= ratio <= 1.0:
        errors.append(issue("PROJECT_INVALID", "rules", "silence.min_active_ratio is out of range"))
    if frame is not None and frame <= 0.0:
        errors.append(issue("PROJECT_INVALID", "rules", "silence.frame_ms must be positive"))
    return SilenceRule(
        threshold_dbfs=threshold if threshold is not None else defaults.threshold_dbfs,
        min_active_ratio=ratio if ratio is not None else defaults.min_active_ratio,
        frame_ms=frame if frame is not None else defaults.frame_ms,
    )


def load_project(
    project_path: Path,
    *,
    rules_override: Path | None = None,
    output_override: Path | None = None,
    block_frames_override: int | None = None,
) -> tuple[Project | None, list[MasteringIssue], dict[str, Any]]:
    errors: list[MasteringIssue] = []
    project_path = project_path.resolve()
    project_payload = _load_json(project_path, "project", errors)
    if project_payload is None:
        return None, errors, {}
    project_dir = project_path.parent

    sample_rate = _positive_int(project_payload.get("sample_rate"), "sample_rate", errors, "project")
    channels = _positive_int(project_payload.get("channels"), "channels", errors, "project")
    bit_depth = _positive_int(project_payload.get("bit_depth"), "bit_depth", errors, "project")
    if bit_depth is not None and bit_depth not in (16, 24, 32):
        errors.append(issue("PROJECT_INVALID", "project", "output bit_depth must be 16, 24, or 32"))
    if channels is not None and not 1 <= channels <= 8:
        errors.append(issue("PROJECT_INVALID", "project", "channels must be between 1 and 8"))

    global_values = {
        key: value
        for key, value in {"sample_rate": sample_rate, "channels": channels, "bit_depth": bit_depth}.items()
        if value is not None
    }
    tracks = _parse_tracks(project_payload.get("tracks"), project_dir, global_values, errors)

    rules_value = project_payload.get("rules", project_payload.get("rules_path"))
    if rules_override is not None:
        rules_path = rules_override.resolve()
    elif isinstance(rules_value, str):
        rules_path = Path(rules_value)
        if not rules_path.is_absolute():
            rules_path = project_dir / rules_path
        rules_path = rules_path.resolve()
    elif isinstance(rules_value, dict):
        rules_path = None
    else:
        errors.append(issue("PROJECT_INVALID", "rules", "project must reference a rules JSON file"))
        rules_path = None

    rules_payload: dict[str, Any] = rules_value if isinstance(rules_value, dict) else {}
    if rules_path is not None:
        loaded_rules = _load_json(rules_path, "rules", errors)
        if loaded_rules is not None:
            rules_payload = loaded_rules

    limiter = _parse_limiter(rules_payload.get("limiter", {}), errors)
    languages = _parse_languages(
        rules_payload.get("languages", {}),
        errors,
        limiter.true_peak_dbtp,
    )
    ducking = _parse_ducking(rules_payload.get("ducking", {}), errors)
    silence = _parse_silence(rules_payload.get("silence", {}), errors)
    duration_raw = rules_payload.get("duration", {})
    duration_raw = duration_raw if isinstance(duration_raw, dict) else {}
    strategy = duration_raw.get("trim_strategy", "trailing_silence")
    if strategy not in ("trailing_silence", "tail_silence_only"):
        errors.append(issue("PROJECT_INVALID", "rules", f"unsupported trim strategy {strategy!r}"))
    duration = DurationRule("trailing_silence")

    buses = rules_payload.get("buses", {})
    buses = buses if isinstance(buses, dict) else {}
    music_target = buses.get("music_target_lufs")
    sfx_target = buses.get("sfx_target_lufs")
    if music_target is not None:
        music_target = _number(music_target, "buses.music_target_lufs", errors, "rules")
    if sfx_target is not None:
        sfx_target = _number(sfx_target, "buses.sfx_target_lufs", errors, "rules")
    music_gain = _number(buses.get("music_gain_db", 0.0), "buses.music_gain_db", errors, "rules")
    sfx_gain = _number(buses.get("sfx_gain_db", 0.0), "buses.sfx_gain_db", errors, "rules")

    voices = [track for track in tracks if track.role == "voice"]
    music = [track for track in tracks if track.role == "music"]
    sfx = [track for track in tracks if track.role == "sfx"]
    if len(music) != 1:
        errors.append(issue("TRACK_MISSING", "music", "project requires exactly one music track"))
    if len(sfx) != 1:
        errors.append(issue("TRACK_MISSING", "sfx", "project requires exactly one sfx track"))

    seen_languages: dict[str, str] = {}
    for track in voices:
        if track.language is None:
            continue
        if track.language in seen_languages:
            errors.append(
                issue(
                    "DUPLICATE_LANGUAGE",
                    track.name,
                    f"language {track.language!r} is also used by {seen_languages[track.language]}",
                )
            )
        else:
            seen_languages[track.language] = track.name

    required_raw = project_payload.get("required_languages")
    if required_raw is None:
        required_languages = list(languages.keys()) if languages else sorted(seen_languages)
    elif isinstance(required_raw, list) and all(
        isinstance(item, str) and item for item in required_raw
    ):
        required_languages = list(dict.fromkeys(required_raw))
    else:
        errors.append(
            issue("PROJECT_INVALID", "required_languages", "required_languages must be an array of strings")
        )
        required_languages = sorted(seen_languages)
    for language in required_languages:
        if language not in seen_languages:
            errors.append(
                issue("MISSING_LANGUAGE", language, f"required language {language!r} has no voice track")
            )

    for track in voices:
        if track.language is None:
            continue
        merged_target = track.target_lufs
        merged_peak = track.true_peak_dbtp
        rule = languages.get(track.language)
        if merged_target is None:
            merged_target = rule.target_lufs if rule is not None else None
        if merged_peak is None:
            merged_peak = rule.true_peak_dbtp if rule is not None else limiter.true_peak_dbtp
        if merged_target is None:
            errors.append(
                issue("LANGUAGE_RULE_MISSING", track.name, f"no target loudness for {track.language}")
            )
        else:
            languages[track.language] = LanguageRule(
                track.language, merged_target, merged_peak if merged_peak is not None else limiter.true_peak_dbtp
            )

    block_frames = (
        block_frames_override
        if block_frames_override is not None
        else project_payload.get("block_frames", 4800)
    )
    block_frames = _positive_int(block_frames, "block_frames", errors, "project")

    output_value = project_payload.get("output_dir", "output")
    if output_override is not None:
        output_dir = output_override.resolve()
    elif isinstance(output_value, str) and output_value:
        output_dir = Path(output_value)
        if not output_dir.is_absolute():
            output_dir = project_dir / output_dir
        output_dir = output_dir.resolve()
    else:
        errors.append(issue("PROJECT_INVALID", "project", "output_dir must be a non-empty string"))
        output_dir = (project_dir / "output").resolve()

    if errors:
        return None, errors, {}

    canonical = {
        "project": project_payload,
        "rules": rules_payload,
        "sample_rate": sample_rate,
        "channels": channels,
        "bit_depth": bit_depth,
        "block_frames": block_frames,
        "required_languages": required_languages,
    }
    project = Project(
        project_path=project_path,
        project_dir=project_dir,
        sample_rate=sample_rate,
        channels=channels,
        bit_depth=bit_depth,
        block_frames=block_frames,
        output_dir=output_dir,
        tracks=tuple(tracks),
        required_languages=tuple(required_languages),
        languages=languages,
        ducking=ducking,
        limiter=limiter,
        silence=silence,
        duration=duration,
        music_target_lufs=music_target,
        sfx_target_lufs=sfx_target,
        music_gain_db=music_gain if music_gain is not None else 0.0,
        sfx_gain_db=sfx_gain if sfx_gain is not None else 0.0,
        canonical=canonical,
    )
    return project, [], canonical

