from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping

from .errors import ErrorCode


SUPPORTED_OPS = {
    "sum": "sum",
    "avg": "avg",
    "min": "min",
    "max": "max",
    "stddev": "stdDev",
    "std_dev": "stdDev",
}

ROUNDING_MODES = {
    "half_even": "half_even",
    "half-even": "half_even",
    "round_half_even": "half_even",
    "toward_zero": "toward_zero",
    "toward-zero": "toward_zero",
    "truncate": "toward_zero",
    "round_down": "toward_zero",
    "up": "up",
    "away_from_zero": "up",
    "round_up": "up",
    "ceiling": "ceiling",
    "round_ceiling": "ceiling",
}


class ConfigError(ValueError):
    def __init__(self, message: str, details: dict[str, Any] | None = None) -> None:
        super().__init__(message)
        self.code = ErrorCode.INVALID_CONFIG
        self.details = details or {}


@dataclass(frozen=True)
class AggregateSpec:
    name: str
    op: str
    field: str

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "AggregateSpec":
        try:
            op_text = str(payload["op"])
            field_name = str(payload["field"])
        except KeyError as exc:
            raise ConfigError(f"聚合定义缺少字段: {exc.args[0]}") from exc
        op = SUPPORTED_OPS.get(op_text.strip().lower())
        if op is None:
            raise ConfigError(f"不支持的聚合算子: {op_text}")
        name = str(payload.get("name") or f"{op}_{field_name}")
        return cls(name=name, op=op, field=field_name)


@dataclass(frozen=True)
class RoundingSpec:
    mode: str
    significant_digits: int

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "RoundingSpec":
        mode_text = str(payload.get("mode", "half_even")).strip().lower()
        mode = ROUNDING_MODES.get(mode_text)
        if mode is None:
            raise ConfigError(f"不支持的舍入模式: {mode_text}")
        digits = int(payload.get("significant_digits", 12))
        if not 1 <= digits <= 100:
            raise ConfigError("significant_digits 必须在 1 到 100 之间")
        return cls(mode=mode, significant_digits=digits)


@dataclass
class EngineConfig:
    batch_size: int = 64
    max_active_groups: int | None = None
    max_state_bytes: int | None = None
    spill_partitions: int = 4
    spill_dir: str | None = None
    fail_spill_on_attempt: int | None = None
    keep_spill: bool = False

    def __post_init__(self) -> None:
        if self.batch_size < 1:
            raise ConfigError("batch_size 必须大于 0")
        if self.max_active_groups is not None and self.max_active_groups < 1:
            raise ConfigError("max_active_groups 必须大于 0")
        if self.max_state_bytes is not None and self.max_state_bytes < 1:
            raise ConfigError("max_state_bytes 必须大于 0")
        if self.spill_partitions < 1:
            raise ConfigError("spill_partitions 必须大于 0")
        if self.fail_spill_on_attempt is not None and self.fail_spill_on_attempt < 1:
            raise ConfigError("fail_spill_on_attempt 必须大于 0")


@dataclass
class PipelineConfig:
    group_by: tuple[str, ...]
    aggregates: tuple[AggregateSpec, ...]
    rounding: dict[str, RoundingSpec] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "PipelineConfig":
        group_by_raw = payload.get("group_by")
        if not isinstance(group_by_raw, list) or not group_by_raw:
            raise ConfigError("group_by 必须是非空字段数组")
        group_by = tuple(str(item) for item in group_by_raw)

        aggregate_raw = payload.get("aggregates")
        if not isinstance(aggregate_raw, list) or not aggregate_raw:
            raise ConfigError("aggregates 必须是非空数组")
        aggregates = tuple(AggregateSpec.from_dict(item) for item in aggregate_raw)
        names = [item.name for item in aggregates]
        if len(names) != len(set(names)):
            raise ConfigError("聚合结果名称不能重复")

        rounding_raw = payload.get("rounding", {})
        if rounding_raw is None:
            rounding_raw = {}
        if not isinstance(rounding_raw, Mapping):
            raise ConfigError("rounding 必须是对象")
        rounding = {
            str(name): RoundingSpec.from_dict(spec)
            for name, spec in rounding_raw.items()
        }
        return cls(group_by=group_by, aggregates=aggregates, rounding=rounding)

    def rounding_for(self, aggregate: AggregateSpec) -> RoundingSpec | None:
        return self.rounding.get(aggregate.name) or self.rounding.get(aggregate.op)

    def to_dict(self) -> dict[str, Any]:
        return {
            "group_by": list(self.group_by),
            "aggregates": [
                {"name": item.name, "op": item.op, "field": item.field}
                for item in self.aggregates
            ],
            "rounding": {
                name: {
                    "mode": spec.mode,
                    "significant_digits": spec.significant_digits,
                }
                for name, spec in sorted(self.rounding.items())
            },
        }
