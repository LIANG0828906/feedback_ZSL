"""Entry-point anomaly injection.

Injectors mutate only the parsed protocol document (or a snapshot/baseline
file after the fact).  Settlement and snapshot modules remain untouched.
"""

from __future__ import annotations

import copy
import os
from decimal import Decimal
from typing import Any

from .errors import E_INPUT_INVALID, fail


SUPPORTED_RECORDING_INJECTIONS = (
    "dup_client_seq",
    "out_of_bounds_coord",
    "nonfinite_heading",
    "late_operation",
)
SUPPORTED_FINAL_INJECTIONS = (
    "truncated_snapshot",
    "corrupt_baseline",
)
SUPPORTED_INJECTIONS = SUPPORTED_RECORDING_INJECTIONS + SUPPORTED_FINAL_INJECTIONS


def _context(name: str, point: str) -> dict[str, Any]:
    return {"name": name, "point": point}


def apply_recording_injections(
    raw: Any,
    names: list[str],
) -> tuple[Any, dict[str, Any] | None]:
    payload = copy.deepcopy(raw)
    context: dict[str, Any] | None = None
    for name in names:
        if name not in SUPPORTED_RECORDING_INJECTIONS:
            fail(
                E_INPUT_INVALID,
                f"unsupported recording injection {name!r}",
                "inject",
                details={"supported": list(SUPPORTED_RECORDING_INJECTIONS)},
            )
        if name == "dup_client_seq":
            operations = payload.get("operations") or []
            if len(operations) < 2:
                fail(E_INPUT_INVALID, "need at least two operations", "inject.dup_client_seq")
            first = operations[0]
            victim_index = 1
            for index in range(1, len(operations)):
                if operations[index].get("entity_id") == first.get("entity_id"):
                    victim_index = index
                    break
            operations[victim_index]["client_seq"] = first.get("client_seq")
            context = _context(name, f"operations[{victim_index}].client_seq")
        elif name == "out_of_bounds_coord":
            entities = (payload.get("initial_state") or {}).get("entities") or []
            if not entities:
                fail(E_INPUT_INVALID, "need an initial entity", "inject.out_of_bounds_coord")
            limit = (payload.get("config") or {}).get("coord_limit", "1000")
            limit_value = Decimal(str(limit))
            entities[0]["position"]["x"] = str(limit_value * 2)
            context = _context(name, "initial_state.entities[0].position.x")
        elif name == "nonfinite_heading":
            operations = payload.get("operations") or []
            if not operations:
                fail(E_INPUT_INVALID, "need an operation", "inject.nonfinite_heading")
            operations[0]["heading"] = float("nan")
            context = _context(name, "operations[0].heading")
        elif name == "late_operation":
            operations = payload.get("operations") or []
            if not operations:
                fail(E_INPUT_INVALID, "need an operation", "inject.late_operation")
            window = (payload.get("config") or {}).get("late_window_steps", 0)
            effective = int(operations[0]["effective_step"])
            operations[0]["arrival_step"] = effective + int(window) + 1
            context = _context(name, "operations[0].arrival_step")
    return payload, context


def apply_baseline_injection(
    raw: Any,
    names: list[str],
) -> tuple[Any, dict[str, Any] | None]:
    payload = copy.deepcopy(raw)
    context: dict[str, Any] | None = None
    for name in names:
        if name != "corrupt_baseline":
            continue
        frames = payload.get("frames")
        if not isinstance(frames, list) or not frames:
            fail(
                "E_BASELINE_CORRUPT",
                "baseline has no frames to corrupt",
                "frames",
                injection=_context(name, "frames"),
            )
        frames[0].pop("state_hash", None)
        context = _context(name, "frames[0].state_hash")
    return payload, context


def truncate_latest_snapshot(directory: str) -> dict[str, Any]:
    from .snapshot import SNAPSHOT_NAME

    candidates: list[tuple[int, str]] = []
    for filename in os.listdir(directory):
        match = SNAPSHOT_NAME.match(filename)
        if match:
            candidates.append((int(match.group(1)), os.path.join(directory, filename)))
    if not candidates:
        fail(
            "E_RECOVERY_INVALID",
            "no snapshot available for truncation injection",
            directory,
            injection=_context("truncated_snapshot", directory),
        )
    candidates.sort()
    step, path = candidates[-1]
    with open(path, "rb") as handle:
        data = handle.read()
    if not data:
        fail(
            "E_RECOVERY_INVALID",
            "snapshot is already empty",
            path,
            step=step,
            injection=_context("truncated_snapshot", path),
        )
    with open(path, "wb") as handle:
        handle.write(data[: max(1, len(data) // 2)])
        handle.flush()
        os.fsync(handle.fileno())
    return _context("truncated_snapshot", path)
