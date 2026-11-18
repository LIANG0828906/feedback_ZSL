"""Business errors and stable error codes."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any


@dataclass(frozen=True)
class BusinessError:
    code: str
    component_id: str
    message: str
    path: str
    details: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        if value["details"] is None:
            del value["details"]
        return value


class SceneValidationError(Exception):
    """Raised when one or more scene validation rules fail."""

    def __init__(self, errors: list[BusinessError]):
        self.errors = errors
        super().__init__(f"{len(errors)} scene validation error(s)")

    def payload(self) -> dict[str, Any]:
        return {
            "status": "error",
            "kind": "scene_validation",
            "errors": [error.to_dict() for error in self.errors],
        }


class RenderMismatchError(Exception):
    """Raised when a resumed result differs from the supplied reference."""

    def __init__(self, diffs: list[dict[str, Any]]):
        self.diffs = diffs
        super().__init__(f"{len(diffs)} deterministic output difference(s)")

    def payload(self) -> dict[str, Any]:
        return {
            "status": "error",
            "kind": "resume_mismatch",
            "code": "AQ-RESUME-001",
            "diffs": self.diffs,
        }


class SnapshotResetRequired(Exception):
    """Internal control flow for an unusable snapshot."""

    def __init__(self, reason: str):
        self.reason = reason
        super().__init__(reason)


# Stable business error catalogue.
ERR_MESH_INDEX = "AQ-GEO-001"
ERR_DEGENERATE = "AQ-GEO-002"
ERR_MESH_INVALID = "AQ-GEO-003"
ERR_THICKNESS = "AQ-MAT-001"
ERR_TRANSMISSION = "AQ-MAT-002"
ERR_DUPLICATE_ID = "AQ-ID-001"
ERR_NEGATIVE_ABSORPTION = "AQ-WAT-001"
ERR_LIGHT_INSIDE = "AQ-LGT-001"
ERR_LAYER_SET = "AQ-LAY-001"
ERR_LIGHT_SET = "AQ-LGT-002"
ERR_CAMERA = "AQ-CAM-001"
ERR_RECEIVER = "AQ-REC-001"
ERR_FISH = "AQ-FSH-001"
ERR_SCHEMA = "AQ-SCH-001"
ERR_IO = "AQ-IO-001"
ERR_SNAPSHOT = "AQ-SNP-001"
ERR_RESUME_MISMATCH = "AQ-RESUME-001"

