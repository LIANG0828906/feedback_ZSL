"""Configuration loading and validation.

All fields that influence the measurement contract are required.  Defaults are
provided by ``MeterConfig`` for use as a Python API, but configuration files are
checked for completeness before a workbench is allowed to run.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field, replace
from pathlib import Path
from typing import Any, Mapping

from .errors import ErrorCode, MeterError, error


SUPPORTED_SAMPLE_RATES = (48_000,)
SUPPORTED_BITS = (16, 24)


@dataclass(frozen=True)
class SchedulerConfig:
    parallelism: int = 2
    memory_budget_bytes: int = 24 * 1024 * 1024
    per_task_memory_budget_bytes: int = 8 * 1024 * 1024
    oversample_batch_samples: int = 4096
    internal_buffer_samples: int = 4096
    degrade_sequence: tuple[str, ...] = (
        "increase_oversample_batch",
        "shrink_internal_buffer",
    )

    def validate(self) -> None:
        if self.parallelism < 1:
            raise error(ErrorCode.E_CONFIG_INVALID, "parallelism must be >= 1")
        if self.memory_budget_bytes < 1:
            raise error(ErrorCode.E_CONFIG_INVALID, "memory_budget_bytes must be > 0")
        if self.per_task_memory_budget_bytes < 1:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "per_task_memory_budget_bytes must be > 0",
            )
        if self.per_task_memory_budget_bytes > self.memory_budget_bytes:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "per_task memory budget cannot exceed the global budget",
            )
        if self.oversample_batch_samples < 1 or self.internal_buffer_samples < 1:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "buffer sizes must be positive",
            )
        allowed = {"increase_oversample_batch", "shrink_internal_buffer"}
        if any(stage not in allowed for stage in self.degrade_sequence):
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "unsupported degrade stage",
                {"degrade_sequence": list(self.degrade_sequence)},
            )

    def to_dict(self) -> dict[str, Any]:
        result = asdict(self)
        result["degrade_sequence"] = list(self.degrade_sequence)
        return result

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "SchedulerConfig":
        required = {
            "parallelism",
            "memory_budget_bytes",
            "per_task_memory_budget_bytes",
            "oversample_batch_samples",
            "internal_buffer_samples",
            "degrade_sequence",
        }
        missing = sorted(required - set(raw))
        if missing:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "scheduler config is incomplete",
                {"missing": missing},
            )
        return cls(
            parallelism=int(raw["parallelism"]),
            memory_budget_bytes=int(raw["memory_budget_bytes"]),
            per_task_memory_budget_bytes=int(raw["per_task_memory_budget_bytes"]),
            oversample_batch_samples=int(raw["oversample_batch_samples"]),
            internal_buffer_samples=int(raw["internal_buffer_samples"]),
            degrade_sequence=tuple(str(x) for x in raw["degrade_sequence"]),
        )


@dataclass(frozen=True)
class MeterConfig:
    """The numerical and resource contract shared by all assets."""

    version: int = 1
    sample_rate: int = 48_000
    block_samples: int = 4_800
    short_term_samples: int = 144_000
    oversample_factor: int = 4
    target_lufs: float = -23.0
    true_peak_limit_dbtp: float = -1.0
    absolute_gate_lufs: float = -70.0
    relative_gate_lu: float = -10.0
    channel_weights: tuple[float, ...] | None = None
    true_peak_include_lfe: bool = True
    lufs_tolerance: float = 1e-6
    true_peak_tolerance: float = 1e-6
    scheduler: SchedulerConfig = field(default_factory=SchedulerConfig)

    def validate(self) -> None:
        if self.version < 1:
            raise error(ErrorCode.E_CONFIG_INVALID, "version must be >= 1")
        if self.sample_rate not in SUPPORTED_SAMPLE_RATES:
            raise error(
                ErrorCode.E_CHANNEL_UNSUPPORTED,
                "only 48 kHz audio is supported",
                {"sample_rate": self.sample_rate},
            )
        if self.block_samples < 1:
            raise error(ErrorCode.E_CONFIG_INVALID, "block_samples must be > 0")
        if self.short_term_samples < self.block_samples:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "short_term_samples must be >= block_samples",
            )
        if self.oversample_factor < 1:
            raise error(ErrorCode.E_CONFIG_INVALID, "oversample_factor must be >= 1")
        if self.absolute_gate_lufs > 0:
            raise error(ErrorCode.E_CONFIG_INVALID, "absolute_gate_lufs looks invalid")
        if self.lufs_tolerance < 0 or self.true_peak_tolerance < 0:
            raise error(ErrorCode.E_CONFIG_INVALID, "tolerances must be non-negative")
        if self.channel_weights is not None:
            if not self.channel_weights:
                raise error(ErrorCode.E_CONFIG_INVALID, "channel_weights is empty")
            if any(w < 0 for w in self.channel_weights):
                raise error(
                    ErrorCode.E_CONFIG_INVALID,
                    "channel weights must be non-negative",
                )
        self.scheduler.validate()

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "sample_rate": self.sample_rate,
            "block_samples": self.block_samples,
            "short_term_samples": self.short_term_samples,
            "oversample_factor": self.oversample_factor,
            "target_lufs": self.target_lufs,
            "true_peak_limit_dbtp": self.true_peak_limit_dbtp,
            "absolute_gate_lufs": self.absolute_gate_lufs,
            "relative_gate_lu": self.relative_gate_lu,
            "channel_weights": (
                list(self.channel_weights)
                if self.channel_weights is not None
                else None
            ),
            "true_peak_include_lfe": self.true_peak_include_lfe,
            "lufs_tolerance": self.lufs_tolerance,
            "true_peak_tolerance": self.true_peak_tolerance,
            "scheduler": self.scheduler.to_dict(),
        }

    def canonical(self) -> dict[str, Any]:
        """Return the canonical measurement contract without scheduler fields."""
        result = self.to_dict()
        result.pop("scheduler", None)
        result.pop("lufs_tolerance", None)
        result.pop("true_peak_tolerance", None)
        return result

    @property
    def config_hash(self) -> str:
        return stable_hash(self.to_dict())

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "MeterConfig":
        required = {
            "version",
            "sample_rate",
            "block_samples",
            "short_term_samples",
            "oversample_factor",
            "target_lufs",
            "true_peak_limit_dbtp",
            "absolute_gate_lufs",
            "relative_gate_lu",
            "channel_weights",
            "true_peak_include_lfe",
            "lufs_tolerance",
            "true_peak_tolerance",
            "scheduler",
        }
        missing = sorted(required - set(raw))
        if missing:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "meter config is incomplete",
                {"missing": missing},
            )
        raw_weights = raw["channel_weights"]
        cfg = cls(
            version=int(raw["version"]),
            sample_rate=int(raw["sample_rate"]),
            block_samples=int(raw["block_samples"]),
            short_term_samples=int(raw["short_term_samples"]),
            oversample_factor=int(raw["oversample_factor"]),
            target_lufs=float(raw["target_lufs"]),
            true_peak_limit_dbtp=float(raw["true_peak_limit_dbtp"]),
            absolute_gate_lufs=float(raw["absolute_gate_lufs"]),
            relative_gate_lu=float(raw["relative_gate_lu"]),
            channel_weights=(
                tuple(float(x) for x in raw_weights)
                if raw_weights is not None
                else None
            ),
            true_peak_include_lfe=bool(raw["true_peak_include_lfe"]),
            lufs_tolerance=float(raw["lufs_tolerance"]),
            true_peak_tolerance=float(raw["true_peak_tolerance"]),
            scheduler=SchedulerConfig.from_mapping(raw["scheduler"]),
        )
        cfg.validate()
        return cfg

    def with_alternatives(self, **changes: Any) -> "MeterConfig":
        """Create a validated config copy used by incremental updates."""
        candidate = replace(self, **changes)
        candidate.validate()
        return candidate


@dataclass(frozen=True)
class AssetSpec:
    asset_id: str
    path: str

    def to_dict(self) -> dict[str, str]:
        return {"id": self.asset_id, "path": self.path}


@dataclass(frozen=True)
class WorkbenchConfig:
    meter: MeterConfig
    assets: tuple[AssetSpec, ...]
    artifact_dir: str = "artifacts/run"
    trace_path: str = "artifacts/run/trace.jsonl"
    snapshot_dir: str = "artifacts/run/snapshots"
    sample_dir: str = "samples"
    seed: int = 20261121

    def validate(self) -> None:
        self.meter.validate()
        if not self.assets:
            raise error(ErrorCode.E_CONFIG_INVALID, "at least one asset is required")
        seen: set[str] = set()
        for spec in self.assets:
            if not spec.asset_id:
                raise error(ErrorCode.E_CONFIG_INVALID, "asset id must not be empty")
            if spec.asset_id in seen:
                raise error(
                    ErrorCode.E_CONFIG_INVALID,
                    "asset id must be unique",
                    {"asset_id": spec.asset_id},
                )
            seen.add(spec.asset_id)

    def to_dict(self) -> dict[str, Any]:
        return {
            "meter": self.meter.to_dict(),
            "assets": [asset.to_dict() for asset in self.assets],
            "artifact_dir": self.artifact_dir,
            "trace_path": self.trace_path,
            "snapshot_dir": self.snapshot_dir,
            "sample_dir": self.sample_dir,
            "seed": self.seed,
        }


def stable_hash(value: Any) -> str:
    encoded = json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def load_workbench_config(path: str | Path) -> WorkbenchConfig:
    path = Path(path)
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise error(
            ErrorCode.E_CONFIG_INVALID,
            "config file does not exist",
            {"path": str(path)},
        ) from None
    except (OSError, ValueError) as exc:
        raise error(
            ErrorCode.E_CONFIG_INVALID,
            "config file cannot be parsed",
            {"path": str(path), "error": str(exc)},
        ) from None
    if not isinstance(raw, Mapping):
        raise error(ErrorCode.E_CONFIG_INVALID, "config root must be an object")
    required = {"meter", "assets", "artifact_dir", "trace_path", "snapshot_dir", "seed"}
    missing = sorted(required - set(raw))
    if missing:
        raise error(
            ErrorCode.E_CONFIG_INVALID,
            "workbench config is incomplete",
            {"missing": missing, "path": str(path)},
        )
    assets_raw = raw["assets"]
    if not isinstance(assets_raw, list):
        raise error(ErrorCode.E_CONFIG_INVALID, "assets must be an array")
    assets: list[AssetSpec] = []
    for item in assets_raw:
        if not isinstance(item, Mapping) or "id" not in item or "path" not in item:
            raise error(
                ErrorCode.E_CONFIG_INVALID,
                "each asset needs id and path",
                {"asset": item},
            )
        assets.append(AssetSpec(str(item["id"]), str(item["path"])))
    cfg = WorkbenchConfig(
        meter=MeterConfig.from_mapping(raw["meter"]),
        assets=tuple(assets),
        artifact_dir=str(raw["artifact_dir"]),
        trace_path=str(raw["trace_path"]),
        snapshot_dir=str(raw["snapshot_dir"]),
        sample_dir=str(raw.get("sample_dir", "samples")),
        seed=int(raw["seed"]),
    )
    cfg.validate()
    return cfg


def load_config(path: str | Path) -> MeterConfig:
    """Load a complete workbench file and return only the meter section."""
    return load_workbench_config(path).meter
