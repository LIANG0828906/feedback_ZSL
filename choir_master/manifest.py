from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .errors import MasterError, ValidationFailure, ValidationIssue
from .util import canonical_json_bytes, sha256_file


VOICE_ORDER = ("soprano", "alto", "tenor", "bass")
VOICE_LABELS_ZH = {
    "soprano": "女高",
    "alto": "女低",
    "tenor": "男高",
    "bass": "男低",
}
EXPECTED_SUBTYPES = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}


@dataclass(frozen=True)
class MicPosition:
    distance_m: float
    azimuth_deg: float


@dataclass(frozen=True)
class TrackSpec:
    track_id: str
    declared_path: str
    path: Path
    position: MicPosition
    reference: bool


@dataclass(frozen=True)
class VoiceSpec:
    voice: str
    label_zh: str
    target_energy_ratio: float
    downmix_weights: tuple[float, float]
    tracks: tuple[TrackSpec, ...]


@dataclass(frozen=True)
class ProjectManifest:
    path: Path
    raw: dict[str, Any]
    seed: int
    sample_rate: int
    bit_depth: int
    channels: int
    duration_samples: int
    transition_band_hz: tuple[float, float]
    passaggio_hz: float
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    block_size: int
    voices: tuple[VoiceSpec, ...]
    digest: str

    def voice(self, name: str) -> VoiceSpec:
        for voice in self.voices:
            if voice.voice == name:
                return voice
        raise KeyError(name)


@dataclass
class LoadedAudio:
    manifest: ProjectManifest
    tracks: dict[str, np.ndarray]
    path_digests: dict[str, str]

    @property
    def total_samples(self) -> int:
        return max((len(audio) for audio in self.tracks.values()), default=0)

    def voice_track_ids(self, voice: str) -> list[str]:
        return [track.track_id for track in self.manifest.voice(voice).tracks]


def _issue(
    code: str,
    message: str,
    *,
    path: str | Path | None = None,
    details: dict[str, Any] | None = None,
) -> ValidationIssue:
    return ValidationIssue(
        code=code,
        message=message,
        path=str(path) if path is not None else None,
        details=details or {},
    )


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _require_number(root: dict[str, Any], key: str, manifest_path: Path) -> float:
    value = root.get(key)
    if not _is_number(value):
        raise MasterError(
            "E_MANIFEST_INVALID",
            f"Manifest field '{key}' must be a finite number.",
            path=str(manifest_path),
            details={"field": key},
        )
    return float(value)


def _require_int(root: dict[str, Any], key: str, manifest_path: Path) -> int:
    value = root.get(key)
    if not isinstance(value, int) or isinstance(value, bool):
        raise MasterError(
            "E_MANIFEST_INVALID",
            f"Manifest field '{key}' must be an integer.",
            path=str(manifest_path),
            details={"field": key},
        )
    return value


def load_manifest(path: str | Path) -> ProjectManifest:
    manifest_path = Path(path).expanduser().resolve()
    if not manifest_path.is_file():
        raise MasterError(
            "E_MANIFEST_MISSING",
            "Project manifest does not exist.",
            path=str(manifest_path),
            exit_code=2,
        )
    import json

    try:
        with manifest_path.open("r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise MasterError(
            "E_MANIFEST_INVALID",
            f"Cannot parse project manifest: {exc}",
            path=str(manifest_path),
        ) from exc
    if not isinstance(raw, dict):
        raise MasterError(
            "E_MANIFEST_INVALID",
            "Manifest root must be a JSON object.",
            path=str(manifest_path),
        )

    seed = _require_int(raw, "seed", manifest_path)
    sample_rate = _require_int(raw, "sample_rate", manifest_path)
    bit_depth = _require_int(raw, "bit_depth", manifest_path)
    channels = _require_int(raw, "channels", manifest_path)
    duration_samples = _require_int(raw, "duration_samples", manifest_path)
    block_size = _require_int(raw, "block_size", manifest_path)
    target_loudness = _require_number(raw, "target_loudness_lufs", manifest_path)
    true_peak_limit = _require_number(raw, "true_peak_limit_dbtp", manifest_path)
    passaggio_hz = _require_number(raw, "passaggio_hz", manifest_path)

    if seed < 0:
        raise MasterError("E_MANIFEST_INVALID", "seed must be non-negative.", path=str(manifest_path))
    if sample_rate < 8000:
        raise MasterError("E_MANIFEST_INVALID", "sample_rate is too low.", path=str(manifest_path))
    if sample_rate != 48_000:
        raise MasterError(
            "E_UNSUPPORTED_SAMPLE_RATE",
            "This mastering chain requires the declared 48 kHz sample rate.",
            path=str(manifest_path),
            details={"expected": 48_000, "actual": sample_rate},
        )
    if bit_depth not in EXPECTED_SUBTYPES:
        raise MasterError(
            "E_MANIFEST_INVALID",
            "bit_depth must be one of 16, 24, or 32.",
            path=str(manifest_path),
        )
    if channels < 1 or channels > 8:
        raise MasterError("E_MANIFEST_INVALID", "channels is out of range.", path=str(manifest_path))
    if duration_samples <= 0:
        raise MasterError("E_MANIFEST_INVALID", "duration_samples must be positive.", path=str(manifest_path))
    if block_size <= 0:
        raise MasterError("E_MANIFEST_INVALID", "block_size must be positive.", path=str(manifest_path))
    if not -70.0 <= target_loudness <= 0.0:
        raise MasterError("E_MANIFEST_INVALID", "target_loudness_lufs is out of range.", path=str(manifest_path))
    if not -30.0 <= true_peak_limit <= 0.0:
        raise MasterError("E_MANIFEST_INVALID", "true_peak_limit_dbtp is out of range.", path=str(manifest_path))

    band_raw = raw.get("transition_band_hz")
    if (
        not isinstance(band_raw, list)
        or len(band_raw) != 2
        or not all(_is_number(item) for item in band_raw)
        or not 0 < float(band_raw[0]) < float(band_raw[1]) < sample_rate / 2
    ):
        raise MasterError(
            "E_MANIFEST_INVALID",
            "transition_band_hz must contain two increasing frequencies inside Nyquist.",
            path=str(manifest_path),
        )
    band = (float(band_raw[0]), float(band_raw[1]))

    voices_raw = raw.get("voices")
    if not isinstance(voices_raw, list):
        raise MasterError(
            "E_MANIFEST_INVALID",
            "Manifest must contain a voices array.",
            path=str(manifest_path),
        )

    voices: list[VoiceSpec] = []
    voice_seen: set[str] = set()
    track_ids_seen: set[str] = set()
    for voice_index, voice_raw in enumerate(voices_raw):
        if not isinstance(voice_raw, dict):
            raise MasterError(
                "E_MANIFEST_INVALID",
                f"voices[{voice_index}] must be an object.",
                path=str(manifest_path),
            )
        name = voice_raw.get("voice")
        if not isinstance(name, str) or not name:
            raise MasterError(
                "E_MANIFEST_INVALID",
                f"voices[{voice_index}].voice must be a non-empty string.",
                path=str(manifest_path),
            )
        if name in voice_seen:
            raise ValidationFailure(
                [
                    _issue(
                        "E_VOICE_DUPLICATE",
                        f"Voice '{name}' is declared more than once.",
                        path=manifest_path,
                        details={"voice": name},
                    )
                ],
                str(manifest_path),
            )
        voice_seen.add(name)
        if name not in VOICE_ORDER:
            raise ValidationFailure(
                [
                    _issue(
                        "E_VOICE_UNKNOWN",
                        f"Voice '{name}' is not one of soprano/alto/tenor/bass.",
                        path=manifest_path,
                        details={"voice": name},
                    )
                ],
                str(manifest_path),
            )
        ratio = voice_raw.get("target_energy_ratio")
        if not _is_number(ratio) or float(ratio) <= 0:
            raise MasterError(
                "E_MANIFEST_INVALID",
                f"voices[{voice_index}].target_energy_ratio must be positive.",
                path=str(manifest_path),
            )
        weights = voice_raw.get("downmix_weights")
        if (
            not isinstance(weights, list)
            or len(weights) != 2
            or not all(_is_number(item) and float(item) >= 0 for item in weights)
            or sum(float(item) ** 2 for item in weights) <= 0
        ):
            raise MasterError(
                "E_MANIFEST_INVALID",
                f"voices[{voice_index}].downmix_weights must contain two non-negative values.",
                path=str(manifest_path),
            )
        tracks_raw = voice_raw.get("tracks")
        if not isinstance(tracks_raw, list):
            raise MasterError(
                "E_MANIFEST_INVALID",
                f"voices[{voice_index}].tracks must be an array.",
                path=str(manifest_path),
            )
        tracks: list[TrackSpec] = []
        reference_count = 0
        for track_index, track_raw in enumerate(tracks_raw):
            if not isinstance(track_raw, dict):
                raise MasterError(
                    "E_MANIFEST_INVALID",
                    f"voices[{voice_index}].tracks[{track_index}] must be an object.",
                    path=str(manifest_path),
                )
            track_id = track_raw.get("id")
            declared_path = track_raw.get("path")
            position_raw = track_raw.get("mic_position")
            if not isinstance(track_id, str) or not track_id:
                raise MasterError(
                    "E_MANIFEST_INVALID",
                    f"voices[{voice_index}].tracks[{track_index}].id is invalid.",
                    path=str(manifest_path),
                )
            if track_id in track_ids_seen:
                raise ValidationFailure(
                    [
                        _issue(
                            "E_TRACK_DUPLICATE",
                            f"Track id '{track_id}' is declared more than once.",
                            path=manifest_path,
                            details={"track_id": track_id},
                        )
                    ],
                    str(manifest_path),
                )
            track_ids_seen.add(track_id)
            if not isinstance(declared_path, str) or not declared_path:
                raise MasterError(
                    "E_MANIFEST_INVALID",
                    f"voices[{voice_index}].tracks[{track_index}].path is invalid.",
                    path=str(manifest_path),
                )
            if (
                not isinstance(position_raw, dict)
                or not _is_number(position_raw.get("distance_m"))
                or float(position_raw["distance_m"]) <= 0
                or not _is_number(position_raw.get("azimuth_deg"))
            ):
                raise ValidationFailure(
                    [
                        _issue(
                            "E_MIC_POSITION_INVALID",
                            f"Track '{track_id}' has an invalid microphone position.",
                            path=manifest_path,
                            details={"track_id": track_id},
                        )
                    ],
                    str(manifest_path),
                )
            reference = bool(track_raw.get("reference", False))
            reference_count += int(reference)
            resolved = (manifest_path.parent / declared_path).resolve()
            tracks.append(
                TrackSpec(
                    track_id=track_id,
                    declared_path=declared_path,
                    path=resolved,
                    position=MicPosition(
                        distance_m=float(position_raw["distance_m"]),
                        azimuth_deg=float(position_raw["azimuth_deg"]),
                    ),
                    reference=reference,
                )
            )
        if len(tracks) != 2:
            raise ValidationFailure(
                [
                    _issue(
                        "E_MIC_COUNT_INVALID",
                        f"Voice '{name}' must have exactly two microphone tracks.",
                        path=manifest_path,
                        details={"voice": name, "actual": len(tracks)},
                    )
                ],
                str(manifest_path),
            )
        if reference_count != 1:
            raise ValidationFailure(
                [
                    _issue(
                        "E_REFERENCE_TRACK_INVALID",
                        f"Voice '{name}' must declare exactly one reference track.",
                        path=manifest_path,
                        details={"voice": name, "actual": reference_count},
                    )
                ],
                str(manifest_path),
            )
        voices.append(
            VoiceSpec(
                voice=name,
                label_zh=str(voice_raw.get("label_zh") or VOICE_LABELS_ZH[name]),
                target_energy_ratio=float(ratio),
                downmix_weights=(float(weights[0]), float(weights[1])),
                tracks=tuple(tracks),
            )
        )

    missing_voices = [name for name in VOICE_ORDER if name not in voice_seen]
    issues: list[ValidationIssue] = []
    for name in missing_voices:
        issues.append(
            _issue(
                "E_VOICE_MISSING",
                f"Required voice '{name}' is missing.",
                path=manifest_path,
                details={"voice": name},
            )
        )
    ratio_sum = sum(voice.target_energy_ratio for voice in voices)
    if abs(ratio_sum - 1.0) > 1e-6:
        issues.append(
            _issue(
                "E_TARGET_RATIO_SUM",
                "target_energy_ratio values must sum to 1.0.",
                path=manifest_path,
                details={"sum": ratio_sum},
            )
        )
    if issues:
        raise ValidationFailure(issues, str(manifest_path))

    digest = canonical_json_bytes(raw).hex()
    return ProjectManifest(
        path=manifest_path,
        raw=raw,
        seed=seed,
        sample_rate=sample_rate,
        bit_depth=bit_depth,
        channels=channels,
        duration_samples=duration_samples,
        transition_band_hz=band,
        passaggio_hz=passaggio_hz,
        target_loudness_lufs=target_loudness,
        true_peak_limit_dbtp=true_peak_limit,
        block_size=block_size,
        voices=tuple(voices),
        digest=digest,
    )


def load_and_validate_audio(path: str | Path) -> LoadedAudio:
    manifest = load_manifest(path)
    issues: list[ValidationIssue] = []
    tracks: dict[str, np.ndarray] = {}
    path_digests: dict[str, str] = {}
    expected_subtype = EXPECTED_SUBTYPES[manifest.bit_depth]

    for voice in manifest.voices:
        for track in voice.tracks:
            if not track.path.is_file():
                issues.append(
                    _issue(
                        "E_FILE_MISSING",
                        f"Audio file for track '{track.track_id}' is missing.",
                        path=track.path,
                        details={
                            "track_id": track.track_id,
                            "voice": voice.voice,
                            "declared_path": track.declared_path,
                        },
                    )
                )
                continue
            try:
                info = sf.info(str(track.path))
            except (RuntimeError, OSError) as exc:
                issues.append(
                    _issue(
                        "E_AUDIO_UNREADABLE",
                        f"Cannot inspect audio file: {exc}",
                        path=track.path,
                        details={"track_id": track.track_id, "voice": voice.voice},
                    )
                )
                continue
            if info.samplerate != manifest.sample_rate:
                issues.append(
                    _issue(
                        "E_SAMPLE_RATE_MISMATCH",
                        "Audio sample rate does not match the manifest.",
                        path=track.path,
                        details={
                            "track_id": track.track_id,
                            "voice": voice.voice,
                            "expected": manifest.sample_rate,
                            "actual": info.samplerate,
                        },
                    )
                )
            if info.subtype != expected_subtype:
                issues.append(
                    _issue(
                        "E_BIT_DEPTH_MISMATCH",
                        "Audio sample format/bit depth does not match the manifest.",
                        path=track.path,
                        details={
                            "track_id": track.track_id,
                            "voice": voice.voice,
                            "expected": expected_subtype,
                            "actual": info.subtype,
                        },
                    )
                )
            if info.channels != manifest.channels:
                issues.append(
                    _issue(
                        "E_CHANNEL_COUNT_MISMATCH",
                        "Audio channel count does not match the manifest.",
                        path=track.path,
                        details={
                            "track_id": track.track_id,
                            "voice": voice.voice,
                            "expected": manifest.channels,
                            "actual": info.channels,
                        },
                    )
                )
            try:
                data, samplerate = sf.read(str(track.path), dtype="float64", always_2d=False)
            except (RuntimeError, OSError) as exc:
                issues.append(
                    _issue(
                        "E_AUDIO_UNREADABLE",
                        f"Cannot read audio file: {exc}",
                        path=track.path,
                        details={"track_id": track.track_id, "voice": voice.voice},
                    )
                )
                continue
            if samplerate != manifest.sample_rate and not any(
                issue.code == "E_SAMPLE_RATE_MISMATCH" and issue.path == str(track.path)
                for issue in issues
            ):
                issues.append(
                    _issue(
                        "E_SAMPLE_RATE_MISMATCH",
                        "Audio sample rate does not match the manifest.",
                        path=track.path,
                        details={
                            "track_id": track.track_id,
                            "expected": manifest.sample_rate,
                            "actual": samplerate,
                        },
                    )
                )
            if data.ndim != 1:
                issues.append(
                    _issue(
                        "E_CHANNEL_COUNT_MISMATCH",
                        "Input microphone files must be mono.",
                        path=track.path,
                        details={"track_id": track.track_id, "actual_ndim": int(data.ndim)},
                    )
                )
            if not np.all(np.isfinite(data)):
                issues.append(
                    _issue(
                        "E_NON_FINITE_AUDIO",
                        "Audio contains NaN or infinite sample values.",
                        path=track.path,
                        details={"track_id": track.track_id, "voice": voice.voice},
                    )
                )
            tracks[track.track_id] = np.asarray(data, dtype=np.float64).reshape(-1)
            path_digests[str(track.path)] = sha256_file(track.path)

    if issues:
        raise ValidationFailure(issues, str(manifest.path))
    return LoadedAudio(manifest=manifest, tracks=tracks, path_digests=path_digests)

