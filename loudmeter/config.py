"""Pipeline configuration loaded from a JSON file.

Global parallelism and the per-task memory budget come from the config file
and must not be raised silently at runtime: ``MeterConfig.update`` rejects
any increase of ``parallelism`` or ``memory_budget_bytes`` with E_CONFIG.
"""

from __future__ import annotations

import dataclasses
import json
from dataclasses import dataclass
from pathlib import Path

from .errors import MeterError


class ConfigError(MeterError):
    """Illegal runtime configuration change."""

    code = "E_CONFIG"


DEFAULT_CHANNEL_WEIGHTS = (1.0, 1.0, 1.0, 0.0, 1.41, 1.41)  # L R C LFE Ls Rs


@dataclass(frozen=True)
class MeterConfig:
    sample_rate: int = 48000
    channels: int = 6
    block_samples: int = 19200  # 400 ms observation block @ 48 kHz
    oversample_factor: int = 4
    parallelism: int = 4
    memory_budget_bytes: int = 64 * 1024 * 1024
    abs_gate_lufs: float = -70.0
    rel_gate_offset_lu: float = -10.0
    target_loudness_lufs: float = -24.0
    true_peak_ceiling_dbtp: float = -2.0
    channel_weights: tuple = DEFAULT_CHANNEL_WEIGHTS
    short_term_window_s: float = 3.0

    def __post_init__(self):
        object.__setattr__(self, "channel_weights", tuple(self.channel_weights))
        if len(self.channel_weights) != self.channels:
            raise ConfigError("channel_weights length must match channels")
        if self.parallelism < 1:
            raise ConfigError("parallelism must not be raised at runtime")
        if self.memory_budget_bytes <= 0:
            raise ConfigError("memory_budget_bytes must be > 0")
        if self.block_samples <= 0 or self.oversample_factor < 1:
            raise ConfigError("invalid block_samples/oversample_factor")

    @classmethod
    def from_file(cls, path: str | Path) -> "MeterConfig":
        data = json.loads(Path(path).read_text(encoding="utf-8"))
        return cls(**data)

    def to_dict(self) -> dict:
        d = dataclasses.asdict(self)
        d["channel_weights"] = list(self.channel_weights)
        return d

    def update(self, **changes) -> "MeterConfig":
        """Return a new config; parallelism/budget may never be raised at runtime."""
        if "parallelism" in changes and changes["parallelism"] > self.parallelism:
            raise ConfigError("parallelism must not be raised at runtime")
        if (
            "memory_budget_bytes" in changes
            and changes["memory_budget_bytes"] > self.memory_budget_bytes
        ):
            raise ConfigError("memory_budget_bytes must not be raised at runtime")
        merged = self.to_dict()
        merged.update(changes)
        return MeterConfig(**merged)

    def task_memory_bytes(self) -> int:
        """Deterministic per-block-task memory estimate.

        Covers float64 per-channel block buffers plus the oversampled copy.
        """
        per_block = self.block_samples * self.channels * 8
        return per_block * (1 + self.oversample_factor)
