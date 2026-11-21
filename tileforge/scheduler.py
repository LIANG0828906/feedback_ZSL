"""Fixed-parallelism scheduler with immutable memory budgets and stable commits."""

from __future__ import annotations

import threading
import time
from collections import OrderedDict
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass
from typing import Any, Callable, Iterable

from .budget import MemoryEstimate, choose_profile
from .config import MemoryConfig
from .errors import E_BUDGET_EXCEEDED, E_CANCELLED, error
from .model import SequenceHeader
from .trace import TraceLog


@dataclass
class _WorkItem:
    tile_id: int
    commit_order: int
    estimate: MemoryEstimate | None = None


@dataclass
class _ActiveItem:
    tile_id: int
    commit_order: int
    estimate: MemoryEstimate
    future: Future


class ConversionScheduler:
    """Schedule independent tile conversions under global/tile memory ceilings.

    The scheduler never changes configured limits. If the smallest declared
    profile does not fit, it emits E_BUDGET_EXCEEDED. Completed results are
    handed to ``commit`` in input order, independently of worker finish order.
    """

    def __init__(
        self,
        header: SequenceHeader,
        config: MemoryConfig,
        trace: TraceLog,
    ) -> None:
        self.header = header
        self.config = config
        self.trace = trace
        self._pause = threading.Event()
        self._pause.set()
        self._cancel = threading.Event()

    def pause(self) -> None:
        self._pause.clear()
        self.trace.emit("scheduler_paused", parallelism=self.config.parallelism)

    def resume(self) -> None:
        self._pause.set()
        self.trace.emit("scheduler_resumed", parallelism=self.config.parallelism)

    def cancel(self) -> None:
        self._cancel.set()
        self._pause.set()
        self.trace.emit("scheduler_cancel_requested", parallelism=self.config.parallelism)

    @property
    def cancelled(self) -> bool:
        return self._cancel.is_set()

    def run(
        self,
        tile_ids: Iterable[int],
        task: Callable[[int, str], Any],
        commit: Callable[[int, Any], None],
        crash_hook: Callable[[int], None] | None = None,
    ) -> list[Any]:
        ordered_ids = list(tile_ids)
        if len(set(ordered_ids)) != len(ordered_ids):
            raise error(E_BUDGET_EXCEEDED, "duplicate tile scheduled", field="tile_ids")
        pending: "OrderedDict[int, _WorkItem]" = OrderedDict(
            (tile_id, _WorkItem(tile_id=tile_id, commit_order=index))
            for index, tile_id in enumerate(ordered_ids)
        )
        active: dict[Future, _ActiveItem] = {}
        ready: dict[int, tuple[_WorkItem, Any]] = {}
        committed_order = 0
        reserved_bytes = 0
        results: list[Any] = []
        failure: BaseException | None = None
        self.trace.emit(
            "scheduler_start",
            tile_count=len(ordered_ids),
            parallelism=self.config.parallelism,
            tile_budget_bytes=self.config.tile_budget_bytes,
            run_budget_bytes=self.config.run_budget_bytes,
            profiles=list(self.config.downgrade_profiles),
        )

        executor = ThreadPoolExecutor(max_workers=self.config.parallelism, thread_name_prefix="tileforge")
        try:
            while pending or active:
                if not self._pause.is_set():
                    if active:
                        done, _ = wait(active.keys(), timeout=0.05, return_when=FIRST_COMPLETED)
                        for future in done:
                            item = active.pop(future)
                            reserved_bytes -= item.estimate.total_bytes
                            try:
                                result = future.result()
                                ready[item.commit_order] = (
                                    _WorkItem(
                                        tile_id=item.tile_id,
                                        commit_order=item.commit_order,
                                        estimate=item.estimate,
                                    ),
                                    result,
                                )
                            except BaseException as exc:
                                if failure is None:
                                    failure = exc
                        if failure is not None:
                            break
                        continue
                    time.sleep(0.02)
                    continue
                if failure is None and self._cancel.is_set():
                    failure = error(E_CANCELLED, "conversion cancelled cooperatively", field="scheduler")
                    self.trace.emit("scheduler_cancelled", pending=len(pending), active=len(active))

                while self._pause.is_set() and failure is None and not self._cancel.is_set():
                    made_progress = False
                    for tile_id in list(pending):
                        if len(active) >= self.config.parallelism:
                            break
                        item = pending[tile_id]
                        try:
                            estimate, attempts = choose_profile(self.header, self.config)
                        except BaseException as exc:
                            failure = exc
                            attempt_details = getattr(exc, "details", {}).get("attempts", [])
                            self.trace.emit(
                                "budget_rejected",
                                tile_id=tile_id,
                                attempts=attempt_details,
                            )
                            break
                        for rejected in attempts[:-1]:
                            self.trace.emit(
                                "downgrade_attempt",
                                tile_id=tile_id,
                                stage=rejected.profile,
                                usage_bytes=rejected.total_bytes,
                                tile_budget_bytes=self.config.tile_budget_bytes,
                                accepted=False,
                            )
                        accepted = attempts[-1]
                        if len(attempts) > 1:
                            self.trace.emit(
                                "downgrade_accepted",
                                tile_id=tile_id,
                                stage=accepted.profile,
                                usage_bytes=accepted.total_bytes,
                                tile_budget_bytes=self.config.tile_budget_bytes,
                                precision_changed=False,
                                bit_depth=self.header.bit_depth,
                            )
                        if accepted.total_bytes > self.config.run_budget_bytes:
                            failure = error(
                                E_BUDGET_EXCEEDED,
                                "tile cannot ever fit the run memory budget",
                                field="memory.run_budget_bytes",
                                tile_id=tile_id,
                                usage_bytes=accepted.total_bytes,
                                run_budget_bytes=self.config.run_budget_bytes,
                                precision_changed=False,
                            )
                            self.trace.emit("budget_rejected", tile_id=tile_id, stage=accepted.profile, usage_bytes=accepted.total_bytes)
                            break
                        if reserved_bytes + accepted.total_bytes > self.config.run_budget_bytes:
                            self.trace.emit(
                                "task_queued",
                                tile_id=tile_id,
                                stage=accepted.profile,
                                usage_bytes=accepted.total_bytes,
                                reserved_bytes=reserved_bytes,
                                reason="run_budget",
                            )
                            continue
                        del pending[tile_id]
                        item.estimate = accepted
                        reserved_bytes += accepted.total_bytes
                        future = executor.submit(task, tile_id, accepted.profile)
                        active[future] = _ActiveItem(
                            tile_id=tile_id,
                            commit_order=item.commit_order,
                            estimate=accepted,
                            future=future,
                        )
                        made_progress = True
                        self.trace.emit(
                            "task_submitted",
                            tile_id=tile_id,
                            commit_order=item.commit_order,
                            stage=accepted.profile,
                            usage_bytes=accepted.total_bytes,
                            reserved_bytes=reserved_bytes,
                        )
                    if failure is not None or self._cancel.is_set():
                        break
                    if made_progress:
                        continue
                    if active:
                        break
                    if pending:
                        first_tile = next(iter(pending))
                        failure = error(
                            E_BUDGET_EXCEEDED,
                            "no pending tile can fit the immutable run memory budget",
                            field="memory.run_budget_bytes",
                            tile_id=first_tile,
                            reserved_bytes=reserved_bytes,
                            run_budget_bytes=self.config.run_budget_bytes,
                            precision_changed=False,
                        )
                    break

                if not active:
                    if failure is not None:
                        break
                    if not pending:
                        break
                    continue

                done, _ = wait(active.keys(), timeout=0.05, return_when=FIRST_COMPLETED)
                for future in done:
                    item = active.pop(future)
                    reserved_bytes -= item.estimate.total_bytes
                    try:
                        result = future.result()
                    except BaseException as exc:
                        if failure is None:
                            failure = exc
                        self.trace.emit(
                            "task_failed",
                            tile_id=item.tile_id,
                            stage=item.estimate.profile,
                            error_type=type(exc).__name__,
                        )
                        continue
                    ready[item.commit_order] = (
                        _WorkItem(tile_id=item.tile_id, commit_order=item.commit_order, estimate=item.estimate),
                        result,
                    )
                    self.trace.emit(
                        "task_completed",
                        tile_id=item.tile_id,
                        commit_order=item.commit_order,
                        stage=item.estimate.profile,
                        usage_bytes=item.estimate.total_bytes,
                        reserved_bytes=reserved_bytes,
                    )

                while committed_order in ready:
                    work, result = ready.pop(committed_order)
                    try:
                        commit(work.tile_id, result)
                    except BaseException as exc:
                        if failure is None:
                            failure = exc
                        self.trace.emit(
                            "commit_failed",
                            tile_id=work.tile_id,
                            commit_order=committed_order,
                            error_type=type(exc).__name__,
                        )
                        break
                    results.append(result)
                    self.trace.emit(
                        "task_committed",
                        tile_id=work.tile_id,
                        commit_order=committed_order,
                        stage=work.estimate.profile if work.estimate else None,
                    )
                    committed_order += 1
                    if crash_hook is not None:
                        crash_hook(committed_order)
                if failure is not None:
                    break
        finally:
            if failure is not None:
                self._cancel.set()
                self.trace.emit(
                    "scheduler_aborted",
                    pending=len(pending),
                    active=len(active),
                    error_code=getattr(failure, "code", type(failure).__name__),
                )
            executor.shutdown(wait=True, cancel_futures=True)
            self.trace.emit(
                "scheduler_end",
                committed=len(results),
                reserved_bytes=reserved_bytes,
                ok=failure is None,
            )
        if failure is not None:
            raise failure
        return results
