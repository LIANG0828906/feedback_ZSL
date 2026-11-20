"""Render configuration and stable fingerprints."""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, fields
from pathlib import Path
from typing import Any

from .errors import config_invalid
from .util import canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class RenderConfig:
    chunk_width: int = 16
    chunk_height: int = 16
    parallelism: int = 2
    task_budget: int = 100_000
    budget_policy: str = "degrade"
    base_step: float = 0.1
    min_step: float = 0.03
    max_step: float = 0.6
    density_adapt_gain: float = 1.25
    phase: float = 0.5
    shadow_samples: int = 2
    shadow_distance: float = 8.0
    precision: str = "float"
    compare_precision: bool = False
    seam_validation: bool = True
    seam_tolerance: float = 2e-5

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @property
    def fingerprint(self) -> str:
        return sha256_bytes(canonical_json_bytes(self.to_dict()))

    @classmethod
    def from_dict(cls, data: dict[str, Any] | None = None, **overrides: Any) -> "RenderConfig":
        if data is None:
            data = {}
        if not isinstance(data, dict):
            raise config_invalid("$", "configuration must be a JSON object")
        source = dict(data)
        if "config" in source and isinstance(source["config"], dict):
            source = dict(source["config"])
        aliases = {
            "chunk_size": ("chunk_width", "chunk_height"),
            "budget": ("task_budget",),
            "tolerance": ("seam_tolerance",),
        }
        for alias, targets in aliases.items():
            if alias in source:
                value = source.pop(alias)
                if alias == "chunk_size":
                    if not isinstance(value, (list, tuple)) or len(value) != 2:
                        raise config_invalid("$.chunk_size", "chunk_size must contain [width, height]")
                    source["chunk_width"], source["chunk_height"] = value
                else:
                    source[targets[0]] = value
        known = {item.name for item in fields(cls)}
        unknown = sorted(set(source) - known)
        if unknown:
            raise config_invalid(f"$.{unknown[0]}", f"unknown configuration field {unknown[0]}")
        source.update(overrides)
        config = cls(**source)
        config.validate()
        return config

    @classmethod
    def load(cls, path: str | Path, **overrides: Any) -> "RenderConfig":
        try:
            data = json.loads(Path(path).read_text(encoding="utf-8"))
        except OSError as exc:
            raise config_invalid("$", f"cannot read config: {exc}") from exc
        except json.JSONDecodeError as exc:
            raise config_invalid("$", f"invalid config JSON: {exc.msg}", line=exc.lineno) from exc
        return cls.from_dict(data, **overrides)

    def validate(self) -> None:
        checks = [
            ("$.chunk_width", self.chunk_width, 1, None),
            ("$.chunk_height", self.chunk_height, 1, None),
            ("$.parallelism", self.parallelism, 1, 256),
            ("$.task_budget", self.task_budget, 1, None),
            ("$.shadow_samples", self.shadow_samples, 1, 1024),
        ]
        for path, value, minimum, maximum in checks:
            if isinstance(value, bool) or not isinstance(value, int):
                raise config_invalid(path, "must be an integer")
            if value < minimum or (maximum is not None and value > maximum):
                raise config_invalid(path, f"must be in [{minimum}, {maximum or 'inf'}]")
        numeric = [
            ("$.base_step", self.base_step),
            ("$.min_step", self.min_step),
            ("$.max_step", self.max_step),
            ("$.density_adapt_gain", self.density_adapt_gain),
            ("$.phase", self.phase),
            ("$.shadow_distance", self.shadow_distance),
            ("$.seam_tolerance", self.seam_tolerance),
        ]
        for path, value in numeric:
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise config_invalid(path, "must be numeric")
            if not (value == value and abs(float(value)) != float("inf")):
                raise config_invalid(path, "must be finite")
        if self.base_step <= 0 or self.min_step <= 0 or self.max_step <= 0:
            raise config_invalid("$.base_step", "steps must be positive")
        if self.min_step > self.base_step or self.base_step > self.max_step:
            raise config_invalid(
                "$.base_step", "require min_step <= base_step <= max_step"
            )
        if self.density_adapt_gain < 0:
            raise config_invalid("$.density_adapt_gain", "must be non-negative")
        if not 0.0 <= self.phase < 1.0:
            raise config_invalid("$.phase", "must be in [0, 1)")
        if self.shadow_distance <= 0:
            raise config_invalid("$.shadow_distance", "must be positive")
        if self.seam_tolerance < 0:
            raise config_invalid("$.seam_tolerance", "must be non-negative")
        if self.budget_policy not in {"degrade", "reject"}:
            raise config_invalid("$.budget_policy", "must be 'degrade' or 'reject'")
        if self.precision not in {"float", "fixed"}:
            raise config_invalid("$.precision", "must be 'float' or 'fixed'")
