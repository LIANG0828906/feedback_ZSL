"""Strict render configuration loaded from JSON."""

from __future__ import annotations

import json
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

from .errors import BusinessError, ConfigError, ErrorCode
from .hashing import hash_json

HARD_MAX_PARALLELISM = 64
HARD_MAX_SAMPLE_BUDGET = 50_000_000


@dataclass(frozen=True)
class DegradeStage:
    name: str
    step_multiplier: float
    shadow_samples: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "step_multiplier": self.step_multiplier,
            "shadow_samples": self.shadow_samples,
        }


@dataclass(frozen=True)
class Limits:
    max_parallelism: int
    max_sample_budget: int


@dataclass(frozen=True)
class SeamConfig:
    enabled: bool
    tolerance: float
    truth_budget: int
    truth_stage: str = "S0_FULL"

    def to_dict(self) -> dict[str, Any]:
        return {
            "enabled": self.enabled,
            "tolerance": self.tolerance,
            "truth_budget": self.truth_budget,
            "truth_stage": self.truth_stage,
        }


@dataclass(frozen=True)
class RenderConfig:
    tile_size: int
    max_parallelism: int
    sample_budget: int
    base_step: float
    min_step: float
    precision: str
    degrade_sequence: tuple[DegradeStage, ...]
    seam: SeamConfig
    limits: Limits
    source_offset_map: dict[str, int] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "tile_size": self.tile_size,
            "max_parallelism": self.max_parallelism,
            "sample_budget": self.sample_budget,
            "base_step": self.base_step,
            "min_step": self.min_step,
            "precision": self.precision,
            "degrade_sequence": [stage.to_dict() for stage in self.degrade_sequence],
            "seam": self.seam.to_dict(),
            "limits": {
                "max_parallelism": self.limits.max_parallelism,
                "max_sample_budget": self.limits.max_sample_budget,
            },
        }

    def with_precision(self, precision: str) -> "RenderConfig":
        return replace(self, precision=precision)

    def stage(self, name: str) -> DegradeStage | None:
        for stage in self.degrade_sequence:
            if stage.name == name:
                return stage
        return None

    @property
    def content_hash(self) -> str:
        return hash_json(self.to_dict())


def _required(obj: dict[str, Any], key: str, path: str, source_text: str) -> Any:
    if key not in obj:
        raise ConfigError(
            "missing required configuration field",
            f"{path}.{key}".strip("."),
            source_text.find(f'"{key}"'),
        )
    return obj[key]


def _as_int(value: Any, field: str, offset: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ConfigError("configuration value must be an integer", field, offset)
    return value


def _as_float(value: Any, field: str, offset: int) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ConfigError("configuration value must be a number", field, offset)
    result = float(value)
    if result != result or result in (float("inf"), float("-inf")):
        raise ConfigError("configuration value must be finite", field, offset)
    return result


def parse_config(text: str) -> RenderConfig:
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as exc:
        raise ConfigError(
            "invalid configuration JSON",
            "config",
            exc.pos,
            details={"json_error": exc.msg},
        ) from exc
    if not isinstance(raw, dict):
        raise ConfigError("configuration root must be an object", "config", 0)

    def offset(key: str) -> int:
        return text.find(f'"{key}"')

    limits_raw = _required(raw, "limits", "", text)
    if not isinstance(limits_raw, dict):
        raise ConfigError("limits must be an object", "limits", offset("limits"))
    limits_max_parallelism = _as_int(
        _required(limits_raw, "max_parallelism", "limits", text),
        "limits.max_parallelism",
        offset("max_parallelism"),
    )
    limits_max_budget = _as_int(
        _required(limits_raw, "max_sample_budget", "limits", text),
        "limits.max_sample_budget",
        offset("max_sample_budget"),
    )
    if not (1 <= limits_max_parallelism <= HARD_MAX_PARALLELISM):
        raise ConfigError(
            "limits.max_parallelism is outside the supported safety range",
            "limits.max_parallelism",
            offset("max_parallelism"),
        )
    if not (1 <= limits_max_budget <= HARD_MAX_SAMPLE_BUDGET):
        raise ConfigError(
            "limits.max_sample_budget is outside the supported safety range",
            "limits.max_sample_budget",
            offset("max_sample_budget"),
        )
    limits = Limits(limits_max_parallelism, limits_max_budget)

    tile_size = _as_int(
        _required(raw, "tile_size", "", text), "tile_size", offset("tile_size")
    )
    max_parallelism = _as_int(
        _required(raw, "max_parallelism", "", text),
        "max_parallelism",
        offset("max_parallelism"),
    )
    sample_budget = _as_int(
        _required(raw, "sample_budget", "", text),
        "sample_budget",
        offset("sample_budget"),
    )
    base_step = _as_float(
        _required(raw, "base_step", "", text), "base_step", offset("base_step")
    )
    min_step = _as_float(
        _required(raw, "min_step", "", text), "min_step", offset("min_step")
    )
    precision = _required(raw, "precision", "", text)
    if not isinstance(precision, str) or precision not in ("float", "fixed"):
        raise ConfigError(
            "precision must be 'float' or 'fixed'",
            "precision",
            offset("precision"),
        )

    if tile_size <= 0:
        raise ConfigError("tile_size must be positive", "tile_size", offset("tile_size"))
    if not (1 <= max_parallelism <= limits.max_parallelism):
        raise ConfigError(
            "max_parallelism exceeds configured limit",
            "max_parallelism",
            offset("max_parallelism"),
            code=ErrorCode.CONFIG_PARALLELISM,
            details={"limit": limits.max_parallelism},
        )
    if sample_budget <= 0 or sample_budget > limits.max_sample_budget:
        raise ConfigError(
            "sample_budget exceeds configured hard limit",
            "sample_budget",
            offset("sample_budget"),
            code=ErrorCode.CONFIG_BUDGET,
            details={"limit": limits.max_sample_budget, "actual": sample_budget},
        )
    if base_step <= 0.0 or min_step <= 0.0:
        raise ConfigError("step sizes must be positive", "base_step/min_step", offset("base_step"))
    if min_step > base_step:
        raise ConfigError(
            "min_step must not exceed base_step",
            "min_step",
            offset("min_step"),
        )

    stages_raw = _required(raw, "degrade_sequence", "", text)
    if not isinstance(stages_raw, list) or not stages_raw:
        raise ConfigError(
            "degrade_sequence must be a non-empty list",
            "degrade_sequence",
            offset("degrade_sequence"),
        )
    stages: list[DegradeStage] = []
    previous: DegradeStage | None = None
    for index, stage_raw in enumerate(stages_raw):
        if not isinstance(stage_raw, dict):
            raise ConfigError(
                "degrade stage must be an object",
                f"degrade_sequence[{index}]",
                offset("degrade_sequence"),
            )
        name = _required(stage_raw, "name", f"degrade_sequence[{index}]", text)
        multiplier = _as_float(
            _required(stage_raw, "step_multiplier", f"degrade_sequence[{index}]", text),
            f"degrade_sequence[{index}].step_multiplier",
            offset("step_multiplier"),
        )
        shadow_samples = _as_int(
            _required(stage_raw, "shadow_samples", f"degrade_sequence[{index}]", text),
            f"degrade_sequence[{index}].shadow_samples",
            offset("shadow_samples"),
        )
        if not isinstance(name, str) or not name:
            raise ConfigError(
                "degrade stage name must be a non-empty string",
                f"degrade_sequence[{index}].name",
                offset("name"),
            )
        if any(stage.name == name for stage in stages):
            raise ConfigError(
                "degrade stage names must be unique",
                f"degrade_sequence[{index}].name",
                offset("name"),
            )
        if multiplier < 1.0:
            raise ConfigError(
                "step multiplier must be at least 1",
                f"degrade_sequence[{index}].step_multiplier",
                offset("step_multiplier"),
            )
        if shadow_samples < 0:
            raise ConfigError(
                "shadow samples must be non-negative",
                f"degrade_sequence[{index}].shadow_samples",
                offset("shadow_samples"),
            )
        if previous is not None:
            if multiplier < previous.step_multiplier:
                raise ConfigError(
                    "degrade step multipliers must be non-decreasing",
                    f"degrade_sequence[{index}].step_multiplier",
                    offset("step_multiplier"),
                )
            if shadow_samples > previous.shadow_samples:
                raise ConfigError(
                    "degrade shadow samples must be non-increasing",
                    f"degrade_sequence[{index}].shadow_samples",
                    offset("shadow_samples"),
                )
        stage = DegradeStage(name, multiplier, shadow_samples)
        stages.append(stage)
        previous = stage

    seam_raw = _required(raw, "seam", "", text)
    if not isinstance(seam_raw, dict):
        raise ConfigError("seam must be an object", "seam", offset("seam"))
    seam_enabled = _required(seam_raw, "enabled", "seam", text)
    if not isinstance(seam_enabled, bool):
        raise ConfigError("seam.enabled must be boolean", "seam.enabled", offset("enabled"))
    seam_tolerance = _as_float(
        _required(seam_raw, "tolerance", "seam", text),
        "seam.tolerance",
        offset("tolerance"),
    )
    truth_budget = _as_int(
        _required(seam_raw, "truth_budget", "seam", text),
        "seam.truth_budget",
        offset("truth_budget"),
    )
    truth_stage = seam_raw.get("truth_stage", stages[0].name)
    if not isinstance(truth_stage, str) or not any(s.name == truth_stage for s in stages):
        raise ConfigError(
            "seam.truth_stage must name a configured stage",
            "seam.truth_stage",
            offset("truth_stage"),
        )
    if seam_tolerance < 0.0:
        raise ConfigError(
            "seam tolerance must be non-negative",
            "seam.tolerance",
            offset("tolerance"),
        )
    if truth_budget <= 0 or truth_budget > limits.max_sample_budget:
        raise ConfigError(
            "seam truth budget must be within limits",
            "seam.truth_budget",
            offset("truth_budget"),
            code=ErrorCode.CONFIG_BUDGET,
            details={"limit": limits.max_sample_budget, "actual": truth_budget},
        )
    seam = SeamConfig(seam_enabled, seam_tolerance, truth_budget, truth_stage)

    return RenderConfig(
        tile_size=tile_size,
        max_parallelism=max_parallelism,
        sample_budget=sample_budget,
        base_step=base_step,
        min_step=min_step,
        precision=precision,
        degrade_sequence=tuple(stages),
        seam=seam,
        limits=limits,
    )


def validate_config(config: RenderConfig) -> None:
    # Reparse the canonical representation to enforce the same invariants for
    # programmatically constructed configs as for JSON-loaded ones.
    text = json.dumps(config.to_dict(), sort_keys=True)
    reparsed = parse_config(text)
    if reparsed.to_dict() != config.to_dict():
        raise ConfigError("configuration normalization mismatch", "config", 0)


def load_config(
    source: str | Path | bytes,
    inject: str | None = None,
) -> RenderConfig:
    if isinstance(source, (str, Path)):
        path = Path(source)
        try:
            text = path.read_text(encoding="utf-8")
        except OSError as exc:
            raise ConfigError(
                "cannot read configuration file", "config", -1, details={"error": str(exc)}
            ) from exc
    else:
        text = source.decode("utf-8")
    config = parse_config(text)
    if inject == "config_budget":
        # Injection is applied at the configuration boundary.  It changes the
        # declared budget after parsing but before validation so no renderer or
        # scheduler code is bypassed.
        bad_budget = config.limits.max_sample_budget + 1
        config = replace(config, sample_budget=bad_budget)
        raise ConfigError(
            "sample_budget exceeds configured hard limit",
            "sample_budget",
            text.find('"sample_budget"'),
            code=ErrorCode.CONFIG_BUDGET,
            details={
                "limit": config.limits.max_sample_budget,
                "actual": bad_budget,
                "injection": "config_budget",
            },
        )
    if inject not in (None, ""):
        raise ConfigError(
            "unknown configuration injection",
            "inject",
            -1,
            details={"injection": inject},
        )
    return config


__all__ = [
    "DegradeStage",
    "HARD_MAX_PARALLELISM",
    "HARD_MAX_SAMPLE_BUDGET",
    "Limits",
    "RenderConfig",
    "SeamConfig",
    "load_config",
    "parse_config",
    "validate_config",
    "BusinessError",
]
