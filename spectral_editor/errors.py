from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_MATERIAL_INVALID = "E_MATERIAL_INVALID"
E_PIPELINE_UNINIT = "E_PIPELINE_UNINIT"
E_VERSION_CONFLICT = "E_VERSION_CONFLICT"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"
E_BUSY = "E_BUSY"
E_SNAPSHOT_STALE = "E_SNAPSHOT_STALE"
E_CANCELLED = "E_CANCELLED"


@dataclass
class BusinessError(Exception):
    code: str
    message: str
    path: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        location = f" at {self.path}" if self.path else ""
        return f"{self.code}: {self.message}{location}"

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.path:
            payload["path"] = self.path
        if self.details:
            payload["details"] = self.details
        return payload


def material_invalid(path: str, message: str) -> BusinessError:
    return BusinessError(E_MATERIAL_INVALID, message, path)


def pipeline_uninit(path: str, message: str) -> BusinessError:
    return BusinessError(E_PIPELINE_UNINIT, message, path)


def version_conflict(message: str, path: str | None = None) -> BusinessError:
    return BusinessError(E_VERSION_CONFLICT, message, path)


def recovery_invalid(message: str, path: str | None = None) -> BusinessError:
    return BusinessError(E_RECOVERY_INVALID, message, path)


def busy(message: str = "a recompute task is already running") -> BusinessError:
    return BusinessError(E_BUSY, message)


def snapshot_stale(message: str = "snapshot handle is stale or already released") -> BusinessError:
    return BusinessError(E_SNAPSHOT_STALE, message)


def cancelled(message: str = "recompute was cancelled") -> BusinessError:
    return BusinessError(E_CANCELLED, message)
