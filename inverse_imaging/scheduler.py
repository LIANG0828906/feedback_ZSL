"""Stable shared resource pool with explicit memory budgeting and degradation."""

from __future__ import annotations

import threading
import inspect
import json
import os
import tempfile
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping

from .errors import E_BUDGET_EXCEEDED, AppError
from .params import canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class BudgetConfig:
    max_parallel: int = 2
    max_memory_mb: float = 512.0
    default_task_quota_mb: float | None = None

    def __post_init__(self) -> None:
        if self.max_parallel < 1:
            raise ValueError("max_parallel must be at least one")
        if self.max_memory_mb <= 0.0:
            raise ValueError("max_memory_mb must be positive")
        if (
            self.default_task_quota_mb is not None
            and self.default_task_quota_mb <= 0.0
        ):
            raise ValueError("default_task_quota_mb must be positive")


def estimate_chunk_memory_mb(
    chunk_size: int,
    *,
    channels: int = 3,
    working_copies: int = 12,
    base_mb: float = 8.0,
) -> float:
    """Conservative deterministic estimate used before a task is admitted."""
    byte_count = base_mb * 1024.0 * 1024.0
    byte_count += (
        max(1, int(chunk_size))
        * max(1, int(chunk_size))
        * channels
        * 8
        * max(1, int(working_copies))
    )
    return byte_count / (1024.0 * 1024.0)


@dataclass
class ResourceJob:
    sequence: int
    name: str
    function: Callable[[], Any] = field(repr=False)
    requested_memory_mb: float
    memory_mb: float
    task_quota_mb: float
    chunk_size: int
    degraded_from: int | None = None
    metadata: dict[str, Any] = field(default_factory=dict)
    status: str = "waiting"
    result: Any = None
    error: BaseException | None = None

    def public(self) -> dict[str, Any]:
        return {
            "sequence": self.sequence,
            "name": self.name,
            "requested_memory_mb": self.requested_memory_mb,
            "memory_mb": self.memory_mb,
            "task_quota_mb": self.task_quota_mb,
            "chunk_size": self.chunk_size,
            "degraded_from": self.degraded_from,
            "status": self.status,
            "metadata": dict(self.metadata),
        }


class ResourcePool:
    """Execute submitted jobs with stable submission-order admission.

    A job occupies one parallel slot. Its reservation is checked against the
    fixed global memory budget and it can only shrink through its declared
    degradation sequence before submission is rejected.
    """

    def __init__(self, config: BudgetConfig) -> None:
        self.config = config
        self._lock = threading.RLock()
        self._next_sequence = 0
        self._jobs: list[ResourceJob] = []
        self._events: list[dict[str, Any]] = []
        self._reserved_memory_mb = 0.0

    @property
    def reserved_memory_mb(self) -> float:
        with self._lock:
            return self._reserved_memory_mb

    def submit(
        self,
        function: Callable[[], Any],
        name: str,
        *,
        chunk_size: int = 16,
        requested_memory_mb: float | None = None,
        task_quota_mb: float | None = None,
        degradation_sequence: Iterable[int] = (16, 12, 8, 6, 4, 2, 1),
        metadata: Mapping[str, Any] | None = None,
    ) -> ResourceJob:
        with self._lock:
            sequence = self._next_sequence
            self._next_sequence += 1
            quota = (
                task_quota_mb
                if task_quota_mb is not None
                else self.config.default_task_quota_mb
                or self.config.max_memory_mb
            )
            requested = (
                float(requested_memory_mb)
                if requested_memory_mb is not None
                else estimate_chunk_memory_mb(chunk_size)
            )
            selected_chunk = int(chunk_size)
            selected_memory = requested
            degraded_from: int | None = None
            if selected_memory > quota:
                degraded_from = selected_chunk
                candidates = [
                    int(size)
                    for size in degradation_sequence
                    if int(size) < selected_chunk and int(size) > 0
                ]
                candidates = sorted(set(candidates), reverse=True)
                for candidate in candidates:
                    candidate_memory = (
                        estimate_chunk_memory_mb(candidate)
                        if requested_memory_mb is None
                        else requested * (candidate / selected_chunk) ** 2
                    )
                    if candidate_memory <= quota:
                        selected_chunk = candidate
                        selected_memory = candidate_memory
                        break
                else:
                    self._event(
                        "rejected",
                        {
                            "sequence": sequence,
                            "name": name,
                            "reason": "task_quota",
                            "requested_memory_mb": requested,
                            "task_quota_mb": quota,
                            "chunk_size": int(chunk_size),
                        },
                    )
                    raise AppError(
                        E_BUDGET_EXCEEDED,
                        "task chunk cannot fit its memory quota",
                        "task_quota_mb",
                        {
                            "sequence": sequence,
                            "requested_memory_mb": requested,
                            "task_quota_mb": quota,
                        },
                    )
            if selected_memory > self.config.max_memory_mb:
                self._event(
                    "rejected",
                    {
                        "sequence": sequence,
                        "name": name,
                        "reason": "global_budget",
                        "memory_mb": selected_memory,
                        "global_budget_mb": self.config.max_memory_mb,
                    },
                )
                raise AppError(
                    E_BUDGET_EXCEEDED,
                    "task can never fit the global memory budget",
                    "memory_mb",
                    {
                        "sequence": sequence,
                        "memory_mb": selected_memory,
                        "global_budget_mb": self.config.max_memory_mb,
                    },
                )
            job = ResourceJob(
                sequence=sequence,
                name=name,
                function=function,
                requested_memory_mb=requested,
                memory_mb=selected_memory,
                task_quota_mb=quota,
                chunk_size=selected_chunk,
                degraded_from=degraded_from,
                metadata=dict(metadata or {}),
            )
            self._jobs.append(job)
            self._event("submitted", job.public())
            if degraded_from is not None:
                self._event(
                    "degraded",
                    {
                        "sequence": sequence,
                        "name": name,
                        "from_chunk_size": degraded_from,
                        "to_chunk_size": selected_chunk,
                        "memory_mb": selected_memory,
                        "task_quota_mb": quota,
                    },
                )
            return job

    def run_all(self) -> list[ResourceJob]:
        pending = sorted(
            (job for job in self._jobs if job.status == "waiting"),
            key=lambda job: job.sequence,
        )
        running: dict[Future[Any], ResourceJob] = {}
        with ThreadPoolExecutor(max_workers=self.config.max_parallel) as executor:
            while pending or running:
                admitted = True
                while admitted and pending and len(running) < self.config.max_parallel:
                    candidate = pending[0]
                    with self._lock:
                        can_fit = (
                            self._reserved_memory_mb + candidate.memory_mb
                            <= self.config.max_memory_mb + 1.0e-12
                        )
                    if not can_fit:
                        self._event(
                            "waiting",
                            {
                                "sequence": candidate.sequence,
                                "name": candidate.name,
                                "reason": "global_memory_budget",
                                "reserved_memory_mb": self._reserved_memory_mb,
                                "requested_memory_mb": candidate.memory_mb,
                            },
                        )
                        admitted = False
                        break
                    pending.pop(0)
                    with self._lock:
                        candidate.status = "running"
                        self._reserved_memory_mb += candidate.memory_mb
                    self._event("admitted", candidate.public())
                    future = executor.submit(self._invoke, candidate)
                    running[future] = candidate
                if not running:
                    if pending:
                        blocked = pending[0]
                        with self._lock:
                            blocked.status = "rejected"
                        self._event(
                            "rejected",
                            {
                                "sequence": blocked.sequence,
                                "name": blocked.name,
                                "reason": "global_memory_budget_no_progress",
                                "memory_mb": blocked.memory_mb,
                            },
                        )
                        blocked.error = AppError(
                            E_BUDGET_EXCEEDED,
                            "job cannot fit the global memory budget",
                            "memory_mb",
                            {
                                "sequence": blocked.sequence,
                                "memory_mb": blocked.memory_mb,
                                "global_budget_mb": self.config.max_memory_mb,
                            },
                        )
                        pending.pop(0)
                        continue
                    break
                done, _ = wait(running, return_when=FIRST_COMPLETED)
                for future in sorted(done, key=lambda item: running[item].sequence):
                    job = running.pop(future)
                    with self._lock:
                        self._reserved_memory_mb -= job.memory_mb
                    try:
                        job.result = future.result()
                        job.status = "completed"
                        self._event("completed", job.public())
                    except BaseException as exc:  # propagate only through the job
                        job.error = exc
                        job.status = "failed"
                        self._event(
                            "failed",
                            {
                                **job.public(),
                                "error": repr(exc),
                            },
                        )
        return list(self._jobs)

    def events(self) -> list[dict[str, Any]]:
        with self._lock:
            return [dict(event) for event in self._events]

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            return {
                "budget": {
                    "max_parallel": self.config.max_parallel,
                    "max_memory_mb": self.config.max_memory_mb,
                    "default_task_quota_mb": self.config.default_task_quota_mb,
                },
                "reserved_memory_mb": self._reserved_memory_mb,
                "jobs": [job.public() for job in self._jobs],
                "queue": [
                    job.public() for job in self._jobs if job.status == "waiting"
                ],
            }

    def _event(self, event_type: str, payload: Mapping[str, Any]) -> None:
        with self._lock:
            event = {"event": event_type, **dict(payload)}
            event["content_hash"] = sha256_bytes(canonical_json_bytes(event))
            self._events.append(event)

    def write_events(self, path: str | os.PathLike[str]) -> str:
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        lines = [
            json.dumps(
                event,
                sort_keys=True,
                separators=(",", ":"),
                ensure_ascii=True,
                allow_nan=False,
            )
            for event in self.events()
        ]
        payload = ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{target.name}.", suffix=".tmp", dir=str(target.parent)
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, target)
        except OSError:
            temporary.unlink(missing_ok=True)
            raise
        return sha256_bytes(payload)

    @staticmethod
    def _invoke(job: ResourceJob) -> Any:
        try:
            signature = inspect.signature(job.function)
            positional = [
                parameter
                for parameter in signature.parameters.values()
                if parameter.kind
                in (
                    inspect.Parameter.POSITIONAL_ONLY,
                    inspect.Parameter.POSITIONAL_OR_KEYWORD,
                )
            ]
        except (TypeError, ValueError):
            positional = []
        if positional:
            return job.function(job)  # type: ignore[call-arg]
        return job.function()


def run_candidates(
    config: BudgetConfig,
    candidates: Iterable[tuple[str, Callable[[], Any]]],
    *,
    chunk_size: int = 16,
    task_quota_mb: float | None = None,
) -> tuple[ResourcePool, list[ResourceJob]]:
    pool = ResourcePool(config)
    for name, function in candidates:
        try:
            pool.submit(
                function,
                name,
                chunk_size=chunk_size,
                task_quota_mb=task_quota_mb,
            )
        except AppError:
            continue
    return pool, pool.run_all()

