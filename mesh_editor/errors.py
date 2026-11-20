from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


E_PARSE_INVALID = "E_PARSE_INVALID"
E_VERSION_LOSS = "E_VERSION_LOSS"
E_COMMIT_FAILED = "E_COMMIT_FAILED"
E_VERSION_STALE = "E_VERSION_STALE"
E_RECOVERY_INVALID = "E_RECOVERY_INVALID"


@dataclass
class MeshEditorError(Exception):
    code: str
    message: str
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    def to_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.file is not None:
            result["file"] = self.file
        if self.line is not None:
            result["line"] = self.line
        if self.offset is not None:
            result["offset"] = self.offset
        if self.faces:
            result["faces"] = sorted(set(int(i) for i in self.faces))
        if self.details:
            result["details"] = self.details
        return result


@dataclass
class ParseError(MeshEditorError):
    code: str = E_PARSE_INVALID
    message: str = "invalid mesh file"
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)


@dataclass
class VersionLossError(MeshEditorError):
    code: str = E_VERSION_LOSS
    message: str = "target format cannot represent every mesh attribute"
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)


@dataclass
class CommitError(MeshEditorError):
    code: str = E_COMMIT_FAILED
    message: str = "atomic commit failed"
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)


@dataclass
class StaleVersionError(MeshEditorError):
    code: str = E_VERSION_STALE
    message: str = "version is stale or its content hash does not match"
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)


@dataclass
class RecoveryError(MeshEditorError):
    code: str = E_RECOVERY_INVALID
    message: str = "recovery could not prove a committed state"
    file: str | None = None
    line: int | None = None
    offset: int | None = None
    faces: list[int] = field(default_factory=list)
    details: dict[str, Any] = field(default_factory=dict)


class EditCancelled(Exception):
    """Raised cooperatively when a background edit is cancelled."""

