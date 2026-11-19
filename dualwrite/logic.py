from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class StatusDecision:
    adopt: bool
    incoming_key: tuple[int, int]
    current_key: tuple[int, int]
    basis: str


def status_decision(
    *,
    incoming_version: int,
    incoming_site: str,
    current_version: int,
    current_site: str,
    priorities: dict[str, int],
) -> StatusDecision:
    incoming_key = (int(incoming_version), int(priorities[incoming_site]))
    current_key = (int(current_version), int(priorities[current_site]))
    if incoming_key > current_key:
        basis = "incoming_row_version_newer" if incoming_key[0] != current_key[0] else "equal_version_site_priority"
        return StatusDecision(True, incoming_key, current_key, basis)
    if incoming_key == current_key:
        basis = "equal_version_and_site"
    else:
        basis = "incoming_row_version_older" if incoming_key[0] != current_key[0] else "equal_version_lower_site_priority"
    return StatusDecision(False, incoming_key, current_key, basis)


def is_duplicate_key(change_key: str | None, seen: set[str]) -> bool:
    return bool(change_key) and change_key in seen


def json_safe(value: Any) -> Any:
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, dict):
        return {str(key): json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(item) for item in value]
    if hasattr(value, "isoformat"):
        return value.isoformat()
    return str(value)
