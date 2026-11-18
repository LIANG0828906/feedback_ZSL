"""Job configuration loading."""

from __future__ import annotations

import json
from dataclasses import dataclass, field, asdict
from pathlib import Path


@dataclass
class JobConfig:
    # data generation
    seed: int = 20261119
    duplicate_ratio: float = 0.05
    disorder_seconds: list = field(default_factory=lambda: [30, 600])
    historical_rows: int = 100_000
    incremental_batches: int = 128
    incremental_rows: int = 40_000
    # windowing / join
    window_seconds: int = 60
    lateness_tolerance_seconds: int = 0
    slots: int = 8
    slot_skew_threshold: float = 0.40
    # resources
    state_mem_threshold_bytes: int = 32 * 1024 * 1024
    memory_limit_bytes: int = 1024 * 1024 * 1024
    spill_entry_bytes: int = 192
    # paths (relative to work_dir)
    work_dir: str = "."
    data_dir: str = "data"
    state_db: str = "state/job_state.db"
    temp_dir: str = "tmp"
    report_path: str = "artifacts/report.json"

    @classmethod
    def load(cls, path: str | Path) -> "JobConfig":
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
        cfg = cls()
        for key, value in raw.items():
            if not hasattr(cfg, key):
                raise ValueError(f"unknown config key: {key}")
            setattr(cfg, key, value)
        return cfg

    def save(self, path: str | Path) -> None:
        p = Path(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(
            json.dumps(asdict(self), indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )

    def abs_path(self, rel: str) -> Path:
        return Path(self.work_dir) / rel
