"""Project JSON loading, schema validation and microphone file verification."""

from __future__ import annotations

import json
import os

import soundfile as sf

from .errors import (
    BusinessError,
    CHANNEL_COUNT_MISMATCH,
    DUPLICATE_MIC_NAME,
    FILE_MISSING,
    INVALID_FREQ_BAND,
    INVALID_PROJECT,
    SAMPLE_RATE_MISMATCH,
    UNSUPPORTED_SAMPLE_RATE,
)

REQUIRED_KEYS = (
    "seed",
    "sample_rate",
    "duration_seconds",
    "microphones",
    "detection_threshold",
    "freq_bands",
    "target_loudness_lufs",
    "true_peak_limit_dbtp",
    "energy_gate",
    "noise_gain_db",
    "block_seconds",
)

# ITU-R BS.1770 K-weighting coefficients below are published for 48 kHz only.
SUPPORTED_SAMPLE_RATES = (48000,)


def load_project(path):
    """Load and fully validate a project JSON file.

    Returns the normalized project dictionary. Raises BusinessError on any
    structural or media mismatch; callers must not produce partial masters.
    """
    if not os.path.isfile(path):
        raise BusinessError(FILE_MISSING, f"project file missing: {path}",
                            {"path": os.path.basename(path)})
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except json.JSONDecodeError as exc:
        raise BusinessError(INVALID_PROJECT, f"invalid JSON: {exc}",
                            {"path": os.path.basename(path)}) from exc
    if not isinstance(data, dict):
        raise BusinessError(INVALID_PROJECT, "project root must be an object")

    _validate_scalars(data)
    mics = _validate_microphones(data, os.path.dirname(os.path.abspath(path)))
    bands = _validate_bands(data)

    project = {
        "project_path": os.path.abspath(path),
        "seed": int(data["seed"]),
        "sample_rate": int(data["sample_rate"]),
        "duration_seconds": float(data["duration_seconds"]),
        "microphones": mics,
        "detection_threshold": float(data["detection_threshold"]),
        "freq_bands": bands,
        "target_loudness_lufs": float(data["target_loudness_lufs"]),
        "true_peak_limit_dbtp": float(data["true_peak_limit_dbtp"]),
        "energy_gate": float(data["energy_gate"]),
        "noise_gain_db": float(data["noise_gain_db"]),
        "block_seconds": float(data["block_seconds"]),
    }
    if project["sample_rate"] not in SUPPORTED_SAMPLE_RATES:
        raise BusinessError(
            UNSUPPORTED_SAMPLE_RATE,
            "BS.1770 K-weighting coefficients are bound to 48000 Hz",
            {"sample_rate": project["sample_rate"]},
        )
    return project


def _require_key(data, key):
    if key not in data:
        raise BusinessError(INVALID_PROJECT, f"missing required key: {key}",
                            {"key": key})


def _as_number(data, key, positive=False, non_negative=False):
    _require_key(data, key)
    value = data[key]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise BusinessError(INVALID_PROJECT, f"{key} must be a number", {"key": key})
    if positive and value <= 0:
        raise BusinessError(INVALID_PROJECT, f"{key} must be > 0", {"key": key, "value": value})
    if non_negative and value < 0:
        raise BusinessError(INVALID_PROJECT, f"{key} must be >= 0", {"key": key, "value": value})
    return value


def _validate_scalars(data):
    for key in REQUIRED_KEYS:
        _require_key(data, key)
    if not isinstance(data["seed"], int) or isinstance(data["seed"], bool) or data["seed"] < 0:
        raise BusinessError(INVALID_PROJECT, "seed must be a non-negative integer",
                            {"seed": data.get("seed")})
    _as_number(data, "sample_rate", positive=True)
    _as_number(data, "duration_seconds", positive=True)
    _as_number(data, "detection_threshold", positive=True)
    _as_number(data, "target_loudness_lufs")
    _as_number(data, "true_peak_limit_dbtp")
    _as_number(data, "energy_gate", positive=True)
    _as_number(data, "noise_gain_db")
    _as_number(data, "block_seconds", positive=True)


def _validate_microphones(data, base_dir):
    raw = data["microphones"]
    if not isinstance(raw, list) or len(raw) < 2:
        raise BusinessError(INVALID_PROJECT, "at least two microphones are required",
                            {"key": "microphones"})
    names = set()
    mics = []
    declared_sr = int(data["sample_rate"])
    for index, item in enumerate(raw):
        if not isinstance(item, dict):
            raise BusinessError(INVALID_PROJECT, "microphone entry must be an object",
                                {"index": index})
        for key in ("name", "file", "channels", "position"):
            if key not in item:
                raise BusinessError(INVALID_PROJECT, f"microphone missing key: {key}",
                                    {"index": index})
        name = item["name"]
        if not isinstance(name, str) or not name:
            raise BusinessError(INVALID_PROJECT, "microphone name must be a non-empty string",
                                {"index": index})
        if name in names:
            raise BusinessError(DUPLICATE_MIC_NAME, f"duplicate microphone name: {name}",
                                {"mic": name})
        names.add(name)
        channels = item["channels"]
        if not isinstance(channels, int) or isinstance(channels, bool) or channels < 1:
            raise BusinessError(INVALID_PROJECT, "microphone channels must be a positive integer",
                                {"mic": name})
        position = item["position"]
        if (not isinstance(position, list) or len(position) != 3
                or any(not isinstance(v, (int, float)) or isinstance(v, bool) for v in position)):
            raise BusinessError(INVALID_PROJECT, "position must be three numbers [x, y, z]",
                                {"mic": name})
        file_path = item["file"]
        if not isinstance(file_path, str) or not file_path:
            raise BusinessError(INVALID_PROJECT, "microphone file must be a non-empty string",
                                {"mic": name})
        abs_path = file_path if os.path.isabs(file_path) else os.path.join(base_dir, file_path)
        if not os.path.isfile(abs_path):
            raise BusinessError(FILE_MISSING, f"microphone WAV missing: {file_path}",
                                {"mic": name, "file": os.path.basename(file_path)})
        info = sf.info(abs_path)
        if info.samplerate != declared_sr:
            raise BusinessError(
                SAMPLE_RATE_MISMATCH,
                f"sample rate mismatch for {name}: {info.samplerate} != {declared_sr}",
                {"mic": name, "declared": declared_sr, "actual": info.samplerate},
            )
        if info.channels != channels:
            raise BusinessError(
                CHANNEL_COUNT_MISMATCH,
                f"channel count mismatch for {name}: {info.channels} != {channels}",
                {"mic": name, "declared": channels, "actual": info.channels},
            )
        mics.append({
            "name": name,
            "file": abs_path,
            "file_rel": file_path,
            "channels": channels,
            "position": [float(v) for v in position],
        })
    return mics


def _validate_bands(data):
    raw = data["freq_bands"]
    if not isinstance(raw, list) or len(raw) < 3:
        raise BusinessError(INVALID_FREQ_BAND, "at least three frequency bands are required",
                            {"key": "freq_bands"})
    sr = float(data["sample_rate"])
    nyquist = sr / 2.0
    names = set()
    bands = []
    previous_upper = 0.0
    for index, item in enumerate(raw):
        if not isinstance(item, dict):
            raise BusinessError(INVALID_FREQ_BAND, "band entry must be an object", {"index": index})
        for key in ("name", "low_hz", "high_hz"):
            if key not in item:
                raise BusinessError(INVALID_FREQ_BAND, f"band missing key: {key}", {"index": index})
        name = item["name"]
        if not isinstance(name, str) or not name:
            raise BusinessError(INVALID_FREQ_BAND, "band name must be a non-empty string",
                                {"index": index})
        if name in names:
            raise BusinessError(INVALID_FREQ_BAND, f"duplicate band name: {name}", {"band": name})
        names.add(name)
        low = item["low_hz"]
        high = item["high_hz"]
        if isinstance(low, bool) or isinstance(high, bool) or \
                not isinstance(low, (int, float)) or not isinstance(high, (int, float)):
            raise BusinessError(INVALID_FREQ_BAND, f"band {name} bounds must be numbers",
                                {"band": name})
        low = float(low)
        high = float(high)
        if low <= 0 or high <= low or high > nyquist:
            raise BusinessError(
                INVALID_FREQ_BAND,
                f"band {name} out of range or inverted (nyquist={nyquist} Hz)",
                {"band": name, "low_hz": low, "high_hz": high},
            )
        # Contiguous bands must not overlap; a small gap is acceptable but
        # rejected here to keep the partition explicit.
        if index > 0 and low != previous_upper:
            raise BusinessError(
                INVALID_FREQ_BAND,
                f"band {name} overlaps or leaves a gap against the previous band",
                {"band": name, "low_hz": low, "expected_low_hz": previous_upper},
            )
        previous_upper = high
        bands.append({"name": name, "low_hz": low, "high_hz": high})
    return bands
