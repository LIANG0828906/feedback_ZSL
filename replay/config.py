"""回放器配置加载。

并发上限与单任务预算只能来自配置文件，配置在加载后冻结（FrozenConfig），
运行期间没有任何写接口，杜绝静默提高资源上限。
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

from .errors import E_INVALID_INPUT, ReplayError

REPLAYER_VERSION = "1.0.0"


@dataclass(frozen=True)
class FrozenConfig:
    version: str
    max_concurrency: int
    task_compute_budget: int
    task_memory_budget: int
    scheduler_poll_interval_ms: int

    def to_dict(self) -> dict:
        return {
            "version": self.version,
            "resource_pool": {
                "max_concurrency": self.max_concurrency,
                "task_compute_budget": self.task_compute_budget,
                "task_memory_budget": self.task_memory_budget,
            },
            "scheduler": {
                "poll_interval_ms": self.scheduler_poll_interval_ms,
            },
        }


DEFAULT_CONFIG = FrozenConfig(
    version=REPLAYER_VERSION,
    max_concurrency=2,
    task_compute_budget=10_000,
    task_memory_budget=10_000,
    scheduler_poll_interval_ms=10,
)


def write_default_config(path: str | Path) -> Path:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(DEFAULT_CONFIG.to_dict(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    return path


def load_config(path: str | Path | None) -> FrozenConfig:
    if path is None:
        return DEFAULT_CONFIG
    path = Path(path)
    if not path.exists():
        raise ReplayError(E_INVALID_INPUT, f"配置文件不存在: {path}")
    raw = json.loads(path.read_text(encoding="utf-8"))
    try:
        pool = raw["resource_pool"]
        cfg = FrozenConfig(
            version=str(raw.get("version", REPLAYER_VERSION)),
            max_concurrency=int(pool["max_concurrency"]),
            task_compute_budget=int(pool["task_compute_budget"]),
            task_memory_budget=int(pool["task_memory_budget"]),
            scheduler_poll_interval_ms=int(
                raw.get("scheduler", {}).get("poll_interval_ms", 10)
            ),
        )
    except (KeyError, TypeError, ValueError) as exc:
        raise ReplayError(E_INVALID_INPUT, f"配置文件字段非法: {exc}") from exc
    if cfg.max_concurrency < 1:
        raise ReplayError(E_INVALID_INPUT, "max_concurrency 必须 >= 1")
    if cfg.task_compute_budget < 1 or cfg.task_memory_budget < 1:
        raise ReplayError(E_INVALID_INPUT, "任务预算必须为正数")
    return cfg
