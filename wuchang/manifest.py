"""Manifest loading, validation and deterministic audio identity."""

from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .errors import business_error, canonical_json_bytes, display_path

SUBTYPE_BITS = {
    "PCM_S8": 8,
    "PCM_U8": 8,
    "PCM_16": 16,
    "PCM_24": 24,
    "PCM_32": 32,
    "FLOAT": 32,
    "DOUBLE": 64,
}


def manifest_digest(document: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(document)).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _is_number(value: Any) -> bool:
    return (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and math.isfinite(float(value))
    )


def load_manifest(
    path: str | Path,
) -> tuple[dict[str, Any] | None, list[dict[str, Any]]]:
    manifest_path = Path(path)
    if not manifest_path.is_file():
        return None, [
            business_error(
                "MISSING_MANIFEST",
                None,
                "manifest file does not exist",
                file=manifest_path.name,
            )
        ]
    try:
        with open(manifest_path, "r", encoding="utf-8") as handle:
            document = json.load(handle)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        return None, [
            business_error(
                "INVALID_MANIFEST",
                None,
                "manifest is not readable JSON",
                reason=str(exc).splitlines()[0][:200],
            )
        ]
    if not isinstance(document, dict):
        return None, [
            business_error(
                "INVALID_MANIFEST", None, "manifest root must be an object"
            )
        ]
    return document, []


def validate_manifest_document(
    document: dict[str, Any],
) -> list[dict[str, Any]]:
    errors: list[dict[str, Any]] = []

    if document.get("schema_version") != 1:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "schema_version must be 1",
                field="schema_version",
            )
        )

    sample_rate = document.get("sample_rate")
    if not _is_int(sample_rate) or sample_rate <= 0:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "sample_rate must be a positive integer",
                field="sample_rate",
            )
        )
    bit_depth = document.get("bit_depth")
    if bit_depth not in (8, 16, 24, 32, 64):
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "bit_depth must be one of 8, 16, 24, 32, 64",
                field="bit_depth",
            )
        )
    duration = document.get("duration_seconds")
    if not _is_number(duration) or float(duration) <= 0.0:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "duration_seconds must be positive",
                field="duration_seconds",
            )
        )
    block_frames = document.get("block_frames")
    if not _is_int(block_frames) or block_frames <= 0:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "block_frames must be a positive integer",
                field="block_frames",
            )
        )
    target_lufs = document.get("target_loudness_lufs")
    if not _is_number(target_lufs) or not (-70.0 <= float(target_lufs) <= 0.0):
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "target_loudness_lufs must be between -70 and 0",
                field="target_loudness_lufs",
            )
        )
    true_peak_limit = document.get("true_peak_limit_dbtp", -1.0)
    if not _is_number(true_peak_limit) or float(true_peak_limit) > 0.0:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "true_peak_limit_dbtp must be numeric and no greater than 0",
                field="true_peak_limit_dbtp",
            )
        )

    channels = document.get("channels")
    if not isinstance(channels, list) or not channels:
        errors.append(
            business_error(
                "INVALID_MANIFEST",
                None,
                "channels must be a non-empty array",
                field="channels",
            )
        )
        return errors

    seen_channels: set[str] = set()
    seen_instruments: set[str] = set()
    for index, channel in enumerate(channels):
        if not isinstance(channel, dict):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    f"channel[{index}]",
                    "channel entry must be an object",
                    field=f"channels[{index}]",
                )
            )
            continue
        name = channel.get("name")
        if not isinstance(name, str) or not name.strip():
            name = f"channel[{index}]"
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "channel name must be a non-empty string",
                    field=f"channels[{index}].name",
                )
            )
        elif name in seen_channels:
            errors.append(
                business_error(
                    "DUPLICATE_CHANNEL",
                    name,
                    "channel name is duplicated",
                    field=f"channels[{index}].name",
                )
            )
        else:
            seen_channels.add(name)

        instrument = channel.get("instrument")
        if not isinstance(instrument, str) or not instrument.strip():
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "instrument must be a non-empty string",
                    field=f"channels[{index}].instrument",
                )
            )
        elif instrument in seen_instruments:
            errors.append(
                business_error(
                    "DUPLICATE_INSTRUMENT",
                    name,
                    "instrument name is duplicated",
                    field=f"channels[{index}].instrument",
                    instrument=instrument,
                )
            )
        else:
            seen_instruments.add(instrument)

        file_value = channel.get("file")
        if not isinstance(file_value, str) or not file_value:
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "file must be a non-empty string",
                    field=f"channels[{index}].file",
                )
            )
        seed = channel.get("seed")
        if not _is_int(seed):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "seed must be an integer",
                    field=f"channels[{index}].seed",
                )
            )
        start_offset = channel.get("start_offset_samples")
        if not _is_int(start_offset):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "start_offset_samples must be an integer",
                    field=f"channels[{index}].start_offset_samples",
                )
            )
        fallback = channel.get("fallback_alignment_samples")
        if not _is_int(fallback):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "fallback_alignment_samples must be an integer",
                    field=f"channels[{index}].fallback_alignment_samples",
                )
            )

        cues = channel.get("cues")
        cue_valid = True
        if not isinstance(cues, list) or not cues:
            errors.append(
                business_error(
                    "INVALID_CUE_TIME",
                    name,
                    "cues must be a non-empty ascending array",
                    field=f"channels[{index}].cues",
                )
            )
            cue_valid = False
        else:
            previous: float | None = None
            for cue_index, cue in enumerate(cues):
                if not _is_number(cue) or float(cue) < 0.0:
                    errors.append(
                        business_error(
                            "INVALID_CUE_TIME",
                            name,
                            "cue time must be finite and non-negative",
                            field=f"channels[{index}].cues[{cue_index}]",
                        )
                    )
                    cue_valid = False
                    continue
                numeric_cue = float(cue)
                if previous is not None and numeric_cue <= previous:
                    errors.append(
                        business_error(
                            "INVALID_CUE_TIME",
                            name,
                            "cue times must be strictly ascending",
                            field=f"channels[{index}].cues[{cue_index}]",
                        )
                    )
                    cue_valid = False
                previous = numeric_cue
                if (
                    _is_number(duration)
                    and numeric_cue >= float(duration)
                    and cue_index == len(cues) - 1
                ):
                    errors.append(
                        business_error(
                            "INVALID_CUE_TIME",
                            name,
                            "cue time lies outside the declared duration",
                            field=f"channels[{index}].cues[{cue_index}]",
                        )
                    )
                    cue_valid = False

        weights = channel.get("mix_weights")
        if (
            not isinstance(weights, list)
            or len(weights) != 2
            or not all(_is_number(value) and float(value) >= 0.0 for value in weights)
        ):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "mix_weights must be two non-negative numbers",
                    field=f"channels[{index}].mix_weights",
                )
            )

        reflection = channel.get("early_reflection", {"enabled": False})
        if not isinstance(reflection, dict):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "early_reflection must be an object",
                    field=f"channels[{index}].early_reflection",
                )
            )
            continue
        enabled = reflection.get("enabled", False)
        if not isinstance(enabled, bool):
            errors.append(
                business_error(
                    "INVALID_MANIFEST",
                    name,
                    "early_reflection.enabled must be boolean",
                    field=f"channels[{index}].early_reflection.enabled",
                )
            )
        if enabled:
            delay_ms = reflection.get("delay_ms")
            gain_db = reflection.get("gain_db")
            window_ms = reflection.get("window_ms")
            if not _is_number(delay_ms) or float(delay_ms) <= 0.0:
                errors.append(
                    business_error(
                        "INVALID_MANIFEST",
                        name,
                        "reflection delay_ms must be positive",
                        field=f"channels[{index}].early_reflection.delay_ms",
                    )
                )
            if not _is_number(gain_db) or float(gain_db) >= 0.0:
                errors.append(
                    business_error(
                        "INVALID_MANIFEST",
                        name,
                        "reflection gain_db must be negative",
                        field=f"channels[{index}].early_reflection.gain_db",
                    )
                )
            window_valid = (
                isinstance(window_ms, list)
                and len(window_ms) == 2
                and all(_is_number(value) and float(value) > 0.0 for value in window_ms)
                and float(window_ms[0]) < float(window_ms[1])
            )
            if not window_valid:
                errors.append(
                    business_error(
                        "INVALID_MANIFEST",
                        name,
                        "reflection window_ms must be two positive ascending values",
                        field=f"channels[{index}].early_reflection.window_ms",
                    )
                )
            elif (
                _is_number(delay_ms)
                and not (
                    float(window_ms[0])
                    <= float(delay_ms)
                    <= float(window_ms[1])
                )
            ):
                errors.append(
                    business_error(
                        "INVALID_MANIFEST",
                        name,
                        "reflection delay_ms must lie inside window_ms",
                        field=f"channels[{index}].early_reflection.delay_ms",
                    )
                )
        if not cue_valid:
            continue

    return errors


def validate_audio_and_load(
    document: dict[str, Any],
    manifest_path: str | Path,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Validate every WAV against the manifest and load mono float data."""

    errors: list[dict[str, Any]] = []
    loaded: list[dict[str, Any]] = []
    base_dir = Path(manifest_path).resolve().parent
    expected_rate = document.get("sample_rate")
    expected_bits = document.get("bit_depth")
    channels = document.get("channels", [])
    workspace_root = Path.cwd().resolve()

    for channel in channels:
        name = str(channel.get("name", "channel"))
        file_value = channel.get("file")
        if not isinstance(file_value, str) or not file_value:
            continue
        file_path = Path(file_value)
        if not file_path.is_absolute():
            file_path = base_dir / file_path
        shown_file = display_path(file_path, workspace_root)
        if not file_path.is_file():
            errors.append(
                business_error(
                    "MISSING_FILE",
                    name,
                    "channel WAV file does not exist",
                    file=shown_file,
                )
            )
            continue
        try:
            info = sf.info(str(file_path))
        except Exception as exc:  # noqa: BLE001 - libsndfile exception set
            errors.append(
                business_error(
                    "UNREADABLE_AUDIO",
                    name,
                    "channel WAV cannot be inspected",
                    file=shown_file,
                    reason=str(exc).splitlines()[0][:200],
                )
            )
            continue
        if info.samplerate != expected_rate:
            errors.append(
                business_error(
                    "SAMPLE_RATE_MISMATCH",
                    name,
                    "actual WAV sample rate differs from the manifest",
                    file=shown_file,
                    expected=expected_rate,
                    actual=info.samplerate,
                )
            )
        actual_bits = SUBTYPE_BITS.get(info.subtype)
        if actual_bits is None or actual_bits != expected_bits:
            errors.append(
                business_error(
                    "BIT_DEPTH_MISMATCH",
                    name,
                    "actual WAV bit depth differs from the manifest",
                    file=shown_file,
                    expected=expected_bits,
                    actual=actual_bits,
                    subtype=info.subtype,
                )
            )
        if info.channels != 1:
            errors.append(
                business_error(
                    "CHANNEL_COUNT_MISMATCH",
                    name,
                    "channel WAV must be mono",
                    file=shown_file,
                    expected=1,
                    actual=info.channels,
                )
            )
        try:
            data, sample_rate = sf.read(
                str(file_path), dtype="float64", always_2d=False
            )
        except Exception as exc:  # noqa: BLE001 - decode error normalization
            errors.append(
                business_error(
                    "UNREADABLE_AUDIO",
                    name,
                    "channel WAV cannot be decoded",
                    file=shown_file,
                    reason=str(exc).splitlines()[0][:200],
                )
            )
            continue
        mono = np.asarray(data, dtype=np.float64)
        if mono.ndim != 1:
            errors.append(
                business_error(
                    "CHANNEL_COUNT_MISMATCH",
                    name,
                    "decoded channel is not mono",
                    file=shown_file,
                )
            )
            continue
        if sample_rate != expected_rate:
            continue
        loaded.append(
            {
                "name": name,
                "instrument": channel.get("instrument"),
                "manifest": channel,
                "path": file_path,
                "file": shown_file,
                "sha256": sha256_file(file_path),
                "data": mono,
                "frames": int(mono.size),
                "sample_rate": int(sample_rate),
            }
        )

    return loaded, errors


def input_digest(
    document_digest: str,
    loaded_channels: list[dict[str, Any]],
) -> str:
    identity = {
        "manifest": document_digest,
        "audio": [
            {"channel": channel["name"], "sha256": channel["sha256"]}
            for channel in loaded_channels
        ],
    }
    return hashlib.sha256(canonical_json_bytes(identity)).hexdigest()

