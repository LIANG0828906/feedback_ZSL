"""Parallel sentence precomputation with stable, ordered commit."""

from __future__ import annotations

import threading
from concurrent.futures import Future, ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import Any, Callable, Iterable

from .errors import LCFTError
from .estimator import estimate_sentence
from .params import LayoutParams
from .timebase import NormalizedSubtitle


class TaskRegistry:
    """Process-local reservation table; duplicate live track starts return E_BUSY."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._active: dict[str, str] = {}

    def acquire(self, track_id: str, job_id: str) -> None:
        with self._lock:
            existing = self._active.get(track_id)
            if existing is not None:
                raise LCFTError(
                    "E_BUSY",
                    "the same track already has a running layout task",
                    field="track_id",
                    details={"track_id": track_id, "active_job": existing},
                )
            self._active[track_id] = job_id

    def release(self, track_id: str, job_id: str) -> None:
        with self._lock:
            if self._active.get(track_id) == job_id:
                del self._active[track_id]

    def active(self) -> dict[str, str]:
        with self._lock:
            return dict(self._active)


GLOBAL_TASK_REGISTRY = TaskRegistry()


@dataclass
class PrecomputeResult:
    sentences: list[dict[str, Any]]
    descriptions: list[dict[str, Any]]
    dispatch_decisions: list[dict[str, Any]] = field(default_factory=list)


def _event(
    callback: Callable[[dict[str, Any]], None] | None, value: dict[str, Any]
) -> None:
    if callback is not None:
        callback(value)


def precompute_track(
    track_id: str,
    sentences: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    descriptions: tuple[NormalizedSubtitle, ...] | list[NormalizedSubtitle],
    params: LayoutParams,
    ticks_per_second: int,
    *,
    workers: int = 1,
    cancel_event: threading.Event | None = None,
    on_event: Callable[[dict[str, Any]], None] | None = None,
) -> PrecomputeResult:
    if workers < 1:
        raise ValueError("workers must be at least 1")
    speech_items = tuple(sorted(sentences, key=lambda item: (item.start_sample, item.id)))
    description_items = tuple(
        sorted(descriptions, key=lambda item: (item.start_sample, item.id))
    )
    jobs: list[tuple[str, int, NormalizedSubtitle, bool]] = []
    for order, item in enumerate(speech_items):
        jobs.append(("sentence", order, item, False))
    for order, item in enumerate(description_items):
        jobs.append(("description", order, item, True))
    decisions: list[dict[str, Any]] = []
    for kind, order, item, description in jobs:
        decision = {
            "event": "precompute_dispatched",
            "track": track_id,
            "kind": kind,
            "sentence_id": item.id,
            "order": order,
            "parallel_eligible": True,
            "ordered_commit": True,
        }
        decisions.append(decision)
        _event(on_event, decision)

    def run_one(
        kind: str, order: int, item: NormalizedSubtitle, description: bool
    ) -> tuple[str, int, dict[str, Any]]:
        if cancel_event is not None and cancel_event.is_set():
            raise LCFTError(
                "E_CANCELLED",
                "precomputation was cooperatively cancelled",
                field="cancel_event",
                details={"track_id": track_id, "sentence_id": item.id},
            )
        estimate = estimate_sentence(
            item,
            params,
            ticks_per_second,
            description=description,
            order=order,
        )
        return kind, order, estimate

    completed: dict[tuple[str, int], dict[str, Any]] = {}
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="lcft-precompute") as pool:
        future_map: dict[Future[tuple[str, int, dict[str, Any]]], tuple[str, int, str]] = {}
        for kind, order, item, description in jobs:
            future = pool.submit(run_one, kind, order, item, description)
            future_map[future] = (kind, order, item.id)
        try:
            for future in as_completed(future_map):
                kind, order, item_id = future_map[future]
                if cancel_event is not None and cancel_event.is_set():
                    for pending in future_map:
                        pending.cancel()
                    raise LCFTError(
                        "E_CANCELLED",
                        "precomputation was cooperatively cancelled",
                        field="cancel_event",
                        details={"track_id": track_id, "sentence_id": item_id},
                    )
                result_kind, result_order, estimate = future.result()
                completed[(result_kind, result_order)] = estimate
        except BaseException:
            for pending in future_map:
                pending.cancel()
            raise

    speech_results = [completed[("sentence", order)] for order in range(len(speech_items))]
    description_results = [
        completed[("description", order)] for order in range(len(description_items))
    ]
    commit = {
        "event": "precompute_committed",
        "track": track_id,
        "rows": len(speech_results) + len(description_results),
        "ordered": True,
    }
    decisions.append(commit)
    _event(on_event, commit)
    return PrecomputeResult(
        sentences=speech_results,
        descriptions=description_results,
        dispatch_decisions=decisions,
    )

