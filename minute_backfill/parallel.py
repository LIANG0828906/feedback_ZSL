"""Bounded process-pool execution of the window Join/aggregation."""

from __future__ import annotations

import multiprocessing
from concurrent.futures import FIRST_COMPLETED, Future, ProcessPoolExecutor, wait
from dataclasses import dataclass
from typing import Any

from .aggregate import aggregate_groups
from .slots import SlotAssignment, assign_slots
from .spill import SpillBuffer


def _worker_init() -> None:
    """Detach worker stdio so a killed parent cannot leave pipe readers behind."""

    import os

    try:
        devnull = os.open(os.devnull, os.O_RDWR)
        os.dup2(devnull, 0)
        os.dup2(devnull, 1)
        os.dup2(devnull, 2)
        if devnull > 2:
            os.close(devnull)
    except OSError:
        pass


@dataclass
class AggregateResult:
    assignment: SlotAssignment
    groups_processed: int
    unmatched_refunds: int
    currency_mismatches: int


class ParallelAggregator:
    def __init__(self, *, workers: int, slots: int, skew_threshold: float) -> None:
        self.slots = max(1, slots)
        self.workers = max(0, min(workers, 2))
        self.skew_threshold = skew_threshold
        self._executor: ProcessPoolExecutor | None = None

    def _ensure_executor(self) -> ProcessPoolExecutor:
        if self._executor is None:
            self._executor = ProcessPoolExecutor(
                max_workers=self.workers,
                mp_context=multiprocessing.get_context("spawn"),
                initializer=_worker_init,
            )
        return self._executor

    def aggregate(
        self,
        groups: list[dict[str, Any]],
        spill: SpillBuffer,
        *,
        chunk_groups: int = 2_000,
    ) -> AggregateResult:
        assignment = assign_slots(
            groups, slots=self.slots, threshold=self.skew_threshold
        )
        if not groups:
            return AggregateResult(assignment, 0, 0, 0)
        if self.workers == 0:
            result = aggregate_groups(groups)
            spill.add_rows(result["rows"])
            return AggregateResult(
                assignment=assignment,
                groups_processed=len(groups),
                unmatched_refunds=int(result["unmatched_refunds"]),
                currency_mismatches=int(result["currency_mismatches"]),
            )

        slot_indices = assignment.group_indices_by_slot()
        payloads: list[list[dict[str, Any]]] = []
        for slot in sorted(slot_indices):
            indices = slot_indices[slot]
            for start in range(0, len(indices), chunk_groups):
                payloads.append([groups[index] for index in indices[start : start + chunk_groups]])

        executor = self._ensure_executor()
        pending: set[Future[dict[str, Any]]] = set()
        payload_iter = iter(payloads)
        max_in_flight = max(1, self.workers * 2)
        for _ in range(min(max_in_flight, len(payloads))):
            pending.add(executor.submit(aggregate_groups, next(payload_iter)))

        unmatched = 0
        mismatches = 0
        processed = 0
        while pending:
            done, pending = wait(pending, return_when=FIRST_COMPLETED)
            for future in done:
                result = future.result()
                rows = result["rows"]
                spill.add_rows(rows)
                processed += len(rows)
                unmatched += int(result["unmatched_refunds"])
                mismatches += int(result["currency_mismatches"])
                try:
                    payload = next(payload_iter)
                except StopIteration:
                    continue
                pending.add(executor.submit(aggregate_groups, payload))
        if processed != len(groups):
            raise RuntimeError(
                f"parallel aggregation lost groups: processed={processed} expected={len(groups)}"
            )
        return AggregateResult(
            assignment=assignment,
            groups_processed=processed,
            unmatched_refunds=unmatched,
            currency_mismatches=mismatches,
        )

    def shutdown(self) -> None:
        if self._executor is not None:
            self._executor.shutdown(wait=True, cancel_futures=True)
            self._executor = None
