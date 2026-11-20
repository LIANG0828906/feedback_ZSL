from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import E_IO, E_WAVE_INVALID, HapticError
from .util import canonical_json_bytes, finite_number

ENVELOPES = ("rectangular", "linear_decay", "sine")
ENVELOPE_ALIASES = {
    "rect": "rectangular",
    "rectangular": "rectangular",
    "rectangle": "rectangular",
    "linear": "linear_decay",
    "linear_decay": "linear_decay",
    "decay": "linear_decay",
    "sine": "sine",
    "sin": "sine",
    "sinusoidal": "sine",
}


@dataclass(frozen=True)
class MotorMap:
    left_ratio: float = 0.8
    right_ratio: float = 0.8
    left_polarity: int = 1
    right_polarity: int = 1

    def to_dict(self) -> dict[str, Any]:
        return {
            "left_ratio": self.left_ratio,
            "right_ratio": self.right_ratio,
            "left_polarity": self.left_polarity,
            "right_polarity": self.right_polarity,
        }


@dataclass(frozen=True)
class ProjectConfig:
    sample_rate: int = 2000
    device_min: float = -1.0
    device_max: float = 1.0
    limiter: str = "hard"
    soft_knee_ratio: float = 0.8
    motor_map: MotorMap = MotorMap()
    chunk_size: int = 64

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_rate": self.sample_rate,
            "device": {
                "min": self.device_min,
                "max": self.device_max,
                "limiter": self.limiter,
                "soft_knee_ratio": self.soft_knee_ratio,
            },
            "motor_map": self.motor_map.to_dict(),
            "chunk_size": self.chunk_size,
        }

    @property
    def nyquist_hz(self) -> float:
        return self.sample_rate / 2.0


@dataclass(frozen=True)
class WaveSegment:
    index: int
    start_ms: float
    duration_ms: float
    frequency_hz: float
    amplitude: float
    envelope: str
    left_ratio: float
    right_ratio: float
    left_polarity: int
    right_polarity: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "start_ms": self.start_ms,
            "duration_ms": self.duration_ms,
            "frequency_hz": self.frequency_hz,
            "amplitude": self.amplitude,
            "envelope": self.envelope,
            "left_ratio": self.left_ratio,
            "right_ratio": self.right_ratio,
            "left_polarity": self.left_polarity,
            "right_polarity": self.right_polarity,
        }

    def impact_key(self) -> bytes:
        return canonical_json_bytes(
            {
                "start_ms": self.start_ms,
                "duration_ms": self.duration_ms,
                "frequency_hz": self.frequency_hz,
                "amplitude": self.amplitude,
                "envelope": self.envelope,
                "left_ratio": self.left_ratio,
                "right_ratio": self.right_ratio,
                "left_polarity": self.left_polarity,
                "right_polarity": self.right_polarity,
            }
        )


@dataclass(frozen=True)
class HapticProject:
    config: ProjectConfig
    segments: tuple[WaveSegment, ...]


def _require_mapping(value: Any, field: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise HapticError(
            E_WAVE_INVALID,
            f"{field} must be an object",
            details={"field": field},
        )
    return value


def _require_present(data: dict[str, Any], field: str, index: int) -> Any:
    if field not in data:
        raise HapticError(
            E_WAVE_INVALID,
            f"missing required field {field}",
            segment_index=index,
            details={"field": field},
        )
    return data[field]


def _parse_polarity(
    value: Any,
    field: str,
    *,
    segment_index: int | None,
) -> int:
    if value in (1, "+", "positive", "normal"):
        return 1
    if value in (-1, "-", "negative", "inverted"):
        return -1
    raise HapticError(
        E_WAVE_INVALID,
        f"{field} must be 1/-1 or +/-",
        segment_index=segment_index,
        details={"field": field},
    )


def _parse_envelope(value: Any, index: int) -> str:
    if not isinstance(value, str):
        raise HapticError(
            E_WAVE_INVALID,
            "envelope must be a string",
            segment_index=index,
            details={"field": "envelope"},
        )
    normalized = ENVELOPE_ALIASES.get(value.strip().lower())
    if normalized is None:
        raise HapticError(
            E_WAVE_INVALID,
            f"unsupported envelope {value!r}",
            segment_index=index,
            details={"field": "envelope", "allowed": list(ENVELOPES)},
        )
    return normalized


def _parse_segment(
    raw: Any,
    index: int,
    config: ProjectConfig,
) -> WaveSegment:
    if not isinstance(raw, dict):
        raise HapticError(
            E_WAVE_INVALID,
            "segment must be an object",
            segment_index=index,
        )

    start_ms = finite_number(
        _require_present(raw, "start_ms", index),
        "start_ms",
        segment_index=index,
    )
    duration_ms = finite_number(
        _require_present(raw, "duration_ms", index),
        "duration_ms",
        segment_index=index,
    )
    frequency_hz = finite_number(
        _require_present(raw, "frequency_hz", index),
        "frequency_hz",
        segment_index=index,
    )
    amplitude = finite_number(
        _require_present(raw, "amplitude", index),
        "amplitude",
        segment_index=index,
    )
    envelope = _parse_envelope(_require_present(raw, "envelope", index), index)

    if start_ms < 0:
        raise HapticError(
            E_WAVE_INVALID,
            "start_ms must be >= 0",
            segment_index=index,
            details={"field": "start_ms", "value": start_ms},
        )
    if duration_ms < 0:
        raise HapticError(
            E_WAVE_INVALID,
            "duration_ms must be >= 0",
            segment_index=index,
            details={"field": "duration_ms", "value": duration_ms},
        )
    if frequency_hz <= 0 or frequency_hz > config.nyquist_hz:
        raise HapticError(
            E_WAVE_INVALID,
            f"frequency_hz must be in (0, {config.nyquist_hz:g}]",
            segment_index=index,
            details={
                "field": "frequency_hz",
                "value": frequency_hz,
                "nyquist_hz": config.nyquist_hz,
            },
        )
    if amplitude < 0:
        raise HapticError(
            E_WAVE_INVALID,
            "amplitude must be >= 0",
            segment_index=index,
            details={"field": "amplitude", "value": amplitude},
        )

    left_ratio = finite_number(
        raw.get("left_ratio", config.motor_map.left_ratio),
        "left_ratio",
        segment_index=index,
    )
    right_ratio = finite_number(
        raw.get("right_ratio", config.motor_map.right_ratio),
        "right_ratio",
        segment_index=index,
    )
    if left_ratio < 0 or right_ratio < 0:
        raise HapticError(
            E_WAVE_INVALID,
            "motor ratios must be >= 0",
            segment_index=index,
            details={
                "field": "motor_ratio",
                "left_ratio": left_ratio,
                "right_ratio": right_ratio,
            },
        )
    left_polarity = _parse_polarity(
        raw.get("left_polarity", config.motor_map.left_polarity),
        "left_polarity",
        segment_index=index,
    )
    right_polarity = _parse_polarity(
        raw.get("right_polarity", config.motor_map.right_polarity),
        "right_polarity",
        segment_index=index,
    )
    return WaveSegment(
        index=index,
        start_ms=start_ms,
        duration_ms=duration_ms,
        frequency_hz=frequency_hz,
        amplitude=amplitude,
        envelope=envelope,
        left_ratio=left_ratio,
        right_ratio=right_ratio,
        left_polarity=left_polarity,
        right_polarity=right_polarity,
    )


def parse_segment(
    raw: dict[str, Any],
    index: int,
    config: ProjectConfig,
) -> WaveSegment:
    return _parse_segment(raw, index, config)


def _parse_config(data: dict[str, Any]) -> ProjectConfig:
    raw_rate = data.get("sample_rate", data.get("sample_rate_hz"))
    if raw_rate is None:
        raise HapticError(
            E_WAVE_INVALID,
            "missing required field sample_rate",
            details={"field": "sample_rate"},
        )
    rate_number = finite_number(raw_rate, "sample_rate")
    if not rate_number.is_integer() or rate_number <= 0 or rate_number > 192000:
        raise HapticError(
            E_WAVE_INVALID,
            "sample_rate must be an integer in [1, 192000]",
            details={"field": "sample_rate", "value": raw_rate},
        )
    sample_rate = int(rate_number)

    device_raw = data.get("device", {})
    device = _require_mapping(device_raw, "device")
    device_min = finite_number(device.get("min", -1.0), "device.min")
    device_max = finite_number(device.get("max", 1.0), "device.max")
    if device_min >= device_max:
        raise HapticError(
            E_WAVE_INVALID,
            "device.min must be less than device.max",
            details={"field": "device", "min": device_min, "max": device_max},
        )
    limiter = device.get("limiter", "hard")
    if limiter not in ("hard", "soft"):
        raise HapticError(
            E_WAVE_INVALID,
            "device.limiter must be hard or soft",
            details={"field": "device.limiter", "value": limiter},
        )
    knee = finite_number(
        device.get("soft_knee_ratio", 0.8),
        "device.soft_knee_ratio",
    )
    if not 0.0 <= knee < 1.0:
        raise HapticError(
            E_WAVE_INVALID,
            "device.soft_knee_ratio must be in [0, 1)",
            details={"field": "soft_knee_ratio", "value": knee},
        )

    map_raw = data.get("motor_map", {})
    motor_map_data = _require_mapping(map_raw, "motor_map")
    left_ratio = finite_number(
        motor_map_data.get("left_ratio", 0.8),
        "motor_map.left_ratio",
    )
    right_ratio = finite_number(
        motor_map_data.get("right_ratio", 0.8),
        "motor_map.right_ratio",
    )
    if left_ratio < 0 or right_ratio < 0:
        raise HapticError(
            E_WAVE_INVALID,
            "motor_map ratios must be >= 0",
            details={"field": "motor_map"},
        )
    motor_map = MotorMap(
        left_ratio=left_ratio,
        right_ratio=right_ratio,
        left_polarity=_parse_polarity(
            motor_map_data.get("left_polarity", 1),
            "motor_map.left_polarity",
            segment_index=None,
        ),
        right_polarity=_parse_polarity(
            motor_map_data.get("right_polarity", 1),
            "motor_map.right_polarity",
            segment_index=None,
        ),
    )

    chunk_size_raw = finite_number(data.get("chunk_size", 64), "chunk_size")
    if not chunk_size_raw.is_integer() or chunk_size_raw < 1 or chunk_size_raw > 65536:
        raise HapticError(
            E_WAVE_INVALID,
            "chunk_size must be an integer in [1, 65536]",
            details={"field": "chunk_size"},
        )
    return ProjectConfig(
        sample_rate=sample_rate,
        device_min=device_min,
        device_max=device_max,
        limiter=limiter,
        soft_knee_ratio=knee,
        motor_map=motor_map,
        chunk_size=int(chunk_size_raw),
    )


def parse_project(data: Any) -> HapticProject:
    if not isinstance(data, dict):
        raise HapticError(E_WAVE_INVALID, "project root must be an object")
    config = _parse_config(data)
    segments_raw = data.get("segments")
    if segments_raw is None:
        raise HapticError(
            E_WAVE_INVALID,
            "missing required field segments",
            details={"field": "segments"},
        )
    if not isinstance(segments_raw, list):
        raise HapticError(
            E_WAVE_INVALID,
            "segments must be an array",
            details={"field": "segments"},
        )
    segments = tuple(
        _parse_segment(raw, index, config) for index, raw in enumerate(segments_raw)
    )
    return HapticProject(config=config, segments=segments)


def load_project(path: str | Path) -> HapticProject:
    source = Path(path)
    try:
        text = source.read_text(encoding="utf-8")
    except OSError as exc:
        raise HapticError(E_IO, f"cannot read {source}: {exc}") from exc
    try:
        data = json.loads(text)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise HapticError(
            E_WAVE_INVALID,
            f"invalid JSON in {source}: {exc}",
        ) from exc
    return parse_project(data)
