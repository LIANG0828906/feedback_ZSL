from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ProbeError(Exception):
    """A stable, machine-readable error returned by every public surface."""

    code: str
    message: str
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {"ok": False, "code": self.code, "message": self.message, "details": self.details}


def scene_invalid(path: str, message: str, *, object_id: str | None = None) -> ProbeError:
    details: dict[str, Any] = {"path": path}
    if object_id is not None:
        details["object_id"] = object_id
    return ProbeError("E_SCENE_INVALID", message, details)


def sh_invalid(path: str, message: str) -> ProbeError:
    return ProbeError("E_SH_INVALID", message, {"path": path})


def interp_failed(message: str, probe_ids: list[str] | tuple[str, ...]) -> ProbeError:
    return ProbeError(
        "E_INTERP_FAILED",
        message,
        {"probe_ids": list(probe_ids), "affected_count": len(probe_ids)},
    )


def version_stale(message: str, **details: Any) -> ProbeError:
    return ProbeError("E_VERSION_STALE", message, details)


def recovery_invalid(path: str, message: str) -> ProbeError:
    return ProbeError("E_RECOVERY_INVALID", message, {"path": path})

