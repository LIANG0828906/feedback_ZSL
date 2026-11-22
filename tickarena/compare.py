"""Automatic tick-vs-batch trace comparison."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from .codes import E_DIVERGE, E_MORE, E_OK, E_STATE, Result
from .diff import Difference, first_difference
from .engine import Engine, EngineLimits
from .events import InMemoryEventSource


@dataclass(frozen=True)
class ModeRun:
    code: str
    engine: Engine
    trace: list[dict[str, Any]]
    message: str = ""


def run_mode(
    initial_state: Any,
    events: list[dict[str, Any]],
    mode: str,
    *,
    batch_ticks: int = 64,
    limits: EngineLimits | None = None,
    snapshot_interval: int = 100,
) -> ModeRun:
    if mode not in {"tick", "batch"}:
        return ModeRun(E_STATE, Engine.__new__(Engine), [], f"unknown mode {mode}")
    source = InMemoryEventSource(initial_state, events)
    engine = Engine(
        initial_state,
        source,
        snapshot_interval=snapshot_interval,
        limits=limits,
        capture_trace=True,
    )
    if mode == "tick":
        while not engine.done:
            step = engine.step()
            if step.code != E_OK:
                return ModeRun(step.code, engine, engine.trace or [], step.message)
    else:
        while not engine.done:
            batch = engine.run_batch(batch_ticks)
            if batch.code not in {E_OK, E_MORE}:
                return ModeRun(batch.code, engine, engine.trace or [], batch.message)
    return ModeRun(E_OK, engine, engine.trace or [])


def compare_modes(
    initial_state: Any,
    events: list[dict[str, Any]],
    *,
    batch_ticks: int = 64,
    limits: EngineLimits | None = None,
) -> Result:
    left = run_mode(
        initial_state,
        events,
        "tick",
        batch_ticks=batch_ticks,
        limits=limits,
    )
    right = run_mode(
        initial_state,
        events,
        "batch",
        batch_ticks=batch_ticks,
        limits=limits,
    )
    if left.code != E_OK:
        return Result(left.code, f"tick mode failed: {left.message}")
    if right.code != E_OK:
        return Result(right.code, f"batch mode failed: {right.message}")
    max_ticks = max(len(left.trace), len(right.trace))
    for index in range(max_ticks):
        if index >= len(left.trace):
            return Result(
                E_DIVERGE,
                "tick mode trace ended earlier",
                tick=index,
                rule="tick_record",
                recorded=None,
                recomputed=right.trace[index],
            )
        if index >= len(right.trace):
            return Result(
                E_DIVERGE,
                "batch mode trace ended earlier",
                tick=index,
                rule="tick_record",
                recorded=left.trace[index],
                recomputed=None,
            )
        diff = first_difference(left.trace[index], right.trace[index])
        if diff is not None:
            return Result(
                E_DIVERGE,
                "tick and batch traces diverged",
                tick=index,
                rule=diff.path,
                recorded=diff.recorded,
                recomputed=diff.recomputed,
            )
    final_diff = first_difference(
        left.engine.full_snapshot(),
        right.engine.full_snapshot(),
    )
    if final_diff is not None:
        return Result(
            E_DIVERGE,
            "final states diverged",
            tick=max(left.engine.state.tick, right.engine.state.tick),
            rule=final_diff.path,
            recorded=final_diff.recorded,
            recomputed=final_diff.recomputed,
        )
    return Result(
        E_OK,
        "tick and batch traces are identical",
        tick=left.engine.state.tick,
    )

