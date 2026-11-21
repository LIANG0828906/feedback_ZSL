"""Replay configuration parsing and immutable budget limits."""

from __future__ import annotations

from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from .errors import ConfigInvalid
from .util import as_decimal, load_json, to_units
from .version import BASELINE_VERSION, MODEL_VERSION, SNAPSHOT_VERSION


def _int_field(
    payload: dict[str, Any],
    name: str,
    default: int | None = None,
    minimum: int | None = None,
) -> int:
    if name not in payload:
        if default is None:
            raise ConfigInvalid(f"missing configuration field {name}", field=name)
        value = default
    else:
        value = payload[name]
    if isinstance(value, bool) or not isinstance(value, int):
        raise ConfigInvalid(f"configuration field {name} must be an integer", field=name)
    if minimum is not None and value < minimum:
        raise ConfigInvalid(
            f"configuration field {name} must be at least {minimum}", field=name
        )
    return value


def _decimal_string(
    payload: dict[str, Any], name: str, default: str
) -> str:
    value = payload.get(name, default)
    try:
        decimal = as_decimal(value, name)
    except Exception:
        raise ConfigInvalid(
            f"configuration field {name} must be a finite number", field=name
        ) from None
    if decimal < 0:
        raise ConfigInvalid(f"configuration field {name} must be non-negative", field=name)
    return format(decimal, "f")


@dataclass(frozen=True, slots=True)
class ReplayConfig:
    physics_steps: int
    fractional_digits: int
    max_workers: int
    memory_budget_per_step: int
    memory_units_per_block: int = 1
    damage_scale: int = 100
    spread_damage_percent: int = 25
    segment_softening_percent: int = 10
    crack_threshold_segments: int = 1
    crack_propagation_damage: str = "0"
    max_crack_iterations: int = 8
    strategy: str = "adaptive"
    full_scan_cost_limit: int = 32768
    model_version: str = MODEL_VERSION
    baseline_version: str = BASELINE_VERSION
    snapshot_version: int = SNAPSHOT_VERSION

    @property
    def fixed_scale(self) -> int:
        return 10 ** self.fractional_digits

    def crack_propagation_units(self) -> int:
        return to_units(self.crack_propagation_damage, self.fractional_digits)

    def with_strategy(self, strategy: str) -> "ReplayConfig":
        return replace(self, strategy=strategy)

    def to_dict(self) -> dict[str, Any]:
        return {
            "physics_steps": self.physics_steps,
            "fractional_digits": self.fractional_digits,
            "max_workers": self.max_workers,
            "memory_budget_per_step": self.memory_budget_per_step,
            "memory_units_per_block": self.memory_units_per_block,
            "damage_scale": self.damage_scale,
            "spread_damage_percent": self.spread_damage_percent,
            "segment_softening_percent": self.segment_softening_percent,
            "crack_threshold_segments": self.crack_threshold_segments,
            "crack_propagation_damage": self.crack_propagation_damage,
            "max_crack_iterations": self.max_crack_iterations,
            "strategy": self.strategy,
            "full_scan_cost_limit": self.full_scan_cost_limit,
            "model_version": self.model_version,
            "baseline_version": self.baseline_version,
            "snapshot_version": self.snapshot_version,
        }


def parse_config(payload: dict[str, Any]) -> ReplayConfig:
    if not isinstance(payload, dict):
        raise ConfigInvalid("configuration root must be an object", field="$")
    physics_steps = _int_field(payload, "physics_steps", minimum=1)
    fractional_digits = _int_field(
        payload, "fractional_digits", default=3, minimum=0
    )
    if fractional_digits > 6:
        raise ConfigInvalid(
            "fractional_digits must not exceed 6", field="fractional_digits"
        )
    max_workers = _int_field(payload, "max_workers", default=1, minimum=1)
    memory_budget = _int_field(
        payload, "memory_budget_per_step", minimum=1
    )
    memory_units = _int_field(
        payload, "memory_units_per_block", default=1, minimum=1
    )
    damage_scale = _int_field(payload, "damage_scale", default=100, minimum=1)
    spread_percent = _int_field(
        payload, "spread_damage_percent", default=25, minimum=0
    )
    softening_percent = _int_field(
        payload, "segment_softening_percent", default=10, minimum=0
    )
    threshold_segments = _int_field(
        payload, "crack_threshold_segments", default=1, minimum=1
    )
    max_crack_iterations = _int_field(
        payload, "max_crack_iterations", default=8, minimum=0
    )
    strategy = payload.get("strategy", "adaptive")
    if strategy not in {"adaptive", "full", "window"}:
        raise ConfigInvalid(
            "strategy must be one of adaptive, full, window", field="strategy"
        )
    full_scan_cost_limit = _int_field(
        payload, "full_scan_cost_limit", default=32768, minimum=0
    )
    model_version = payload.get("model_version", MODEL_VERSION)
    baseline_version = payload.get("baseline_version", BASELINE_VERSION)
    snapshot_version = _int_field(
        payload, "snapshot_version", default=SNAPSHOT_VERSION, minimum=1
    )
    if not isinstance(model_version, str) or not model_version:
        raise ConfigInvalid("model_version must be a non-empty string", field="model_version")
    if not isinstance(baseline_version, str) or not baseline_version:
        raise ConfigInvalid(
            "baseline_version must be a non-empty string", field="baseline_version"
        )
    return ReplayConfig(
        physics_steps=physics_steps,
        fractional_digits=fractional_digits,
        max_workers=max_workers,
        memory_budget_per_step=memory_budget,
        memory_units_per_block=memory_units,
        damage_scale=damage_scale,
        spread_damage_percent=spread_percent,
        segment_softening_percent=softening_percent,
        crack_threshold_segments=threshold_segments,
        crack_propagation_damage=_decimal_string(
            payload, "crack_propagation_damage", "0"
        ),
        max_crack_iterations=max_crack_iterations,
        strategy=strategy,
        full_scan_cost_limit=full_scan_cost_limit,
        model_version=model_version,
        baseline_version=baseline_version,
        snapshot_version=snapshot_version,
    )


def load_config(path: str | Path) -> ReplayConfig:
    try:
        payload = load_json(path)
    except (OSError, ValueError) as error:
        raise ConfigInvalid(f"cannot read configuration: {error}", field="$") from None
    return parse_config(payload)
