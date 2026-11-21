from __future__ import annotations

import json
import math
import re
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .config import Config
from .errors import E_META_INVALID, AppError
from .loudness import round_db, validate_downmix_weights
from .util import canonical_json_bytes, deep_copy_json, hash_without
from .wav import DEFAULT_LAYOUTS, WavInfo

ASSET_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
CHECKSUM_ALGORITHM = "sha256(canonical-json:v1, excluding archive_checksum)"


@dataclass(frozen=True)
class SegmentDeclaration:
    index: int
    start_sample: int | None
    end_sample: int | None
    short_term_lufs: float
    true_peak_dbtp: float
    true_peak_limit_dbtp: float
    over_limit: bool
    excess_db: float | None

    def to_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "short_term_lufs": self.short_term_lufs,
            "true_peak_dbtp": self.true_peak_dbtp,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
            "over_limit": self.over_limit,
            "excess_db": self.excess_db,
        }


@dataclass(frozen=True)
class MetadataDocument:
    version: int
    asset_id: str
    sample_rate: int
    duration_samples: int
    integrated_lufs: float
    target_offset_lu: float
    segment_count: int
    true_peak_limit_dbtp: float | None
    channel_layout: str | None
    channel_mask: int | None
    downmix_weights: tuple[tuple[str, float], ...] | None
    segments: tuple[SegmentDeclaration, ...]
    archive_checksum: str | None
    quantization_tolerance_db: float | None
    boundary_source: str
    raw: dict[str, Any]

    def limits(self) -> tuple[float, ...]:
        if not self.segments:
            return ()
        if self.version == 3:
            return tuple(segment.true_peak_limit_dbtp for segment in self.segments)
        assert self.true_peak_limit_dbtp is not None
        return tuple(self.true_peak_limit_dbtp for _ in self.segments)

    def declared_segments(self) -> tuple[SegmentDeclaration, ...]:
        return self.segments

    def summary(self) -> dict[str, object]:
        return {
            "version": self.version,
            "asset_id": self.asset_id,
            "sample_rate": self.sample_rate,
            "duration_samples": self.duration_samples,
            "segment_count": self.segment_count,
            "channel_layout": self.channel_layout,
            "channel_mask": (
                f"0x{self.channel_mask:04X}" if self.channel_mask is not None else None
            ),
            "boundary_source": self.boundary_source,
            "archive_checksum": self.archive_checksum,
            "quantization_tolerance_db": self.quantization_tolerance_db,
            "integrated_lufs": self.integrated_lufs,
            "target_offset_lu": self.target_offset_lu,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
        }


@dataclass(frozen=True)
class DerivedBoundaries:
    source: str
    rule: str
    segments: tuple[tuple[int, int], ...]
    parameters: dict[str, object]
    details: tuple[dict[str, object], ...]


@dataclass(frozen=True)
class BoundaryDiff:
    old_segments: tuple[tuple[int, int], ...]
    new_segments: tuple[tuple[int, int], ...]
    changed_indices: tuple[int, ...]
    affected_segment_count: int
    details: tuple[dict[str, object], ...]

    def to_dict(self) -> dict[str, object]:
        return {
            "old_segments": [
                {"index": index, "start_sample": start, "end_sample": end}
                for index, (start, end) in enumerate(self.old_segments)
            ],
            "new_segments": [
                {"index": index, "start_sample": start, "end_sample": end}
                for index, (start, end) in enumerate(self.new_segments)
            ],
            "changed_indices": list(self.changed_indices),
            "affected_segment_count": self.affected_segment_count,
            "details": list(self.details),
        }


def _invalid(field: str, message: str) -> AppError:
    return AppError(E_META_INVALID, message, field=field)


def _normalize_version(value: Any) -> int:
    if isinstance(value, str):
        normalized = value.lower()
        if normalized in {"v1", "1"}:
            return 1
        if normalized in {"v2", "2"}:
            return 2
        if normalized in {"v3", "3"}:
            return 3
    if isinstance(value, int) and not isinstance(value, bool) and value in (1, 2, 3):
        return value
    raise _invalid("version", f"unknown metadata version: {value!r}")


def _require_mapping(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise _invalid(field, "must be a JSON object")
    return value


def _require_list(value: Any, field: str) -> list[Any]:
    if not isinstance(value, list):
        raise _invalid(field, "must be a JSON array")
    return value


def _require_field(mapping: dict[str, Any], key: str, field: str | None = None) -> Any:
    if key not in mapping:
        raise _invalid(field or key, "required field is missing")
    return mapping[key]


def _require_int(value: Any, field: str, minimum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise _invalid(field, "must be an integer")
    if minimum is not None and value < minimum:
        raise _invalid(field, f"must be >= {minimum}")
    return value


def _require_bool(value: Any, field: str) -> bool:
    if not isinstance(value, bool):
        raise _invalid(field, "must be a boolean")
    return value


def _require_number(value: Any, field: str, minimum: float | None = None) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise _invalid(field, "must be a finite number")
    result = float(value)
    if not math.isfinite(result):
        raise _invalid(field, "must be a finite number")
    if minimum is not None and result < minimum:
        raise _invalid(field, f"must be >= {minimum}")
    return result


def _require_string(value: Any, field: str) -> str:
    if not isinstance(value, str) or not value:
        raise _invalid(field, "must be a non-empty string")
    return value


def _parse_common(raw: dict[str, Any]) -> tuple[str, int, int, float, float]:
    asset_id = _require_string(_require_field(raw, "asset_id"), "asset_id")
    if not ASSET_ID_PATTERN.fullmatch(asset_id):
        raise _invalid(
            "asset_id",
            "must contain only letters, digits, dot, underscore or hyphen",
        )
    sample_rate = _require_int(_require_field(raw, "sample_rate"), "sample_rate", 1)
    duration_samples = _require_int(
        _require_field(raw, "duration_samples"),
        "duration_samples",
        1,
    )
    integrated = _require_number(
        _require_field(raw, "integrated_lufs"),
        "integrated_lufs",
    )
    target_offset = _require_number(
        _require_field(raw, "target_offset_lu"),
        "target_offset_lu",
    )
    return asset_id, sample_rate, duration_samples, integrated, target_offset


def _parse_v2_segments(
    raw: dict[str, Any],
    count: int,
    duration_samples: int,
    true_peak_limit: float,
) -> tuple[SegmentDeclaration, ...]:
    rows = _require_list(_require_field(raw, "segments"), "segments")
    if len(rows) != count:
        raise _invalid(
            "segments",
            f"segment array length {len(rows)} does not match segment_count {count}",
        )
    if count > duration_samples:
        raise _invalid("segment_count", "cannot exceed duration_samples")
    result: list[SegmentDeclaration] = []
    for position, item in enumerate(rows):
        base = f"segments[{position}]"
        row = _require_mapping(item, base)
        index = _require_int(_require_field(row, "index", f"{base}.index"), f"{base}.index", 0)
        if index != position:
            raise _invalid(f"{base}.index", f"expected index {position}, got {index}")
        short_term = _require_number(
            _require_field(row, "short_term_lufs", f"{base}.short_term_lufs"),
            f"{base}.short_term_lufs",
        )
        true_peak = _require_number(
            _require_field(row, "true_peak_dbtp", f"{base}.true_peak_dbtp"),
            f"{base}.true_peak_dbtp",
        )
        over_limit = _require_bool(
            _require_field(row, "over_limit", f"{base}.over_limit"),
            f"{base}.over_limit",
        )
        result.append(
            SegmentDeclaration(
                index=index,
                start_sample=None,
                end_sample=None,
                short_term_lufs=short_term,
                true_peak_dbtp=true_peak,
                true_peak_limit_dbtp=true_peak_limit,
                over_limit=over_limit,
                excess_db=max(0.0, true_peak - true_peak_limit),
            )
        )
    return tuple(result)


def _parse_v3_segments(
    raw: dict[str, Any],
    duration_samples: int,
) -> tuple[SegmentDeclaration, ...]:
    rows = _require_list(_require_field(raw, "segments"), "segments")
    if not rows:
        raise _invalid("segments", "must contain at least one segment")
    result: list[SegmentDeclaration] = []
    expected_start = 0
    for position, item in enumerate(rows):
        base = f"segments[{position}]"
        row = _require_mapping(item, base)
        index = _require_int(_require_field(row, "index", f"{base}.index"), f"{base}.index", 0)
        if index != position:
            raise _invalid(f"{base}.index", f"expected index {position}, got {index}")
        start = _require_int(
            _require_field(row, "start_sample", f"{base}.start_sample"),
            f"{base}.start_sample",
            0,
        )
        end = _require_int(
            _require_field(row, "end_sample", f"{base}.end_sample"),
            f"{base}.end_sample",
            1,
        )
        if start != expected_start:
            raise _invalid(
                f"{base}.start_sample",
                f"expected contiguous start {expected_start}, got {start}",
            )
        if end <= start:
            raise _invalid(f"{base}.end_sample", "must be greater than start_sample")
        if end > duration_samples:
            raise _invalid(f"{base}.end_sample", "exceeds duration_samples")
        short_term = _require_number(
            _require_field(row, "short_term_lufs", f"{base}.short_term_lufs"),
            f"{base}.short_term_lufs",
        )
        true_peak = _require_number(
            _require_field(row, "true_peak_dbtp", f"{base}.true_peak_dbtp"),
            f"{base}.true_peak_dbtp",
        )
        limit = _require_number(
            _require_field(row, "true_peak_limit_dbtp", f"{base}.true_peak_limit_dbtp"),
            f"{base}.true_peak_limit_dbtp",
        )
        over_limit = _require_bool(
            _require_field(row, "over_limit", f"{base}.over_limit"),
            f"{base}.over_limit",
        )
        excess = _require_number(
            _require_field(row, "excess_db", f"{base}.excess_db"),
            f"{base}.excess_db",
            0.0,
        )
        result.append(
            SegmentDeclaration(
                index=index,
                start_sample=start,
                end_sample=end,
                short_term_lufs=short_term,
                true_peak_dbtp=true_peak,
                true_peak_limit_dbtp=limit,
                over_limit=over_limit,
                excess_db=excess,
            )
        )
        expected_start = end
    if expected_start != duration_samples:
        raise _invalid(
            f"segments[{len(rows) - 1}].end_sample",
            f"last boundary {expected_start} does not equal duration_samples {duration_samples}",
        )
    return tuple(result)


def _validate_against_wav(
    raw: dict[str, Any],
    version: int,
    sample_rate: int,
    duration_samples: int,
    channel_layout: str | None,
    channel_mask: int | None,
    wav: WavInfo | None,
) -> None:
    if sample_rate != 48000:
        raise _invalid("sample_rate", "metadata sample rate must be 48000")
    if wav is None:
        return
    if sample_rate != wav.sample_rate:
        raise _invalid("sample_rate", f"metadata {sample_rate} != WAV {wav.sample_rate}")
    if duration_samples != wav.frames:
        raise _invalid(
            "duration_samples",
            f"metadata {duration_samples} != WAV frames {wav.frames}",
        )
    if version >= 3:
        if channel_layout != wav.channel_layout:
            raise _invalid(
                "channel_layout",
                f"metadata {channel_layout!r} != WAV header {wav.channel_layout!r}",
            )
        if channel_mask != wav.channel_mask:
            raise _invalid(
                "channel_mask",
                f"metadata 0x{channel_mask:04X} != WAV header 0x{wav.channel_mask:04X}",
            )


def parse_metadata(raw: Any, wav: WavInfo | None = None) -> MetadataDocument:
    document = _require_mapping(raw, "$")
    version = _normalize_version(_require_field(document, "version"))
    asset_id, sample_rate, duration_samples, integrated, target_offset = _parse_common(document)

    if version == 1:
        _validate_against_wav(
            document,
            version,
            sample_rate,
            duration_samples,
            None,
            None,
            wav,
        )
        return MetadataDocument(
            version=1,
            asset_id=asset_id,
            sample_rate=sample_rate,
            duration_samples=duration_samples,
            integrated_lufs=integrated,
            target_offset_lu=target_offset,
            segment_count=0,
            true_peak_limit_dbtp=None,
            channel_layout=None,
            channel_mask=None,
            downmix_weights=None,
            segments=(),
            archive_checksum=None,
            quantization_tolerance_db=None,
            boundary_source="derived_fixed_window:v1",
            raw=deep_copy_json(document),
        )

    if version == 2:
        segment_count = _require_int(
            _require_field(document, "segment_count"),
            "segment_count",
            1,
        )
        limit = _require_number(
            _require_field(document, "true_peak_limit_dbtp"),
            "true_peak_limit_dbtp",
        )
        segments = _parse_v2_segments(document, segment_count, duration_samples, limit)
        _validate_against_wav(
            document,
            version,
            sample_rate,
            duration_samples,
            None,
            None,
            wav,
        )
        return MetadataDocument(
            version=2,
            asset_id=asset_id,
            sample_rate=sample_rate,
            duration_samples=duration_samples,
            integrated_lufs=integrated,
            target_offset_lu=target_offset,
            segment_count=segment_count,
            true_peak_limit_dbtp=limit,
            channel_layout=None,
            channel_mask=None,
            downmix_weights=None,
            segments=segments,
            archive_checksum=None,
            quantization_tolerance_db=None,
            boundary_source="derived_equal_count:v2",
            raw=deep_copy_json(document),
        )

    layout = _require_string(_require_field(document, "channel_layout"), "channel_layout")
    channel_mask = _require_int(
        _require_field(document, "channel_mask"),
        "channel_mask",
        1,
    )
    if wav is not None:
        expected_layout = wav.channel_layout
    else:
        expected_layout = layout
    if layout != expected_layout:
        raise _invalid(
            "channel_layout",
            f"metadata {layout!r} does not match expected {expected_layout!r}",
        )
    if wav is not None and channel_mask != wav.channel_mask:
        raise _invalid(
            "channel_mask",
            f"metadata 0x{channel_mask:04X} != WAV header 0x{wav.channel_mask:04X}",
        )

    weights_raw = _require_list(
        _require_field(document, "downmix_weights"),
        "downmix_weights",
    )
    weights: list[tuple[str, float]] = []
    for position, item in enumerate(weights_raw):
        base = f"downmix_weights[{position}]"
        row = _require_mapping(item, base)
        name = _require_string(_require_field(row, "channel", f"{base}.channel"), f"{base}.channel")
        weight = _require_number(
            _require_field(row, "weight", f"{base}.weight"),
            f"{base}.weight",
            0.0,
        )
        weights.append((name, weight))
    if wav is not None:
        channel_names = wav.channel_names
    elif layout in DEFAULT_LAYOUTS:
        channel_names = DEFAULT_LAYOUTS[layout][1]
    else:
        raise _invalid("channel_layout", f"unknown channel layout {layout!r}")
    normalized_weights = validate_downmix_weights(weights, channel_names)

    limit = _require_number(
        _require_field(document, "true_peak_limit_dbtp"),
        "true_peak_limit_dbtp",
    )
    tolerance = _require_number(
        _require_field(document, "quantization_tolerance_db"),
        "quantization_tolerance_db",
        0.0,
    )
    segments = _parse_v3_segments(document, duration_samples)
    _validate_against_wav(
        document,
        version,
        sample_rate,
        duration_samples,
        layout,
        channel_mask,
        wav,
    )
    checksum = _require_string(
        _require_field(document, "archive_checksum"),
        "archive_checksum",
    )
    expected_checksum = hash_without(document, "archive_checksum")
    if checksum != expected_checksum:
        raise _invalid(
            "archive_checksum",
            f"checksum mismatch: expected {expected_checksum}, got {checksum}",
        )

    return MetadataDocument(
        version=3,
        asset_id=asset_id,
        sample_rate=sample_rate,
        duration_samples=duration_samples,
        integrated_lufs=integrated,
        target_offset_lu=target_offset,
        segment_count=len(segments),
        true_peak_limit_dbtp=limit,
        channel_layout=layout,
        channel_mask=channel_mask,
        downmix_weights=normalized_weights,
        segments=segments,
        archive_checksum=checksum,
        quantization_tolerance_db=tolerance,
        boundary_source="metadata.explicit_boundaries",
        raw=deep_copy_json(document),
    )


def load_metadata(path: str | Path, wav: WavInfo | None = None) -> MetadataDocument:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except FileNotFoundError as exc:
        raise _invalid("path", f"metadata file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise _invalid("$", f"invalid JSON at line {exc.lineno} column {exc.colno}") from exc
    except OSError as exc:
        raise _invalid("path", f"cannot read metadata file: {exc}") from exc
    return parse_metadata(raw, wav=wav)


def _equal_boundaries(duration_samples: int, count: int) -> tuple[tuple[int, int], ...]:
    if count > duration_samples:
        raise _invalid("segment_count", "segment count exceeds available sample frames")
    boundaries = []
    for index in range(count):
        start = (index * duration_samples) // count
        end = ((index + 1) * duration_samples) // count
        if end <= start:
            raise _invalid(
                "segment_count",
                f"derived segment {index} would be empty",
            )
        boundaries.append((start, end))
    return tuple(boundaries)


def derive_boundaries(document: MetadataDocument, config: Config) -> DerivedBoundaries:
    if document.version == 3:
        segments = tuple(
            (int(segment.start_sample), int(segment.end_sample))
            for segment in document.segments
        )
        return DerivedBoundaries(
            source="metadata.explicit_boundaries",
            rule="use v3 start_sample/end_sample exactly",
            segments=segments,
            parameters={"duration_samples": document.duration_samples},
            details=tuple(
                {
                    "index": index,
                    "start_sample": start,
                    "end_sample": end,
                    "decision": "declared",
                }
                for index, (start, end) in enumerate(segments)
            ),
        )
    if document.version == 2:
        segments = _equal_boundaries(document.duration_samples, document.segment_count)
        return DerivedBoundaries(
            source="derived_equal_count:v2",
            rule="start_i=floor(i*duration/count), end_i=floor((i+1)*duration/count)",
            segments=segments,
            parameters={
                "duration_samples": document.duration_samples,
                "segment_count": document.segment_count,
            },
            details=tuple(
                {
                    "index": index,
                    "start_sample": start,
                    "end_sample": end,
                    "decision": "derived",
                    "inputs": {
                        "duration_samples": document.duration_samples,
                        "segment_count": document.segment_count,
                    },
                }
                for index, (start, end) in enumerate(segments)
            ),
        )
    duration = document.duration_samples
    width = config.default_segment_samples
    segments = tuple(
        (start, min(start + width, duration))
        for start in range(0, duration, width)
    )
    return DerivedBoundaries(
        source="derived_fixed_window:v1",
        rule="start_i=i*default_segment_samples, end_i=min(start_i+default_segment_samples,duration)",
        segments=segments,
        parameters={
            "duration_samples": duration,
            "default_segment_samples": width,
        },
        details=tuple(
            {
                "index": index,
                "start_sample": start,
                "end_sample": end,
                "decision": "derived",
                "inputs": {
                    "duration_samples": duration,
                    "default_segment_samples": width,
                },
            }
            for index, (start, end) in enumerate(segments)
        ),
    )


def compare_boundaries(
    old_segments: Iterable[tuple[int, int]],
    new_segments: Iterable[tuple[int, int]],
) -> BoundaryDiff:
    old = tuple(old_segments)
    new = tuple(new_segments)
    count = max(len(old), len(new))
    changed: list[int] = []
    details: list[dict[str, object]] = []
    for index in range(count):
        before = old[index] if index < len(old) else None
        after = new[index] if index < len(new) else None
        if before != after:
            changed.append(index)
            details.append(
                {
                    "index": index,
                    "old": (
                        {"start_sample": before[0], "end_sample": before[1]}
                        if before is not None
                        else None
                    ),
                    "new": (
                        {"start_sample": after[0], "end_sample": after[1]}
                        if after is not None
                        else None
                    ),
                }
            )
    return BoundaryDiff(
        old_segments=old,
        new_segments=new,
        changed_indices=tuple(changed),
        affected_segment_count=len(changed),
        details=tuple(details),
    )


def build_v3_document(
    *,
    asset_id: str,
    sample_rate: int,
    duration_samples: int,
    integrated_lufs: float,
    target_offset_lu: float,
    channel_layout: str,
    channel_mask: int,
    downmix_weights: Iterable[tuple[str, float]],
    true_peak_limit_dbtp: float,
    quantization_tolerance_db: float,
    segments: Iterable[dict[str, Any]],
) -> dict[str, Any]:
    document: dict[str, Any] = {
        "version": 3,
        "asset_id": asset_id,
        "sample_rate": sample_rate,
        "duration_samples": duration_samples,
        "integrated_lufs": round_db(integrated_lufs),
        "target_offset_lu": round_db(target_offset_lu),
        "channel_layout": channel_layout,
        "channel_mask": channel_mask,
        "downmix_weights": [
            {"channel": name, "weight": weight} for name, weight in downmix_weights
        ],
        "true_peak_limit_dbtp": round_db(true_peak_limit_dbtp),
        "quantization_tolerance_db": round_db(quantization_tolerance_db),
        "segments": list(segments),
    }
    document["archive_checksum"] = hash_without(document, "archive_checksum")
    return document


def refresh_checksum(document: dict[str, Any]) -> dict[str, Any]:
    result = deep_copy_json(document)
    result["archive_checksum"] = hash_without(result, "archive_checksum")
    return result


def checksum_bytes(document: dict[str, Any]) -> bytes:
    return canonical_json_bytes(
        {key: value for key, value in document.items() if key != "archive_checksum"}
    )

