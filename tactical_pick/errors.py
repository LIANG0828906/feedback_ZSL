"""Business error model shared by every entry point."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Mapping


E_SCENE_INVALID = "E_SCENE_INVALID"
E_RAY_INVALID = "E_RAY_INVALID"
E_VERSION_STALE = "E_VERSION_STALE"
E_STALE_HANDLE = "E_STALE_HANDLE"
E_REGION_INVALID = "E_REGION_INVALID"
E_TRACE_INVALID = "E_TRACE_INVALID"
E_SNAPSHOT_INVALID = "E_SNAPSHOT_INVALID"
E_BATCH_CONFLICT = "E_BATCH_CONFLICT"
E_CANCELLED = "E_CANCELLED"
E_GUI_UNAVAILABLE = "E_GUI_UNAVAILABLE"

W_RAY_GRAZING = "W_RAY_GRAZING"

E_REGION_UNWALKABLE = "E_REGION_UNWALKABLE"
E_REGION_UNIT_OVERLAP = "E_REGION_UNIT_OVERLAP"
E_REGION_COVER_OVERLAP = "E_REGION_COVER_OVERLAP"

E_INJECT_DEGENERATE_BOUNDS = "E_INJECT_DEGENERATE_BOUNDS"
E_INJECT_ZERO_RAY = "E_INJECT_ZERO_RAY"
E_INJECT_SCREEN_RANGE = "E_INJECT_SCREEN_RANGE"
E_INJECT_SELF_INTERSECT = "E_INJECT_SELF_INTERSECT"
E_INJECT_CORRUPT_SNAPSHOT = "E_INJECT_CORRUPT_SNAPSHOT"
E_INJECT_UNKNOWN = "E_INJECT_UNKNOWN"

INJECTION_CODES = {
    "degenerate-bounds": E_INJECT_DEGENERATE_BOUNDS,
    "zero-ray": E_INJECT_ZERO_RAY,
    "out-of-range-screen": E_INJECT_SCREEN_RANGE,
    "self-intersect-region": E_INJECT_SELF_INTERSECT,
    "corrupt-snapshot": E_INJECT_CORRUPT_SNAPSHOT,
}


@dataclass
class PickError(Exception):
    """A stable, machine-readable failure with a locatable field.

    ``location`` is deliberately a plain mapping so it can be written as
    canonical JSON without bespoke encoders.  Common keys are ``field``,
    ``entity_id``, ``operation`` and ``injection_point``.
    """

    code: str
    message: str
    location: Mapping[str, Any] = field(default_factory=dict)
    cause_code: str | None = None

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)
        self.location = dict(self.location)

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "ok": False,
            "code": self.code,
            "message": self.message,
            "location": dict(self.location),
        }
        if self.cause_code is not None:
            payload["cause_code"] = self.cause_code
        return payload


def fail(
    code: str,
    message: str,
    *,
    field: str | None = None,
    entity_id: int | str | None = None,
    **location: Any,
) -> None:
    """Raise a :class:`PickError` from a compact call site."""

    details = dict(location)
    if field is not None:
        details["field"] = field
    if entity_id is not None:
        details["entity_id"] = entity_id
    raise PickError(code=code, message=message, location=details)
