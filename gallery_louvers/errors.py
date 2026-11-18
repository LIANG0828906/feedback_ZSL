"""Business error codes and structured validation failures."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any


class ErrorCode:
    MESH_INDEX_OUT_OF_RANGE = "GLV-G001"
    DEGENERATE_TRIANGLE = "GLV-G002"
    THICKNESS_OUT_OF_RANGE = "GLV-G003"
    LEAF_ID_MISSING_OR_DUPLICATE = "GLV-G004"
    LAYER_ID_MISSING_OR_DUPLICATE = "GLV-G005"
    ZERO_BRUSH_DIRECTION = "GLV-G006"
    SEED_MISSING_OR_INVALID = "GLV-G007"
    LIGHT_INVALID = "GLV-G008"
    CAMERA_INVALID = "GLV-G009"
    RECEIVER_INVALID = "GLV-G010"
    SCENE_SCHEMA_INVALID = "GLV-G011"
    GEOMETRY_BOUNDS_INVALID = "GLV-G012"
    RENDER_CONFIG_INVALID = "GLV-G013"
    SNAPSHOT_INVALID = "GLV-S001"
    RESUME_MISMATCH = "GLV-S002"
    IO_FAILURE = "GLV-S003"


@dataclass(frozen=True)
class ValidationIssue:
    code: str
    message: str
    layer: int | None = None
    leaf: int | None = None
    path: str = ""
    context: dict[str, Any] | None = None

    def as_dict(self) -> dict[str, Any]:
        record: dict[str, Any] = {
            "severity": "error",
            "code": self.code,
            "layer": self.layer,
            "leaf": self.leaf,
            "path": self.path,
            "message": self.message,
        }
        if self.context:
            record["context"] = self.context
        return record


class SceneValidationError(ValueError):
    """Raised after all recoverable scene validation errors are collected."""

    exit_code = 2

    def __init__(self, issues: list[ValidationIssue]):
        self.issues = issues
        super().__init__("; ".join(f"{i.code}: {i.message}" for i in issues))

    def as_dicts(self) -> list[dict[str, Any]]:
        return [issue.as_dict() for issue in self.issues]


class SnapshotError(RuntimeError):
    """Base error for snapshot and resume operations."""


class SnapshotInvalid(SnapshotError):
    code = ErrorCode.SNAPSHOT_INVALID


class ResumeMismatch(SnapshotError):
    code = ErrorCode.RESUME_MISMATCH
