"""Bounded parallel shard scheduler."""

from __future__ import annotations

from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from dataclasses import dataclass
from typing import Any

from .errors import E_BUDGET_EXCEEDED, E_INVALID_ARGUMENT, GatewayError


@dataclass(frozen=True)
class ShardTask:
    number: int
    data: bytes
    expected_sha256: str | None = None
    expected_length: int | None = None


def schedule_shards(
    gateway: Any,
    session_id: str,
    tasks: list[ShardTask],
    *,
    max_inflight: int,
    space_budget: int,
    fault_phase: str | None = None,
) -> dict[str, Any]:
    """Write shards in parallel under an in-flight count and byte budget.

    The byte budget reserves bytes for shards currently being staged/replaced.
    Once a shard is durably confirmed in session metadata its reservation is
    released, so the budget bounds transient single-session disk pressure while
    the optional session quota independently bounds retained shard bytes.
    """

    if not isinstance(max_inflight, int) or max_inflight < 1:
        raise GatewayError(E_INVALID_ARGUMENT, "max_inflight must be at least 1")
    if not isinstance(space_budget, int) or space_budget < 1:
        raise GatewayError(E_INVALID_ARGUMENT, "space_budget must be at least 1")
    numbers = [task.number for task in tasks]
    if len(numbers) != len(set(numbers)):
        raise GatewayError(E_INVALID_ARGUMENT, "scheduler tasks contain duplicate shard numbers")
    for task in tasks:
        if len(task.data) > space_budget:
            raise GatewayError(
                E_BUDGET_EXCEEDED,
                f"shard {task.number} is larger than the entire space budget",
                shard=task.number,
                shard_bytes=len(task.data),
                space_budget=space_budget,
            )

    pending = sorted(tasks, key=lambda item: item.number)
    inflight: dict[Future[dict[str, Any]], ShardTask] = {}
    inflight_bytes = 0
    inflight_peak = 0
    budget_peak = 0
    wait_count = 0
    waited_tasks: set[int] = set()
    results: list[dict[str, Any]] = []
    errors: list[GatewayError] = []

    with ThreadPoolExecutor(max_workers=max_inflight) as executor:
        while pending or inflight:
            if not errors:
                while pending and len(inflight) < max_inflight:
                    candidate = pending[0]
                    candidate_bytes = len(candidate.data)
                    if inflight_bytes + candidate_bytes > space_budget:
                        if candidate.number not in waited_tasks:
                            waited_tasks.add(candidate.number)
                            wait_count += 1
                        break
                    pending.pop(0)
                    future = executor.submit(
                        gateway.write_shard,
                        session_id=session_id,
                        shard_number=candidate.number,
                        data=candidate.data,
                        expected_sha256=candidate.expected_sha256,
                        expected_length=candidate.expected_length,
                        fault_phase=fault_phase,
                    )
                    inflight[future] = candidate
                    inflight_bytes += candidate_bytes
                    inflight_peak = max(inflight_peak, len(inflight))
                    budget_peak = max(budget_peak, inflight_bytes)

            if not inflight:
                if pending:
                    raise GatewayError(
                        E_BUDGET_EXCEEDED,
                        "queued shard can never fit in the space budget",
                        shard=pending[0].number,
                        shard_bytes=len(pending[0].data),
                        space_budget=space_budget,
                    )
                break

            done, _ = wait(inflight, return_when=FIRST_COMPLETED)
            for future in sorted(done, key=lambda item: inflight[item].number):
                task = inflight.pop(future)
                inflight_bytes -= len(task.data)
                try:
                    results.append(future.result())
                except GatewayError as exc:
                    errors.append(exc)
                except BaseException as exc:  # pragma: no cover - defensive propagation
                    errors.append(
                        GatewayError(
                            E_INVALID_ARGUMENT,
                            f"shard {task.number} worker failed: {exc}",
                        )
                    )
            if errors:
                pending.clear()

    if errors:
        raise errors[0]
    results.sort(key=lambda item: item["shard_number"])
    return {
        "session_id": session_id,
        "submitted_shards": len(tasks),
        "completed_shards": len(results),
        "max_inflight": max_inflight,
        "space_budget": space_budget,
        "inflight_peak": inflight_peak,
        "budget_bytes_peak": budget_peak,
        "wait_count": wait_count,
        "results": results,
    }
