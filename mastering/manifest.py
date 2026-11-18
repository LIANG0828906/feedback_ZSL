from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import BusinessError, BusinessFailure, EXIT_VALIDATION
from .util import canonical_json, sample_round, sha256_bytes
from .wavfile import WavData, read_wav


SUPPORTED_KINDS = {"dialogue", "narration", "music"}


@dataclass(frozen=True)
class TrackSpec:
    name: str
    kind: str
    path: Path
    relative_path: str
    sample_rate: int
    channels: int
    bit_depth: int
    offset_seconds: float
    offset_samples: int
    synth: dict[str, Any]


@dataclass(frozen=True)
class Manifest:
    path: Path
    raw: dict[str, Any]
    digest: str
    seed: int
    sample_rate: int
    bit_depth: int
    dialogue_points_seconds: tuple[float, ...]
    dialogue_points_samples: tuple[int, ...]
    mono_fold: dict[str, Any]
    loudness: dict[str, Any]
    ducking: dict[str, Any]
    render: dict[str, Any]
    tracks: tuple[TrackSpec, ...]
    output_dir: Path
    snapshot_path: Path

    @property
    def dialogue_tracks(self) -> tuple[TrackSpec, ...]:
        return tuple(track for track in self.tracks if track.kind == "dialogue")

    @property
    def narration_tracks(self) -> tuple[TrackSpec, ...]:
        return tuple(track for track in self.tracks if track.kind == "narration")

    @property
    def music_tracks(self) -> tuple[TrackSpec, ...]:
        return tuple(track for track in self.tracks if track.kind == "music")


@dataclass(frozen=True)
class ValidatedInput:
    manifest: Manifest
    audio: dict[str, WavData]


def _error(code: str, message: str, track: str | None = None, **details: Any) -> BusinessError:
    return BusinessError(code, message, track, details)


def _number(value: Any, field: str, errors: list[BusinessError], *, track: str | None = None) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
        errors.append(_error("E_MANIFEST_FIELD", f"{field} must be a finite number", track, field=field))
        return None
    return float(value)


def _integer(value: Any, field: str, errors: list[BusinessError], *, track: str | None = None) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int):
        errors.append(_error("E_MANIFEST_FIELD", f"{field} must be an integer", track, field=field))
        return None
    return value


def _required_object(value: Any, field: str, errors: list[BusinessError]) -> dict[str, Any]:
    if not isinstance(value, dict):
        errors.append(_error("E_MANIFEST_FIELD", f"{field} must be an object", field=field))
        return {}
    return value


def _resolve(base: Path, relative: Any) -> tuple[Path, str]:
    text = str(relative)
    path = Path(text)
    if not path.is_absolute():
        path = base / path
    return path.resolve(), text


def load_manifest(path: Path, *, validate_audio: bool = True) -> ValidatedInput:
    errors: list[BusinessError] = []
    manifest_path = path.resolve()
    try:
        raw_bytes = manifest_path.read_bytes()
        raw = json.loads(raw_bytes.decode("utf-8"))
    except FileNotFoundError:
        errors.append(_error("E_MANIFEST_MISSING", "manifest file does not exist", path=str(path)))
        raise BusinessFailure(errors, EXIT_VALIDATION)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        errors.append(_error("E_MANIFEST_PARSE", f"cannot read manifest: {exc}", path=str(path)))
        raise BusinessFailure(errors, EXIT_VALIDATION)

    if not isinstance(raw, dict):
        errors.append(_error("E_MANIFEST_SCHEMA", "manifest root must be an object"))
        raise BusinessFailure(errors, EXIT_VALIDATION)

    if raw.get("schema_version") != 1:
        errors.append(_error("E_MANIFEST_SCHEMA", "schema_version must be 1"))

    digest = sha256_bytes(canonical_json(raw))
    base = manifest_path.parent
    seed = _integer(raw.get("seed"), "seed", errors)
    sample_rate = _integer(raw.get("sample_rate"), "sample_rate", errors)
    bit_depth = _integer(raw.get("bit_depth"), "bit_depth", errors)
    if sample_rate is not None and not 8000 <= sample_rate <= 192000:
        errors.append(_error("E_MANIFEST_FIELD", "sample_rate must be between 8000 and 192000", field="sample_rate"))
    if seed is not None and seed < 0:
        errors.append(_error("E_MANIFEST_FIELD", "seed must be non-negative", field="seed"))
    if bit_depth is not None and bit_depth not in (16, 24, 32):
        errors.append(_error("E_MANIFEST_FIELD", "bit_depth must be 16, 24 or 32", field="bit_depth"))

    points_raw = raw.get("dialogue_points_seconds")
    points: list[float] = []
    if not isinstance(points_raw, list) or not points_raw:
        errors.append(_error("E_CUE_ORDER", "dialogue_points_seconds must be a non-empty list"))
    else:
        for index, value in enumerate(points_raw):
            number = _number(value, f"dialogue_points_seconds[{index}]", errors)
            if number is not None:
                points.append(number)
        if any(value < 0 for value in points):
            errors.append(_error("E_CUE_ORDER", "dialogue points must be non-negative"))
        if any(right <= left for left, right in zip(points, points[1:])):
            errors.append(_error("E_CUE_ORDER", "dialogue points must be strictly ascending"))
    points_samples = tuple(sample_round(value, sample_rate) for value in points) if sample_rate else ()

    mono_fold = _required_object(raw.get("mono_fold"), "mono_fold", errors)
    fold_rule = mono_fold.get("rule")
    if fold_rule not in {"sum_0.5", "equal_power", "weighted_sum"}:
        errors.append(_error("E_FOLD_RULE", "mono_fold.rule must be sum_0.5, equal_power or weighted_sum", field="mono_fold.rule"))
    tolerance = _number(mono_fold.get("energy_tolerance"), "mono_fold.energy_tolerance", errors)
    if tolerance is not None and not 0 <= tolerance <= 1:
        errors.append(_error("E_FOLD_RULE", "mono_fold.energy_tolerance must be in [0, 1]", field="mono_fold.energy_tolerance"))
    band = mono_fold.get("band_hz", [300.0, 3400.0])
    if (
        not isinstance(band, list)
        or len(band) != 2
        or any(isinstance(item, bool) or not isinstance(item, (int, float)) for item in band)
        or not 0 <= float(band[0]) < float(band[1]) <= (sample_rate or 8000) / 2
    ):
        errors.append(_error("E_FOLD_RULE", "mono_fold.band_hz must be an ascending in-band pair", field="mono_fold.band_hz"))
    if fold_rule == "weighted_sum":
        left_gain = _number(mono_fold.get("left_gain"), "mono_fold.left_gain", errors)
        right_gain = _number(mono_fold.get("right_gain"), "mono_fold.right_gain", errors)
        if left_gain is None or right_gain is None:
            pass
        elif abs(left_gain) < 1e-12 and abs(right_gain) < 1e-12:
            errors.append(_error("E_FOLD_RULE", "weighted mono fold cannot be all zero", field="mono_fold"))

    loudness = _required_object(raw.get("loudness"), "loudness", errors)
    target_lufs = _number(loudness.get("target_lufs"), "loudness.target_lufs", errors)
    true_peak = _number(loudness.get("true_peak_dbtp", -1.0), "loudness.true_peak_dbtp", errors)
    tolerance_lu = _number(loudness.get("tolerance_lu", 0.5), "loudness.tolerance_lu", errors)
    if target_lufs is not None and not -70 <= target_lufs <= 0:
        errors.append(_error("E_LOUDNESS_TARGET", "target_lufs is outside [-70, 0]", field="loudness.target_lufs"))
    if true_peak is not None and not -30 <= true_peak <= 0:
        errors.append(_error("E_TRUE_PEAK_TARGET", "true_peak_dbtp is outside [-30, 0]", field="loudness.true_peak_dbtp"))
    if tolerance_lu is not None and not 0 < tolerance_lu <= 1:
        errors.append(_error("E_LOUDNESS_TARGET", "tolerance_lu must be in (0, 1]", field="loudness.tolerance_lu"))

    ducking = _required_object(raw.get("ducking"), "ducking", errors)
    depth_db = _number(ducking.get("depth_db"), "ducking.depth_db", errors)
    attack_ms = _number(ducking.get("attack_ms"), "ducking.attack_ms", errors)
    recovery_ms = _number(ducking.get("recovery_ms"), "ducking.recovery_ms", errors)
    sir_improvement = _number(
        ducking.get("sir_min_improvement_db", 3.0),
        "ducking.sir_min_improvement_db",
        errors,
    )
    if depth_db is not None and depth_db > 0:
        errors.append(_error("E_DUCKING_PARAM", "ducking.depth_db must be zero or negative", field="ducking.depth_db"))
    if attack_ms is not None and attack_ms < 0:
        errors.append(_error("E_DUCKING_PARAM", "ducking.attack_ms must be non-negative", field="ducking.attack_ms"))
    if recovery_ms is not None and recovery_ms < 0:
        errors.append(_error("E_DUCKING_PARAM", "ducking.recovery_ms must be non-negative", field="ducking.recovery_ms"))
    if sir_improvement is not None and sir_improvement < 0:
        errors.append(_error("E_DUCKING_PARAM", "sir_min_improvement_db must be non-negative", field="ducking.sir_min_improvement_db"))

    render = _required_object(raw.get("render", {}), "render", errors)
    block_frames = _integer(render.get("block_frames", 4096), "render.block_frames", errors)
    if block_frames is not None and block_frames <= 0:
        errors.append(_error("E_MANIFEST_FIELD", "render.block_frames must be positive", field="render.block_frames"))
    output_dir_value = render.get("output_dir", "deliverables")
    snapshot_value = render.get("snapshot_path", "state/render.snapshot.json")
    if not isinstance(output_dir_value, str) or not output_dir_value:
        errors.append(_error("E_MANIFEST_FIELD", "render.output_dir must be a non-empty string", field="render.output_dir"))
        output_dir_value = "deliverables"
    if not isinstance(snapshot_value, str) or not snapshot_value:
        errors.append(_error("E_MANIFEST_FIELD", "render.snapshot_path must be a non-empty string", field="render.snapshot_path"))
        snapshot_value = "state/render.snapshot.json"
    output_dir, _ = _resolve(base, output_dir_value)
    snapshot_path, _ = _resolve(base, snapshot_value)
    master_stereo = str(render.get("master_stereo", "master_stereo.wav"))
    master_mono = str(render.get("master_mono", "master_mono.wav"))
    report_name = str(render.get("report", "mastering_report.json"))
    recovery_report_name = str(render.get("recovery_report", "recovery_report.json"))
    if master_stereo == master_mono:
        errors.append(_error("E_MANIFEST_FIELD", "stereo and mono output filenames must differ", field="render"))
    if len({master_stereo, master_mono, report_name, recovery_report_name}) != 4:
        errors.append(_error("E_MANIFEST_FIELD", "render output filenames must be unique", field="render"))
    for field, filename in (
        ("render.master_stereo", master_stereo),
        ("render.master_mono", master_mono),
        ("render.report", report_name),
        ("render.recovery_report", recovery_report_name),
    ):
        if Path(filename).name != filename or filename in {"", ".", ".."}:
            errors.append(_error("E_MANIFEST_FIELD", f"{field} must be a plain filename", field=field))

    tracks_raw = raw.get("tracks")
    tracks: list[TrackSpec] = []
    seen_names: set[str] = set()
    if not isinstance(tracks_raw, list) or not tracks_raw:
        errors.append(_error("E_MANIFEST_SCHEMA", "tracks must be a non-empty list"))
    else:
        for index, item in enumerate(tracks_raw):
            track_errors_start = len(errors)
            if not isinstance(item, dict):
                errors.append(_error("E_MANIFEST_SCHEMA", f"tracks[{index}] must be an object", track=f"tracks[{index}]"))
                continue
            name = item.get("name")
            if not isinstance(name, str) or not name:
                name = f"tracks[{index}]"
                errors.append(_error("E_TRACK_NAME", "track name must be a non-empty string", name))
            elif name in seen_names:
                errors.append(_error("E_TRACK_DUPLICATE", "track name is duplicated", name))
            seen_names.add(name)
            kind = item.get("kind")
            if kind not in SUPPORTED_KINDS:
                errors.append(_error("E_TRACK_KIND", "track kind must be dialogue, narration or music", name, kind=kind))
            rel_path = item.get("path")
            if not isinstance(rel_path, str) or not rel_path:
                errors.append(_error("E_FILE_MISSING", "track path is missing", name))
                rel_path = ""
            path_value, relative_path = _resolve(base, rel_path)
            track_sr = _integer(item.get("sample_rate", sample_rate), "sample_rate", errors, track=name)
            track_channels = _integer(item.get("channels"), "channels", errors, track=name)
            track_depth = _integer(item.get("bit_depth", bit_depth), "bit_depth", errors, track=name)
            offset = _number(item.get("offset_seconds", 0.0), "offset_seconds", errors, track=name)
            if track_channels is not None and track_channels <= 0:
                errors.append(_error("E_CHANNEL_MISMATCH", "declared channels must be positive", name))
            if offset is not None and offset < 0:
                errors.append(_error("E_MANIFEST_FIELD", "offset_seconds must be non-negative", name))
            synth = _required_object(item.get("synth"), "synth", errors)
            if kind == "dialogue":
                cue_indices = synth.get("cue_indices")
                if not isinstance(cue_indices, list) or not cue_indices:
                    errors.append(_error("E_CUE_ORDER", "dialogue synth.cue_indices must be non-empty", name))
                else:
                    if any(isinstance(value, bool) or not isinstance(value, int) for value in cue_indices):
                        errors.append(_error("E_CUE_ORDER", "cue indices must be integers", name))
                    elif any(right <= left for left, right in zip(cue_indices, cue_indices[1:])):
                        errors.append(_error("E_CUE_ORDER", "cue indices must be strictly ascending", name))
                    elif points and (cue_indices[0] < 0 or cue_indices[-1] >= len(points)):
                        errors.append(_error("E_CUE_OUT_OF_RANGE", "cue index exceeds dialogue point list", name))
            if kind == "narration":
                segments = synth.get("segments")
                if not isinstance(segments, list) or not segments:
                    errors.append(_error("E_NARRATION_SEGMENTS", "narration synth.segments must be non-empty", name))
                else:
                    previous = -1
                    for segment_index, segment in enumerate(segments):
                        if not isinstance(segment, dict):
                            errors.append(_error("E_NARRATION_SEGMENTS", "narration segment must be an object", name))
                            continue
                        cue_index = _integer(segment.get("cue_index"), "segments.cue_index", errors, track=name)
                        duration = _number(segment.get("duration_seconds"), "segments.duration_seconds", errors, track=name)
                        if cue_index is not None:
                            if cue_index < 0 or (points and cue_index >= len(points)):
                                errors.append(_error("E_CUE_OUT_OF_RANGE", "narration cue index is out of range", name))
                            if cue_index <= previous:
                                errors.append(_error("E_CUE_ORDER", "narration cue indices must be strictly ascending", name))
                            previous = cue_index
                        if duration is not None and duration <= 0:
                            errors.append(_error("E_NARRATION_SEGMENTS", "narration duration must be positive", name))
            if kind == "music":
                duration = _number(synth.get("duration_seconds"), "synth.duration_seconds", errors, track=name)
                if duration is not None and duration <= 0:
                    errors.append(_error("E_MANIFEST_FIELD", "music duration must be positive", name))
            if kind in {"dialogue", "narration"}:
                duration = _number(synth.get("local_duration_seconds"), "synth.local_duration_seconds", errors, track=name)
                if duration is not None and duration <= 0:
                    errors.append(_error("E_MANIFEST_FIELD", "local duration must be positive", name))

            if track_sr is not None and sample_rate is not None and track_sr != sample_rate:
                errors.append(
                    _error(
                        "E_SAMPLE_RATE_MISMATCH",
                        "declared track sample rate differs from manifest",
                        name,
                        declared=track_sr,
                        manifest=sample_rate,
                    )
                )
            if track_depth is not None and bit_depth is not None and track_depth != bit_depth:
                errors.append(
                    _error(
                        "E_BIT_DEPTH_MISMATCH",
                        "declared track bit depth differs from manifest",
                        name,
                        declared=track_depth,
                        manifest=bit_depth,
                    )
                )
            if len(errors) == track_errors_start and all(
                value is not None for value in (track_sr, track_channels, track_depth, offset)
            ):
                offset_samples = sample_round(float(offset), int(track_sr))
                tracks.append(
                    TrackSpec(
                        name=str(name),
                        kind=str(kind),
                        path=path_value,
                        relative_path=relative_path,
                        sample_rate=int(track_sr),
                        channels=int(track_channels),
                        bit_depth=int(track_depth),
                        offset_seconds=float(offset),
                        offset_samples=offset_samples,
                        synth=dict(synth),
                    )
                )

    if not errors:
        dialogue_count = sum(track.kind == "dialogue" for track in tracks)
        narration_count = sum(track.kind == "narration" for track in tracks)
        music_count = sum(track.kind == "music" for track in tracks)
        if dialogue_count < 2:
            errors.append(_error("E_TRACK_SET", "at least two dialogue tracks are required"))
        if narration_count != 1:
            errors.append(_error("E_TRACK_SET", "exactly one narration track is required"))
        if music_count != 1:
            errors.append(_error("E_TRACK_SET", "exactly one music track is required"))

    if errors:
        raise BusinessFailure(errors, EXIT_VALIDATION)

    assert seed is not None and sample_rate is not None and bit_depth is not None
    assert block_frames is not None and target_lufs is not None and true_peak is not None
    assert tolerance_lu is not None and tolerance is not None
    assert depth_db is not None and attack_ms is not None and recovery_ms is not None
    assert sir_improvement is not None

    manifest = Manifest(
        path=manifest_path,
        raw=raw,
        digest=digest,
        seed=seed,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        dialogue_points_seconds=tuple(points),
        dialogue_points_samples=points_samples,
        mono_fold=dict(mono_fold),
        loudness={
            "target_lufs": target_lufs,
            "true_peak_dbtp": true_peak,
            "tolerance_lu": tolerance_lu,
        },
        ducking={
            "depth_db": depth_db,
            "attack_ms": attack_ms,
            "recovery_ms": recovery_ms,
            "sir_min_improvement_db": sir_improvement,
        },
        render={
            "block_frames": block_frames,
            "master_stereo": master_stereo,
            "master_mono": master_mono,
            "report": report_name,
            "recovery_report": recovery_report_name,
        },
        tracks=tuple(tracks),
        output_dir=output_dir,
        snapshot_path=snapshot_path,
    )

    audio: dict[str, WavData] = {}
    if validate_audio:
        for track in manifest.tracks:
            if not track.path.is_file():
                errors.append(_error("E_FILE_MISSING", "track WAV file does not exist", track.name, path=track.relative_path))
                continue
            try:
                loaded = read_wav(track.path)
            except (OSError, ValueError, EOFError) as exc:
                errors.append(_error("E_WAV_INVALID", f"cannot decode WAV: {exc}", track.name, path=track.relative_path))
                continue
            if loaded.info.sample_rate != manifest.sample_rate:
                errors.append(
                    _error(
                        "E_SAMPLE_RATE_MISMATCH",
                        "actual WAV sample rate differs from declaration",
                        track.name,
                        declared=manifest.sample_rate,
                        actual=loaded.info.sample_rate,
                    )
                )
            if loaded.info.bit_depth != track.bit_depth:
                errors.append(
                    _error(
                        "E_BIT_DEPTH_MISMATCH",
                        "actual WAV bit depth differs from declaration",
                        track.name,
                        declared=track.bit_depth,
                        actual=loaded.info.bit_depth,
                    )
                )
            if loaded.info.channels != track.channels:
                errors.append(
                    _error(
                        "E_CHANNEL_MISMATCH",
                        "actual WAV channel count differs from declaration",
                        track.name,
                        declared=track.channels,
                        actual=loaded.info.channels,
                    )
                )
            if loaded.info.frames <= 0:
                errors.append(_error("E_WAV_INVALID", "WAV contains no samples", track.name))
            actual_end_sample = track.offset_samples + loaded.info.frames
            if track.kind == "dialogue":
                for cue_index in track.synth.get("cue_indices", []):
                    if isinstance(cue_index, int) and 0 <= cue_index < len(manifest.dialogue_points_samples):
                        cue_sample = manifest.dialogue_points_samples[cue_index]
                        if cue_sample < track.offset_samples or cue_sample >= actual_end_sample:
                            errors.append(
                                _error(
                                    "E_CUE_OUT_OF_RANGE",
                                    "dialogue cue lies outside actual WAV duration",
                                    track.name,
                                    cue_index=cue_index,
                                    cue_sample=cue_sample,
                                    actual_track_start_sample=track.offset_samples,
                                    actual_track_end_sample=actual_end_sample,
                                )
                            )
            elif track.kind == "narration":
                for segment in track.synth.get("segments", []):
                    cue_index = segment.get("cue_index") if isinstance(segment, dict) else None
                    segment_duration = segment.get("duration_seconds") if isinstance(segment, dict) else None
                    if (
                        isinstance(cue_index, int)
                        and 0 <= cue_index < len(manifest.dialogue_points_samples)
                        and isinstance(segment_duration, (int, float))
                        and not isinstance(segment_duration, bool)
                    ):
                        segment_end = manifest.dialogue_points_samples[cue_index] + sample_round(
                            float(segment_duration), manifest.sample_rate
                        )
                        if (
                            manifest.dialogue_points_samples[cue_index] < track.offset_samples
                            or segment_end > actual_end_sample
                        ):
                            errors.append(
                                _error(
                                    "E_CUE_OUT_OF_RANGE",
                                    "narration segment lies outside actual WAV duration",
                                    track.name,
                                    cue_index=cue_index,
                                    segment_end_sample=segment_end,
                                    actual_track_start_sample=track.offset_samples,
                                    actual_track_end_sample=actual_end_sample,
                                )
                            )
            audio[track.name] = loaded

        if audio:
            overall_end = max(
                track.offset_samples + audio[track.name].info.frames
                for track in manifest.tracks
                if track.name in audio
            )
            overall_start = min(
                track.offset_samples
                for track in manifest.tracks
                if track.name in audio
            )
            for cue_index, cue_sample in enumerate(manifest.dialogue_points_samples):
                if cue_sample < overall_start or cue_sample >= overall_end:
                    errors.append(
                        _error(
                            "E_CUE_OUT_OF_RANGE",
                            "dialogue point lies outside the overall source timeline",
                            "<timeline>",
                            cue_index=cue_index,
                            cue_sample=cue_sample,
                            overall_start_sample=overall_start,
                            overall_end_sample=overall_end,
                        )
                    )

        # Validate cue points against the declared translated source duration.
        for track in manifest.tracks:
            if track.kind not in {"dialogue", "narration"}:
                continue
            if "local_duration_seconds" not in track.synth:
                continue
            duration = track.synth.get("local_duration_seconds")
            if not isinstance(duration, (int, float)):
                continue
            last_sample = track.offset_samples + sample_round(float(duration), manifest.sample_rate)
            if track.kind == "dialogue":
                indices = track.synth.get("cue_indices", [])
                for cue_index in indices:
                    if isinstance(cue_index, int) and 0 <= cue_index < len(manifest.dialogue_points_samples):
                        cue_sample = manifest.dialogue_points_samples[cue_index]
                        if cue_sample < track.offset_samples or cue_sample >= last_sample:
                            errors.append(
                                _error(
                                    "E_CUE_OUT_OF_RANGE",
                                    "dialogue cue lies outside track duration after declared offset",
                                    track.name,
                                    cue_index=cue_index,
                                    cue_sample=cue_sample,
                                    track_end_sample=last_sample,
                                )
                            )
            else:
                for segment in track.synth.get("segments", []):
                    cue_index = segment.get("cue_index") if isinstance(segment, dict) else None
                    segment_duration = segment.get("duration_seconds") if isinstance(segment, dict) else None
                    if (
                        isinstance(cue_index, int)
                        and 0 <= cue_index < len(manifest.dialogue_points_samples)
                        and isinstance(segment_duration, (int, float))
                        and not isinstance(segment_duration, bool)
                    ):
                        end_sample = manifest.dialogue_points_samples[cue_index] + sample_round(
                            float(segment_duration), manifest.sample_rate
                        )
                        if manifest.dialogue_points_samples[cue_index] < track.offset_samples or end_sample > last_sample:
                            errors.append(
                                _error(
                                    "E_CUE_OUT_OF_RANGE",
                                    "narration segment lies outside track duration after declared offset",
                                    track.name,
                                    cue_index=cue_index,
                                    segment_end_sample=end_sample,
                                    track_end_sample=last_sample,
                                )
                            )

    if errors:
        raise BusinessFailure(errors, EXIT_VALIDATION)
    return ValidatedInput(manifest=manifest, audio=audio)
