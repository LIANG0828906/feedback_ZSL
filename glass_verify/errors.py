from __future__ import annotations

from dataclasses import dataclass
from typing import Any


@dataclass
class SceneIssue:
    code: str
    reason: str
    sector_id: int | None = None
    pane_id: int | None = None
    path: str = "$"
    value: Any = None

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "sector_id": self.sector_id,
            "pane_id": self.pane_id,
            "reason": self.reason,
            "path": self.path,
        }
        if self.value is not None:
            result["value"] = self.value
        return result


class SceneValidationError(Exception):
    """One or more scene validation failures; the scene is rejected as a whole."""

    def __init__(self, issues: list[SceneIssue]):
        self.issues = issues
        super().__init__("; ".join(issue.reason for issue in issues))

    def as_dict(self) -> dict[str, Any]:
        return {
            "ok": False,
            "error_kind": "scene_validation",
            "errors": [issue.as_dict() for issue in self.issues],
        }


class SnapshotError(Exception):
    """A snapshot cannot be trusted and must be discarded."""

