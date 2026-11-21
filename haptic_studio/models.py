from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from enum import StrEnum
from typing import Any

from .canonical import content_hash
from .errors import config_error, wave_error

MIN_FREQUENCY_HZ = 0.01
MAX_FREQUENCY_HZ = 10_000.0
MIN_SAMPLE_RATE = 64
MAX_SAMPLE_RATE = 384_000
MIN_BIT_DEPTH = 2
MAX_BIT_DEPTH = 24


class Envelope(StrEnum):
    RECTANGULAR = "rectangular"
    LINEAR_DECAY = "linear_decay"
    SINE = "sine"

    @classmethod
    def parse(cls, value: Any, *, segment_index: int) -> Envelope:
        aliases = {
            "rect": cls.RECTANGULAR,
            "rectangle": cls.RECTANGULAR,
            "rectangular": cls.RECTANGULAR,
            "矩形": cls.RECTANGULAR,
            "linear": cls.LINEAR_DECAY,
            "linear_decay": cls.LINEAR_DECAY,
            "decay": cls.LINEAR_DECAY,
            "线性衰减": cls.LINEAR_DECAY,
            "sine": cls.SINE,
            "sin": cls.SINE,
            "正弦": cls.SINE,
        }
        if not isinstance(value, str) or value.strip().lower() not in aliases:
            raise wave_error(
                f"unsupported envelope {value!r}",
                segment_index=segment_index,
                field_name="envelope",
            )
        return aliases[value.strip().lower()]


def _number(
    mapping: Mapping[str, Any],
    key: str,
    *,
    segment_index: int,
    default: float | None = None,
) -> float:
    if key not in mapping:
        if default is not None:
            return default
        raise wave_error(
            f"missing required field {key}", segment_index=segment_index, field_name=key
        )
    value = mapping[key]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise wave_error(
            f"{key} must be a finite number",
            segment_index=segment_index,
            field_name=key,
        )
    result = float(value)
    if not math.isfinite(result):
        raise wave_error(
            f"{key} must be finite", segment_index=segment_index, field_name=key
        )
    return result


@dataclass(frozen=True, slots=True)
class WaveSegment:
    index: int
    start_ms: float
    duration_ms: float
    frequency_hz: float
    amplitude: float
    envelope: Envelope

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "start_ms": self.start_ms,
            "duration_ms": self.duration_ms,
            "frequency_hz": self.frequency_hz,
            "amplitude": self.amplitude,
            "envelope": self.envelope.value,
        }

    @property
    def content_hash(self) -> str:
        return content_hash(self.to_dict())

    @property
    def end_ms(self) -> float:
        return self.start_ms + self.duration_ms

    @classmethod
    def from_mapping(
        cls, mapping: Mapping[str, Any], *, segment_index: int
    ) -> WaveSegment:
        if not isinstance(mapping, Mapping):
            raise wave_error(
                "segment must be an object",
                segment_index=segment_index,
                field_name="segments",
            )
        start_ms = _number(mapping, "start_ms", segment_index=segment_index)
        duration_ms = _number(mapping, "duration_ms", segment_index=segment_index)
        frequency_hz = _number(mapping, "frequency_hz", segment_index=segment_index)
        amplitude = _number(mapping, "amplitude", segment_index=segment_index)
        if "envelope" not in mapping:
            raise wave_error(
                "missing required field envelope",
                segment_index=segment_index,
                field_name="envelope",
            )
        envelope = Envelope.parse(mapping["envelope"], segment_index=segment_index)
        return cls(
            index=segment_index,
            start_ms=start_ms,
            duration_ms=duration_ms,
            frequency_hz=frequency_hz,
            amplitude=amplitude,
            envelope=envelope,
        )


@dataclass(frozen=True, slots=True)
class WaveSequence:
    sequence_id: str
    total_duration_ms: float
    segments: tuple[WaveSegment, ...]
    schema_version: int = 1

    @property
    def sorted_segments(self) -> tuple[WaveSegment, ...]:
        return tuple(
            sorted(self.segments, key=lambda item: (item.start_ms, item.index))
        )

    @property
    def content_hash(self) -> str:
        return content_hash(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "sequence_id": self.sequence_id,
            "total_duration_ms": self.total_duration_ms,
            "segments": [segment.to_dict() for segment in self.segments],
        }

    @classmethod
    def from_mapping(cls, mapping: Mapping[str, Any]) -> WaveSequence:
        if not isinstance(mapping, Mapping):
            raise wave_error("wave description must be an object", field_name="root")
        if "segments" not in mapping:
            raise wave_error("missing required field segments", field_name="segments")
        raw_segments = mapping["segments"]
        if not isinstance(raw_segments, Sequence) or isinstance(
            raw_segments, (str, bytes, bytearray)
        ):
            raise wave_error("segments must be an array", field_name="segments")
        if "total_duration_ms" in mapping:
            total_duration_ms = _root_number(mapping, "total_duration_ms")
        elif "total_ms" in mapping:
            total_duration_ms = _root_number(mapping, "total_ms")
        else:
            raise wave_error(
                "missing required field total_duration_ms",
                field_name="total_duration_ms",
            )
        if total_duration_ms <= 0:
            raise wave_error(
                "total_duration_ms must be positive", field_name="total_duration_ms"
            )
        sequence_id = mapping.get("sequence_id", "sequence")
        if not isinstance(sequence_id, str) or not sequence_id:
            raise wave_error(
                "sequence_id must be a non-empty string", field_name="sequence_id"
            )
        schema_version = mapping.get("schema_version", 1)
        if not isinstance(schema_version, int) or isinstance(schema_version, bool):
            raise wave_error(
                "schema_version must be an integer", field_name="schema_version"
            )
        segments = tuple(
            WaveSegment.from_mapping(item, segment_index=index)
            for index, item in enumerate(raw_segments)
        )
        sequence = cls(
            sequence_id=sequence_id,
            total_duration_ms=total_duration_ms,
            segments=segments,
            schema_version=schema_version,
        )
        sequence.validate()
        return sequence

    def validate(self) -> None:
        for segment in self.segments:
            if segment.start_ms < 0:
                raise wave_error(
                    "start_ms must be non-negative",
                    segment_index=segment.index,
                    field_name="start_ms",
                )
            if segment.duration_ms < 0:
                raise wave_error(
                    "duration_ms must be non-negative",
                    segment_index=segment.index,
                    field_name="duration_ms",
                )
            if not (MIN_FREQUENCY_HZ <= segment.frequency_hz <= MAX_FREQUENCY_HZ):
                raise wave_error(
                    f"frequency_hz must be in [{MIN_FREQUENCY_HZ}, {MAX_FREQUENCY_HZ}]",
                    segment_index=segment.index,
                    field_name="frequency_hz",
                )
            if not (0.0 <= segment.amplitude <= 1.0):
                raise wave_error(
                    "amplitude must be in [0, 1]",
                    segment_index=segment.index,
                    field_name="amplitude",
                )
            if segment.end_ms > self.total_duration_ms + 1e-9:
                raise wave_error(
                    "segment interval exceeds total_duration_ms",
                    segment_index=segment.index,
                    field_name="duration_ms",
                )


def _root_number(mapping: Mapping[str, Any], key: str) -> float:
    if key not in mapping:
        raise wave_error(f"missing required field {key}", field_name=key)
    value = mapping[key]
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise wave_error(f"{key} must be a finite number", field_name=key)
    result = float(value)
    if not math.isfinite(result):
        raise wave_error(f"{key} must be finite", field_name=key)
    return result


@dataclass(frozen=True, slots=True)
class DeviceConfig:
    sample_rate: int = 1000
    bit_depth: int = 16
    left_ratio: float = 0.5
    right_ratio: float = 0.5
    left_polarity: int = 1
    right_polarity: int = 1
    limiter: str = "hard"

    @property
    def max_code(self) -> int:
        return (1 << (self.bit_depth - 1)) - 1

    @property
    def min_code(self) -> int:
        return -(1 << (self.bit_depth - 1))

    @property
    def content_hash(self) -> str:
        return content_hash(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_rate": self.sample_rate,
            "bit_depth": self.bit_depth,
            "allocation_ratio": {
                "left": self.left_ratio,
                "right": self.right_ratio,
            },
            "polarity": {
                "left": self.left_polarity,
                "right": self.right_polarity,
            },
            "limiter": self.limiter,
        }

    @classmethod
    def from_mapping(cls, mapping: Mapping[str, Any]) -> DeviceConfig:
        if not isinstance(mapping, Mapping):
            raise config_error("device config must be an object", field_name="root")
        sample_rate = _config_int(mapping, "sample_rate", 1000)
        bit_depth = _config_int(mapping, "bit_depth", 16)
        if not (MIN_SAMPLE_RATE <= sample_rate <= MAX_SAMPLE_RATE):
            raise config_error(
                f"sample_rate must be in [{MIN_SAMPLE_RATE}, {MAX_SAMPLE_RATE}]",
                field_name="sample_rate",
            )
        if not (MIN_BIT_DEPTH <= bit_depth <= MAX_BIT_DEPTH):
            raise config_error(
                f"bit_depth must be in [{MIN_BIT_DEPTH}, {MAX_BIT_DEPTH}]",
                field_name="bit_depth",
            )
        left_ratio, right_ratio = _parse_allocation(mapping)
        left_polarity, right_polarity = _parse_polarity(mapping)
        limiter = _parse_limiter(mapping)
        return cls(
            sample_rate=sample_rate,
            bit_depth=bit_depth,
            left_ratio=left_ratio,
            right_ratio=right_ratio,
            left_polarity=left_polarity,
            right_polarity=right_polarity,
            limiter=limiter,
        )


def _config_int(mapping: Mapping[str, Any], key: str, default: int) -> int:
    value = mapping.get(key, default)
    if isinstance(value, bool) or not isinstance(value, int):
        raise config_error(f"{key} must be an integer", field_name=key)
    return value


def _parse_allocation(mapping: Mapping[str, Any]) -> tuple[float, float]:
    value: Any
    if "allocation_ratio" in mapping:
        value = mapping["allocation_ratio"]
    elif "allocation" in mapping:
        value = mapping["allocation"]
    elif "left_ratio" in mapping or "right_ratio" in mapping:
        value = [mapping.get("left_ratio", 0.5), mapping.get("right_ratio", 0.5)]
    else:
        value = [0.5, 0.5]
    if isinstance(value, Mapping):
        raw_left = value.get("left")
        raw_right = value.get("right")
    elif isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
        if len(value) != 2:
            raise config_error(
                "allocation_ratio must contain two values",
                field_name="allocation_ratio",
            )
        raw_left, raw_right = value
    else:
        raise config_error(
            "allocation_ratio must be an array or object",
            field_name="allocation_ratio",
        )
    if (
        isinstance(raw_left, bool)
        or isinstance(raw_right, bool)
        or not isinstance(raw_left, (int, float))
        or not isinstance(raw_right, (int, float))
    ):
        raise config_error(
            "allocation_ratio values must be numbers",
            field_name="allocation_ratio",
        )
    left, right = float(raw_left), float(raw_right)
    if not (math.isfinite(left) and math.isfinite(right)):
        raise config_error(
            "allocation_ratio values must be finite", field_name="allocation_ratio"
        )
    if left < 0 or right < 0 or abs(left + right - 1.0) > 1e-6:
        raise config_error(
            "allocation_ratio must be non-negative and sum to 1",
            field_name="allocation_ratio",
        )
    return left, right


def _parse_polarity(mapping: Mapping[str, Any]) -> tuple[int, int]:
    value: Any
    if "polarity" in mapping:
        value = mapping["polarity"]
    elif "left_polarity" in mapping or "right_polarity" in mapping:
        value = [mapping.get("left_polarity", 1), mapping.get("right_polarity", 1)]
    else:
        value = [1, 1]
    if isinstance(value, Mapping):
        raw_left = value.get("left")
        raw_right = value.get("right")
    elif isinstance(value, Sequence) and not isinstance(value, (str, bytes)):
        if len(value) != 2:
            raise config_error(
                "polarity must contain two values", field_name="polarity"
            )
        raw_left, raw_right = value
    else:
        raise config_error("polarity must be an array or object", field_name="polarity")
    return _normalize_polarity(raw_left), _normalize_polarity(raw_right)


def _normalize_polarity(value: Any) -> int:
    if isinstance(value, bool):
        return 1 if value else -1
    if isinstance(value, str):
        normalized = value.strip().lower()
        if normalized in {"+", "+1", "positive", "normal", "1"}:
            return 1
        if normalized in {"-", "-1", "negative", "inverted", "-1.0"}:
            return -1
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if value == 1:
            return 1
        if value == -1:
            return -1
    raise config_error("polarity must be +1/-1", field_name="polarity")


def _parse_limiter(mapping: Mapping[str, Any]) -> str:
    value = mapping.get("limiter", mapping.get("limiter_strategy", "hard"))
    if isinstance(value, Mapping):
        value = value.get("mode", "hard")
    if not isinstance(value, str):
        raise config_error("limiter must be hard or soft", field_name="limiter")
    normalized = value.strip().lower()
    if normalized not in {"hard", "soft"}:
        raise config_error("limiter must be hard or soft", field_name="limiter")
    return normalized
