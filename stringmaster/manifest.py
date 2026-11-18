"""Manifest loading, validation, and canonical digesting."""

from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass
from typing import Any

from .audio import WaveInfo, inspect_wav, sha256_file
from .errors import BusinessError, Issue, ManifestError


INSTRUMENTS = ("第一小提琴", "第二小提琴", "中提琴", "大提琴")
INSTRUMENT_ALIASES = {
    "第一小提琴": "第一小提琴",
    "第二小提琴": "第二小提琴",
    "中提琴": "中提琴",
    "大提琴": "大提琴",
    "violin_1": "第一小提琴",
    "violin_2": "第二小提琴",
    "viola": "中提琴",
    "cello": "大提琴",
}


@dataclass(frozen=True)
class TrackSpec:
    path: str
    abs_path: str
    instrument: str
    distance_m: float
    azimuth_deg: float
    target_energy_ratio: float
    downmix_weights: tuple[float, float]
    wav: WaveInfo | None = None
    sha256: str | None = None


@dataclass(frozen=True)
class ManifestContext:
    manifest_path: str
    manifest_dir: str
    raw: dict[str, Any]
    digest: str
    seed: int
    sample_rate: int
    bit_depth: int
    duration_seconds: float
    band_low_hz: float
    band_high_hz: float
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    block_size: int
    tracks: tuple[TrackSpec, ...]

    def track(self, instrument: str) -> TrackSpec:
        for track in self.tracks:
            if track.instrument == instrument:
                return track
        raise KeyError(instrument)


def canonical_json(value: Any) -> bytes:
    return json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    ).encode("utf-8")


def manifest_digest(raw: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json(raw)).hexdigest()


def _issue_list(errors: list[Issue]) -> None:
    if errors:
        raise ManifestError(errors[0].file, errors)


def _finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _normalize_instrument(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    return INSTRUMENT_ALIASES.get(value.strip())


def _resolve_track_path(manifest_dir: str, path: str) -> str:
    expanded = os.path.expanduser(path)
    if os.path.isabs(expanded):
        return os.path.abspath(expanded)
    return os.path.abspath(os.path.join(manifest_dir, expanded))


def validate_manifest_structure(
    raw: Any,
    manifest_path: str,
    *,
    require_files: bool,
) -> ManifestContext:
    issues: list[Issue] = []
    manifest_abs = os.path.abspath(manifest_path)
    manifest_dir = os.path.dirname(manifest_abs)

    def add(code: str, file: str, message: str) -> None:
        issues.append(Issue(code, file, message))

    if not isinstance(raw, dict):
        add("E_MANIFEST_SCHEMA", manifest_abs, "manifest root must be a JSON object")
        _issue_list(issues)
        raise AssertionError("unreachable")

    schema_version = raw.get("schema_version", 1)
    if schema_version != 1:
        add("E_MANIFEST_SCHEMA", manifest_abs, "schema_version must be 1")

    seed = raw.get("seed")
    if not isinstance(seed, int) or isinstance(seed, bool):
        add("E_MANIFEST_SCHEMA", manifest_abs, "seed must be an integer")
        seed = 0

    sample_rate = raw.get("sample_rate")
    if not isinstance(sample_rate, int) or isinstance(sample_rate, bool) or sample_rate <= 0:
        add("E_MANIFEST_SCHEMA", manifest_abs, "sample_rate must be a positive integer")
        sample_rate = 0

    bit_depth = raw.get("bit_depth")
    if bit_depth not in (16, 24, 32):
        add("E_MANIFEST_SCHEMA", manifest_abs, "bit_depth must be 16, 24, or 32")

    duration = raw.get("duration_seconds")
    if not _finite_number(duration) or float(duration) <= 0:
        add("E_MANIFEST_SCHEMA", manifest_abs, "duration_seconds must be a positive number")
        duration = 0.0

    band = raw.get("crossover_band")
    band_low = band_high = 0.0
    if not isinstance(band, dict):
        add("E_BAND_INVALID", manifest_abs, "crossover_band must be an object")
    else:
        band_low = band.get("low_hz")
        band_high = band.get("high_hz")
        if not _finite_number(band_low) or not _finite_number(band_high):
            add("E_BAND_INVALID", manifest_abs, "crossover_band frequencies must be finite numbers")
        else:
            band_low = float(band_low)
            band_high = float(band_high)
            nyquist = sample_rate / 2.0
            if not (0.0 < band_low < band_high):
                add("E_BAND_INVALID", manifest_abs, "crossover band requires 0 < low_hz < high_hz")
            if sample_rate and band_high >= nyquist:
                add(
                    "E_BAND_INVALID",
                    manifest_abs,
                    f"crossover band high_hz {band_high:g} must be below Nyquist {nyquist:g}",
                )

    target_lufs = raw.get("target_loudness_lufs")
    if not _finite_number(target_lufs):
        add("E_MANIFEST_SCHEMA", manifest_abs, "target_loudness_lufs must be a finite number")
        target_lufs = -20.0

    tp_limit = raw.get("true_peak_limit_dbtp", -1.0)
    if not _finite_number(tp_limit):
        add("E_MANIFEST_SCHEMA", manifest_abs, "true_peak_limit_dbtp must be a finite number")
        tp_limit = -1.0

    block_size = raw.get("block_size", 16384)
    if not isinstance(block_size, int) or isinstance(block_size, bool) or block_size <= 0:
        add("E_MANIFEST_SCHEMA", manifest_abs, "block_size must be a positive integer")
        block_size = 16384

    tracks_raw = raw.get("tracks")
    if not isinstance(tracks_raw, list):
        add("E_MANIFEST_SCHEMA", manifest_abs, "tracks must be an array")
        tracks_raw = []

    track_specs: list[TrackSpec] = []
    actual_channels: list[tuple[str, int]] = []
    seen: dict[str, str] = {}

    for index, track_raw in enumerate(tracks_raw):
        if not isinstance(track_raw, dict):
            add("E_MANIFEST_SCHEMA", manifest_abs, f"tracks[{index}] must be an object")
            continue
        path_value = track_raw.get("path")
        path_text = path_value if isinstance(path_value, str) and path_value.strip() else ""
        abs_path = _resolve_track_path(manifest_dir, path_text) if path_text else manifest_abs
        instrument = _normalize_instrument(track_raw.get("instrument"))
        if instrument is None:
            add(
                "E_MANIFEST_SCHEMA",
                abs_path,
                f"tracks[{index}].instrument must be one of {', '.join(INSTRUMENTS)}",
            )
            instrument = f"未知乐器[{index}]"
        if instrument in seen:
            add(
                "E_INSTRUMENT_DUPLICATE",
                abs_path,
                f"instrument {instrument} is duplicated; first declared at {seen[instrument]}",
            )
        else:
            seen[instrument] = abs_path

        if not path_text:
            add("E_MANIFEST_SCHEMA", manifest_abs, f"tracks[{index}].path must be a non-empty string")

        distance = track_raw.get("distance_m")
        if not _finite_number(distance) or float(distance) < 0:
            add("E_MANIFEST_SCHEMA", abs_path, "distance_m must be a non-negative finite number")
            distance = 0.0
        azimuth = track_raw.get("azimuth_deg")
        if not _finite_number(azimuth):
            add("E_MANIFEST_SCHEMA", abs_path, "azimuth_deg must be a finite number")
            azimuth = 0.0
        ratio = track_raw.get("target_energy_ratio")
        if not _finite_number(ratio) or float(ratio) <= 0:
            add("E_MANIFEST_SCHEMA", abs_path, "target_energy_ratio must be a positive finite number")
            ratio = 0.0

        weights_raw = track_raw.get("downmix_weights")
        if weights_raw is None:
            weights = (1.0, 1.0)
        elif (
            isinstance(weights_raw, list)
            and len(weights_raw) == 2
            and all(_finite_number(v) and float(v) >= 0 for v in weights_raw)
            and any(float(v) > 0 for v in weights_raw)
        ):
            weights = (float(weights_raw[0]), float(weights_raw[1]))
        else:
            add(
                "E_MANIFEST_SCHEMA",
                abs_path,
                "downmix_weights must be two non-negative finite values with a positive sum",
            )
            weights = (1.0, 1.0)

        wav_info: WaveInfo | None = None
        source_sha: str | None = None
        if require_files and path_text:
            if not os.path.isfile(abs_path):
                add("E_FILE_MISSING", abs_path, "declared audio file does not exist")
            else:
                try:
                    wav_info = inspect_wav(abs_path)
                    source_sha = sha256_file(abs_path)
                except BusinessError as exc:
                    add(exc.code, exc.file, exc.message)
                except OSError as exc:
                    add("E_FILE_READ", abs_path, str(exc))
                if wav_info is not None:
                    actual_channels.append((abs_path, wav_info.channels))
                    if wav_info.sample_rate != sample_rate:
                        add(
                            "E_SAMPLE_RATE_MISMATCH",
                            abs_path,
                            f"actual sample rate {wav_info.sample_rate} != manifest {sample_rate}",
                        )
                    if wav_info.bit_depth != bit_depth:
                        add(
                            "E_BIT_DEPTH_MISMATCH",
                            abs_path,
                            f"actual bit depth {wav_info.bit_depth} != manifest {bit_depth}",
                        )
                    if wav_info.channels not in (1, 2):
                        add(
                            "E_CHANNEL_UNSUPPORTED",
                            abs_path,
                            f"actual channel count {wav_info.channels}; only mono and stereo are supported",
                        )

        track_specs.append(
            TrackSpec(
                path=path_text,
                abs_path=abs_path,
                instrument=instrument,
                distance_m=float(distance),
                azimuth_deg=float(azimuth),
                target_energy_ratio=float(ratio),
                downmix_weights=weights,
                wav=wav_info,
                sha256=source_sha,
            )
        )

    found = {track.instrument for track in track_specs if track.instrument in INSTRUMENTS}
    for instrument in INSTRUMENTS:
        if instrument not in found:
            add(
                "E_INSTRUMENT_MISSING",
                manifest_abs,
                f"required instrument {instrument} is missing",
            )

    if actual_channels:
        expected_channels = actual_channels[0][1]
        for path, channels in actual_channels:
            if channels != expected_channels:
                add(
                    "E_CHANNEL_MISMATCH",
                    path,
                    f"channel count {channels} differs from quartet channel count {expected_channels}",
                )

    total_ratio = sum(track.target_energy_ratio for track in track_specs)
    if track_specs and abs(total_ratio - 1.0) > 1e-6:
        add(
            "E_RATIO_SUM",
            manifest_abs,
            f"target energy ratios sum to {total_ratio:.12g}, expected 1 within 1e-6",
        )

    _issue_list(issues)

    return ManifestContext(
        manifest_path=manifest_abs,
        manifest_dir=manifest_dir,
        raw=raw,
        digest=manifest_digest(raw),
        seed=seed,
        sample_rate=int(sample_rate),
        bit_depth=int(bit_depth),
        duration_seconds=float(duration),
        band_low_hz=float(band_low),
        band_high_hz=float(band_high),
        target_loudness_lufs=float(target_lufs),
        true_peak_limit_dbtp=float(tp_limit),
        block_size=int(block_size),
        tracks=tuple(track_specs),
    )


def load_manifest(path: str, *, require_files: bool = True) -> ManifestContext:
    abs_path = os.path.abspath(path)
    if not os.path.isfile(abs_path):
        raise BusinessError("E_FILE_MISSING", abs_path, "manifest file does not exist")
    try:
        with open(abs_path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except json.JSONDecodeError as exc:
        raise BusinessError(
            "E_MANIFEST_JSON",
            abs_path,
            f"invalid JSON at line {exc.lineno}, column {exc.colno}: {exc.msg}",
        ) from exc
    except OSError as exc:
        raise BusinessError("E_FILE_READ", abs_path, str(exc)) from exc
    return validate_manifest_structure(raw, abs_path, require_files=require_files)
