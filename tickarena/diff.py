"""Structural difference diagnostics shared by mode and replay comparison."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class Difference:
    path: str
    recorded: Any
    recomputed: Any


def first_difference(recorded: Any, recomputed: Any, path: str = "") -> Difference | None:
    """Return the first structural difference with a stable field path."""

    if type(recorded) is not type(recomputed):
        return Difference(path or "$", recorded, recomputed)
    if isinstance(recorded, dict):
        recorded_keys = sorted(recorded)
        recomputed_keys = sorted(recomputed)
        if recorded_keys != recomputed_keys:
            missing_left = [key for key in recomputed_keys if key not in recorded]
            missing_right = [key for key in recorded_keys if key not in recomputed]
            token = missing_left[0] if missing_left else missing_right[0]
            child_path = f"{path}.{token}" if path else str(token)
            return Difference(
                child_path,
                recorded.get(token),
                recomputed.get(token),
            )
        for key in recorded_keys:
            child_path = f"{path}.{key}" if path else str(key)
            diff = first_difference(recorded[key], recomputed[key], child_path)
            if diff is not None:
                return diff
        return None
    if isinstance(recorded, (list, tuple)):
        if len(recorded) != len(recomputed):
            return Difference(path or "$", len(recorded), len(recomputed))
        for index, (left, right) in enumerate(zip(recorded, recomputed)):
            child_path = f"{path}[{index}]" if path else f"[{index}]"
            diff = first_difference(left, right, child_path)
            if diff is not None:
                return diff
        return None
    if recorded != recomputed:
        return Difference(path or "$", recorded, recomputed)
    return None

