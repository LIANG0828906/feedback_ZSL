"""Deterministic bounded-concurrency cover settlement scheduler."""

from __future__ import annotations

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from decimal import ROUND_CEILING, Decimal
from typing import Any

from .damage import shot_damage_units
from .errors import E_BUDGET_EXCEEDED, E_CANCELLED, ReplayError
from .model import CoverSpec, ReplayConfig, Shot


@dataclass(frozen=True)
class CoverDamageTask:
    submission_index: int
    cover: CoverSpec
    shots: tuple[Shot, ...]
    base_memory_bytes: int
    reserved_memory_bytes: int = 0
    degradation_scale: str = "1"


@dataclass(frozen=True)
class CoverDamageResult:
    task: CoverDamageTask
    damages: tuple[tuple[str, int], ...]


def _prepare_tasks(
    covers: tuple[CoverSpec, ...],
    shots: tuple[Shot, ...],
    config: ReplayConfig,
) -> tuple[list[CoverDamageTask], list[dict[str, Any]]]:
    tasks: list[CoverDamageTask] = []
    events: list[dict[str, Any]] = []
    shots_by_cover: dict[str, list[Shot]] = {cover.cover_id: [] for cover in covers}
    for shot in shots:
        shots_by_cover[shot.cover_id].append(shot)
    for cover in covers:
        ordered_shots = tuple(
            sorted(
                shots_by_cover[cover.cover_id],
                key=lambda item: (item.player_id, item.client_seq, item.arrival_seq, item.index),
            )
        )
        base_memory = cover.temporary_buffer_bytes
        reserved_memory = base_memory
        scale_text = "1"
        if base_memory > config.memory_budget_bytes:
            selected = None
            attempts: list[dict[str, Any]] = []
            for scale in config.degradation_sequence:
                candidate = max(
                    1,
                    int(
                        (Decimal(base_memory) * scale).to_integral_value(
                            rounding=ROUND_CEILING
                        )
                    ),
                )
                attempts.append(
                    {
                        "scale": str(scale),
                        "candidate_memory_bytes": candidate,
                    }
                )
                if candidate <= config.memory_budget_bytes:
                    selected = (candidate, scale)
                    break
            if selected is None:
                raise ReplayError(
                    E_BUDGET_EXCEEDED,
                    "cover temporary buffer exceeds memory budget after all degradations",
                    cover.record_index,
                    f"covers[{cover.record_index}].temporary_buffer_bytes",
                    {
                        "cover_id": cover.cover_id,
                        "base_memory_bytes": base_memory,
                        "memory_budget_bytes": config.memory_budget_bytes,
                        "degradation_attempts": attempts,
                    },
                )
            reserved_memory, selected_scale = selected
            scale_text = str(selected_scale)
            events.append(
                {
                    "event": "degrade",
                    "cover_id": cover.cover_id,
                    "base_memory_bytes": base_memory,
                    "reserved_memory_bytes": reserved_memory,
                    "scale": scale_text,
                }
            )
        tasks.append(
            CoverDamageTask(
                submission_index=len(tasks),
                cover=cover,
                shots=ordered_shots,
                base_memory_bytes=base_memory,
                reserved_memory_bytes=reserved_memory,
                degradation_scale=scale_text,
            )
        )
    return tasks, events


def _execute_task(task: CoverDamageTask, config: ReplayConfig) -> CoverDamageResult:
    damages = tuple(
        (shot.key, shot_damage_units(shot, task.cover, config)) for shot in task.shots
    )
    return CoverDamageResult(task=task, damages=damages)


def run_damage_schedule(
    covers: tuple[CoverSpec, ...],
    shots: tuple[Shot, ...],
    config: ReplayConfig,
    cancel_event: threading.Event | None = None,
) -> tuple[dict[str, dict[str, int]], dict[str, Any]]:
    """Settle cover blocks in stable submission order under fixed limits.

    Work is admitted in stable cover order and executed in deterministic waves.
    A wave never exceeds either the configured worker count or memory budget.
    Completion records are emitted in submission order, not completion order.
    """

    cancel_event = cancel_event or threading.Event()
    if cancel_event.is_set():
        raise ReplayError(E_CANCELLED, "replay cancelled before step settlement")
    tasks, prep_events = _prepare_tasks(covers, shots, config)
    events: list[dict[str, Any]] = list(prep_events)
    pending = list(tasks)
    results: dict[str, dict[str, int]] = {}
    committed: list[dict[str, Any]] = []
    peak_memory = 0
    wave_number = 0
    queued_reported: set[int] = set()

    with ThreadPoolExecutor(
        max_workers=config.parallelism,
        thread_name_prefix="cover-settle",
    ) as executor:
        while pending:
            if cancel_event.is_set():
                raise ReplayError(E_CANCELLED, "replay cancelled during step settlement")
            wave: list[CoverDamageTask] = []
            used_memory = 0
            while pending and len(wave) < config.parallelism:
                candidate = pending[0]
                if used_memory + candidate.reserved_memory_bytes > config.memory_budget_bytes:
                    if candidate.submission_index not in queued_reported:
                        events.append(
                            {
                                "event": "queue",
                                "wave": wave_number,
                                "submission_index": candidate.submission_index,
                                "cover_id": candidate.cover.cover_id,
                                "reason": "memory_budget",
                                "in_use_bytes": used_memory,
                                "requested_bytes": candidate.reserved_memory_bytes,
                            }
                        )
                        queued_reported.add(candidate.submission_index)
                    break
                pending.pop(0)
                wave.append(candidate)
                used_memory += candidate.reserved_memory_bytes
            if not wave:
                # Preflight guarantees each individual task fits an empty budget.
                raise ReplayError(
                    E_BUDGET_EXCEEDED,
                    "scheduler could not admit a task within the configured budget",
                    None,
                    "config.memory_budget_bytes",
                    {
                        "next_cover": pending[0].cover.cover_id,
                        "requested_bytes": pending[0].reserved_memory_bytes,
                    },
                )
            peak_memory = max(peak_memory, used_memory)
            events.append(
                {
                    "event": "wave_start",
                    "wave": wave_number,
                    "submission_indices": [task.submission_index for task in wave],
                    "reserved_memory_bytes": used_memory,
                }
            )
            for task in wave:
                events.append(
                    {
                        "event": "submit",
                        "wave": wave_number,
                        "submission_index": task.submission_index,
                        "cover_id": task.cover.cover_id,
                        "reserved_memory_bytes": task.reserved_memory_bytes,
                    }
                )
            futures = [executor.submit(_execute_task, task, config) for task in wave]
            wave_results = [future.result() for future in futures]
            if cancel_event.is_set():
                raise ReplayError(
                    E_CANCELLED,
                    "replay cancelled before the completed wave was published",
                )
            for result in wave_results:
                task = result.task
                results[task.cover.cover_id] = dict(result.damages)
                committed.append(
                    {
                        "submission_index": task.submission_index,
                        "cover_id": task.cover.cover_id,
                        "base_memory_bytes": task.base_memory_bytes,
                        "reserved_memory_bytes": task.reserved_memory_bytes,
                        "degradation_scale": task.degradation_scale,
                    }
                )
                events.append(
                    {
                        "event": "commit",
                        "wave": wave_number,
                        "submission_index": task.submission_index,
                        "cover_id": task.cover.cover_id,
                    }
                )
            events.append(
                {
                    "event": "wave_release",
                    "wave": wave_number,
                    "released_memory_bytes": used_memory,
                }
            )
            wave_number += 1

    budget_usage = {
        "parallelism": config.parallelism,
        "memory_budget_bytes": config.memory_budget_bytes,
        "peak_memory_bytes": peak_memory,
        "wave_count": wave_number,
        "tasks": committed,
        "events": events,
    }
    return results, budget_usage

