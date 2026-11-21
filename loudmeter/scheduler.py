"""Deterministic parallel block-metering scheduler.

Global parallelism and the per-task memory budget come from MeterConfig and
cannot be raised at runtime (see config.MeterConfig.update). Block tasks of
different assets run in parallel; blocks of one asset are submitted in
order. Runnable tasks are dispatched in stable submission-sequence order;
when the cumulative memory estimate would exceed the budget, tasks enter a
waiting queue. Every scheduling decision is written to the trace with a
logical tick (never wall-clock time), so identical inputs give byte
identical traces. Submitting the same asset twice raises E_BUSY.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Callable

from .config import MeterConfig, ConfigError
from .errors import BusyError


class TraceLog:
    """Append-only deterministic trace. ``tick`` is a logical clock."""

    def __init__(self):
        self.entries: list[dict] = []

    def record(self, tick: int, event: str, **fields) -> None:
        entry = {"tick": tick, "event": event}
        entry.update(fields)
        self.entries.append(entry)


@dataclass
class BlockTask:
    asset_id: str
    block_index: int
    seq: int
    mem_bytes: int
    duration: int  # deterministic cost in logical ticks
    fn: Callable[[], object]


class Scheduler:
    def __init__(self, config: MeterConfig, trace: TraceLog):
        self.config = config
        self.trace = trace
        self.clock = 0
        self._seq = 0
        self._waiting: list[BlockTask] = []
        self._running: dict[int, tuple[BlockTask, int]] = {}  # slot -> (task, finish)
        self._active_assets: dict[str, int] = {}  # asset_id -> remaining tasks
        self._budget_used = 0
        self.results: dict[tuple[str, int], object] = {}

    # -- submission ------------------------------------------------------
    def submit_asset(self, asset_id: str,
                     block_fns: list[tuple[int, Callable[[], object]]]) -> None:
        """Submit one asset's blocks in order. Duplicate asset -> E_BUSY."""
        if asset_id in self._active_assets:
            raise BusyError(f"asset {asset_id!r} is already being metered")
        mem = self.config.task_memory_bytes()
        if mem > self.config.memory_budget_bytes:
            raise ConfigError("single task memory estimate exceeds budget")
        self._active_assets[asset_id] = len(block_fns)
        for block_index, fn in block_fns:
            task = BlockTask(
                asset_id=asset_id,
                block_index=block_index,
                seq=self._seq,
                mem_bytes=mem,
                duration=max(1, self.config.block_samples // 9600),
                fn=fn,
            )
            self._seq += 1
            self._waiting.append(task)
            self.trace.record(self.clock, "submit",
                              asset_id=asset_id, block_index=block_index,
                              seq=task.seq)

    # -- deterministic event loop ---------------------------------------
    def run(self) -> None:
        while self._waiting or self._running:
            self._dispatch()
            if not self._running:
                break  # nothing runnable (should not happen with valid config)
            # advance logical clock to the next completion
            nxt = min(finish for _, finish in self._running.values())
            self.clock = max(self.clock, nxt)
            done_slots = [s for s, (_, f) in self._running.items() if f <= self.clock]
            for slot in sorted(done_slots):
                task, _ = self._running.pop(slot)
                self._budget_used -= task.mem_bytes
                self._active_assets[task.asset_id] -= 1
                if self._active_assets[task.asset_id] == 0:
                    del self._active_assets[task.asset_id]
                self.trace.record(self.clock, "complete",
                                  asset_id=task.asset_id,
                                  block_index=task.block_index,
                                  seq=task.seq, slot=slot,
                                  budget_used=self._budget_used,
                                  budget_total=self.config.memory_budget_bytes,
                                  parallelism=self.config.parallelism)

    def _dispatch(self) -> None:
        for slot in range(self.config.parallelism):
            if slot in self._running:
                continue
            chosen = None
            for task in self._waiting:  # stable submission-sequence order
                if self._budget_used + task.mem_bytes <= self.config.memory_budget_bytes:
                    chosen = task
                    break
                self.trace.record(self.clock, "wait_budget",
                                  asset_id=task.asset_id,
                                  block_index=task.block_index,
                                  seq=task.seq,
                                  budget_used=self._budget_used,
                                  budget_total=self.config.memory_budget_bytes,
                                  parallelism=self.config.parallelism)
            if chosen is None:
                return
            self._waiting.remove(chosen)
            self._budget_used += chosen.mem_bytes
            finish = self.clock + chosen.duration
            self._running[slot] = (chosen, finish)
            self.trace.record(self.clock, "dispatch",
                              asset_id=chosen.asset_id,
                              block_index=chosen.block_index,
                              seq=chosen.seq, slot=slot,
                              budget_used=self._budget_used,
                              budget_total=self.config.memory_budget_bytes,
                              parallelism=self.config.parallelism)
            # execute inline: computation itself is deterministic
            self.results[(chosen.asset_id, chosen.block_index)] = chosen.fn()
