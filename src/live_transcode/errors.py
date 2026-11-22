from __future__ import annotations

from dataclasses import dataclass, field as dataclass_field
from typing import Any


@dataclass
class TaskError(Exception):
    code: str
    message: str
    field: str | None = None
    details: dict[str, Any] = dataclass_field(default_factory=dict)

    def __str__(self) -> str:
        location = f" field={self.field}" if self.field else ""
        return f"{self.code}{location}: {self.message}"

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.field is not None:
            payload["field"] = self.field
        if self.details:
            payload["details"] = self.details
        return payload


E_WAV_INVALID = "E_WAV_INVALID"
E_SIDECAR_DRIFT = "E_SIDECAR_DRIFT"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_NOT_FOUND = "E_NOT_FOUND"
E_CANCELLED = "E_CANCELLED"
E_INVALID_ARGUMENT = "E_INVALID_ARGUMENT"


def wav_error(message: str, field: str, **details: Any) -> TaskError:
    return TaskError(E_WAV_INVALID, message, field=field, details=details)


def drift_error(
    message: str,
    first_slice: int,
    stale_side: str,
    **details: Any,
) -> TaskError:
    payload = {
        "first_drift_slice": first_slice,
        "stale_side": stale_side,
        **details,
    }
    return TaskError(E_SIDECAR_DRIFT, message, details=payload)


def version_error(message: str, field: str | None = None, **details: Any) -> TaskError:
    return TaskError(E_VERSION_STALE, message, field=field, details=details)


def recovery_error(message: str, slice_index: int | None = None, **details: Any) -> TaskError:
    payload = dict(details)
    if slice_index is not None:
        payload["slice_index"] = slice_index
    return TaskError(E_RECOVERY_INVALID, message, details=payload)

