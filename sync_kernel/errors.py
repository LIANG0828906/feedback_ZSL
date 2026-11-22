from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_VER = "E_VER"
E_DECODE = "E_DECODE"
E_SEMANTIC = "E_SEMANTIC"
E_SEQ = "E_SEQ"
E_LOSSY = "E_LOSSY"
E_RESOURCE = "E_RESOURCE"


@dataclass(frozen=True)
class KernelIssue:
    code: str
    message: str
    path: str = "$"
    source_version: int | str | None = None
    object_id: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "path": self.path,
        }
        if self.source_version is not None:
            result["source_version"] = self.source_version
        if self.object_id is not None:
            result["object_id"] = self.object_id
        if self.details:
            result["details"] = self.details
        return result


class KernelError(Exception):
    def __init__(
        self,
        code: str,
        message: str,
        *,
        path: str = "$",
        source_version: int | str | None = None,
        object_id: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.issue = KernelIssue(
            code=code,
            message=message,
            path=path,
            source_version=source_version,
            object_id=object_id,
            details=details or {},
        )

    @property
    def code(self) -> str:
        return self.issue.code

    def as_dict(self) -> dict[str, Any]:
        return self.issue.as_dict()

    def __str__(self) -> str:
        parts = [self.code, self.issue.message, f"path={self.issue.path}"]
        if self.issue.source_version is not None:
            parts.append(f"source_version={self.issue.source_version}")
        if self.issue.object_id is not None:
            parts.append(f"object_id={self.issue.object_id}")
        return " | ".join(parts)


def warn_lossy(
    path: str,
    message: str,
    *,
    source_version: int,
    object_id: str | None = None,
    details: dict[str, Any] | None = None,
) -> KernelIssue:
    return KernelIssue(
        code=E_LOSSY,
        message=message,
        path=path,
        source_version=source_version,
        object_id=object_id,
        details=details or {},
    )

