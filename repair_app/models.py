from __future__ import annotations

from dataclasses import asdict, dataclass, field, replace
from typing import Any, Iterable

from .constants import MAX_SAMPLE_RATE, MIN_SAMPLE_RATE
from .errors import AppError


def _require_bool(name: str, value: Any) -> bool:
    if not isinstance(value, bool):
        raise AppError("E_INVALID_ARGUMENT", f"{name} 必须是布尔值")
    return value


def _require_number(name: str, value: Any, minimum: float, maximum: float) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise AppError("E_INVALID_ARGUMENT", f"{name} 必须是数字")
    number = float(value)
    if not minimum <= number <= maximum:
        raise AppError(
            "E_INVALID_ARGUMENT",
            f"{name} 超出范围 [{minimum}, {maximum}]",
            value=number,
        )
    return number


@dataclass(frozen=True)
class DCParams:
    enabled: bool = True
    per_channel: bool = True

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "DCParams":
        return cls(
            enabled=_require_bool("dc_removal.enabled", value.get("enabled", True)),
            per_channel=_require_bool(
                "dc_removal.per_channel", value.get("per_channel", True)
            ),
        )


@dataclass(frozen=True)
class NoiseGateParams:
    enabled: bool = True
    threshold_db: float = -42.0
    attack_ms: float = 4.0
    release_ms: float = 80.0
    attenuation_db: float = -60.0

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "NoiseGateParams":
        return cls(
            enabled=_require_bool("noise_gate.enabled", value.get("enabled", True)),
            threshold_db=_require_number(
                "noise_gate.threshold_db", value.get("threshold_db", -42.0), -120.0, 0.0
            ),
            attack_ms=_require_number(
                "noise_gate.attack_ms", value.get("attack_ms", 4.0), 0.1, 1000.0
            ),
            release_ms=_require_number(
                "noise_gate.release_ms", value.get("release_ms", 80.0), 0.1, 5000.0
            ),
            attenuation_db=_require_number(
                "noise_gate.attenuation_db",
                value.get("attenuation_db", -60.0),
                -120.0,
                0.0,
            ),
        )


@dataclass(frozen=True)
class SpectralSubtractionParams:
    enabled: bool = True
    n_fft: int = 512
    hop_length: int = 128
    noise_percentile: float = 20.0
    oversubtraction: float = 1.5
    reduction_db: float = 12.0
    floor_db: float = -45.0

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "SpectralSubtractionParams":
        n_fft = value.get("n_fft", 512)
        hop = value.get("hop_length", 128)
        if isinstance(n_fft, bool) or not isinstance(n_fft, int) or n_fft < 64 or n_fft > 8192:
            raise AppError("E_INVALID_ARGUMENT", "spectral_subtraction.n_fft 必须在 64..8192")
        if isinstance(hop, bool) or not isinstance(hop, int) or hop < 16 or hop >= n_fft:
            raise AppError(
                "E_INVALID_ARGUMENT", "spectral_subtraction.hop_length 必须在 16..n_fft-1"
            )
        return cls(
            enabled=_require_bool(
                "spectral_subtraction.enabled", value.get("enabled", True)
            ),
            n_fft=n_fft,
            hop_length=hop,
            noise_percentile=_require_number(
                "spectral_subtraction.noise_percentile",
                value.get("noise_percentile", 20.0),
                0.0,
                100.0,
            ),
            oversubtraction=_require_number(
                "spectral_subtraction.oversubtraction",
                value.get("oversubtraction", 1.5),
                0.0,
                10.0,
            ),
            reduction_db=_require_number(
                "spectral_subtraction.reduction_db",
                value.get("reduction_db", 12.0),
                0.0,
                60.0,
            ),
            floor_db=_require_number(
                "spectral_subtraction.floor_db",
                value.get("floor_db", -45.0),
                -120.0,
                0.0,
            ),
        )


@dataclass(frozen=True)
class LimiterParams:
    enabled: bool = True
    threshold_db: float = -1.0

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "LimiterParams":
        return cls(
            enabled=_require_bool("limiter.enabled", value.get("enabled", True)),
            threshold_db=_require_number(
                "limiter.threshold_db", value.get("threshold_db", -1.0), -30.0, 0.0
            ),
        )


@dataclass(frozen=True)
class ChainConfig:
    dc_removal: DCParams = field(default_factory=DCParams)
    noise_gate: NoiseGateParams = field(default_factory=NoiseGateParams)
    spectral_subtraction: SpectralSubtractionParams = field(
        default_factory=SpectralSubtractionParams
    )
    limiter: LimiterParams = field(default_factory=LimiterParams)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, value: dict[str, Any] | None) -> "ChainConfig":
        if value is None:
            return cls()
        if not isinstance(value, dict):
            raise AppError("E_INVALID_ARGUMENT", "processing_chain 必须是对象")
        return cls(
            dc_removal=DCParams.from_dict(value.get("dc_removal", {})),
            noise_gate=NoiseGateParams.from_dict(value.get("noise_gate", {})),
            spectral_subtraction=SpectralSubtractionParams.from_dict(
                value.get("spectral_subtraction", {})
            ),
            limiter=LimiterParams.from_dict(value.get("limiter", {})),
        )

    def merged(self, updates: dict[str, Any]) -> "ChainConfig":
        current = self.to_dict()
        for section, section_updates in updates.items():
            if section not in current:
                raise AppError("E_INVALID_ARGUMENT", f"未知处理链参数段: {section}")
            if not isinstance(section_updates, dict):
                raise AppError("E_INVALID_ARGUMENT", f"{section} 更新必须是对象")
            current[section].update(section_updates)
        return ChainConfig.from_dict(current)


@dataclass(frozen=True)
class Interval:
    id: int
    start_frame: int
    end_frame: int
    gain_db: float = 0.0
    fade_in_ms: float = 10.0
    fade_out_ms: float = 10.0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, value: dict[str, Any], total_frames: int | None = None) -> "Interval":
        if not isinstance(value, dict):
            raise AppError("E_INVALID_ARGUMENT", "区间必须是对象")
        try:
            interval_id = int(value["id"])
            start = int(value["start_frame"])
            end = int(value["end_frame"])
        except (KeyError, TypeError, ValueError) as exc:
            raise AppError("E_INVALID_ARGUMENT", "区间缺少 id/start_frame/end_frame") from exc
        if interval_id < 1:
            raise AppError("E_INVALID_ARGUMENT", "区间 id 必须大于 0")
        if start < 0 or end <= start:
            raise AppError("E_INVALID_ARGUMENT", "区间必须满足 0 <= start_frame < end_frame")
        if total_frames is not None and end > total_frames:
            raise AppError(
                "E_INVALID_ARGUMENT",
                "区间超出音频长度",
                start_frame=start,
                end_frame=end,
                total_frames=total_frames,
            )
        gain_db = _require_number("interval.gain_db", value.get("gain_db", 0.0), -60.0, 24.0)
        fade_in_ms = _require_number(
            "interval.fade_in_ms", value.get("fade_in_ms", 10.0), 0.0, 10_000.0
        )
        fade_out_ms = _require_number(
            "interval.fade_out_ms", value.get("fade_out_ms", 10.0), 0.0, 10_000.0
        )
        return cls(
            id=interval_id,
            start_frame=start,
            end_frame=end,
            gain_db=gain_db,
            fade_in_ms=fade_in_ms,
            fade_out_ms=fade_out_ms,
        )

    def with_frames(self, start_frame: int, end_frame: int) -> "Interval":
        return replace(self, start_frame=start_frame, end_frame=end_frame)


def validate_intervals(
    intervals: Iterable[Interval], total_frames: int, *, allow_overlap: bool = False
) -> list[Interval]:
    ordered = sorted(intervals, key=lambda item: (item.start_frame, item.end_frame, item.id))
    seen_ids: set[int] = set()
    previous_end = -1
    for item in ordered:
        if item.id in seen_ids:
            raise AppError("E_INVALID_ARGUMENT", f"区间 id 重复: {item.id}")
        seen_ids.add(item.id)
        if item.start_frame < 0 or item.end_frame > total_frames or item.end_frame <= item.start_frame:
            raise AppError("E_INVALID_ARGUMENT", "区间边界非法", interval=item.to_dict())
        if not allow_overlap and item.start_frame < previous_end:
            raise AppError("E_INVALID_ARGUMENT", "修复区间不允许重叠", interval=item.to_dict())
        previous_end = max(previous_end, item.end_frame)
    return ordered


def validate_sample_rate(sample_rate: int) -> int:
    if (
        isinstance(sample_rate, bool)
        or not isinstance(sample_rate, int)
        or not MIN_SAMPLE_RATE <= sample_rate <= MAX_SAMPLE_RATE
    ):
        raise AppError(
            "E_INVALID_INPUT",
            f"采样率必须在 {MIN_SAMPLE_RATE}..{MAX_SAMPLE_RATE} Hz",
            sample_rate=sample_rate,
        )
    return sample_rate

