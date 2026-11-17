from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .audio_io import inspect_wav
from .errors import IssueCollector


MISSING = object()

INSTRUMENT_ALIASES: dict[str, str] = {
    "主唱": "vocal",
    "人声": "vocal",
    "vocal": "vocal",
    "voice": "vocal",
    "lead": "vocal",
    "leadvocal": "vocal",
    "lead_vocal": "vocal",
    "吉他": "guitar",
    "guitar": "guitar",
    "gtr": "guitar",
    "贝斯": "bass",
    "低音": "bass",
    "bass": "bass",
    "鼓组": "drums",
    "鼓": "drums",
    "drums": "drums",
    "drum": "drums",
    "环境": "environment",
    "舞台环境": "environment",
    "氛围": "environment",
    "environment": "environment",
    "ambience": "environment",
    "ambient": "environment",
    "观众": "crowd",
    "观众氛围": "crowd",
    "crowd": "crowd",
    "audience": "crowd",
}

INSTRUMENT_DISPLAY = {
    "vocal": "主唱",
    "guitar": "吉他",
    "bass": "贝斯",
    "drums": "鼓组",
    "environment": "环境",
    "crowd": "观众",
}

GROUP_ALIASES = {
    "人声组": "vocal",
    "人声": "vocal",
    "vocal": "vocal",
    "voice": "vocal",
    "乐器组": "instrument",
    "乐器": "instrument",
    "instrument": "instrument",
    "instruments": "instrument",
    "环境组": "ambience",
    "环境": "ambience",
    "空间": "ambience",
    "ambience": "ambience",
    "ambient": "ambience",
    "environment": "ambience",
    "crowd": "ambience",
}

GROUP_DISPLAY = {
    "vocal": "人声组",
    "instrument": "乐器组",
    "ambience": "环境组",
}

INSTRUMENT_GROUP = {
    "vocal": "vocal",
    "guitar": "instrument",
    "bass": "instrument",
    "drums": "instrument",
    "environment": "ambience",
    "crowd": "ambience",
}

REQUIRED_INSTRUMENTS = ("vocal", "guitar", "bass", "drums", "environment", "crowd")


@dataclass(frozen=True)
class ChannelSpec:
    channel_id: str
    instrument: str
    instrument_key: str
    group: str
    path: str
    path_report: str
    anchor: tuple[float, float, float]
    start_offset_samples: int
    end_offset_samples: int | None
    expected_sample_rate: int
    expected_bit_depth: int
    expected_channels: int
    downmix_weight: float
    source_duration_seconds: float


@dataclass(frozen=True)
class Manifest:
    raw: dict[str, Any]
    digest: str
    base_dir: Path
    seed: int
    sample_rate: int
    bit_depth: int
    num_channels: int
    stage_bounds: dict[str, tuple[float, float]]
    listener: tuple[float, float, float]
    target_loudness_lufs: float
    true_peak_limit_dbtp: float
    spatial_stability_threshold_db: float
    block_samples: int
    source_duration_seconds: float
    group_gains_db: dict[str, float]
    group_weights: dict[str, dict[str, float]]
    channels: tuple[ChannelSpec, ...]


def _canonical(value: Any) -> str:
    return str(value).strip().lower().replace("-", "_").replace(" ", "_")


def canonical_instrument(value: Any) -> str | None:
    key = _canonical(value)
    return INSTRUMENT_ALIASES.get(key)


def canonical_group(value: Any) -> str | None:
    key = _canonical(value)
    return GROUP_ALIASES.get(key)


def _pick(mapping: dict[str, Any], keys: tuple[str, ...], default: Any = MISSING) -> Any:
    for key in keys:
        if key in mapping:
            return mapping[key]
    return default


def _as_float(value: Any) -> float | None:
    if isinstance(value, bool):
        return None
    try:
        result = float(value)
    except (TypeError, ValueError):
        return None
    return result if math.isfinite(result) else None


def _as_int(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and math.isfinite(value) and value.is_integer():
        return int(value)
    return None


def _coord(anchor: Any) -> tuple[float, float, float] | None:
    if isinstance(anchor, dict):
        x = _as_float(_pick(anchor, ("x", "X"), MISSING))
        y = _as_float(_pick(anchor, ("y", "Y"), MISSING))
        z = _as_float(_pick(anchor, ("z", "Z"), 0.0))
        if x is None or y is None or z is None:
            return None
        return x, y, z
    if isinstance(anchor, (list, tuple)) and len(anchor) in (2, 3):
        x = _as_float(anchor[0])
        y = _as_float(anchor[1])
        z = _as_float(anchor[2]) if len(anchor) == 3 else 0.0
        if x is None or y is None or z is None:
            return None
        return x, y, z
    return None


def _stage_bounds(raw: dict[str, Any]) -> dict[str, tuple[float, float]]:
    defaults = {"x": (-5.0, 5.0), "y": (0.0, 10.0), "z": (0.0, 5.0)}
    stage = _pick(raw, ("stage", "stage_coordinates"), {})
    if not isinstance(stage, dict):
        return defaults
    bounds = _pick(stage, ("bounds", "limits", "ranges"), stage)
    if not isinstance(bounds, dict):
        return defaults
    result = dict(defaults)
    for axis in ("x", "y", "z"):
        pair: Any = MISSING
        if axis in bounds and isinstance(bounds[axis], (list, tuple)) and len(bounds[axis]) == 2:
            pair = bounds[axis]
        elif f"{axis}_range" in bounds:
            pair = bounds[f"{axis}_range"]
        elif f"{axis}_min" in bounds and f"{axis}_max" in bounds:
            pair = (bounds[f"{axis}_min"], bounds[f"{axis}_max"])
        if pair is not MISSING:
            lo = _as_float(pair[0])
            hi = _as_float(pair[1])
            if lo is not None and hi is not None and lo <= hi:
                result[axis] = (lo, hi)
    return result


def _listener(raw: dict[str, Any]) -> tuple[float, float, float]:
    value = _pick(raw, ("listener", "listener_position", "audience_position"), None)
    parsed = _coord(value) if value is not None else None
    return parsed or (0.0, -0.5, 1.7)


def _source_duration(channel: dict[str, Any], default: float) -> float:
    value = _as_float(_pick(channel, ("duration_seconds", "source_duration_seconds"), default))
    if value is None or value <= 0:
        return default
    return value


def _path_report(path: str, base_dir: Path) -> str:
    candidate = Path(path)
    if candidate.is_absolute():
        try:
            candidate = candidate.resolve().relative_to(base_dir.resolve())
        except ValueError:
            candidate = Path(candidate.name)
    return candidate.as_posix()


def _digest(raw: dict[str, Any]) -> str:
    canonical = json.dumps(
        raw,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
        allow_nan=False,
    )
    return hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def _parse_groups(
    raw: dict[str, Any],
    channels: list[dict[str, Any]],
    collector: IssueCollector,
) -> tuple[dict[str, float], dict[str, dict[str, float]]]:
    gains = {key: 0.0 for key in GROUP_DISPLAY}
    weights: dict[str, dict[str, float]] = {key: {} for key in GROUP_DISPLAY}

    groups = _pick(raw, ("groups", "group_gains"), {})
    if isinstance(groups, dict):
        for raw_name, value in groups.items():
            group = canonical_group(raw_name)
            if group is None:
                collector.add(
                    "E_GROUP",
                    "MASTER",
                    f"unknown group name: {raw_name}",
                    details={"group": str(raw_name)},
                )
                continue
            if isinstance(value, (int, float)) and not isinstance(value, bool):
                gains[group] = float(value)
                continue
            if not isinstance(value, dict):
                collector.add("E_GROUP", "MASTER", f"invalid group definition: {raw_name}")
                continue
            gain = _pick(value, ("gain_db", "db", "gain"), 0.0)
            parsed_gain = _as_float(gain)
            if parsed_gain is None:
                collector.add("E_GROUP", "MASTER", f"invalid gain for group: {raw_name}")
            else:
                gains[group] = parsed_gain
            group_weights = _pick(value, ("weights", "downmix_weights"), {})
            if isinstance(group_weights, dict):
                for channel_id, weight in group_weights.items():
                    parsed_weight = _as_float(weight)
                    if parsed_weight is None or parsed_weight < 0:
                        collector.add(
                            "E_GROUP",
                            str(channel_id),
                            f"invalid downmix weight for group {raw_name}",
                        )
                    else:
                        weights[group][str(channel_id)] = parsed_weight

    group_gains = _pick(raw, ("group_gains_db",), {})
    if isinstance(group_gains, dict):
        for raw_name, value in group_gains.items():
            group = canonical_group(raw_name)
            parsed = _as_float(value)
            if group is None:
                collector.add("E_GROUP", "MASTER", f"unknown group name: {raw_name}")
            elif parsed is None:
                collector.add("E_GROUP", "MASTER", f"invalid gain for group: {raw_name}")
            else:
                gains[group] = parsed
    return gains, weights


def load_manifest(path: Path, *, check_audio: bool = True) -> Manifest:
    collector = IssueCollector()
    try:
        raw_bytes = path.read_bytes()
    except OSError as exc:
        collector.add("E_MISSING_FILE", "MASTER", f"cannot read manifest: {exc}")
        collector.raise_if_any()
    try:
        raw = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        collector.add("E_MANIFEST_JSON", "MASTER", f"invalid manifest JSON: {exc}")
        collector.raise_if_any()
    if not isinstance(raw, dict):
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "manifest root must be an object")
        collector.raise_if_any()

    digest = _digest(raw)
    base_dir = path.resolve().parent

    seed = _as_int(_pick(raw, ("seed", "random_seed"), MISSING))
    if seed is None:
        collector.add("E_SEED", "MASTER", "manifest seed must be an integer")

    sample_rate = _as_int(_pick(raw, ("sample_rate", "sample_rate_hz", "fs"), 48000))
    if sample_rate is None or sample_rate < 8000:
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "invalid manifest sample rate")
        sample_rate = 48000

    bit_depth = _as_int(_pick(raw, ("bit_depth", "bits_per_sample"), 24))
    if bit_depth not in (16, 24, 32):
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "manifest bit depth must be 16, 24, or 32")
        bit_depth = 24

    num_channels = _as_int(_pick(raw, ("num_channels", "channel_count"), 1))
    if num_channels not in (1, 2):
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "manifest channel count must be 1 or 2")
        num_channels = 1

    target = _as_float(
        _pick(
            raw,
            (
                "target_loudness_lufs",
                "target_loudness_lu",
                "target_lufs",
                "target_loudness",
                "target_loudness_lkfs",
            ),
            MISSING,
        )
    )
    if target is None or target < -70.0 or target > 0.0:
        collector.add("E_TARGET_LOUDNESS", "MASTER", "target master loudness is missing or invalid")

    peak_limit = _as_float(
        _pick(raw, ("true_peak_limit_dbtp", "true_peak_dbtp", "max_true_peak_dbtp"), -1.0)
    )
    if peak_limit is None or peak_limit > 0.0 or peak_limit < -30.0:
        collector.add("E_TRUE_PEAK_LIMIT", "MASTER", "true peak limit is invalid")
        peak_limit = -1.0

    stability = _as_float(
        _pick(raw, ("spatial_stability_threshold_db", "ild_stability_threshold_db"), 0.01)
    )
    if stability is None or stability <= 0.0 or stability > 24.0:
        collector.add(
            "E_MANIFEST_SCHEMA",
            "MASTER",
            "spatial stability threshold must be a positive decibel value",
        )
        stability = 0.01

    block_samples = _as_int(
        _pick(raw, ("block_samples", "render_block_samples", "snapshot_block_samples"), 2048)
    )
    if block_samples is None or block_samples <= 0:
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "render block size must be a positive integer")
        block_samples = 2048

    source_duration = _as_float(
        _pick(raw, ("source_duration_seconds", "demo_duration_seconds"), 8.0)
    )
    if source_duration is None or source_duration <= 0 or source_duration > 300:
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "source duration is invalid")
        source_duration = 8.0

    stage_bounds = _stage_bounds(raw)
    listener = _listener(raw)
    group_gains, group_weights = _parse_groups(raw, [], collector)

    raw_channels = _pick(raw, ("channels", "tracks", "sources"), MISSING)
    channel_specs: list[ChannelSpec] = []
    seen_instruments: dict[str, str] = {}
    seen_ids: set[str] = set()
    if not isinstance(raw_channels, list) or not raw_channels:
        collector.add("E_MANIFEST_SCHEMA", "MASTER", "manifest channels must be a non-empty list")
        raw_channels = []

    for index, channel in enumerate(raw_channels):
        if not isinstance(channel, dict):
            collector.add(
                "E_MANIFEST_SCHEMA",
                f"channel_{index}",
                "channel entry must be an object",
            )
            continue

        channel_id_raw = _pick(channel, ("id", "name", "channel_id"), f"channel_{index}")
        channel_id = str(channel_id_raw).strip() or f"channel_{index}"
        instrument_raw = _pick(channel, ("instrument", "instrument_name", "role"), MISSING)
        instrument_key = canonical_instrument(instrument_raw)
        instrument = str(instrument_raw) if instrument_raw is not MISSING else channel_id
        if instrument_key is None:
            collector.add(
                "E_INSTRUMENT_SET",
                channel_id,
                f"unknown instrument: {instrument_raw}",
                instrument=instrument,
            )
            continue

        if channel_id in seen_ids:
            collector.add("E_CHANNEL_ID", channel_id, "duplicate channel id", instrument=instrument)
        seen_ids.add(channel_id)

        if instrument_key in seen_instruments:
            collector.add(
                "E_DUPLICATE_INSTRUMENT",
                channel_id,
                f"duplicate instrument: {instrument}",
                instrument=instrument,
                details={"first_channel": seen_instruments[instrument_key]},
            )
        else:
            seen_instruments[instrument_key] = channel_id

        path_value = _pick(channel, ("path", "file", "wav_path", "file_path"), MISSING)
        if path_value is MISSING or not str(path_value).strip():
            collector.add(
                "E_MANIFEST_SCHEMA",
                channel_id,
                "channel file path is missing",
                instrument=instrument,
            )
            continue
        path_text = str(path_value)
        resolved_path = Path(path_text)
        if not resolved_path.is_absolute():
            resolved_path = base_dir / resolved_path

        anchor = _coord(_pick(channel, ("anchor", "position", "source_anchor"), MISSING))
        if anchor is None:
            collector.add(
                "E_ANCHOR_MISSING",
                channel_id,
                "channel anchor is missing or invalid",
                instrument=instrument,
            )
            anchor = (0.0, 0.0, 0.0)
        else:
            for axis, value in zip(("x", "y", "z"), anchor):
                lo, hi = stage_bounds[axis]
                if value < lo or value > hi:
                    collector.add(
                        "E_ANCHOR_RANGE",
                        channel_id,
                        f"anchor {axis}={value} is outside [{lo}, {hi}]",
                        instrument=instrument,
                        details={"axis": axis, "minimum": lo, "maximum": hi, "value": value},
                    )
            dx = anchor[0] - listener[0]
            dy = anchor[1] - listener[1]
            if math.hypot(dx, dy) < 1e-3:
                collector.add(
                    "E_ANCHOR_RANGE",
                    channel_id,
                    "anchor coincides with listener horizontal position",
                    instrument=instrument,
                )

        channel_sr = _as_int(
            _pick(channel, ("sample_rate", "sample_rate_hz", "expected_sample_rate"), sample_rate)
        )
        if channel_sr is None or channel_sr < 8000:
            collector.add("E_SAMPLE_RATE", channel_id, "channel sample rate is invalid", instrument=instrument)
            channel_sr = sample_rate
        channel_bits = _as_int(
            _pick(channel, ("bit_depth", "bits_per_sample", "expected_bit_depth"), bit_depth)
        )
        if channel_bits not in (16, 24, 32):
            collector.add("E_BIT_DEPTH", channel_id, "channel bit depth is invalid", instrument=instrument)
            channel_bits = bit_depth
        channel_count = _as_int(
            _pick(channel, ("num_channels", "channel_count", "expected_channels"), num_channels)
        )
        if channel_count not in (1, 2):
            collector.add("E_CHANNELS", channel_id, "channel count is invalid", instrument=instrument)
            channel_count = num_channels

        start_samples = _as_int(
            _pick(channel, ("start_offset_samples", "offset_samples", "start_sample"), MISSING)
        )
        if start_samples is None:
            offset_ms = _as_float(
                _pick(channel, ("start_offset_ms", "offset_ms", "start_ms"), MISSING)
            )
            if offset_ms is not None:
                start_samples = int(round(offset_ms * channel_sr / 1000.0))
        if start_samples is None or start_samples < 0:
            collector.add(
                "E_OFFSET",
                channel_id,
                "start offset must be a non-negative integer sample count or millisecond value",
                instrument=instrument,
            )
            start_samples = 0

        end_samples = _as_int(
            _pick(channel, ("end_offset_samples", "end_sample"), MISSING)
        )
        if end_samples is None:
            end_ms = _as_float(_pick(channel, ("end_offset_ms", "end_ms"), MISSING))
            if end_ms is not None:
                end_samples = int(round(end_ms * channel_sr / 1000.0))
        if end_samples is not None and end_samples <= start_samples:
            collector.add(
                "E_OFFSET",
                channel_id,
                "end offset must be greater than start offset",
                instrument=instrument,
            )
            end_samples = None

        group_value = _pick(channel, ("group", "bus"), None)
        if group_value is None:
            group = INSTRUMENT_GROUP[instrument_key]
        else:
            group = canonical_group(group_value)
            if group is None:
                collector.add(
                    "E_GROUP",
                    channel_id,
                    f"unknown group: {group_value}",
                    instrument=instrument,
                )
                group = INSTRUMENT_GROUP[instrument_key]

        weight = _as_float(
            _pick(channel, ("downmix_weight", "weight", "mix_weight"), 1.0)
        )
        if weight is None or weight < 0:
            collector.add(
                "E_GROUP",
                channel_id,
                "downmix weight must be non-negative",
                instrument=instrument,
            )
            weight = 1.0
        weight = group_weights.get(group, {}).get(channel_id, weight)

        channel_specs.append(
            ChannelSpec(
                channel_id=channel_id,
                instrument=instrument,
                instrument_key=instrument_key,
                group=group,
                path=str(resolved_path),
                path_report=_path_report(path_text, base_dir),
                anchor=anchor,
                start_offset_samples=start_samples,
                end_offset_samples=end_samples,
                expected_sample_rate=channel_sr,
                expected_bit_depth=channel_bits,
                expected_channels=channel_count,
                downmix_weight=weight,
                source_duration_seconds=_source_duration(channel, source_duration),
            )
        )

    missing_instruments = [key for key in REQUIRED_INSTRUMENTS if key not in seen_instruments]
    if missing_instruments:
        collector.add(
            "E_INSTRUMENT_SET",
            "MASTER",
            "manifest must contain exactly the six festival instruments",
            details={"missing": [INSTRUMENT_DISPLAY[key] for key in missing_instruments]},
        )
    extra_keys = set(seen_instruments) - set(REQUIRED_INSTRUMENTS)
    if extra_keys:
        collector.add(
            "E_INSTRUMENT_SET",
            "MASTER",
            "manifest contains unsupported instruments",
            details={"unsupported": sorted(extra_keys)},
        )

    collector.raise_if_any()

    channels = tuple(channel_specs)
    for spec in channels:
        if not check_audio:
            continue
        if not Path(spec.path).exists():
            collector.add(
                "E_MISSING_FILE",
                spec.channel_id,
                f"channel file is missing: {spec.path_report}",
                instrument=spec.instrument,
                details={"path": spec.path_report},
            )
            continue
        try:
            info, _ = inspect_wav(Path(spec.path))
        except (OSError, ValueError) as exc:
            collector.add(
                "E_AUDIO_READ",
                spec.channel_id,
                f"cannot read channel WAV: {exc}",
                instrument=spec.instrument,
                details={"path": spec.path_report},
            )
            continue
        if info.sample_rate != spec.expected_sample_rate:
            collector.add(
                "E_SAMPLE_RATE",
                spec.channel_id,
                f"sample rate {info.sample_rate} does not match manifest {spec.expected_sample_rate}",
                instrument=spec.instrument,
                details={"expected": spec.expected_sample_rate, "actual": info.sample_rate},
            )
        if info.bit_depth != spec.expected_bit_depth:
            collector.add(
                "E_BIT_DEPTH",
                spec.channel_id,
                f"bit depth {info.bit_depth} does not match manifest {spec.expected_bit_depth}",
                instrument=spec.instrument,
                details={"expected": spec.expected_bit_depth, "actual": info.bit_depth},
            )
        if info.channels != spec.expected_channels:
            collector.add(
                "E_CHANNELS",
                spec.channel_id,
                f"channel count {info.channels} does not match manifest {spec.expected_channels}",
                instrument=spec.instrument,
                details={"expected": spec.expected_channels, "actual": info.channels},
            )
        if info.frames <= 0:
            collector.add(
                "E_AUDIO_READ",
                spec.channel_id,
                "channel WAV contains no samples",
                instrument=spec.instrument,
            )
    collector.raise_if_any()

    return Manifest(
        raw=raw,
        digest=digest,
        base_dir=base_dir,
        seed=int(seed),
        sample_rate=int(sample_rate),
        bit_depth=int(bit_depth),
        num_channels=int(num_channels),
        stage_bounds=stage_bounds,
        listener=listener,
        target_loudness_lufs=float(target),
        true_peak_limit_dbtp=float(peak_limit),
        spatial_stability_threshold_db=float(stability),
        block_samples=int(block_samples),
        source_duration_seconds=float(source_duration),
        group_gains_db=group_gains,
        group_weights=group_weights,
        channels=channels,
    )


def safe_relative_path(path: Path, base_dir: Path) -> str:
    try:
        return os.path.relpath(path, base_dir).replace(os.sep, "/")
    except ValueError:
        return path.name
