"""Job configuration object and JSON loading / validation."""

from __future__ import annotations

import json
import os
from dataclasses import asdict, dataclass, field

from .errors import ConfigError


@dataclass
class JobConfig:
    data_dir: str
    state_dir: str
    result_dir: str
    spill_dir: str
    manifest: str = "manifest.json"
    # Event-time semantics
    allowed_lateness_ms: int = 120_000
    window_size_ms: int = 60_000
    # Parallelism (processing units keyed by payment id)
    partitions: int = 4
    skew_ratio: float = 0.40
    workers: int = 2
    # State / resource limits
    state_memory_bytes: int = 64 * 1024 * 1024
    memory_hard_limit_bytes: int = 1024 * 1024 * 1024
    temp_hard_limit_bytes: int = 2 * 1024 * 1024 * 1024
    # Path selection: "batch" (pure batch) or "mixed" (history + streaming batches)
    mode: str = "mixed"
    fault_config: str | None = None

    @staticmethod
    def load(path: str) -> "JobConfig":
        try:
            with open(path, "r", encoding="utf-8") as fh:
                raw = json.load(fh)
        except FileNotFoundError as exc:
            raise ConfigError(f"job config not found: {path}") from exc
        except json.JSONDecodeError as exc:
            raise ConfigError(f"invalid job config json: {exc}") from exc
        known = JobConfig.__dataclass_fields__.keys()
        unknown = sorted(set(raw) - set(known))
        if unknown:
            raise ConfigError(f"unknown config keys: {unknown}")
        try:
            cfg = JobConfig(**raw)
        except TypeError as exc:
            raise ConfigError(f"invalid job config: {exc}") from exc
        cfg.validate()
        return cfg

    def validate(self) -> None:
        if self.allowed_lateness_ms < 0:
            raise ConfigError("allowed_lateness_ms must be >= 0")
        if self.window_size_ms <= 0:
            raise ConfigError("window_size_ms must be > 0")
        if self.partitions <= 0:
            raise ConfigError("partitions must be > 0")
        if not 0.0 < self.skew_ratio <= 1.0:
            raise ConfigError("skew_ratio must be within (0, 1]")
        if self.workers <= 0:
            raise ConfigError("workers must be > 0")
        if self.mode not in ("batch", "mixed"):
            raise ConfigError("mode must be 'batch' or 'mixed'")
        for label, path in (
            ("data_dir", self.data_dir),
            ("state_dir", self.state_dir),
            ("result_dir", self.result_dir),
            ("spill_dir", self.spill_dir),
        ):
            if not os.path.isabs(path) and not os.path.exists(os.path.dirname(path) or "."):
                raise ConfigError(f"{label} parent does not exist: {path}")

    def to_dict(self) -> dict:
        return asdict(self)


@dataclass
class FaultRule:
    """One declarative fault injection rule (no engine source changes)."""

    fault: str
    batch: int | None = None  # affected source batch id; None means any / once
    once: bool = True


@dataclass
class FaultPlan:
    rules: list[FaultRule] = field(default_factory=list)

    _SUPPORTED = (
        "kill_after_commit",
        "state_locked",
        "state_corrupt",
        "early_watermark",
        "batch_read_failure",
    )

    @staticmethod
    def load(path: str | None) -> "FaultPlan":
        if not path:
            return FaultPlan()
        try:
            with open(path, "r", encoding="utf-8") as fh:
                raw = json.load(fh)
        except FileNotFoundError as exc:
            raise ConfigError(f"fault config not found: {path}") from exc
        except json.JSONDecodeError as exc:
            raise ConfigError(f"invalid fault config json: {exc}") from exc
        rules = []
        for item in raw.get("rules", []):
            fault = item.get("fault")
            if fault not in FaultPlan._SUPPORTED:
                raise ConfigError(
                    f"unsupported fault '{fault}', expected one of {FaultPlan._SUPPORTED}"
                )
            rules.append(
                FaultRule(
                    fault=fault,
                    batch=item.get("batch"),
                    once=bool(item.get("once", True)),
                )
            )
        return FaultPlan(rules=rules)
