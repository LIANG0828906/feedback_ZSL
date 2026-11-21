"""Deterministic block scheduler with hard worker and memory limits."""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Callable, Generic, Mapping, TypeVar

from .config import ReplayConfig
from .errors import BusyError, MemoryBudgetError

T = TypeVar("T")


@dataclass(frozen=True, slots=True)
class ScheduledWave:
    wave: int
    block_ids: tuple[int, ...]
    memory_units: int
    waiting_block_ids: tuple[int, ...]


@dataclass(frozen=True, slots=True)
class ScheduleOutcome(Generic[T]):
    results: dict[int, T]
    trace: tuple[dict[str, object], ...]
    waves: tuple[ScheduledWave, ...]
    queue_watermark: int
    peak_memory_units: int
    memory_budget: int
    max_workers: int

    def summary(self) -> dict[str, object]:
        return {
            "memory_budget": self.memory_budget,
            "peak_memory_used": self.peak_memory_units,
            "queue_watermark": self.queue_watermark,
            "worker_limit": self.max_workers,
            "wave_count": len(self.waves),
        }


class DeterministicBlockScheduler:
    """Runs one step's independent cover tasks in stable block-number waves.

    Tasks are submitted in ascending block-id order. A task that would exceed
    either the worker count or the current-step memory budget waits for the next
    wave. Results are committed in ascending block-id order even though workers
    execute concurrently.
    """

    def __init__(self, config: ReplayConfig) -> None:
        self.config = config
        self._seen_steps: set[int] = set()
        self._lock = threading.Lock()

    def has_submitted(self, step: int) -> bool:
        with self._lock:
            return step in self._seen_steps

    def mark_submitted(self, step: int) -> None:
        with self._lock:
            self._seen_steps.add(step)

    def execute(
        self,
        step: int,
        tasks: Mapping[int, tuple[int, Callable[[], T]]],
    ) -> ScheduleOutcome[T]:
        block_ids = sorted(tasks)
        if not block_ids:
            raise ValueError("a schedule must contain at least one block task")
        for block_id in block_ids:
            memory_units, _ = tasks[block_id]
            if memory_units > self.config.memory_budget_per_step:
                raise MemoryBudgetError(
                    step,
                    block_id,
                    memory_units,
                    self.config.memory_budget_per_step,
                )
        with self._lock:
            if step in self._seen_steps:
                raise BusyError(step)
            self._seen_steps.add(step)

        remaining = list(block_ids)
        results: dict[int, T] = {}
        trace: list[dict[str, object]] = []
        waves: list[ScheduledWave] = []
        queue_watermark = 0
        peak_memory = 0
        wave_number = 0
        trace.append(
            {
                "event": "step_start",
                "step": step,
                "block_ids": block_ids,
                "memory_budget": self.config.memory_budget_per_step,
                "worker_limit": self.config.max_workers,
            }
        )

        with ThreadPoolExecutor(
            max_workers=self.config.max_workers,
            thread_name_prefix=f"cover-step-{step}",
        ) as executor:
            while remaining:
                wave_number += 1
                wave_ids: list[int] = []
                wave_memory = 0
                while remaining and len(wave_ids) < self.config.max_workers:
                    block_id = remaining[0]
                    memory_units, _ = tasks[block_id]
                    if wave_memory + memory_units > self.config.memory_budget_per_step:
                        break
                    remaining.pop(0)
                    wave_ids.append(block_id)
                    wave_memory += memory_units
                if not wave_ids:
                    block_id = remaining[0]
                    memory_units, _ = tasks[block_id]
                    raise MemoryBudgetError(
                        step,
                        block_id,
                        memory_units,
                        self.config.memory_budget_per_step,
                    )
                waiting = tuple(remaining)
                queue_watermark = max(queue_watermark, len(waiting))
                peak_memory = max(peak_memory, wave_memory)
                waves.append(
                    ScheduledWave(
                        wave=wave_number,
                        block_ids=tuple(wave_ids),
                        memory_units=wave_memory,
                        waiting_block_ids=waiting,
                    )
                )
                trace.append(
                    {
                        "event": "submit",
                        "step": step,
                        "wave": wave_number,
                        "block_ids": wave_ids,
                        "memory_units": wave_memory,
                        "waiting_block_ids": list(waiting),
                        "queue_watermark": len(waiting),
                    }
                )
                futures = {
                    block_id: executor.submit(tasks[block_id][1])
                    for block_id in wave_ids
                }
                for block_id in wave_ids:
                    results[block_id] = futures[block_id].result()
                trace.append(
                    {
                        "event": "commit",
                        "step": step,
                        "wave": wave_number,
                        "block_ids": wave_ids,
                        "commit_order": wave_ids,
                    }
                )
        trace.append(
            {
                "event": "step_end",
                "step": step,
                "queue_watermark": queue_watermark,
                "peak_memory_used": peak_memory,
                "wave_count": wave_number,
            }
        )
        return ScheduleOutcome(
            results=results,
            trace=tuple(trace),
            waves=tuple(waves),
            queue_watermark=queue_watermark,
            peak_memory_units=peak_memory,
            memory_budget=self.config.memory_budget_per_step,
            max_workers=self.config.max_workers,
        )
