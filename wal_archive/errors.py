"""Stable business errors used by the archive service.

The numeric/string codes are part of the public contract.  Do not renumber
them: operators and automation use them to distinguish malformed archive
entries from transient or request-level failures.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ArchiveError(Exception):
    code: str
    name: str
    message: str
    rel_path: str | None = None
    http_status: int = 400
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "name": self.name,
            "message": self.message,
        }
        if self.rel_path is not None:
            payload["path"] = self.rel_path
        if self.details:
            payload["details"] = self.details
        return payload


def err(
    code: str,
    name: str,
    message: str,
    *,
    rel_path: str | None = None,
    http_status: int = 400,
    details: dict[str, Any] | None = None,
) -> ArchiveError:
    return ArchiveError(
        code=code,
        name=name,
        message=message,
        rel_path=rel_path,
        http_status=http_status,
        details=details or {},
    )


E_ARCHIVE_ROOT_MISSING = ("WAL001", "E_ARCHIVE_ROOT_MISSING")
E_LAYOUT_UNEXPECTED = ("WAL002", "E_LAYOUT_UNEXPECTED")
E_WAL_NAME_INVALID = ("WAL003", "E_WAL_NAME_INVALID")
E_SEGMENT_READ_ERROR = ("WAL004", "E_SEGMENT_READ_ERROR")
E_SEGMENT_CHANGED = ("WAL005", "E_SEGMENT_CHANGED_DURING_SCAN")
E_TIMELINE_MARKER_INVALID = ("WAL006", "E_TIMELINE_MARKER_INVALID")
E_INVALID_REQUEST = ("WAL007", "E_INVALID_REQUEST")
E_SOURCE_INSTANCE_UNKNOWN = ("WAL008", "E_SOURCE_INSTANCE_UNKNOWN")
E_TIMELINE_UNKNOWN = ("WAL009", "E_TIMELINE_UNKNOWN")
E_NO_RECOVERY_POINT = ("WAL010", "E_NO_RECOVERY_POINT")
E_RECOVERY_GAP = ("WAL011", "E_RECOVERY_GAP")
E_SEGMENT_TRUNCATED = ("WAL012", "E_SEGMENT_TRUNCATED")
E_SEGMENT_CHECKSUM_FAILED = ("WAL013", "E_SEGMENT_CHECKSUM_FAILED")
E_RECOVERY_VERIFY_FAILED = ("WAL014", "E_RECOVERY_VERIFY_FAILED")
E_DATABASE_ERROR = ("WAL015", "E_DATABASE_ERROR")
E_DATABASE_BUSY = ("WAL016", "E_DATABASE_BUSY")
E_METHOD_NOT_ALLOWED = ("WAL017", "E_METHOD_NOT_ALLOWED")
E_NOT_FOUND = ("WAL018", "E_NOT_FOUND")
E_INTERNAL = ("WAL019", "E_INTERNAL")
E_UNSAFE_PATH = ("WAL020", "E_UNSAFE_PATH")


def root_missing(root_label: str) -> ArchiveError:
    code, name = E_ARCHIVE_ROOT_MISSING
    return err(
        code,
        name,
        "archive root does not exist or is not a directory",
        details={"root_label": root_label},
        http_status=500,
    )


def layout_unexpected(rel_path: str, detail: str) -> ArchiveError:
    code, name = E_LAYOUT_UNEXPECTED
    return err(code, name, detail, rel_path=rel_path)


def wal_name_invalid(rel_path: str, detail: str) -> ArchiveError:
    code, name = E_WAL_NAME_INVALID
    return err(code, name, detail, rel_path=rel_path)


def marker_invalid(rel_path: str, detail: str) -> ArchiveError:
    code, name = E_TIMELINE_MARKER_INVALID
    return err(code, name, detail, rel_path=rel_path)


def invalid_request(detail: str, *, details: dict[str, Any] | None = None) -> ArchiveError:
    code, name = E_INVALID_REQUEST
    return err(code, name, detail, details=details)


def unknown_instance(instance: str) -> ArchiveError:
    code, name = E_SOURCE_INSTANCE_UNKNOWN
    return err(
        code,
        name,
        "source instance is not present in the current archive view",
        http_status=404,
        details={"instance": instance},
    )


def unknown_timeline(instance: str, timeline: int) -> ArchiveError:
    code, name = E_TIMELINE_UNKNOWN
    return err(
        code,
        name,
        "timeline is not present for the source instance in the current archive view",
        http_status=404,
        details={"instance": instance, "timeline": timeline},
    )


def no_recovery_point(instance: str, timeline: int, target_time: str) -> ArchiveError:
    code, name = E_NO_RECOVERY_POINT
    return err(
        code,
        name,
        "no complete segment at or before the requested recovery target",
        http_status=404,
        details={
            "instance": instance,
            "timeline": timeline,
            "target_time": target_time,
        },
    )


def recovery_gap(gaps: list[int], instance: str, timeline: int) -> ArchiveError:
    code, name = E_RECOVERY_GAP
    return err(
        code,
        name,
        "predecessor chain has gaps; no reachable recovery point was committed",
        http_status=409,
        details={
            "instance": instance,
            "timeline": timeline,
            "gaps": gaps,
        },
    )
