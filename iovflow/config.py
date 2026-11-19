from __future__ import annotations

import hashlib
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Any

from .errors import EngineError


MAX_CPU = 2
MAX_TOTAL_MEMORY_MB = 1280  # 1.25 GiB
MAX_ENGINE_MEMORY_MB = 1024
MAX_TEMP_MB = 2048


@dataclass
class RunConfig:
    job_id: str
    sql_path: str
    input_dir: str
    mode: str = "stream"
    state_db: str = "state/iovflow_state.db"
    results_db: str = "results/results.db"
    temp_dir: str = "tmp"
    report_dir: str = "reports"
    parallelism: int = 2
    max_shards: int = 4
    allowed_lateness: int = 300
    state_memory_mb: float = 32.0
    memory_limit_mb: int = 1024
    temp_limit_mb: int = 2048
    no_progress_timeout: float = 30.0
    max_takeovers: int = 8
    vehicle_column: str = "vehicle_id"
    dedupe_keys: tuple[str, ...] = (
        "vehicle_id",
        "trip_id",
        "event_time",
        "event_type",
    )
    shard_skew_cap: float = 0.40
    keep_temp: bool = False
    resume: bool = False
    inject_kill: str | None = None
    inject_hang: str | None = None
    inject_delay_ms: int = 40
    metrics_port: int | None = None

    def __post_init__(self) -> None:
        if self.mode not in ("batch", "stream"):
            raise EngineError("IOV_INPUT_INVALID", f"unknown mode {self.mode!r}")
        self.parallelism = max(1, int(self.parallelism))
        if self.parallelism > MAX_CPU:
            # The task contract is a hard two logical CPU budget.  Clamp rather
            # than silently using more workers.
            self.parallelism = MAX_CPU
        self.max_shards = max(self.parallelism, int(self.max_shards))
        self.memory_limit_mb = min(int(self.memory_limit_mb), MAX_ENGINE_MEMORY_MB)
        self.temp_limit_mb = min(int(self.temp_limit_mb), MAX_TEMP_MB)
        if self.allowed_lateness < 0:
            raise EngineError("IOV_INPUT_INVALID", "allowed_lateness must be >= 0")
        if not (0.05 <= self.shard_skew_cap <= 0.95):
            raise EngineError("IOV_INPUT_INVALID", "shard_skew_cap must be between 0.05 and 0.95")

    @property
    def sql_text(self) -> str:
        return Path(self.sql_path).read_text(encoding="utf-8")

    @property
    def sql_hash(self) -> str:
        return hashlib.sha256(self.sql_text.encode("utf-8")).hexdigest()

    def to_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["dedupe_keys"] = list(self.dedupe_keys)
        data["sql_hash"] = self.sql_hash
        return data

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "RunConfig":
        allowed = {f.name for f in cls.__dataclass_fields__.values()}  # type: ignore[attr-defined]
        clean = {k: v for k, v in data.items() if k in allowed}
        if "dedupe_keys" in clean:
            clean["dedupe_keys"] = tuple(clean["dedupe_keys"])
        return cls(**clean)

    def validate_existing(self, existing: dict[str, Any]) -> None:
        """Reject a resume that changes the meaning of the running job."""
        checks = {
            "sql_hash": self.sql_hash,
            "mode": self.mode,
            "allowed_lateness": self.allowed_lateness,
            "vehicle_column": self.vehicle_column,
        }
        for key, expected in checks.items():
            got = existing.get(key)
            if got != expected:
                raise EngineError(
                    "IOV_STATE_INCOMPATIBLE",
                    f"resume changes {key}: existing={got!r} requested={expected!r}",
                    context={"field": key, "existing": got, "requested": expected},
                )

