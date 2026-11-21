"""Deterministic, bounded parallel scheduling for observation blocks."""

from __future__ import annotations

import json
import threading
from collections.abc import Callable, Iterable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, TypeVar

from .errors import E_BUSY, E_CONFIG_INVALID, DiagnosticError

T = TypeVar("T")


@dataclass(frozen=True)
class SchedulerConfig:
    parallelism: int
    memory_budget_bytes: int
    block_memory_bytes: int

    def __post_init__(self) -> None:
        if self.parallelism < 1:
            raise DiagnosticError(
                E_CONFIG_INVALID, "parallelism must be at least 1"
            )
        if self.memory_budget_bytes < 1:
            raise DiagnosticError(
                E_CONFIG_INVALID, "memory_budget_bytes must be positive"
            )
        if self.block_memory_bytes < 1:
            raise DiagnosticError(
                E_CONFIG_INVALID, "block_memory_bytes must be positive"
            )
        if self.block_memory_bytes > self.memory_budget_bytes:
            raise DiagnosticError(
                E_CONFIG_INVALID,
                "block_memory_bytes exceeds memory_budget_bytes",
            )

    def as_dict(self) -> dict[str, int]:
        return {
            "parallelism": self.parallelism,
            "memory_budget_bytes": self.memory_budget_bytes,
            "block_memory_bytes": self.block_memory_bytes,
        }


def scheduler_config_from_mapping(value: dict[str, Any]) -> SchedulerConfig:
    if not isinstance(value, dict):
        raise DiagnosticError(E_CONFIG_INVALID, "scheduler config must be an object")
    try:
        parallelism = value.get("parallelism", value.get("max_workers"))
        memory_budget = value.get("memory_budget_bytes")
        block_memory = value.get(
            "block_memory_bytes", value.get("memory_per_task_bytes")
        )
    except AttributeError as exc:
        raise DiagnosticError(E_CONFIG_INVALID, "invalid scheduler config") from exc
    if parallelism is None or memory_budget is None or block_memory is None:
        raise DiagnosticError(
            E_CONFIG_INVALID,
            "config requires parallelism, memory_budget_bytes and block_memory_bytes",
        )
    if not all(isinstance(item, int) for item in (parallelism, memory_budget, block_memory)):
        raise DiagnosticError(
            E_CONFIG_INVALID, "scheduler values must be integers"
        )
    return SchedulerConfig(
        parallelism=parallelism,
        memory_budget_bytes=memory_budget,
        block_memory_bytes=block_memory,
    )


def load_scheduler_config(path: str | Path) -> SchedulerConfig:
    try:
        with open(path, "rb") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise DiagnosticError(E_CONFIG_INVALID, f"cannot read config: {exc}") from exc
    return scheduler_config_from_mapping(value)


def _plan_waves(
    block_indices: list[int], config: SchedulerConfig
) -> tuple[list[list[tuple[int, int]]], list[str | None]]:
    """Return deterministic waves of (submit_seq, block_index) in block order."""

    waves: list[list[tuple[int, int]]] = []
    wait_reasons: list[str | None] = [None]
    current: list[tuple[int, int]] = []
    current_memory = 0
    for submit_seq, block_index in enumerate(block_indices):
        if current:
            if current_memory + config.block_memory_bytes > config.memory_budget_bytes:
                waves.append(current)
                wait_reasons.append("memory_budget")
                current = []
                current_memory = 0
            elif len(current) >= config.parallelism:
                waves.append(current)
                wait_reasons.append("parallelism")
                current = []
                current_memory = 0
        current.append((submit_seq, block_index))
        current_memory += config.block_memory_bytes
    if current:
        waves.append(current)
    return waves, wait_reasons


class BlockScheduler:
    """Run block tasks in deterministic waves under fixed resource limits.

    A shared executor is capped by ``parallelism``. Only one material is
    dispatched at a time so trace ordering is reproducible; duplicate starts of
    an active material fail immediately with ``E_BUSY``.
    """

    def __init__(self, config: SchedulerConfig) -> None:
        self.config = config
        self._executor = ThreadPoolExecutor(
            max_workers=config.parallelism,
            thread_name_prefix="loudness-block",
        )
        self._active: set[str] = set()
        self._active_lock = threading.Lock()
        self._dispatch_lock = threading.Lock()

    def close(self) -> None:
        self._executor.shutdown(wait=True)

    def __enter__(self) -> BlockScheduler:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    def run(
        self,
        asset_id: str,
        block_indices: Iterable[int],
        task: Callable[[int], T],
    ) -> tuple[list[T], list[dict[str, Any]]]:
        indices = list(block_indices)
        if any(not isinstance(item, int) for item in indices):
            raise DiagnosticError(E_CONFIG_INVALID, "block indices must be integers")
        if len(set(indices)) != len(indices):
            raise DiagnosticError(E_CONFIG_INVALID, "duplicate block index")
        indices = sorted(indices)

        with self._active_lock:
            if asset_id in self._active:
                raise DiagnosticError(
                    E_BUSY, f"material {asset_id!r} is already being evaluated"
                )
            self._active.add(asset_id)

        try:
            with self._dispatch_lock:
                return self._run_locked(asset_id, indices, task)
        finally:
            with self._active_lock:
                self._active.discard(asset_id)

    def _run_locked(
        self,
        asset_id: str,
        indices: list[int],
        task: Callable[[int], T],
    ) -> tuple[list[T], list[dict[str, Any]]]:
        waves, wait_reasons = _plan_waves(indices, self.config)
        wave_of: dict[int, int] = {}
        for wave_index, wave in enumerate(waves):
            for _, block_index in wave:
                wave_of[block_index] = wave_index

        trace: list[dict[str, Any]] = [
            {
                "event": "asset_submitted",
                "asset_id": asset_id,
                "block_count": len(indices),
                **self.config.as_dict(),
            }
        ]
        for submit_seq, block_index in enumerate(indices):
            wave_index = wave_of[block_index]
            trace.append(
                {
                    "event": "task_queued",
                    "asset_id": asset_id,
                    "block_index": block_index,
                    "submit_seq": submit_seq,
                    "wave_index": wave_index,
                    "memory_bytes": self.config.block_memory_bytes,
                    "wait_reason": wait_reasons[wave_index],
                }
            )

        results_by_seq: dict[int, T] = {}
        for wave_index, wave in enumerate(waves):
            wave_indices = [block_index for _, block_index in wave]
            used_memory = len(wave) * self.config.block_memory_bytes
            trace.append(
                {
                    "event": "wave_started",
                    "asset_id": asset_id,
                    "wave_index": wave_index,
                    "block_indices": wave_indices,
                    "submit_sequences": [seq for seq, _ in wave],
                    "running_tasks": len(wave),
                    "used_memory_bytes": used_memory,
                    "memory_budget_bytes": self.config.memory_budget_bytes,
                    "parallelism": self.config.parallelism,
                }
            )
            futures = [
                (submit_seq, block_index, self._executor.submit(task, block_index))
                for submit_seq, block_index in wave
            ]
            for submit_seq, block_index, future in futures:
                results_by_seq[submit_seq] = future.result()
            trace.append(
                {
                    "event": "wave_completed",
                    "asset_id": asset_id,
                    "wave_index": wave_index,
                    "block_indices": wave_indices,
                    "completed_tasks": len(wave),
                    "released_memory_bytes": used_memory,
                }
            )

        trace.append(
            {
                "event": "asset_completed",
                "asset_id": asset_id,
                "block_count": len(indices),
            }
        )
        return [results_by_seq[seq] for seq in range(len(indices))], trace

